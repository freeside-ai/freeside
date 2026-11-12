package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestUpdateDrainTransitionValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	valid := domain.UpdateDrainTransition{
		State:      domain.UpdateDrainDraining,
		Build:      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		OccurredAt: at,
	}

	cases := []struct {
		name    string
		mutate  func(*domain.UpdateDrainTransition)
		wantErr error
	}{
		{"draining", func(*domain.UpdateDrainTransition) {}, nil},
		{"forced draining", func(tr *domain.UpdateDrainTransition) { tr.Forced = true }, nil},
		{"lifted", func(tr *domain.UpdateDrainTransition) { tr.State = domain.UpdateDrainLifted }, nil},
		{"zero state", func(tr *domain.UpdateDrainTransition) { tr.State = "" }, domain.ErrInvalidUpdateDrainState},
		{"unknown state", func(tr *domain.UpdateDrainTransition) { tr.State = "paused" }, domain.ErrInvalidUpdateDrainState},
		{"missing build", func(tr *domain.UpdateDrainTransition) { tr.Build = "" }, domain.ErrInvalidDigest},
		{"malformed build", func(tr *domain.UpdateDrainTransition) { tr.Build = "sha256:short" }, domain.ErrInvalidDigest},
		{"forced lift", func(tr *domain.UpdateDrainTransition) {
			tr.State = domain.UpdateDrainLifted
			tr.Forced = true
		}, domain.ErrUpdateDrainLiftForced},
		{"zero instant", func(tr *domain.UpdateDrainTransition) { tr.OccurredAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-utc instant", func(tr *domain.UpdateDrainTransition) {
			tr.OccurredAt = at.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := valid
			tc.mutate(&tr)
			err := tr.Validate()
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
