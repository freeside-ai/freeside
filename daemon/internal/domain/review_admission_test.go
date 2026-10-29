package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// reviewAdmissionClosure builds a coherent review snapshot over the shared
// derivation closure. The launch keeps the review launch's forbidden
// auxiliary inference, which the adapter fixture does not declare: a review
// snapshot is not held to launch coverage.
func reviewAdmissionClosure(t *testing.T) (
	domain.ReviewAdmission, domain.AgentDefinition, domain.AdapterFragment,
	domain.RouteFragment, domain.OfferFragment, domain.LaunchSpec,
	domain.ClientEnrollment, domain.EnrollmentGeneration,
) {
	t.Helper()
	execution, agent, adapter, route, offer, _, enrollment, generationEntry := derivationClosure(t)
	launch := launchSpec(t)
	binding := *execution.AgentBinding
	binding.LaunchDigest = launch.Digest
	binding.SelectionSource = domain.AgentSelectionSourceLineup
	admission, err := domain.NewReviewAdmission(domain.ReviewAdmission{
		InvocationID: "review-run-1-1", RunID: "run-1", Role: domain.RoleReviewer,
		AuthIdentityID: enrollment.AuthIdentityID, LineageGroup: offer.LineageGroup,
		AgentBinding: binding, AdmittedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return admission, agent, adapter, route, offer, launch, enrollment, generationEntry
}

func TestReviewAdmissionValidate(t *testing.T) {
	t.Parallel()
	valid, _, _, _, _, _, _, _ := reviewAdmissionClosure(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid review admission = %v", err)
	}
	shadow := valid
	shadow.Role = domain.RoleShadowReviewer
	if err := shadow.Validate(); err != nil {
		t.Fatalf("shadow review admission = %v", err)
	}
	// Unknown lineage is recorded, not refused.
	unknown := valid
	unknown.LineageGroup = ""
	if err := unknown.Validate(); err != nil {
		t.Fatalf("review admission of unknown lineage = %v", err)
	}
	// A task line selects a review agent, citing the line by id.
	taskLine := valid
	taskLine.AgentBinding.SelectionSource = domain.AgentSelectionSourceTaskLine
	taskLine.AgentBinding.SelectionRecordID = taskLine.AgentBinding.AgentDigest
	if err := taskLine.Validate(); err != nil {
		t.Fatalf("task line review admission = %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*domain.ReviewAdmission)
		want   error
	}{
		{"no invocation", func(a *domain.ReviewAdmission) { a.InvocationID = "" }, domain.ErrEmptyID},
		{"no run", func(a *domain.ReviewAdmission) { a.RunID = "" }, domain.ErrEmptyID},
		{"writer role", func(a *domain.ReviewAdmission) { a.Role = domain.RoleImplementer }, domain.ErrInvalidRoleName},
		{"no role", func(a *domain.ReviewAdmission) { a.Role = "" }, domain.ErrInvalidRoleName},
		{"no identity", func(a *domain.ReviewAdmission) { a.AuthIdentityID = "" }, domain.ErrEmptyID},
		{"no time", func(a *domain.ReviewAdmission) { a.AdmittedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"local time", func(a *domain.ReviewAdmission) {
			a.AdmittedAt = a.AdmittedAt.In(time.FixedZone("x", 3600))
		}, domain.ErrTimestampNotUTC},
		{"malformed binding", func(a *domain.ReviewAdmission) { a.AgentBinding.AgentDigest = "nope" }, domain.ErrInvalidDigest},
		{"no model", func(a *domain.ReviewAdmission) { a.AgentBinding.RouteModelID = "" }, domain.ErrEmptyField},
		{"no effort", func(a *domain.ReviewAdmission) {
			a.AgentBinding.RequestedEffort, a.AgentBinding.NativeEffort = "", ""
		}, domain.ErrEmptyField},
		{"card selection", func(a *domain.ReviewAdmission) {
			a.AgentBinding.SelectionSource = domain.AgentSelectionSourceCard
			a.AgentBinding.SelectionRecordID = a.AgentBinding.AgentDigest
		}, domain.ErrReviewAdmissionInvalid},
		{"task line selection without a record", func(a *domain.ReviewAdmission) {
			a.AgentBinding.SelectionSource = domain.AgentSelectionSourceTaskLine
		}, domain.ErrInvalidDigest},
		{"no selection source", func(a *domain.ReviewAdmission) { a.AgentBinding.SelectionSource = "" }, domain.ErrReviewAdmissionInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			admission := valid
			tc.mutate(&admission)
			if err := admission.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewReviewAdmissionCanonicalizes(t *testing.T) {
	t.Parallel()
	valid, _, _, _, _, _, _, _ := reviewAdmissionClosure(t)
	in := valid
	in.AdmittedAt = valid.AdmittedAt.In(time.FixedZone("x", 3600))
	in.AgentBinding.EffectiveEgress = []string{"b.example:443", "a.example:443", "b.example:443"}
	got, err := domain.NewReviewAdmission(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.AdmittedAt.Location() != time.UTC || !got.AdmittedAt.Equal(valid.AdmittedAt) {
		t.Fatalf("admitted_at = %v, want %v in UTC", got.AdmittedAt, valid.AdmittedAt)
	}
	if egress := got.AgentBinding.EffectiveEgress; len(egress) != 2 || egress[0] != "a.example:443" {
		t.Fatalf("effective egress = %v, want it sorted and deduplicated", egress)
	}
	if len(in.AgentBinding.EffectiveEgress) != 3 {
		t.Fatal("NewReviewAdmission rewrote the caller's egress list")
	}
}

func TestReviewAdmissionEqual(t *testing.T) {
	t.Parallel()
	valid, _, _, _, _, _, _, _ := reviewAdmissionClosure(t)
	if !valid.Equal(valid) {
		t.Fatal("a snapshot does not equal itself")
	}
	for name, mutate := range map[string]func(*domain.ReviewAdmission){
		"identity":   func(a *domain.ReviewAdmission) { a.AuthIdentityID = "auth-other" },
		"lineage":    func(a *domain.ReviewAdmission) { a.LineageGroup = "other" },
		"role":       func(a *domain.ReviewAdmission) { a.Role = domain.RoleShadowReviewer },
		"time":       func(a *domain.ReviewAdmission) { a.AdmittedAt = a.AdmittedAt.Add(time.Second) },
		"agent":      func(a *domain.ReviewAdmission) { a.AgentBinding.AgentDigest = a.AgentBinding.LaunchDigest },
		"generation": func(a *domain.ReviewAdmission) { a.AgentBinding.EnrollmentGeneration++ },
		"model":      func(a *domain.ReviewAdmission) { a.AgentBinding.RouteModelID += "-x" },
		"egress": func(a *domain.ReviewAdmission) {
			a.AgentBinding.EffectiveEgress = append([]string{"a.example:443"}, a.AgentBinding.EffectiveEgress...)
		},
	} {
		other := valid
		mutate(&other)
		if valid.Equal(other) {
			t.Errorf("snapshots that differ in %s compare equal", name)
		}
	}
}

func TestReviewAdmissionModelConfiguration(t *testing.T) {
	t.Parallel()
	valid, _, _, _, _, _, _, _ := reviewAdmissionClosure(t)
	want := valid.AgentBinding.RouteModelID + "/" + valid.AgentBinding.NativeEffort
	if got := valid.ModelConfiguration(); got != want {
		t.Fatalf("ModelConfiguration = %q, want %q", got, want)
	}
}

// TestValidateReviewAdmissionDerivations pins the review snapshot's closure
// recheck: the shared joins hold, the identity and lineage group join the
// enrollment and the offer, and launch coverage is not applied.
func TestValidateReviewAdmissionDerivations(t *testing.T) {
	t.Parallel()
	check := func(
		a domain.ReviewAdmission, agent domain.AgentDefinition, adapter domain.AdapterFragment,
		route domain.RouteFragment, offer domain.OfferFragment, launch domain.LaunchSpec,
		enrollment domain.ClientEnrollment, generationEntry domain.EnrollmentGeneration,
	) error {
		return domain.ValidateReviewAdmissionDerivations(
			a, agent, adapter, route, offer, launch, enrollment, generationEntry)
	}
	admission, agent, adapter, route, offer, launch, enrollment, generationEntry := reviewAdmissionClosure(t)
	if err := check(admission, agent, adapter, route, offer, launch, enrollment, generationEntry); err != nil {
		t.Fatalf("coherent closure = %v", err)
	}
	if missing := domain.MissingLaunchCapabilities(adapter.LaunchCapabilities, launch.RequiredCapabilities()); len(missing) == 0 {
		t.Fatal("the fixture launch is covered, so this test does not show coverage is left out")
	}
	// The egress join is left out too: the snapshot records what the review
	// proxy admits, which an API-key credential holds outside its route.
	outside := admission
	outside.AgentBinding.EffectiveEgress = []string{"outside.example:443"}
	if err := check(outside, agent, adapter, route, offer, launch, enrollment, generationEntry); err != nil {
		t.Fatalf("egress outside the route = %v, want it recorded", err)
	}

	cases := []struct {
		name   string
		mutate func(*domain.ReviewAdmission, *domain.OfferFragment,
			*domain.ClientEnrollment, *domain.EnrollmentGeneration)
	}{
		{"foreign agent digest", func(a *domain.ReviewAdmission, _ *domain.OfferFragment,
			_ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
		) {
			a.AgentBinding.AgentDigest = a.AgentBinding.LaunchDigest
		}},
		{"foreign launch digest", func(a *domain.ReviewAdmission, _ *domain.OfferFragment,
			_ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
		) {
			a.AgentBinding.LaunchDigest = a.AgentBinding.AgentDigest
		}},
		{"a model the closure does not derive", func(a *domain.ReviewAdmission, _ *domain.OfferFragment,
			_ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
		) {
			a.AgentBinding.RouteModelID += "-other"
		}},
		{"identity disagrees with the enrollment", func(a *domain.ReviewAdmission, _ *domain.OfferFragment,
			_ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
		) {
			a.AuthIdentityID = "auth-2"
		}},
		{"lineage group disagrees with the offer", func(a *domain.ReviewAdmission, _ *domain.OfferFragment,
			_ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
		) {
			a.LineageGroup += "-other"
		}},
		{"generation ordinal disagrees with the binding", func(_ *domain.ReviewAdmission, _ *domain.OfferFragment,
			_ *domain.ClientEnrollment, g *domain.EnrollmentGeneration,
		) {
			g.Ordinal = 2
		}},
		{"store manifest disagrees with the generation", func(a *domain.ReviewAdmission, _ *domain.OfferFragment,
			_ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
		) {
			a.AgentBinding.StoreManifestDigest = a.AgentBinding.AgentDigest
		}},
		{"an offer the agent does not pin", func(_ *domain.ReviewAdmission, o *domain.OfferFragment,
			_ *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
		) {
			o.PricingRevision += "-next"
			digest, err := o.ComputeDigest()
			if err != nil {
				t.Fatal(err)
			}
			o.Digest = digest
		}},
		{"an enrollment for another client", func(_ *domain.ReviewAdmission, _ *domain.OfferFragment,
			e *domain.ClientEnrollment, _ *domain.EnrollmentGeneration,
		) {
			e.HarnessClient = domain.HarnessClientClaudeCode
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			admission, agent, adapter, route, offer, launch, enrollment, generationEntry := reviewAdmissionClosure(t)
			tc.mutate(&admission, &offer, &enrollment, &generationEntry)
			err := check(admission, agent, adapter, route, offer, launch, enrollment, generationEntry)
			if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
				t.Fatalf("ValidateReviewAdmissionDerivations = %v, want %v",
					err, domain.ErrAdmissionDerivationMismatch)
			}
		})
	}

	t.Run("a launch for another stage", func(t *testing.T) {
		t.Parallel()
		admission, agent, adapter, route, offer, launch, enrollment, generationEntry := reviewAdmissionClosure(t)
		launch.Stage = domain.StageNameImplementation
		digest, err := launch.ComputeDigest()
		if err != nil {
			t.Fatal(err)
		}
		launch.Digest, admission.AgentBinding.LaunchDigest = digest, digest
		err = check(admission, agent, adapter, route, offer, launch, enrollment, generationEntry)
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("ValidateReviewAdmissionDerivations = %v, want %v",
				err, domain.ErrAdmissionDerivationMismatch)
		}
	})
}
