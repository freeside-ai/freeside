package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestUnattendedOperationTransitionValidate(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	command := "cmd-1"
	emptyCommand := ""
	valid := domain.UnattendedOperationTransition{
		State: domain.UnattendedStopped, CommandID: &command,
		Reason: "operator stopped unattended operation", OccurredAt: at,
	}

	cases := []struct {
		name    string
		mutate  func(*domain.UnattendedOperationTransition)
		wantErr error
	}{
		{"stopped with command", func(*domain.UnattendedOperationTransition) {}, nil},
		{"resumed without command", func(tr *domain.UnattendedOperationTransition) {
			tr.State = domain.UnattendedResumed
			tr.CommandID = nil
			tr.Reason = ""
		}, nil},
		{"zero state", func(tr *domain.UnattendedOperationTransition) {
			tr.State = ""
		}, domain.ErrInvalidUnattendedOperationState},
		{"unknown state", func(tr *domain.UnattendedOperationTransition) {
			tr.State = "paused"
		}, domain.ErrInvalidUnattendedOperationState},
		{"present empty command id", func(tr *domain.UnattendedOperationTransition) {
			tr.CommandID = &emptyCommand
		}, domain.ErrEmptyID},
		{"zero instant", func(tr *domain.UnattendedOperationTransition) {
			tr.OccurredAt = time.Time{}
		}, domain.ErrMissingTimestamp},
		{"non-utc instant", func(tr *domain.UnattendedOperationTransition) {
			tr.OccurredAt = at.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
