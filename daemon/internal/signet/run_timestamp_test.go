package signet

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestRunSnapshotProjectsObservationTimestamps(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	invocationID := domain.InvocationID("inv-1")
	instant := func(offset time.Duration) time.Time { return base.Add(offset) }
	pointer := func(value time.Time) *time.Time { return &value }
	milestone := func(kind domain.RunMilestoneKind, offset time.Duration) domain.RunMilestone {
		return domain.RunMilestone{
			RunID: "run-1", Kind: kind, InvocationID: &invocationID,
			RecordedAt: instant(offset),
		}
	}

	cases := []struct {
		name             string
		observation      domain.RunObservation
		wantCreated      *time.Time
		wantLastActivity *time.Time
	}{
		{
			name:        "legacy run",
			observation: domain.RunObservation{RunID: "run-1"},
		},
		{
			name: "milestone only",
			observation: domain.RunObservation{
				RunID: "run-1",
				Milestones: []domain.RunMilestone{
					milestone(domain.MilestoneRunSubmitted, 0),
					milestone(domain.MilestoneInvocationAdmitted, time.Minute),
				},
			},
			wantCreated:      pointer(instant(0)),
			wantLastActivity: pointer(instant(time.Minute)),
		},
		{
			name: "mixed facts take newest instant",
			observation: domain.RunObservation{
				RunID: "run-1",
				Milestones: []domain.RunMilestone{
					milestone(domain.MilestoneRunSubmitted, 0),
					milestone(domain.MilestoneInvocationStarted, time.Minute),
				},
				Invocations: []domain.InvocationObservation{{
					InvocationID: invocationID, RunID: "run-1",
					Status: domain.ObservedStatusRunning, Live: true,
					ObservedAt: instant(3 * time.Minute),
				}},
				Hold: &domain.RunHoldObservation{
					RunID: "run-1", InvocationID: &invocationID,
					Reason:          domain.HoldVerificationFindings,
					FirstObservedAt: instant(2 * time.Minute),
					LastObservedAt:  instant(4 * time.Minute),
				},
			},
			wantCreated:      pointer(instant(0)),
			wantLastActivity: pointer(instant(4 * time.Minute)),
		},
		{
			name: "activity without submission",
			observation: domain.RunObservation{
				RunID: "run-1",
				Milestones: []domain.RunMilestone{
					milestone(domain.MilestoneInvocationStarted, time.Minute),
				},
			},
			wantLastActivity: pointer(instant(time.Minute)),
		},
	}

	run := domain.Run{
		ID: "run-1", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runSnapshot(
				run, store.Snapshot{EntityVersion: 1}, tc.observation,
				domain.ConcludeRun(tc.observation), 1, nil,
			).Run
			assertOptionalInstant(t, "created_at", got.CreatedAt, tc.wantCreated)
			assertOptionalInstant(t, "last_activity_at", got.LastActivityAt, tc.wantLastActivity)
		})
	}
}

func assertOptionalInstant(t *testing.T, name string, got, want *time.Time) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
		return
	}
	if !got.Equal(*want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
