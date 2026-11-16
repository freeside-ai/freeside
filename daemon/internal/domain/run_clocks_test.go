package domain_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestRunClocksOf(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	at := func(seconds float64) time.Time { return t0.Add(time.Duration(seconds * float64(time.Second))) }
	ptr := func(v time.Time) *time.Time { return &v }
	milestone := func(kind domain.RunMilestoneKind, invocation domain.InvocationID, seconds float64) domain.RunMilestone {
		m := domain.RunMilestone{RunID: "run-1", Kind: kind, InvocationID: &invocation, RecordedAt: at(seconds)}
		switch kind {
		case domain.MilestoneExecutionOutcomeRecorded:
			outcome := domain.ExecutionOutcomeFailed
			m.Outcome = &outcome
		case domain.MilestoneTerminalRecorded:
			terminal := domain.ObservedStatusCompleted
			m.Terminal = &terminal
		case domain.MilestonePublicationBlocked:
			reason := domain.HoldVerificationFindings
			m.Reason = &reason
		case domain.MilestoneRunSubmitted, domain.MilestoneInvocationAdmitted, domain.MilestoneInvocationStarted,
			domain.MilestoneExecutionExportRecorded, domain.MilestonePublicationReady, domain.MilestoneWorkUnitCompleted:
		}
		return m
	}
	submitted := func(seconds float64) domain.RunMilestone {
		return milestone(domain.MilestoneRunSubmitted, "submit", seconds)
	}
	started := func(invocation domain.InvocationID, seconds float64) domain.RunMilestone {
		return milestone(domain.MilestoneInvocationStarted, invocation, seconds)
	}
	exported := func(invocation domain.InvocationID, seconds float64) domain.RunMilestone {
		return milestone(domain.MilestoneExecutionExportRecorded, invocation, seconds)
	}
	interval := func(invocation domain.InvocationID, from float64, to *time.Time) domain.RunActiveInterval {
		return domain.RunActiveInterval{InvocationID: invocation, StartedAt: at(from), EndedAt: to}
	}

	cases := []struct {
		name       string
		milestones []domain.RunMilestone
		finished   bool
		asOf       time.Time
		want       *domain.RunClocks
	}{
		{
			name: "no milestone has no clocks",
			asOf: at(100),
		},
		{
			name:       "submitted but not started counts elapsed only",
			milestones: []domain.RunMilestone{submitted(0)},
			asOf:       at(90.9),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 90, ActiveIntervals: []domain.RunActiveInterval{},
			},
		},
		{
			name: "no run_submitted starts at the earliest milestone",
			milestones: []domain.RunMilestone{
				milestone(domain.MilestoneInvocationAdmitted, "inv-1", 10),
				started("inv-1", 5),
				exported("inv-1", 40),
			},
			asOf: at(60),
			want: &domain.RunClocks{
				StartedAt: at(5), ElapsedSeconds: 55, ActiveSeconds: 35,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 5, ptr(at(40)))},
			},
		},
		{
			name:       "an invocation working at asOf runs to asOf with no end",
			milestones: []domain.RunMilestone{submitted(0), started("inv-1", 10)},
			asOf:       at(70),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 70, ActiveSeconds: 60,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 10, nil)},
			},
		},
		{
			name: "a finished run ends at its newest milestone",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), exported("inv-1", 50),
				milestone(domain.MilestoneWorkUnitCompleted, "publish", 80),
			},
			finished: true,
			asOf:     at(500),
			want: &domain.RunClocks{
				StartedAt: at(0), EndedAt: ptr(at(80)), ElapsedSeconds: 80, ActiveSeconds: 40,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 10, ptr(at(50)))},
			},
		},
		{
			name: "a finished run's unended invocation stops at the run's end",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), milestone(domain.MilestoneInvocationAdmitted, "inv-2", 30),
			},
			finished: true,
			asOf:     at(500),
			want: &domain.RunClocks{
				StartedAt: at(0), EndedAt: ptr(at(30)), ElapsedSeconds: 30, ActiveSeconds: 20,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 10, ptr(at(30)))},
			},
		},
		{
			name: "two invocations in sequence add up",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), exported("inv-1", 20),
				started("inv-2", 30), exported("inv-2", 45),
			},
			asOf: at(50),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 50, ActiveSeconds: 25,
				ActiveIntervals: []domain.RunActiveInterval{
					interval("inv-1", 10, ptr(at(20))), interval("inv-2", 30, ptr(at(45))),
				},
			},
		},
		{
			name: "an export ends the invocation",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), exported("inv-1", 25),
			},
			asOf: at(30),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 30, ActiveSeconds: 15,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 10, ptr(at(25)))},
			},
		},
		{
			name: "an outcome ends the invocation",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), milestone(domain.MilestoneExecutionOutcomeRecorded, "inv-1", 25),
			},
			asOf: at(30),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 30, ActiveSeconds: 15,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 10, ptr(at(25)))},
			},
		},
		{
			name: "a terminal ends the invocation",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), milestone(domain.MilestoneTerminalRecorded, "inv-1", 25),
			},
			asOf: at(30),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 30, ActiveSeconds: 15,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 10, ptr(at(25)))},
			},
		},
		{
			name: "another invocation's ending doesn't end this one",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), exported("inv-0", 15), exported("inv-1", 25),
			},
			asOf: at(30),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 30, ActiveSeconds: 15,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 10, ptr(at(25)))},
			},
		},
		{
			name: "an invocation with no ending works until the next start",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), started("inv-2", 30), exported("inv-2", 40),
			},
			asOf: at(60),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 60, ActiveSeconds: 30,
				ActiveIntervals: []domain.RunActiveInterval{
					interval("inv-1", 10, ptr(at(30))), interval("inv-2", 30, ptr(at(40))),
				},
			},
		},
		{
			name: "two invocations working at once count once",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), started("inv-2", 15),
				exported("inv-1", 30), exported("inv-2", 40),
			},
			asOf: at(60),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 60, ActiveSeconds: 30,
				ActiveIntervals: []domain.RunActiveInterval{
					interval("inv-1", 10, ptr(at(30))), interval("inv-2", 15, ptr(at(40))),
				},
			},
		},
		{
			name: "overlapping and nested intervals count shared time once",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10), started("inv-2", 12),
				exported("inv-1", 30), started("inv-3", 14), exported("inv-3", 20), exported("inv-2", 35),
			},
			asOf: at(60),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 60, ActiveSeconds: 25,
				ActiveIntervals: []domain.RunActiveInterval{
					interval("inv-1", 10, ptr(at(30))), interval("inv-2", 12, ptr(at(35))),
					interval("inv-3", 14, ptr(at(20))),
				},
			},
		},
		{
			name: "an asOf before the newest milestone counts to the newest milestone",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 10),
				milestone(domain.MilestoneInvocationAdmitted, "inv-2", 40),
			},
			asOf: at(20),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 40, ActiveSeconds: 30,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 10, nil)},
			},
		},
		{
			name: "an ending recorded at an earlier instant serves a zero-length interval",
			milestones: []domain.RunMilestone{
				submitted(0), started("inv-1", 20), exported("inv-1", 15),
			},
			asOf: at(60),
			want: &domain.RunClocks{
				StartedAt: at(0), ElapsedSeconds: 60, ActiveSeconds: 0,
				ActiveIntervals: []domain.RunActiveInterval{interval("inv-1", 20, ptr(at(20)))},
			},
		},
		{
			name: "totals round down from exact instants",
			milestones: []domain.RunMilestone{
				submitted(0.4), started("inv-1", 1.6), exported("inv-1", 2.5), started("inv-2", 3.2), exported("inv-2", 4.1),
			},
			asOf: at(5.3),
			want: &domain.RunClocks{
				StartedAt: at(0.4), ElapsedSeconds: 4, ActiveSeconds: 1,
				ActiveIntervals: []domain.RunActiveInterval{
					interval("inv-1", 1.6, ptr(at(2.5))), interval("inv-2", 3.2, ptr(at(4.1))),
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, m := range tc.milestones {
				if err := m.Validate(); err != nil {
					t.Fatalf("fixture milestone %s: %v", m.Kind, err)
				}
			}
			got := domain.RunClocksOf(tc.milestones, tc.finished, tc.asOf)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("RunClocksOf:\n got %+v\nwant %+v", got, tc.want)
			}
			if got != nil && got.ActiveSeconds > got.ElapsedSeconds {
				t.Fatalf("active %d exceeds elapsed %d", got.ActiveSeconds, got.ElapsedSeconds)
			}
		})
	}
}

// A finished run's clocks share no pointer between fields, so a caller that
// edits one end can't move another.
func TestRunClocksOfFinishedEndsAreDistinct(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	invocation := domain.InvocationID("inv-1")
	clocks := domain.RunClocksOf([]domain.RunMilestone{
		{RunID: "run-1", Kind: domain.MilestoneInvocationStarted, InvocationID: &invocation, RecordedAt: t0},
		{RunID: "run-1", Kind: domain.MilestoneInvocationAdmitted, InvocationID: &invocation, RecordedAt: t0.Add(time.Minute)},
	}, true, t0.Add(time.Hour))
	if clocks.EndedAt == clocks.ActiveIntervals[0].EndedAt {
		t.Fatal("the run's end and the interval's end share a pointer")
	}
}
