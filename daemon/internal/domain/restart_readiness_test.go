package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestNewRestartReadiness(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	drain := &domain.UpdateDrainTransition{
		State:      domain.UpdateDrainDraining,
		Build:      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		OccurredAt: at,
	}
	cases := []struct {
		name      string
		drain     *domain.UpdateDrainTransition
		counts    map[domain.RestartBlockerKind]int
		wantReady bool
		wantKinds []domain.RestartBlockerKind
	}{
		{"no drain, nothing held", nil, nil, false, nil},
		{"drain, nothing held", drain, map[domain.RestartBlockerKind]int{domain.RestartBlockerReviewLaunch: 0}, true, nil},
		{"drain, work held", drain, map[domain.RestartBlockerKind]int{
			domain.RestartBlockerCredentialLease: 1, domain.RestartBlockerExecutionLaunch: 3,
		}, false, []domain.RestartBlockerKind{domain.RestartBlockerExecutionLaunch, domain.RestartBlockerCredentialLease}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NewRestartReadiness(tc.drain, tc.counts, at)
			if err != nil {
				t.Fatalf("NewRestartReadiness: %v", err)
			}
			if got.Ready != tc.wantReady || got.Blockers == nil {
				t.Fatalf("readiness = %+v, want ready %t and a non-nil blocker list", got, tc.wantReady)
			}
			var kinds []domain.RestartBlockerKind
			for _, blocker := range got.Blockers {
				kinds = append(kinds, blocker.Kind)
			}
			if !slices.Equal(kinds, tc.wantKinds) {
				t.Fatalf("blocker kinds = %v, want %v", kinds, tc.wantKinds)
			}
		})
	}
}

func TestRestartReadinessValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	drain := domain.UpdateDrainTransition{
		State:      domain.UpdateDrainDraining,
		Build:      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		OccurredAt: at,
	}
	valid := func() domain.RestartReadiness {
		d := drain
		return domain.RestartReadiness{
			Drain:    &d,
			Blockers: []domain.RestartBlocker{{Kind: domain.RestartBlockerExecutionLaunch, Count: 1}},
			AsOf:     at,
		}
	}
	cases := []struct {
		name    string
		mutate  func(*domain.RestartReadiness)
		wantErr error
	}{
		{"not ready with blockers", func(*domain.RestartReadiness) {}, nil},
		{"ready with a drain and no blockers", func(r *domain.RestartReadiness) {
			r.Blockers = []domain.RestartBlocker{}
			r.Ready = true
		}, nil},
		{"ready with blockers", func(r *domain.RestartReadiness) { r.Ready = true }, domain.ErrRestartReadinessInconsistent},
		{"ready with no drain", func(r *domain.RestartReadiness) {
			r.Drain = nil
			r.Blockers = []domain.RestartBlocker{}
			r.Ready = true
		}, domain.ErrRestartReadinessInconsistent},
		{"not ready with a drain and no blockers", func(r *domain.RestartReadiness) {
			r.Blockers = []domain.RestartBlocker{}
		}, domain.ErrRestartReadinessInconsistent},
		{"lifted drain", func(r *domain.RestartReadiness) { r.Drain.State = domain.UpdateDrainLifted }, domain.ErrRestartReadinessInconsistent},
		{"invalid drain", func(r *domain.RestartReadiness) { r.Drain.Build = "" }, domain.ErrInvalidDigest},
		{"nil blockers", func(r *domain.RestartReadiness) { r.Blockers = nil }, domain.ErrRestartReadinessInconsistent},
		{"unknown kind", func(r *domain.RestartReadiness) { r.Blockers[0].Kind = "daemon" }, domain.ErrInvalidRestartBlockerKind},
		{"zero count", func(r *domain.RestartReadiness) { r.Blockers[0].Count = 0 }, domain.ErrRestartReadinessInconsistent},
		{"out of order", func(r *domain.RestartReadiness) {
			r.Blockers = []domain.RestartBlocker{
				{Kind: domain.RestartBlockerCredentialLease, Count: 1},
				{Kind: domain.RestartBlockerExecutionLaunch, Count: 1},
			}
		}, domain.ErrRestartReadinessInconsistent},
		{"repeated kind", func(r *domain.RestartReadiness) {
			r.Blockers = append(r.Blockers, r.Blockers[0])
		}, domain.ErrRestartReadinessInconsistent},
		{"zero as_of", func(r *domain.RestartReadiness) { r.AsOf = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-utc as_of", func(r *domain.RestartReadiness) {
			r.AsOf = at.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := valid()
			tc.mutate(&r)
			err := r.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
