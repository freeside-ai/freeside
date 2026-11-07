package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func adapterConformanceRecord(t *testing.T) domain.AdapterConformance {
	t.Helper()
	adapter := adapterFragment(t)
	record, err := domain.NewAdapterConformance(domain.AdapterConformanceInput{
		AdapterDigest: adapter.Digest,
		Outcome:       domain.ConformancePassed,
		ProvedCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
			domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput,
			domain.LaunchCapContextSeverance, domain.LaunchCapRouteStoreContract,
			domain.LaunchCapAuxiliaryInferenceControl,
		),
		ProvedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestAdapterConformanceValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.AdapterConformance)
		wantErr error
	}{
		{"valid", func(*domain.AdapterConformance) {}, nil},
		{"digest is a name", func(c *domain.AdapterConformance) { c.AdapterDigest = "codex_proto_v1" }, domain.ErrInvalidDigest},
		{"unknown outcome", func(c *domain.AdapterConformance) { c.Outcome = "mostly" }, domain.ErrInvalidConformanceOutcome},
		{"unknown capability", func(c *domain.AdapterConformance) {
			c.ProvedCapabilities = domain.LaunchCapabilitySet{"telepathy"}
		}, domain.ErrInvalidLaunchCapability},
		{
			// A failed pass proves nothing; letting it keep capabilities
			// would let a broken adapter keep admitting.
			"capabilities on a failed pass",
			func(c *domain.AdapterConformance) { c.Outcome = domain.ConformanceFailed },
			domain.ErrConformanceCapabilitiesWithoutPass,
		},
		{"no proved_at", func(c *domain.AdapterConformance) { c.ProvedAt = time.Time{} }, domain.ErrMissingTimestamp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := adapterConformanceRecord(t)
			tc.mutate(&record)
			if err := record.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateAdapterLaunchCoverage is the acceptance fixture: a launch whose
// required capabilities exceed the record's proved set fails admission with a
// typed error.
func TestValidateAdapterLaunchCoverage(t *testing.T) {
	record := adapterConformanceRecord(t)
	adapter := adapterFragment(t)
	launch := launchSpec(t)
	if err := domain.ValidateAdapterLaunchCoverage(record, adapter.Digest, launch); err != nil {
		t.Fatalf("covered launch = %v", err)
	}
	t.Run("capabilities beyond the proved set", func(t *testing.T) {
		narrow := record
		narrow.ProvedCapabilities = domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapInstructionDelivery,
		)
		err := domain.ValidateAdapterLaunchCoverage(narrow, adapter.Digest, launch)
		if !errors.Is(err, domain.ErrLaunchCapabilityUnproved) {
			t.Fatalf("uncovered launch = %v, want %v", err, domain.ErrLaunchCapabilityUnproved)
		}
	})
	t.Run("record for another build", func(t *testing.T) {
		other := adapterFragment(t)
		other.HarnessBuild = "codex-cli 0.30.0"
		digest, err := other.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		err = domain.ValidateAdapterLaunchCoverage(record, digest, launch)
		if !errors.Is(err, domain.ErrLaunchCapabilityUnproved) {
			t.Fatalf("foreign build = %v, want %v", err, domain.ErrLaunchCapabilityUnproved)
		}
	})
	t.Run("superseded record", func(t *testing.T) {
		superseded := record
		superseded.Outcome = domain.ConformanceSuperseded
		superseded.ProvedCapabilities = nil
		err := domain.ValidateAdapterLaunchCoverage(superseded, adapter.Digest, launch)
		if !errors.Is(err, domain.ErrLaunchCapabilityUnproved) {
			t.Fatalf("superseded record = %v, want %v", err, domain.ErrLaunchCapabilityUnproved)
		}
	})
}

func agentBinding() domain.AdmissionAgentBinding {
	return domain.AdmissionAgentBinding{
		AgentDigest:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		LaunchDigest:         "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreatmentDigest:      "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		PricingRevision:      "pricing-2026-01",
		LineupRevision:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		EnrollmentID:         "enroll-1",
		EnrollmentGeneration: 1,
		StoreManifestDigest:  manifestDigest,
		EffectiveEgress:      []string{"chatgpt.com"},
	}
}

// codexInstructionInput is the shared admission input with its stage inputs
// rebuilt to carry the explicit codex vendor-instruction binding every
// agent-bound (v4) admission must record.
func codexInstructionInput(t *testing.T) domain.ExecutionAdmissionInput {
	t.Helper()
	return vendorInstructionInput(t, domain.AgentVendorCodex)
}

func vendorInstructionInput(t *testing.T, vendor domain.AgentVendor) domain.ExecutionAdmissionInput {
	t.Helper()
	in := admissionInput()
	snapshot := *in.StageInputs
	snapshot.VendorInstructions = &domain.VendorInstructionSnapshot{
		Vendor:   vendor,
		Delivery: domain.VendorInstructionDeliveryAppendFile,
	}
	id, err := snapshot.ComputeID()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ID = id
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	in.StageInputs = &snapshot
	return in
}

func TestAdmissionAgentBindingValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.AdmissionAgentBinding)
		wantErr error
	}{
		{"valid", func(*domain.AdmissionAgentBinding) {}, nil},
		{"agent digest is a name", func(b *domain.AdmissionAgentBinding) { b.AgentDigest = "sol-via-codex" }, domain.ErrInvalidDigest},
		{"treatment digest is a name", func(b *domain.AdmissionAgentBinding) { b.TreatmentDigest = "treatment-a" }, domain.ErrInvalidDigest},
		{"no pricing revision", func(b *domain.AdmissionAgentBinding) { b.PricingRevision = "" }, domain.ErrEmptyField},
		{"no enrollment", func(b *domain.AdmissionAgentBinding) { b.EnrollmentID = "" }, domain.ErrEmptyID},
		{"zero generation", func(b *domain.AdmissionAgentBinding) { b.EnrollmentGeneration = 0 }, domain.ErrNonPositive},
		{"no egress", func(b *domain.AdmissionAgentBinding) { b.EffectiveEgress = nil }, domain.ErrEmptyField},
		{"empty authority", func(b *domain.AdmissionAgentBinding) { b.EffectiveEgress = []string{""} }, domain.ErrEmptyField},
		{"unsorted egress", func(b *domain.AdmissionAgentBinding) {
			b.EffectiveEgress = []string{"chatgpt.com", "auth.openai.com"}
		}, domain.ErrKeysNotCanonical},
		{"explicit model and effort", func(b *domain.AdmissionAgentBinding) {
			b.RouteModelID, b.RequestedEffort, b.NativeEffort = "claude-opus-5-5", domain.EffortMax, "max"
		}, nil},
		{"requested effort without native", func(b *domain.AdmissionAgentBinding) {
			b.RequestedEffort = domain.EffortMax
		}, domain.ErrEmptyField},
		{"native effort without requested", func(b *domain.AdmissionAgentBinding) {
			b.NativeEffort = "max"
		}, domain.ErrEmptyField},
		{"unknown requested effort", func(b *domain.AdmissionAgentBinding) {
			b.RequestedEffort, b.NativeEffort = "xhigh", "xhigh"
		}, domain.ErrInvalidEffortLevel},
		{"harness_default recorded as a sent effort", func(b *domain.AdmissionAgentBinding) {
			b.RequestedEffort, b.NativeEffort = domain.EffortHarnessDefault, "high"
		}, domain.ErrInvalidEffortLevel},
		// The valid case above carries no source: a binding admitted before
		// the field existed must keep validating.
		{"lineup source", func(b *domain.AdmissionAgentBinding) {
			b.SelectionSource = domain.AgentSelectionSourceLineup
		}, nil},
		{"lineup source with a record", func(b *domain.AdmissionAgentBinding) {
			b.SelectionSource, b.SelectionRecordID = domain.AgentSelectionSourceLineup, manifestDigest
		}, domain.ErrAgentSelectionInconsistent},
		{"task-line source", func(b *domain.AdmissionAgentBinding) {
			b.SelectionSource, b.SelectionRecordID = domain.AgentSelectionSourceTaskLine, manifestDigest
		}, nil},
		{"task-line source without a record", func(b *domain.AdmissionAgentBinding) {
			b.SelectionSource = domain.AgentSelectionSourceTaskLine
		}, domain.ErrInvalidDigest},
		{"card source", func(b *domain.AdmissionAgentBinding) {
			b.SelectionSource, b.SelectionRecordID = domain.AgentSelectionSourceCard, manifestDigest
		}, nil},
		{"card source with a malformed record", func(b *domain.AdmissionAgentBinding) {
			b.SelectionSource, b.SelectionRecordID = domain.AgentSelectionSourceCard, "card-1"
		}, domain.ErrInvalidDigest},
		{"record without a source", func(b *domain.AdmissionAgentBinding) {
			b.SelectionRecordID = manifestDigest
		}, domain.ErrAgentSelectionInconsistent},
		{"unknown source", func(b *domain.AdmissionAgentBinding) {
			b.SelectionSource = "intake"
		}, domain.ErrInvalidAgentSelectionSource},
		// The valid case above carries no selection either: a binding
		// admitted before the selection existed must keep validating.
		{"a whole selection", func(b *domain.AdmissionAgentBinding) {
			b.SelectionDigest, b.SelectionModelID, b.SelectionEffort = otherSelectionDigest, "gpt-5.6-sol", "xhigh"
		}, nil},
		{"a selection digest alone", func(b *domain.AdmissionAgentBinding) {
			b.SelectionDigest = manifestDigest
		}, domain.ErrAgentSelectionInconsistent},
		{"a selection with no effort", func(b *domain.AdmissionAgentBinding) {
			b.SelectionDigest, b.SelectionModelID = manifestDigest, "gpt-5.6-sol"
		}, domain.ErrAgentSelectionInconsistent},
		{"a selection model and effort with no digest", func(b *domain.AdmissionAgentBinding) {
			b.SelectionModelID, b.SelectionEffort = "gpt-5.6-sol", "xhigh"
		}, domain.ErrAgentSelectionInconsistent},
		{"a selection digest that is a name", func(b *domain.AdmissionAgentBinding) {
			b.SelectionDigest, b.SelectionModelID, b.SelectionEffort = "sol-xhigh", "gpt-5.6-sol", "xhigh"
		}, domain.ErrInvalidDigest},
		// Admitted under a selection, the agent digest is the selection's,
		// and neither an offer's pricing nor a Freeside effort level rides it.
		{"admitted under a selection", func(b *domain.AdmissionAgentBinding) {
			selectionAdmitted(b)
		}, nil},
		{"admitted under a selection with a pricing revision", func(b *domain.AdmissionAgentBinding) {
			selectionAdmitted(b)
			b.PricingRevision = "pricing-2026-01"
		}, domain.ErrSelectionBindingInconsistent},
		{"admitted under a selection with a requested effort", func(b *domain.AdmissionAgentBinding) {
			selectionAdmitted(b)
			b.RequestedEffort = domain.EffortMax
		}, domain.ErrSelectionBindingInconsistent},
		{"admitted under a selection at harness_default", func(b *domain.AdmissionAgentBinding) {
			selectionAdmitted(b)
			b.NativeEffort = ""
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binding := agentBinding()
			tc.mutate(&binding)
			if err := binding.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// otherSelectionDigest is a selection digest that is not agentBinding's
// agent digest, so the binding reads as admitted under an agent.
const otherSelectionDigest domain.Digest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

// selectionAdmitted turns agentBinding into a binding admitted under a
// selection: the agent digest is the selection digest, with a native effort
// and no requested level or pricing revision.
func selectionAdmitted(b *domain.AdmissionAgentBinding) {
	b.SelectionDigest, b.SelectionModelID, b.SelectionEffort = manifestDigest, "gpt-5.6-sol", "xhigh"
	b.AgentDigest = manifestDigest
	b.PricingRevision = ""
	b.RouteModelID, b.NativeEffort = "gpt-5.6-sol", "xhigh"
}

// TestAdmissionAgentEncoding pins the v4 encoding: an agent-bound admission
// round-trips under a new identity, a legacy admission's identity is
// untouched by the field's existence, and the versioned canonical encodings
// stay distinct.
func TestAdmissionAgentEncoding(t *testing.T) {
	legacy := mustAdmission(t, admissionInput())

	bound := codexInstructionInput(t)
	binding := agentBinding()
	bound.AgentBinding = &binding
	v4 := mustAdmission(t, bound)

	if v4.ID == legacy.ID {
		t.Fatal("agent binding did not change the admission identity")
	}
	if err := v4.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(v4)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.ExecutionAdmission
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded v4 admission: %v", err)
	}
	if decoded.AgentBinding == nil || decoded.AgentBinding.AgentDigest != binding.AgentDigest {
		t.Fatalf("round-trip lost the binding: %+v", decoded.AgentBinding)
	}

	// The legacy encoding is untouched: a body written before the field
	// existed decodes to the same identity it was written with (the permanent
	// legacy reader's substrate).
	legacyBody, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var legacyDecoded domain.ExecutionAdmission
	if err := json.Unmarshal(legacyBody, &legacyDecoded); err != nil {
		t.Fatal(err)
	}
	if err := legacyDecoded.Validate(); err != nil {
		t.Fatalf("legacy admission: %v", err)
	}
	if legacyDecoded.ID != legacy.ID {
		t.Fatal("legacy identity moved")
	}

	// Canonicalization: an unsorted egress list converges on the same body
	// and identity.
	shuffled := codexInstructionInput(t)
	shuffledBinding := agentBinding()
	shuffledBinding.EffectiveEgress = []string{"chatgpt.com", "chatgpt.com"}
	shuffled.AgentBinding = &shuffledBinding
	again := mustAdmission(t, shuffled)
	if again.ID != v4.ID {
		t.Fatalf("canonicalized replay diverged: %s vs %s", again.ID, v4.ID)
	}
}

func TestAdmissionAgentBindingCrossChecks(t *testing.T) {
	t.Run("agent binding requires instruction provenance", func(t *testing.T) {
		// All three provenance-less shapes are valid history for a legacy
		// admission and incoherent for a newly written v4 one: absent stage
		// inputs, a pre-v2 snapshot with no vendor-instruction claim, and
		// the historical Claude v2 implicit delivery.
		for name, mutate := range map[string]func(*domain.ExecutionAdmissionInput){
			"nil stage inputs": func(in *domain.ExecutionAdmissionInput) {
				in.StageInputs = nil
			},
			"nil vendor instructions": func(*domain.ExecutionAdmissionInput) {},
			"implicit v2 delivery": func(in *domain.ExecutionAdmissionInput) {
				snapshot := *in.StageInputs
				snapshot.VendorInstructions = &domain.VendorInstructionSnapshot{
					Vendor: domain.AgentVendorClaude,
				}
				id, err := snapshot.ComputeID()
				if err != nil {
					t.Fatal(err)
				}
				snapshot.ID = id
				in.StageInputs = &snapshot
			},
		} {
			t.Run(name, func(t *testing.T) {
				in := admissionInput()
				binding := agentBinding()
				in.AgentBinding = &binding
				mutate(&in)
				if _, err := domain.NewExecutionAdmission(in); !errors.Is(err, domain.ErrAdmissionInconsistent) {
					t.Fatalf("NewExecutionAdmission = %v, want %v", err, domain.ErrAdmissionInconsistent)
				}
			})
		}
	})
	t.Run("agent binding requires an identity", func(t *testing.T) {
		in := admissionInput()
		binding := agentBinding()
		in.AgentBinding = &binding
		in.AuthIdentityID = nil
		in.EgressProfile = domain.EgressCleanVerification
		if _, err := domain.NewExecutionAdmission(in); !errors.Is(err, domain.ErrAuthIdentityInconsistent) {
			t.Fatalf("NewExecutionAdmission = %v, want %v", err, domain.ErrAuthIdentityInconsistent)
		}
	})
}

// derivationClosure builds one coherent admitted-agent closure: the resolved
// agent, its fragments, the stage's launch, the enrollment and generation,
// and a v4 admission whose derived fields agree with all of them.
func derivationClosure(t *testing.T) (
	domain.ExecutionAdmission, domain.AgentDefinition, domain.AdapterFragment,
	domain.RouteFragment, domain.OfferFragment, domain.LaunchSpec,
	domain.ClientEnrollment, domain.EnrollmentGeneration,
) {
	t.Helper()
	return derivationClosureFor(t, agentResolution(t), domain.AgentVendorCodex)
}

// derivationIdentity is the identity derivationClosure's enrollment belongs
// to.
func derivationIdentity(t *testing.T) domain.AuthIdentity {
	t.Helper()
	return agentResolution(t).Identity
}

// derivationClosureFor builds a coherent closure around one resolution, the
// binding recording the launch selection the closure derives.
func derivationClosureFor(t *testing.T, resolution domain.AgentResolutionInput, vendor domain.AgentVendor) (
	domain.ExecutionAdmission, domain.AgentDefinition, domain.AdapterFragment,
	domain.RouteFragment, domain.OfferFragment, domain.LaunchSpec,
	domain.ClientEnrollment, domain.EnrollmentGeneration,
) {
	t.Helper()
	agent, err := domain.ResolveAgentDefinition(resolution)
	if err != nil {
		t.Fatal(err)
	}
	generationEntry := generation()
	launch := launchSpec(t)
	// The shared launch fixture demands auxiliary-inference control, which
	// the shared adapter fixture does not declare; a coherent closure
	// observes instead, so the capability join holds.
	launch.AuxiliaryInference = domain.AuxiliaryObserved
	launchDigest, err := launch.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	launch.Digest = launchDigest
	selection, err := domain.DeriveAgentLaunchSelection(agent, resolution.Adapter, resolution.Offer)
	if err != nil {
		t.Fatal(err)
	}
	reduced, err := domain.ReduceAgentSelection(agent, resolution.Adapter, resolution.Offer)
	if err != nil {
		t.Fatal(err)
	}
	in := vendorInstructionInput(t, vendor)
	identity := domain.AuthIdentityID(resolution.Enrollment.AuthIdentityID)
	in.AuthIdentityID = &identity
	in.CredentialMode = resolution.Enrollment.CredentialMode
	in.AgentBinding = &domain.AdmissionAgentBinding{
		AgentDigest:          agent.Digest,
		LaunchDigest:         launch.Digest,
		TreatmentDigest:      "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		PricingRevision:      "pricing-2026-01",
		LineupRevision:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		EnrollmentID:         resolution.Enrollment.ID,
		EnrollmentGeneration: generationEntry.Ordinal,
		StoreManifestDigest:  generationEntry.StoreManifestDigest,
		EffectiveEgress:      resolution.Route.InferenceAuthorities,
		RouteModelID:         selection.RouteModelID,
		RequestedEffort:      selection.RequestedEffort,
		NativeEffort:         selection.NativeEffort,
		SelectionDigest:      reduced.Digest,
		SelectionModelID:     reduced.ModelID,
		SelectionEffort:      reduced.Effort,
		HarnessClient:        resolution.Adapter.ClientKind,
		LineageGroup:         resolution.Offer.LineageGroup,
		CostOwner:            resolution.Identity.CostOwner,
	}
	offer := resolution.Offer
	digest, err := offer.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	offer.Digest = digest
	return mustAdmission(t, in), agent, resolution.Adapter, resolution.Route,
		offer, launch, resolution.Enrollment, generationEntry
}

// TestValidateAdmissionAgentDerivations pins §5.4 admission step 5's
// reconstruction recheck: every derived field must agree with the closure the
// binding names, and each disagreement is a typed refusal.
func TestValidateAdmissionAgentDerivations(t *testing.T) {
	admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
	if err := domain.ValidateAdmissionAgentDerivations(
		admission, agent, adapter, route, offer, launch, domain.StageNameReview,
		enrollment, generationEntry, derivationIdentity(t)); err != nil {
		t.Fatalf("coherent closure = %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*domain.ExecutionAdmission, *domain.AgentDefinition,
			*domain.ClientEnrollment, *domain.EnrollmentGeneration)
	}{
		{
			"foreign agent digest",
			func(a *domain.ExecutionAdmission, agent *domain.AgentDefinition,
				_ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
			) {
				agent.Effort = domain.EffortHigh
				digest, err := agent.ComputeDigest()
				if err != nil {
					t.Fatal(err)
				}
				agent.Digest = digest
			},
		},
		{
			"identity disagrees with the enrollment",
			func(a *domain.ExecutionAdmission, _ *domain.AgentDefinition,
				e *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
			) {
				e.AuthIdentityID = "auth-2"
			},
		},
		{
			"credential mode disagrees with the enrollment",
			func(a *domain.ExecutionAdmission, _ *domain.AgentDefinition,
				e *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
			) {
				e.CredentialMode = domain.CredentialLocalTrusted
			},
		},
		{
			"generation ordinal disagrees with the binding",
			func(a *domain.ExecutionAdmission, _ *domain.AgentDefinition,
				_ *domain.ClientEnrollment, g *domain.EnrollmentGeneration,
			) {
				g.Ordinal = 2
			},
		},
		{
			"store manifest disagrees with the generation",
			func(a *domain.ExecutionAdmission, _ *domain.AgentDefinition,
				_ *domain.ClientEnrollment, g *domain.EnrollmentGeneration,
			) {
				g.StoreManifestDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
			tc.mutate(&admission, &agent, &enrollment, &generationEntry)
			err := domain.ValidateAdmissionAgentDerivations(
				admission, agent, adapter, route, offer, launch, domain.StageNameReview,
				enrollment, generationEntry, derivationIdentity(t))
			if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
				t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
					err, domain.ErrAdmissionDerivationMismatch)
			}
		})
	}

	t.Run("foreign launch digest", func(t *testing.T) {
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
		// An authentic launch the binding never named: the recheck must
		// refuse to attribute the admission to it.
		launch.OutputContract = "review-findings/v4"
		digest, err := launch.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		launch.Digest = digest
		err = domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, derivationIdentity(t))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("foreign offer digest", func(t *testing.T) {
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
		// An authentic offer the agent never pinned: the recheck must refuse
		// to attribute the admitted model to it.
		offer.RouteModelID = "gpt-5.7-sol"
		digest, err := offer.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		offer.Digest = digest
		err = domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, derivationIdentity(t))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("effort outside the offer's allowance", func(t *testing.T) {
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
		// Rebuild a coherent digest chain around an offer that no longer
		// allows the agent's effort, so only the re-run join can refuse.
		offer.AllowedEfforts = []domain.EffortLevel{domain.EffortLow}
		offerDigest, err := offer.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		offer.Digest = offerDigest
		agent.OfferDigest = offerDigest
		agentDigest, err := agent.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		agent.Digest = agentDigest
		admission.AgentBinding.AgentDigest = agentDigest
		id, err := admission.ComputeID()
		if err != nil {
			t.Fatal(err)
		}
		admission.ID = id
		err = domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, derivationIdentity(t))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("enrollment client disagrees with the adapter", func(t *testing.T) {
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
		enrollment.HarnessClient = domain.HarnessClientClaudeCode
		err := domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, derivationIdentity(t))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("instruction vendor disagrees with the adapter", func(t *testing.T) {
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
		// A valid claude binding on a codex-adapter closure: only the vendor
		// derivation can refuse.
		snapshot := *admission.StageInputs
		snapshot.VendorInstructions = &domain.VendorInstructionSnapshot{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
		}
		snapshotID, err := snapshot.ComputeID()
		if err != nil {
			t.Fatal(err)
		}
		snapshot.ID = snapshotID
		admission.StageInputs = &snapshot
		id, err := admission.ComputeID()
		if err != nil {
			t.Fatal(err)
		}
		admission.ID = id
		err = domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, derivationIdentity(t))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("launch beyond the adapter's declared capabilities", func(t *testing.T) {
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
		// Rebuild a coherent digest chain around a launch demanding
		// auxiliary-inference control the adapter does not declare, so only
		// the re-run capability join can refuse.
		launch.AuxiliaryInference = domain.AuxiliaryForbidden
		launchDigest, err := launch.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		launch.Digest = launchDigest
		admission.AgentBinding.LaunchDigest = launchDigest
		id, err := admission.ComputeID()
		if err != nil {
			t.Fatal(err)
		}
		admission.ID = id
		err = domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, derivationIdentity(t))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("launch for the wrong stage", func(t *testing.T) {
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
		// The binding's exact launch, but the admission's stage resolves to a
		// different role than the one the launch was authored for.
		err := domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameImplementation,
			enrollment, generationEntry, derivationIdentity(t))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("egress outside the route's authorities", func(t *testing.T) {
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosure(t)
		route.InferenceAuthorities = []string{"api.openai.com"}
		digest, err := route.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		route.Digest = digest
		agent.RouteDigest = digest
		agentDigest, err := agent.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		agent.Digest = agentDigest
		admission.AgentBinding.AgentDigest = agentDigest
		id, err := admission.ComputeID()
		if err != nil {
			t.Fatal(err)
		}
		admission.ID = id
		err = domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, derivationIdentity(t))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})
}

// TestValidateAdmissionAgentLaunchSelection pins the recheck of the model and
// effort a binding records: they must be exactly what the closure derives,
// so a stored model or effort is never trusted on its own.
func TestValidateAdmissionAgentLaunchSelection(t *testing.T) {
	validate := func(t *testing.T, resolution domain.AgentResolutionInput, vendor domain.AgentVendor,
		mutate func(*domain.AdmissionAgentBinding),
	) error {
		t.Helper()
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosureFor(t, resolution, vendor)
		mutate(admission.AgentBinding)
		id, err := admission.ComputeID()
		if err != nil {
			t.Fatal(err)
		}
		admission.ID = id
		return domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, resolution.Identity)
	}

	t.Run("coherent claude closures", func(t *testing.T) {
		if err := validate(t, claudeAgentResolution(t), domain.AgentVendorClaude,
			func(*domain.AdmissionAgentBinding) {}); err != nil {
			t.Fatalf("explicit model and effort = %v", err)
		}
		nativeDefault := claudeAgentResolution(t)
		nativeDefault.Offer.RouteModelID = domain.ClaudeCodeNativeDefaultRouteModelID
		nativeDefault.Offer.Digest = mustComputeDigest(t, nativeDefault.Offer.ComputeDigest)
		nativeDefault.Source.Effort = domain.EffortHarnessDefault
		if err := validate(t, nativeDefault, domain.AgentVendorClaude,
			func(b *domain.AdmissionAgentBinding) {
				if b.RouteModelID != "" || b.RequestedEffort != "" || b.NativeEffort != "" ||
					b.SelectionModelID != domain.ClaudeCodeNativeDefaultRouteModelID ||
					b.SelectionEffort != string(domain.EffortHarnessDefault) {
					t.Fatalf("native default binding records %+v", b)
				}
			}); err != nil {
			t.Fatalf("native default = %v", err)
		}
	})

	t.Run("coherent codex closure", func(t *testing.T) {
		if err := validate(t, agentResolution(t), domain.AgentVendorCodex,
			func(b *domain.AdmissionAgentBinding) {
				if b.RouteModelID != "gpt-5.6-sol" || b.RequestedEffort != domain.EffortMax || b.NativeEffort != "xhigh" ||
					b.SelectionModelID != "gpt-5.6-sol" || b.SelectionEffort != "xhigh" {
					t.Fatalf("codex binding records %+v", b)
				}
			}); err != nil {
			t.Fatalf("offer model and clamped effort = %v", err)
		}
	})

	cases := []struct {
		name       string
		resolution func(*testing.T) domain.AgentResolutionInput
		vendor     domain.AgentVendor
		mutate     func(*domain.AdmissionAgentBinding)
	}{
		{
			"claude wrong model", claudeAgentResolution, domain.AgentVendorClaude,
			func(b *domain.AdmissionAgentBinding) { b.RouteModelID = "claude-fable-5-1" },
		},
		{
			"claude reserved id recorded as a model", claudeAgentResolution, domain.AgentVendorClaude,
			func(b *domain.AdmissionAgentBinding) { b.RouteModelID = domain.ClaudeCodeNativeDefaultRouteModelID },
		},
		{
			"claude wrong requested effort", claudeAgentResolution, domain.AgentVendorClaude,
			func(b *domain.AdmissionAgentBinding) { b.RequestedEffort = domain.EffortHigh },
		},
		{
			"claude wrong native effort", claudeAgentResolution, domain.AgentVendorClaude,
			func(b *domain.AdmissionAgentBinding) { b.NativeEffort = "high" },
		},
		{
			"claude binding omits the selection", claudeAgentResolution, domain.AgentVendorClaude,
			func(b *domain.AdmissionAgentBinding) { b.RouteModelID, b.RequestedEffort, b.NativeEffort = "", "", "" },
		},
		{
			"codex wrong model", agentResolution, domain.AgentVendorCodex,
			func(b *domain.AdmissionAgentBinding) { b.RouteModelID = "gpt-5.6-luna" },
		},
		{
			"codex clamp recorded unclamped", agentResolution, domain.AgentVendorCodex,
			func(b *domain.AdmissionAgentBinding) { b.NativeEffort = "max" },
		},
		{
			// What a codex binding held while codex derived nothing.
			"codex binding omits the selection", agentResolution, domain.AgentVendorCodex,
			func(b *domain.AdmissionAgentBinding) { b.RouteModelID, b.RequestedEffort, b.NativeEffort = "", "", "" },
		},
		// The recorded selection (#2596): its digest, model, and effort must
		// be the ones the agent reduces to, and a fresh admission records one.
		{
			"codex selection digest disagrees", agentResolution, domain.AgentVendorCodex,
			func(b *domain.AdmissionAgentBinding) { b.SelectionDigest = manifestDigest },
		},
		{
			"codex selection model disagrees", agentResolution, domain.AgentVendorCodex,
			func(b *domain.AdmissionAgentBinding) { b.SelectionModelID = "gpt-5.6-luna" },
		},
		{
			"codex selection effort recorded unclamped", agentResolution, domain.AgentVendorCodex,
			func(b *domain.AdmissionAgentBinding) { b.SelectionEffort = "max" },
		},
		{
			"claude binding omits the recorded selection", claudeAgentResolution, domain.AgentVendorClaude,
			func(b *domain.AdmissionAgentBinding) {
				b.SelectionDigest, b.SelectionModelID, b.SelectionEffort = "", "", ""
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(t, tc.resolution(t), tc.vendor, tc.mutate)
			if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
				t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
					err, domain.ErrAdmissionDerivationMismatch)
			}
		})
	}
}

// TestValidateAdmissionAgentRecordedFacts pins the recheck of the run facts a
// binding records for its clients: the harness, lineage group, and cost owner
// must be the adapter's, the offer's, and the enrollment identity's, so a
// projection that reads them never shows a value the closure did not hold.
func TestValidateAdmissionAgentRecordedFacts(t *testing.T) {
	resolution := func(t *testing.T) domain.AgentResolutionInput {
		t.Helper()
		in := agentResolution(t)
		in.Offer.LineageGroup = "openai"
		in.Offer.Digest = mustComputeDigest(t, in.Offer.ComputeDigest)
		in.Identity.CostOwner = "platform-team"
		return in
	}
	validate := func(t *testing.T, in domain.AgentResolutionInput,
		mutate func(*domain.AdmissionAgentBinding, *domain.AuthIdentity),
	) error {
		t.Helper()
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := derivationClosureFor(t, in, domain.AgentVendorCodex)
		identity := in.Identity
		mutate(admission.AgentBinding, &identity)
		admission.ID = mustComputeDigest(t, admission.ComputeID)
		return domain.ValidateAdmissionAgentDerivations(
			admission, agent, adapter, route, offer, launch, domain.StageNameReview,
			enrollment, generationEntry, identity)
	}
	if err := validate(t, resolution(t), func(b *domain.AdmissionAgentBinding, _ *domain.AuthIdentity) {
		if b.HarnessClient != domain.HarnessClientCodexCLI || b.LineageGroup != "openai" ||
			b.CostOwner != "platform-team" {
			t.Fatalf("binding records %+v", b)
		}
	}); err != nil {
		t.Fatalf("coherent closure = %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*domain.AdmissionAgentBinding, *domain.AuthIdentity)
	}{
		{"harness omitted", func(b *domain.AdmissionAgentBinding, _ *domain.AuthIdentity) { b.HarnessClient = "" }},
		{"foreign harness", func(b *domain.AdmissionAgentBinding, _ *domain.AuthIdentity) {
			b.HarnessClient = domain.HarnessClientClaudeCode
		}},
		{"lineage omitted", func(b *domain.AdmissionAgentBinding, _ *domain.AuthIdentity) { b.LineageGroup = "" }},
		{"foreign lineage", func(b *domain.AdmissionAgentBinding, _ *domain.AuthIdentity) { b.LineageGroup = "anthropic" }},
		{"cost owner omitted", func(b *domain.AdmissionAgentBinding, _ *domain.AuthIdentity) { b.CostOwner = "" }},
		{"cost owner moved", func(_ *domain.AdmissionAgentBinding, i *domain.AuthIdentity) { i.CostOwner = "research" }},
		{"identity not the enrollment's", func(_ *domain.AdmissionAgentBinding, i *domain.AuthIdentity) { i.ID = "auth-2" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(t, resolution(t), tc.mutate)
			if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
				t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
					err, domain.ErrAdmissionDerivationMismatch)
			}
		})
	}
}

// selectionClosure builds one coherent selection-admitted closure around the
// shared agent resolution's enrollment, adapter, and route: the selection
// resolved under the bridge (no catalog), the stage's launch, and a v4
// admission whose binding names the selection as its agent digest.
func selectionClosure(t *testing.T) (
	domain.ExecutionAdmission, domain.AgentSelection, domain.AdapterFragment, domain.RouteFragment,
	domain.LaunchSpec, domain.ClientEnrollment, domain.EnrollmentGeneration, domain.AuthIdentity,
) {
	t.Helper()
	resolution := agentResolution(t)
	selection, err := domain.ResolveAgentSelection(domain.AgentSelectionInput{
		Names: domain.AgentSelectionNames{
			Enrollment: string(resolution.Enrollment.ID), Model: resolution.Offer.RouteModelID, Effort: "xhigh",
		},
		Identity: resolution.Identity, Enrollment: resolution.Enrollment, Adapter: resolution.Adapter,
	})
	if err != nil {
		t.Fatal(err)
	}
	generationEntry := generation()
	launch := launchSpec(t)
	launch.AuxiliaryInference = domain.AuxiliaryObserved
	launch.Digest = mustComputeDigest(t, launch.ComputeDigest)
	treatment, err := domain.SelectionTreatmentDigest(resolution.Route, resolution.Adapter, launch.Digest, selection)
	if err != nil {
		t.Fatal(err)
	}
	model, nativeEffort := selection.LaunchValues(resolution.Adapter.ClientKind)
	in := vendorInstructionInput(t, domain.AgentVendorCodex)
	identity := domain.AuthIdentityID(resolution.Enrollment.AuthIdentityID)
	in.AuthIdentityID = &identity
	in.CredentialMode = resolution.Enrollment.CredentialMode
	in.AgentBinding = &domain.AdmissionAgentBinding{
		AgentDigest: selection.Digest, LaunchDigest: launch.Digest, TreatmentDigest: treatment,
		LineupRevision:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		EnrollmentID:         resolution.Enrollment.ID,
		EnrollmentGeneration: generationEntry.Ordinal,
		StoreManifestDigest:  generationEntry.StoreManifestDigest,
		EffectiveEgress:      resolution.Route.InferenceAuthorities,
		RouteModelID:         model, NativeEffort: nativeEffort,
		SelectionDigest: selection.Digest, SelectionModelID: selection.ModelID, SelectionEffort: selection.Effort,
		HarnessClient: resolution.Adapter.ClientKind,
		LineageGroup:  domain.LineageGroupOf(resolution.Adapter.ClientKind, selection.ModelID),
		CostOwner:     resolution.Identity.CostOwner,
	}
	return mustAdmission(t, in), selection, resolution.Adapter, resolution.Route, launch,
		resolution.Enrollment, generationEntry, resolution.Identity
}

// TestValidateAdmissionSelectionDerivations pins the recheck of a binding
// admitted under a selection: every recorded field must be what the
// selection's closure derives, and a binding admitted under an agent never
// passes as one admitted under a selection.
func TestValidateAdmissionSelectionDerivations(t *testing.T) {
	admission, selection, adapter, route, launch, enrollment, generationEntry, identity := selectionClosure(t)
	if err := domain.ValidateAdmissionSelectionDerivations(
		admission, selection, adapter, route, launch, domain.StageNameReview,
		enrollment, generationEntry, identity); err != nil {
		t.Fatalf("coherent closure = %v", err)
	}
	if got := admission.AgentBinding.LineageGroup; got != domain.LineageGroupOpenAI {
		t.Fatalf("derived lineage group %q, want %q", got, domain.LineageGroupOpenAI)
	}

	cases := []struct {
		name   string
		mutate func(*domain.AdmissionAgentBinding, *domain.ClientEnrollment, *domain.EnrollmentGeneration)
	}{
		{"agent digest names an agent", func(b *domain.AdmissionAgentBinding, _ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration) {
			b.AgentDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			b.PricingRevision, b.RequestedEffort = "pricing-2026-01", domain.EffortMax
		}},
		{"treatment of another model", func(b *domain.AdmissionAgentBinding, _ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration) {
			b.TreatmentDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		}},
		{"launch passes another model", func(b *domain.AdmissionAgentBinding, _ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration) {
			b.RouteModelID = "gpt-5.7"
		}},
		{"guessed lineage group", func(b *domain.AdmissionAgentBinding, _ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration) {
			b.LineageGroup = domain.LineageGroupAnthropic
		}},
		{"egress outside the route", func(b *domain.AdmissionAgentBinding, _ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration) {
			b.EffectiveEgress = []string{"api.example.com"}
		}},
		{"identity disagrees with the enrollment", func(_ *domain.AdmissionAgentBinding, e *domain.ClientEnrollment, _ *domain.EnrollmentGeneration) {
			e.AuthIdentityID = "auth-2"
		}},
		{"generation ordinal disagrees with the binding", func(_ *domain.AdmissionAgentBinding, _ *domain.ClientEnrollment, g *domain.EnrollmentGeneration) {
			g.Ordinal = 2
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admission, selection, adapter, route, launch, enrollment, generationEntry, identity := selectionClosure(t)
			tc.mutate(admission.AgentBinding, &enrollment, &generationEntry)
			admission.ID = mustComputeDigest(t, admission.ComputeID)
			err := domain.ValidateAdmissionSelectionDerivations(
				admission, selection, adapter, route, launch, domain.StageNameReview,
				enrollment, generationEntry, identity)
			if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
				t.Fatalf("ValidateAdmissionSelectionDerivations = %v, want a derivation mismatch", err)
			}
		})
	}
}
