package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var triageRecordedAt = time.Date(2026, 11, 3, 12, 0, 0, 7, time.UTC)

func recordTaskTriage(ctx context.Context, s *store.Store, record domain.TaskTriage) error {
	return s.Write(ctx, func(tx *store.WriteTx) error { return tx.RecordTaskTriage(ctx, record) })
}

func readTaskTriage(ctx context.Context, s *store.Store, taskID domain.TaskID) (domain.TaskTriage, bool, error) {
	var (
		record domain.TaskTriage
		found  bool
	)
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, found, err = tx.GetTaskTriage(ctx, taskID)
		return err
	})
	return record, found, err
}

// Each outcome round-trips through the store, with its tier and reason when
// it has them.
func TestTaskTriageRoundTripsEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, outcome := range domain.AllTriageOutcomes {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openStore(t, store.Options{})
			taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
			if _, found, err := readTaskTriage(ctx, s, taskID); err != nil || found {
				t.Fatalf("triage before any record = %t, %v", found, err)
			}
			record := domain.TaskTriage{TaskID: taskID, Outcome: outcome, Escalated: true, RecordedAt: triageRecordedAt}
			if outcome == domain.TriageOutcomeAnswered {
				record.Tier, record.Escalated = new(domain.TriageTierStandard), false
				record.Reason = new("A one-file docs fix.")
			}
			if err := recordTaskTriage(ctx, s, record); err != nil {
				t.Fatal(err)
			}
			got, found, err := readTaskTriage(ctx, s, taskID)
			if err != nil || !found || !reflect.DeepEqual(got, record) {
				t.Fatalf("triage = %+v, %t, %v; want %+v", got, found, err, record)
			}
		})
	}
}

// A task holds one triage record: a second is refused and the first stays.
func TestTaskTriageRefusesASecondRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	first := domain.TaskTriage{
		TaskID: taskID, Outcome: domain.TriageOutcomeCallFailed, Escalated: true, RecordedAt: triageRecordedAt,
	}
	if err := recordTaskTriage(ctx, s, first); err != nil {
		t.Fatal(err)
	}
	second := domain.TaskTriage{
		TaskID: taskID, Outcome: domain.TriageOutcomeAnswered, Tier: new(domain.TriageTierStandard),
		RecordedAt: triageRecordedAt.Add(time.Minute),
	}
	if err := recordTaskTriage(ctx, s, second); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("second record = %v, want %v", err, domain.ErrDuplicate)
	}
	if got, _, err := readTaskTriage(ctx, s, taskID); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("triage after the refusal = %+v, %v; want %+v", got, err, first)
	}
}

// An invalid record and a record for no task write nothing.
func TestTaskTriageRefusesBadRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	quiet := domain.TaskTriage{TaskID: taskID, Outcome: domain.TriageOutcomeBudgetSpent, RecordedAt: triageRecordedAt}
	if err := recordTaskTriage(ctx, s, quiet); !errors.Is(err, domain.ErrTaskTriageInconsistent) {
		t.Fatalf("a skip that did not escalate = %v", err)
	}
	orphan := domain.TaskTriage{
		TaskID: "no-such-task", Outcome: domain.TriageOutcomeBudgetSpent, Escalated: true, RecordedAt: triageRecordedAt,
	}
	if err := recordTaskTriage(ctx, s, orphan); err == nil {
		t.Fatal("a record for no task was stored")
	}
	if _, found, err := readTaskTriage(ctx, s, taskID); err != nil || found {
		t.Fatalf("triage after refusals = %t, %v", found, err)
	}
}

func recordSpecifierTriage(
	ctx context.Context, s *store.Store, record domain.TaskTriage, counterpart string,
) (*domain.TaskLine, error) {
	var line *domain.TaskLine
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		var selection *domain.TaskLineSelection
		if counterpart != "" {
			selection = testSelection(counterpart)
		}
		line, err = tx.RecordSpecifierTriage(ctx, record, selection)
		return err
	})
	return line, err
}

