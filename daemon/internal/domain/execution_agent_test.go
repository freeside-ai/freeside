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
		LineupRevision:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		EnrollmentID:         "enroll-1",
		EnrollmentGeneration: 1,
		StoreManifestDigest:  manifestDigest,
		EffectiveEgress:      []string{"chatgpt.com"},
		Attended:             true,
	}
}

// codexInstructionInput is the shared admission input with its stage inputs
// rebuilt to carry the explicit codex vendor-instruction binding every
// agent-bound (v4) admission must record.
func codexInstructionInput(t *testing.T) domain.ExecutionAdmissionInput {
	t.Helper()
	in := admissionInput()
	snapshot := *in.StageInputs
	snapshot.VendorInstructions = &domain.VendorInstructionSnapshot{
		Vendor:   domain.AgentVendorCodex,
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
		{"no enrollment", func(b *domain.AdmissionAgentBinding) { b.EnrollmentID = "" }, domain.ErrEmptyID},
		{"zero generation", func(b *domain.AdmissionAgentBinding) { b.EnrollmentGeneration = 0 }, domain.ErrNonPositive},
		{"no egress", func(b *domain.AdmissionAgentBinding) { b.EffectiveEgress = nil }, domain.ErrEmptyField},
		{"empty authority", func(b *domain.AdmissionAgentBinding) { b.EffectiveEgress = []string{""} }, domain.ErrEmptyField},
		{"unsorted egress", func(b *domain.AdmissionAgentBinding) {
			b.EffectiveEgress = []string{"chatgpt.com", "auth.openai.com"}
		}, domain.ErrKeysNotCanonical},
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
	t.Run("attended flag must agree with the mode", func(t *testing.T) {
		in := admissionInput()
		binding := agentBinding()
		binding.Attended = false // mode is attended_dev
		in.AgentBinding = &binding
		if _, err := domain.NewExecutionAdmission(in); !errors.Is(err, domain.ErrAdmissionInconsistent) {
			t.Fatalf("NewExecutionAdmission = %v, want %v", err, domain.ErrAdmissionInconsistent)
		}
	})
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
	resolution := agentResolution(t)
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
	in := codexInstructionInput(t)
	identity := domain.AuthIdentityID(resolution.Enrollment.AuthIdentityID)
	in.AuthIdentityID = &identity
	in.CredentialMode = resolution.Enrollment.CredentialMode
	in.AgentBinding = &domain.AdmissionAgentBinding{
		AgentDigest:          agent.Digest,
		LaunchDigest:         launch.Digest,
		LineupRevision:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		EnrollmentID:         resolution.Enrollment.ID,
		EnrollmentGeneration: generationEntry.Ordinal,
		StoreManifestDigest:  generationEntry.StoreManifestDigest,
		EffectiveEgress:      resolution.Route.InferenceAuthorities,
		Attended:             true,
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
		enrollment, generationEntry); err != nil {
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
				enrollment, generationEntry)
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
			enrollment, generationEntry)
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
			enrollment, generationEntry)
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
			enrollment, generationEntry)
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
			enrollment, generationEntry)
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
			enrollment, generationEntry)
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
			enrollment, generationEntry)
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
			enrollment, generationEntry)
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
			enrollment, generationEntry)
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateAdmissionAgentDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})
}
