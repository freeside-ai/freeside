package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// reviewAttributionCase is one record kind seen through the attribution
// fields both kinds share, so the rule is exercised once for each.
type reviewAttributionCase struct {
	name string
	role domain.RoleName
	// build returns a record of this kind's validation and labels under the
	// given admission. The labels read the admission, so they are read only
	// for a record that carries one.
	build func(admission *domain.ReviewAdmission) (func() error, func() (string, string))
}

func reviewAttributionCases() []reviewAttributionCase {
	return []reviewAttributionCase{
		{
			name: "review record", role: domain.RoleReviewer,
			build: func(admission *domain.ReviewAdmission) (func() error, func() (string, string)) {
				record := validReviewRecord()
				record.Admission = admission
				return record.Validate, func() (string, string) { return record.Provider(), record.ModelConfiguration() }
			},
		},
		{
			name: "shadow review record", role: domain.RoleShadowReviewer,
			build: func(admission *domain.ReviewAdmission) (func() error, func() (string, string)) {
				record := validShadowReviewRecord()
				record.Admission = admission
				return record.Validate, func() (string, string) { return record.Provider(), record.ModelConfiguration() }
			},
		},
	}
}

// TestReviewRecordAttributionIsItsSnapshot covers the rule both record kinds
// share: a record names its reviewer through an admission snapshot of its own
// invocation, run, and role, and a record without one is refused.
func TestReviewRecordAttributionIsItsSnapshot(t *testing.T) {
	t.Parallel()
	for _, kind := range reviewAttributionCases() {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			runID, id := domain.RunID("run-1"), domain.InvocationID("review-run-1-1")
			if kind.role == domain.RoleShadowReviewer {
				id = "shadow-run-1-1"
			}
			snapshot := func(mutate func(*domain.ReviewAdmission)) *domain.ReviewAdmission {
				admission := fake.RecordAdmission(kind.role, runID, id)
				if mutate != nil {
					mutate(admission)
				}
				return admission
			}
			otherRole := domain.RoleShadowReviewer
			if kind.role == domain.RoleShadowReviewer {
				otherRole = domain.RoleReviewer
			}
			for _, tc := range []struct {
				name      string
				admission *domain.ReviewAdmission
				want      error
			}{
				{name: "snapshot", admission: snapshot(nil)},
				{name: "no snapshot", want: domain.ErrEmptyField},
				{
					name:      "snapshot of another invocation",
					admission: snapshot(func(a *domain.ReviewAdmission) { a.InvocationID = "another" }),
					want:      domain.ErrParentKeyMismatch,
				},
				{
					name:      "snapshot of another run",
					admission: snapshot(func(a *domain.ReviewAdmission) { a.RunID = "another" }),
					want:      domain.ErrParentKeyMismatch,
				},
				{
					name:      "snapshot of the other arm",
					admission: snapshot(func(a *domain.ReviewAdmission) { a.Role = otherRole }),
					want:      domain.ErrReviewAdmissionInvalid,
				},
				{
					name:      "invalid snapshot",
					admission: snapshot(func(a *domain.ReviewAdmission) { a.AuthIdentityID = "" }),
					want:      domain.ErrEmptyID,
				},
			} {
				validate, _ := kind.build(tc.admission)
				if err := validate(); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
					t.Errorf("%s: Validate = %v, want %v", tc.name, err, tc.want)
				}
			}
		})
	}
}

// TestReviewRecordLabelsComeFromItsAttribution pins what a reader shows: a
// snapshot's lineage group and admitted model and effort, with an unrecorded
// lineage shown as unknown.
func TestReviewRecordLabelsComeFromItsAttribution(t *testing.T) {
	t.Parallel()
	for _, kind := range reviewAttributionCases() {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()
			admission := fake.RecordAdmission(kind.role, "run-1", "review-run-1-1")
			admission.LineageGroup = "anthropic"
			admission.AgentBinding.RouteModelID, admission.AgentBinding.NativeEffort = "claude-test", "max"
			_, labels := kind.build(admission)
			provider, model := labels()
			if provider != "anthropic" || model != "claude-test/max" {
				t.Errorf("snapshot labels = %q, %q", provider, model)
			}
			admission.LineageGroup = ""
			_, labels = kind.build(admission)
			if provider, _ = labels(); provider != domain.ReviewProviderUnknown {
				t.Errorf("unrecorded lineage shown as %q", provider)
			}
		})
	}
}

// TestReviewRecordSharesSegment pins which records convergence reads as one
// reviewer's consecutive passes: the same review configuration and the same
// reviewer agent.
func TestReviewRecordSharesSegment(t *testing.T) {
	t.Parallel()
	configuration := domain.Digest("sha256:" + strings.Repeat("c", 64))
	otherConfiguration := domain.Digest("sha256:" + strings.Repeat("d", 64))
	admitted := func(configuration domain.Digest, agent string, mutate func(*domain.ReviewAdmission)) domain.ReviewRecord {
		admission := fake.RecordAdmission(domain.RoleReviewer, "run-1", "review-run-1-1")
		admission.AgentBinding.AgentDigest = domain.Digest("sha256:" + strings.Repeat(agent, 64))
		if mutate != nil {
			mutate(admission)
		}
		return domain.ReviewRecord{ConfigurationDigest: configuration, Admission: admission}
	}
	for _, tc := range []struct {
		name        string
		left, right domain.ReviewRecord
		want        bool
	}{
		{name: "same reviewer", left: admitted(configuration, "a", nil), right: admitted(configuration, "a", nil), want: true},
		{
			name: "same reviewer admitted again", left: admitted(configuration, "a", nil),
			right: admitted(configuration, "a", func(a *domain.ReviewAdmission) {
				a.InvocationID, a.AuthIdentityID = "review-run-1-2", "another-identity"
				a.AgentBinding.LaunchDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
				a.AgentBinding.EnrollmentGeneration++
			}),
			want: true,
		},
		{name: "another reviewer", left: admitted(configuration, "a", nil), right: admitted(configuration, "b", nil)},
		{
			// A reviewer line set between rounds selects another agent under
			// the same configuration, which opens a new segment.
			name: "a reviewer line selects another agent", left: admitted(configuration, "a", nil),
			right: admitted(configuration, "b", func(a *domain.ReviewAdmission) {
				a.InvocationID = "review-run-1-2"
				a.AgentBinding.SelectionSource = domain.AgentSelectionSourceTaskLine
				a.AgentBinding.SelectionRecordID = domain.Digest("sha256:" + strings.Repeat("e", 64))
			}),
		},
		{
			name: "another configuration", left: admitted(configuration, "a", nil),
			right: admitted(otherConfiguration, "a", nil),
		},
	} {
		if got := tc.left.SharesSegment(tc.right); got != tc.want {
			t.Errorf("%s: SharesSegment = %v, want %v", tc.name, got, tc.want)
		}
		if got := tc.right.SharesSegment(tc.left); got != tc.want {
			t.Errorf("%s reversed: SharesSegment = %v, want %v", tc.name, got, tc.want)
		}
	}
}
