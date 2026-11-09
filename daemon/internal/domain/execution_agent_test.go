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

// selectionClosure builds one coherent selection-admitted closure around the
// shared agent resolution's enrollment, adapter, and route: the selection
// resolved under the bridge (no catalog), the stage's launch, and a v4
// admission whose binding names the selection as its agent digest.
func selectionClosure(t *testing.T) (
	domain.ExecutionAdmission, domain.AgentSelection, domain.AdapterFragment, domain.RouteFragment,
	domain.LaunchSpec, domain.ClientEnrollment, domain.EnrollmentGeneration, domain.AuthIdentity,
) {
	t.Helper()
	resolution := selectionInput(t)
	resolution.Catalog = nil
	route := routeFragment(t)
	selection, err := domain.ResolveAgentSelection(resolution)
	if err != nil {
		t.Fatal(err)
	}
	generationEntry := generation()
	launch := launchSpec(t)
	launch.AuxiliaryInference = domain.AuxiliaryObserved
	launch.Digest = mustComputeDigest(t, launch.ComputeDigest)
	treatment, err := domain.SelectionTreatmentDigest(route, resolution.Adapter, launch.Digest, selection)
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
		EffectiveEgress:      route.InferenceAuthorities,
		RouteModelID:         model, NativeEffort: nativeEffort,
		SelectionDigest: selection.Digest, SelectionModelID: selection.ModelID, SelectionEffort: selection.Effort,
		HarnessClient: resolution.Adapter.ClientKind,
		LineageGroup:  domain.LineageGroupOf(resolution.Adapter.ClientKind, selection.ModelID),
		CostOwner:     resolution.Identity.CostOwner,
	}
	return mustAdmission(t, in), selection, resolution.Adapter, route, launch,
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