func taskSnapshot(t *testing.T, ctx context.Context, s *store.Store, taskID domain.TaskID) store.Snapshot {
	t.Helper()
	var snapshot store.Snapshot
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetTaskSnapshot(ctx, taskID)
		snapshot = got.Snapshot
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// Every outcome writes its record and advances the task; only an escalation
// writes a line, the specifier's first version, set by the task at the
// record's instant and naming the counterpart.
func TestRecordSpecifierTriage(t *testing.T) {
	t.Parallel()
	for _, outcome := range domain.AllTriageOutcomes {
		for _, tier := range []domain.TriageTier{domain.TriageTierStandard, domain.TriageTierEscalated} {
			if outcome != domain.TriageOutcomeAnswered && tier == domain.TriageTierStandard {
				continue
			}
			t.Run(string(outcome)+"/"+string(tier), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				s := openStore(t, store.Options{})
				taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
				record := domain.TaskTriage{TaskID: taskID, Outcome: outcome, Escalated: true, RecordedAt: triageRecordedAt}
				if outcome == domain.TriageOutcomeAnswered {
					record.Tier, record.Escalated = new(tier), tier == domain.TriageTierEscalated
				}
				counterpart := ""
				if record.Escalated {
					counterpart = "claude-escalated"
				}
				before := taskSnapshot(t, ctx, s, taskID)
				line, err := recordSpecifierTriage(ctx, s, record, counterpart)
				if err != nil {
					t.Fatal(err)
				}
				if after := taskSnapshot(t, ctx, s, taskID); after.EntityVersion != before.EntityVersion+1 ||
					after.AsOfRevision <= before.AsOfRevision {
					t.Fatalf("task snapshot = %+v after %+v, want it advanced", after, before)
				}
				if got, found, err := readTaskTriage(ctx, s, taskID); err != nil || !found || !reflect.DeepEqual(got, record) {
					t.Fatalf("triage = %+v, %t, %v", got, found, err)
				}
				lines, _, err := readTaskLines(t, s, taskID)
				if err != nil {
					t.Fatal(err)
				}
				if !record.Escalated {
					if line != nil || len(lines) != 0 {
						t.Fatalf("a standard answer wrote %+v (%+v)", line, lines)
					}
					return
				}
				if line == nil || len(lines) != 1 || !reflect.DeepEqual(lines[0], *line) ||
					line.Role != domain.RoleSpecifier || *line.Selection != *testSelection(counterpart) || line.Version != 1 ||
					line.Source != domain.TaskLineSourceTriage || line.SetBy != string(taskID) ||
					!line.SetAt.Equal(record.RecordedAt) {
					t.Fatalf("triage line = %+v, lines %+v", line, lines)
				}
			})
		}
	}
}

