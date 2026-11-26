package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const bumpTestSchedule domain.ScheduleID = "schedule-agent-base-bump"

// recordBumpOccurrences arms the bump schedule and records one pending
// occurrence per nominal fire time, as the scheduler does before it runs the
// handler, and returns the events the handler would receive.
func recordBumpOccurrences(t *testing.T, st *store.Store, nominals ...time.Time) []domain.ScheduleEvent {
	t.Helper()
	ctx := context.Background()
	interval := int64(7 * 24 * 60 * 60)
	schedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: bumpTestSchedule, ProjectID: "project-system", Kind: domain.ScheduleAgentBaseBump,
		Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		CreatedAt: nominals[0].Add(-time.Hour), IntervalSeconds: &interval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutSchedule(ctx, schedule) }); err != nil {
		t.Fatal(err)
	}
	events := make([]domain.ScheduleEvent, 0, len(nominals))
	for _, nominal := range nominals {
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.CreatePendingScheduleOccurrence(ctx, domain.ScheduleOccurrence{
				ScheduleID: bumpTestSchedule, Generation: 1, NominalFireAt: nominal,
				Status: domain.OccurrencePending, CreatedAt: nominal,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		events = append(events, domain.ScheduleEvent{
			ScheduleID: bumpTestSchedule, Generation: 1, NominalFireAt: nominal,
		})
	}
	return events
}

func requireNoBumpSubmission(t *testing.T, st *store.Store, event domain.ScheduleEvent) {
	t.Helper()
	identity := domain.AgentBaseBumpSubmissionIdentity(event.ScheduleID, event.Generation, event.NominalFireAt)
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		if _, err := tx.GetTaskSubmission(context.Background(), identity); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("submission %s = %v, want ErrNotFound", identity, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A bump occurrence opens one daemon-origin task from the fixed text, and
// submitting it again converges on it. While that task is open a later
// occurrence opens nothing and returns it; once the task closes, the later
// occurrence opens its own.
func TestSubmitAgentBaseBumpTaskKeepsOneTaskOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, st := newImageRepairHarness(t, []domain.ProjectID{repairTestProject})
	nominal := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	events := recordBumpOccurrences(t, st, nominal, nominal.Add(7*24*time.Hour))

	submission, err := service.SubmitAgentBaseBumpTask(ctx, repairTestProject, events[0])
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.AgentBaseBumpSubmissionIdentity(bumpTestSchedule, 1, nominal)
	if submission.CommandID != identity || submission.Origin != domain.TaskSubmissionOriginDaemon ||
		submission.DeviceID != nil || submission.SourceDigest != domain.AgentBaseBumpSourceDigest() ||
		submission.TaskID == "" {
		t.Fatalf("submission = %+v", submission)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetTask(ctx, submission.TaskID)
		if err != nil {
			return err
		}
		if task.Name.Text != domain.AgentBaseBumpTaskName {
			t.Fatalf("task name = %+v", task.Name)
		}
		source, err := tx.GetArtifact(ctx, task.Source.WorkItemArtifactID)
		if err != nil {
			return err
		}
		if source.Digest != domain.AgentBaseBumpSourceDigest() {
			t.Fatalf("source artifact digest = %s, want the fixed text's", source.Digest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if again, err := service.SubmitAgentBaseBumpTask(ctx, repairTestProject, events[0]); err != nil ||
		!reflect.DeepEqual(again, submission) {
		t.Fatalf("resubmit = %+v err=%v, want the recorded %+v", again, err, submission)
	}

	if open, err := service.SubmitAgentBaseBumpTask(ctx, repairTestProject, events[1]); err != nil ||
		!reflect.DeepEqual(open, submission) {
		t.Fatalf("later occurrence while open = %+v err=%v, want the open %+v", open, err, submission)
	}
	requireNoBumpSubmission(t, st, events[1])

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AbandonTask(ctx, submission.TaskID, nominal.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := service.SubmitAgentBaseBumpTask(ctx, repairTestProject, events[1])
	if err != nil {
		t.Fatal(err)
	}
	if reopened.TaskID == submission.TaskID ||
		reopened.CommandID != domain.AgentBaseBumpSubmissionIdentity(bumpTestSchedule, 1, events[1].NominalFireAt) {
		t.Fatalf("after the task closed = %+v, want a new task for the later occurrence", reopened)
	}
}

// Only a recorded bump occurrence opens a task, and a project with no
// submission policy opens nothing.
func TestSubmitAgentBaseBumpTaskRefusesAnUnrecordedOccurrence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nominal := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	service, st := newImageRepairHarness(t, []domain.ProjectID{repairTestProject})
	recordBumpOccurrences(t, st, nominal)
	unrecorded := domain.ScheduleEvent{ScheduleID: bumpTestSchedule, Generation: 2, NominalFireAt: nominal}
	if _, err := service.SubmitAgentBaseBumpTask(ctx, repairTestProject, unrecorded); !errors.Is(err, store.ErrDaemonSubmissionUnsourced) {
		t.Fatalf("unrecorded occurrence: submit = %v, want ErrDaemonSubmissionUnsourced", err)
	}
	requireNoBumpSubmission(t, st, unrecorded)

	unconfigured, unconfiguredStore := newImageRepairHarness(t, nil)
	events := recordBumpOccurrences(t, unconfiguredStore, nominal)
	if _, err := unconfigured.SubmitAgentBaseBumpTask(ctx, repairTestProject, events[0]); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unconfigured project: submit = %v, want ErrNotFound", err)
	}
	requireNoBumpSubmission(t, unconfiguredStore, events[0])
}
