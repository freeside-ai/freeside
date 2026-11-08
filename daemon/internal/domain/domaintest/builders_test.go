package domaintest_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/domain/domaintest"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// TestBuildersAreValid checks each builder's default, and its output under
// options for the fields tests vary, against its type's validation. The
// builders already panic on an invalid result; validating again here keeps
// the check visible if that panic is ever dropped.
func TestBuildersAreValid(t *testing.T) {
	t.Parallel()
	type validator interface{ Validate() error }
	cases := []struct {
		name  string
		value validator
	}{
		{"run default", domaintest.Run()},
		{"run on another id", domaintest.Run(func(r *domain.Run) {
			r.ID, r.Stages[0].RunID = "run-2", "run-2"
		})},
		{"run initial attempt", domaintest.Run(func(r *domain.Run) {
			r.AttemptNumber, r.AttemptReason, r.ParentRunID = 1, "", ""
		})},
		{"run legacy without lineage", domaintest.Run(func(r *domain.Run) {
			r.CampaignID, r.AttemptNumber, r.AttemptReason, r.ParentRunID = "", 0, "", ""
		})},
		{"run without stages", domaintest.Run(func(r *domain.Run) { r.Stages = nil })},

		{"review record default", domaintest.ReviewRecord()},
		{"review record clean", domaintest.ReviewRecord(func(r *domain.ReviewRecord) {
			r.Outcome, r.FindingIDs = domain.ReviewClean, nil
		})},
		{"review record later round", domaintest.ReviewRecord(func(r *domain.ReviewRecord) {
			r.InvocationID, r.Round, r.HeadSHA = "review-run-1-2", 2, "feedface"
			r.Admission = fake.SeededReviewAdmission(r.RunID, r.InvocationID)
		})},

		{"execution admission default", domaintest.ExecutionAdmission()},
		{"execution admission registry egress", domaintest.ExecutionAdmission(func(in *domain.ExecutionAdmissionInput) {
			in.EgressProfile = domain.EgressProviderRegistry
		})},
		{"execution admission another attempt", domaintest.ExecutionAdmission(func(in *domain.ExecutionAdmissionInput) {
			in.InvocationID, in.AttemptID = "inv-2", "attempt-2"
		})},

		{"auth identity default", domaintest.AuthIdentity()},
		{"auth identity codex disabled", domaintest.AuthIdentity(func(i *domain.AuthIdentity) {
			i.ID, i.Provider, i.Enabled = "auth-codex-owner", "openai", false
		})},
		{"auth identity without interim facts", domaintest.AuthIdentity(func(i *domain.AuthIdentity) {
			i.AuthStoreMutationLease, i.Interim = false, domain.InterimClientFacts{}
		})},
	}
	for _, c := range cases {
		if err := c.value.Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// TestConstructorBuildersRunTheirConstructor shows options on the constructed
// types land before the constructor runs: finding IDs come back canonical,
// and the admission's content-addressed ID covers the edited field.
func TestConstructorBuildersRunTheirConstructor(t *testing.T) {
	t.Parallel()
	record := domaintest.ReviewRecord(func(r *domain.ReviewRecord) {
		r.FindingIDs = []domain.FindingID{"find-2", "find-1", "find-2"}
	})
	if want := []domain.FindingID{"find-1", "find-2"}; !slices.Equal(record.FindingIDs, want) {
		t.Errorf("finding ids = %v, want %v", record.FindingIDs, want)
	}

	base := domaintest.ExecutionAdmission()
	registry := domaintest.ExecutionAdmission(func(in *domain.ExecutionAdmissionInput) {
		in.EgressProfile = domain.EgressProviderRegistry
	})
	if base.ID == registry.ID {
		t.Errorf("admission id %s did not change with the egress profile", base.ID)
	}
	if want := domaintest.ExecutionAdmission(); want.ID != base.ID {
		t.Errorf("default admission id = %s, then %s: not deterministic", base.ID, want.ID)
	}
}

// TestBuildersPanicOnInvalidResult pins the MustConclude convention: an
// option that breaks the value is a broken fixture, so the builder panics
// and names the builder.
func TestBuildersPanicOnInvalidResult(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		build func()
	}{
		{"Run", func() { domaintest.Run(func(r *domain.Run) { r.ProjectID = "" }) }},
		{"ReviewRecord", func() {
			domaintest.ReviewRecord(func(r *domain.ReviewRecord) { r.Outcome = domain.ReviewClean })
		}},
		{"ExecutionAdmission", func() {
			domaintest.ExecutionAdmission(func(in *domain.ExecutionAdmissionInput) { in.InvocationID = "" })
		}},
		{"AuthIdentity", func() { domaintest.AuthIdentity(func(i *domain.AuthIdentity) { i.Provider = "" }) }},
	}
	for _, c := range cases {
		func() {
			defer func() {
				got, _ := recover().(string)
				if !strings.HasPrefix(got, "domaintest."+c.name+"(") {
					t.Errorf("%s: panic = %q, want a domaintest.%s panic", c.name, got, c.name)
				}
			}()
			c.build()
		}()
	}
}