// A refused write leaves nothing: no record, no line, and no task version.
func TestRecordSpecifierTriageRefusals(t *testing.T) {
	t.Parallel()
	escalated := func(taskID domain.TaskID) domain.TaskTriage {
		return domain.TaskTriage{TaskID: taskID, Outcome: domain.TriageOutcomeCallFailed, Escalated: true, RecordedAt: triageRecordedAt}
	}
	for name, tc := range map[string]struct {
		record      func(domain.TaskID) domain.TaskTriage
		counterpart string
		seed        func(*testing.T, *store.Store, domain.TaskID)
		want        error
	}{
		"escalation without a counterpart": {record: escalated, want: domain.ErrTaskTriageInconsistent},
		"standard with a counterpart": {
			record: func(taskID domain.TaskID) domain.TaskTriage {
				return domain.TaskTriage{
					TaskID: taskID, Outcome: domain.TriageOutcomeAnswered, Tier: new(domain.TriageTierStandard),
					RecordedAt: triageRecordedAt,
				}
			},
			counterpart: "claude-escalated", want: domain.ErrTaskTriageInconsistent,
		},
		"specifier already lined": {
			record: escalated, counterpart: "claude-escalated", want: domain.ErrTaskLineInconsistent,
			seed: func(t *testing.T, s *store.Store, taskID domain.TaskID) {
				if _, err := appendTaskLine(t, s, domain.TaskLineInput{
					TaskID: taskID, Role: domain.RoleSpecifier, Agent: "claude-a",
					Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
				}, triageRecordedAt.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			},
		},
		"second record": {
			record: escalated, counterpart: "claude-escalated", want: domain.ErrDuplicate,
			seed: func(t *testing.T, s *store.Store, taskID domain.TaskID) {
				if err := recordTaskTriage(context.Background(), s, escalated(taskID)); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openStore(t, store.Options{})
			taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
			if tc.seed != nil {
				tc.seed(t, s, taskID)
			}
			_, priorFound, _ := readTaskTriage(ctx, s, taskID)
			priorLines, _, _ := readTaskLines(t, s, taskID)
			before := taskSnapshot(t, ctx, s, taskID)
			if _, err := recordSpecifierTriage(ctx, s, tc.record(taskID), tc.counterpart); !errors.Is(err, tc.want) {
				t.Fatalf("write = %v, want %v", err, tc.want)
			}
			if after := taskSnapshot(t, ctx, s, taskID); after.EntityVersion != before.EntityVersion {
				t.Fatalf("a refused write advanced the task: %+v after %+v", after, before)
			}
			if _, found, err := readTaskTriage(ctx, s, taskID); err != nil || found != priorFound {
				t.Fatalf("record after the refusal = %t, %v", found, err)
			}
			if lines, _, err := readTaskLines(t, s, taskID); err != nil || len(lines) != len(priorLines) {
				t.Fatalf("lines after the refusal = %+v, %v", lines, err)
			}
		})
	}
}

// An escalated specifier's router line lands as version 2 over the task's
// triage line. Every other second version stays refused: a router line over
// an operator's line, a second routing over the router's own line, and any
// role but the specifier.
func TestRecordRouterTaskLinesOverTriage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	routeAt := triageRecordedAt.Add(time.Second)
	record := func(s *store.Store, taskID domain.TaskID, inputs ...domain.TaskLineInput) ([]domain.TaskLine, error) {
		var lines []domain.TaskLine
		err := s.Write(ctx, func(tx *store.WriteTx) error {
			var err error
			lines, err = tx.RecordRouterTaskLines(ctx, taskID, inputs, routeAt)
			return err
		})
		return lines, err
	}
	escalate := func(t *testing.T, s *store.Store, taskID domain.TaskID) *domain.TaskLine {
		t.Helper()
		line, err := recordSpecifierTriage(ctx, s, domain.TaskTriage{
			TaskID: taskID, Outcome: domain.TriageOutcomeCallFailed, Escalated: true, RecordedAt: triageRecordedAt,
		}, "claude-escalated")
		if err != nil {
			t.Fatal(err)
		}
		return line
	}

	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	triage := escalate(t, s, taskID)
	specifier := routerTaskLineInput(taskID)
	specifier.Role, specifier.Agent = domain.RoleSpecifier, "claude-escalated-b"
	implementer := routerTaskLineInput(taskID)
	lines, err := record(s, taskID, specifier, implementer)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].Version != 2 || lines[0].PredecessorID == nil ||
		*lines[0].PredecessorID != triage.ID || lines[1].Version != 1 {
		t.Fatalf("lines = %+v, want the specifier's second version over %s", lines, triage.ID)
	}
	if _, err := record(s, taskID, specifier); !errors.Is(err, domain.ErrTaskLineInconsistent) {
		t.Fatalf("a routing over the router's line: err = %v, want %v", err, domain.ErrTaskLineInconsistent)
	}

	operator := openStore(t, store.Options{})
	operatorTask, _ := seedTaskSubmissionTarget(t, ctx, operator)
	if _, err := recordTaskLineChange(t, operator, operatorTask, domain.RoleSpecifier, testSelection("claude-a"), nil, triageRecordedAt); err != nil {
		t.Fatal(err)
	}
	specifier.TaskID, specifier.SetBy = operatorTask, "router:"+string(operatorTask)
	if _, err := record(operator, operatorTask, specifier); !errors.Is(err, domain.ErrTaskLineInconsistent) {
		t.Fatalf("a routing over an operator's line: err = %v, want %v", err, domain.ErrTaskLineInconsistent)
	}

	// A triage-sourced line on another role is one no triage writes (the
	// store's own append takes it; admission's anchor refuses it).
	other := openStore(t, store.Options{})
	otherTask, _ := seedTaskSubmissionTarget(t, ctx, other)
	if err := other.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AppendTaskLine(ctx, domain.TaskLineInput{
			TaskID: otherTask, Role: domain.RoleImplementer, Agent: "claude-a",
			Source: domain.TaskLineSourceTriage, SetBy: string(otherTask),
		}, triageRecordedAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	implementer.TaskID, implementer.SetBy = otherTask, "router:"+string(otherTask)
	if _, err := record(other, otherTask, implementer); !errors.Is(err, domain.ErrTaskLineInconsistent) {
		t.Fatalf("an implementer over a triage line: err = %v, want %v", err, domain.ErrTaskLineInconsistent)
	}
}
