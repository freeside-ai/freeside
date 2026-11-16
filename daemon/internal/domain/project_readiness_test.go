package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var readinessAt = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func passingReadiness() domain.ProjectReadiness {
	r := domain.ProjectReadiness{
		ProjectID: "proj-1", Settings: &domain.SettingsRevisions{Instance: 3, Project: 2}, EvaluatedAt: readinessAt,
	}
	for _, check := range domain.AllProjectReadinessChecks {
		r.Results = append(r.Results, domain.ProjectReadinessResult{
			Check: check, Status: domain.ProjectReadinessPassed, CheckedAt: readinessAt,
		})
	}
	return r
}

func TestProjectReadinessValidate(t *testing.T) {
	t.Parallel()
	ready := passingReadiness()
	if err := ready.Validate(); err != nil {
		t.Fatalf("passing record: %v", err)
	}
	if !ready.Ready() || len(ready.NotPassed()) != 0 {
		t.Fatal("passing record is not ready")
	}
	for name, tc := range map[string]struct {
		edit func(*domain.ProjectReadiness)
		want error
	}{
		"no project":         {func(r *domain.ProjectReadiness) { r.ProjectID = "" }, domain.ErrEmptyID},
		"half-set revisions": {func(r *domain.ProjectReadiness) { r.Settings = &domain.SettingsRevisions{Instance: 1} }, domain.ErrNonPositive},
		"missing result":     {func(r *domain.ProjectReadiness) { r.Results = r.Results[1:] }, domain.ErrProjectReadinessInconsistent},
		"out of order": {func(r *domain.ProjectReadiness) {
			r.Results[0], r.Results[1] = r.Results[1], r.Results[0]
		}, domain.ErrProjectReadinessInconsistent},
		"unknown status":     {func(r *domain.ProjectReadiness) { r.Results[2].Status = "skipped" }, domain.ErrProjectReadinessInconsistent},
		"failure, no detail": {func(r *domain.ProjectReadiness) { r.Results[2].Status = domain.ProjectReadinessFailed }, domain.ErrProjectReadinessInconsistent},
		"pass with detail":   {func(r *domain.ProjectReadiness) { r.Results[2].Detail = "fine" }, domain.ErrProjectReadinessInconsistent},
		"credential in detail": {func(r *domain.ProjectReadiness) {
			r.Results[3].Status = domain.ProjectReadinessFailed
			r.Results[3].Detail = "request with ghs_" + "abcdefghijklmnopqrstuvwxyz0123 refused"
		}, domain.ErrProjectReadinessInconsistent},
		"non-UTC check":   {func(r *domain.ProjectReadiness) { r.Results[0].CheckedAt = readinessAt.Local() }, domain.ErrTimestampNotUTC},
		"no evaluated at": {func(r *domain.ProjectReadiness) { r.EvaluatedAt = time.Time{} }, domain.ErrMissingTimestamp},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := passingReadiness()
			r.Results = append([]domain.ProjectReadinessResult(nil), r.Results...)
			tc.edit(&r)
			if err := r.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestProjectReadinessNotPassed(t *testing.T) {
	t.Parallel()
	r := passingReadiness()
	r.Results[0].Status, r.Results[0].Detail = domain.ProjectReadinessFailed, "no profile"
	r.Results[4].Status, r.Results[4].Detail = domain.ProjectReadinessUnavailable, "not evaluated"
	if r.Ready() {
		t.Fatal("record with a failed check is ready")
	}
	got := r.NotPassed()
	if len(got) != 2 || got[0].Check != domain.ProjectReadinessTrustProfile || got[1].Check != domain.ProjectReadinessWriterImage {
		t.Fatalf("NotPassed() = %+v", got)
	}
	for _, check := range domain.AllProjectReadinessChecks {
		want := check == domain.ProjectReadinessTrustProfile || check == domain.ProjectReadinessReviewConfiguration
		if check.StoreLocal() != want {
			t.Errorf("%s.StoreLocal() = %v, want %v", check, check.StoreLocal(), want)
		}
	}
}
