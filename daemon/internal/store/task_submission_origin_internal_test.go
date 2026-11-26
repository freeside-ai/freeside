package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	originTestRepositoryID = 1278475858
	originTestRepository   = "freeasinbird/gh-imgup"
)

// seedOriginTestTarget registers the project a daemon-origin submission's
// refusal must name and the task and specification run its row references.
func seedOriginTestTarget(t *testing.T, ctx context.Context, st *Store) (domain.TaskID, domain.RunID) {
	t.Helper()
	var taskID domain.TaskID
	runID := domain.RunID("run-origin-1")
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{
			ID: "proj-1", Repo: originTestRepository, RepositoryID: originTestRepositoryID,
		}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
			Kind: domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{
				Repo: originTestRepository, RepositoryID: originTestRepositoryID, IssueNumber: 42,
			},
		})
		if err != nil {
			return err
		}
		taskID = task.ID
		return tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "proj-1", TaskID: taskID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		})
	}); err != nil {
		t.Fatal(err)
	}
	return taskID, runID
}

func originTestRefusal(
	t *testing.T, class domain.ImageUpkeepRefusalClass, repositoryID int64,
) domain.ImageUpkeepRefusal {
	t.Helper()
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: repositoryID, Repository: originTestRepository,
		Harness: domain.HarnessClientClaudeCode, TipSHA: strings.Repeat("e", 40),
		MeasuredFromImageID: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		Class:               class,
		Findings: []domain.ImageUpkeepClauseFinding{{
			Clause: domain.RebuildRefusalLockfileInconsistent, Detail: "lockfile root disagrees",
		}},
		RecordedAt: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

// TestDaemonSubmissionRequiresRepairRefusal: only project-image upkeep
// submits a daemon-origin task, and only for a repair-class refusal on the
// project's own repository, so the store refuses any other daemon-origin
// write and fails closed on a stored one whose refusal no longer backs it.
func TestDaemonSubmissionRequiresRepairRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openProjectImageStore(t)
	taskID, runID := seedOriginTestTarget(t, ctx, st)
	repair := originTestRefusal(t, domain.ImageUpkeepRefusalRepair, originTestRepositoryID)
	policy := originTestRefusal(t, domain.ImageUpkeepRefusalPolicy, originTestRepositoryID)
	foreign := originTestRefusal(t, domain.ImageUpkeepRefusalRepair, 42)
	submission := func(source domain.Digest) domain.TaskSubmission {
		return domain.TaskSubmission{
			CommandID: domain.ImageUpkeepSubmissionIdentity(repair.ID), Origin: domain.TaskSubmissionOriginDaemon, ProjectID: "proj-1",
			SourceDigest: source, TaskID: taskID, SpecificationRunID: runID,
			Name: domain.DisplayName{Text: "Repair the npm lockfile", Source: domain.DisplayNameSourceName},
		}
	}
	put := func(s domain.TaskSubmission) error {
		return st.Write(ctx, func(tx *WriteTx) error { return tx.PutTaskSubmission(ctx, s) })
	}

	if err := put(submission(repair.ID)); !errors.Is(err, ErrDaemonSubmissionUnsourced) {
		t.Fatalf("unrecorded refusal: put = %v, want ErrDaemonSubmissionUnsourced", err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		for _, refusal := range []domain.ImageUpkeepRefusal{repair, policy, foreign} {
			if _, err := tx.RecordImageUpkeepRefusal(ctx, refusal); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]domain.Digest{
		"policy-class refusal":      policy.ID,
		"another repository's":      foreign.ID,
		"digest that is no refusal": domain.Digest("sha256:" + strings.Repeat("0", 64)),
	} {
		if err := put(submission(source)); !errors.Is(err, ErrDaemonSubmissionUnsourced) {
			t.Fatalf("%s: put = %v, want ErrDaemonSubmissionUnsourced", name, err)
		}
	}

	// The command ID's namespace says which kind of source the row names, so
	// a daemon command outside both namespaces is refused even when its
	// source is a repair refusal.
	outside := submission(repair.ID)
	outside.CommandID = "daemon-cmd-1"
	if err := put(outside); !errors.Is(err, ErrDaemonSubmissionUnsourced) {
		t.Fatalf("command outside the daemon namespaces: put = %v, want ErrDaemonSubmissionUnsourced", err)
	}

	want := submission(repair.ID)
	if err := put(want); err != nil {
		t.Fatalf("repair refusal: put = %v", err)
	}
	read := func() (domain.TaskSubmission, error) {
		var got domain.TaskSubmission
		err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			got, _, err = tx.GetTaskSubmissionSnapshot(ctx, want.CommandID)
			return err
		})
		return got, err
	}
	got, err := read()
	if err != nil || got.Origin != domain.TaskSubmissionOriginDaemon || got.DeviceID != nil ||
		got.SourceDigest != repair.ID {
		t.Fatalf("read = %+v err=%v, want the daemon-origin submission", got, err)
	}

	// The refusal row is the authority, re-read on decode: a stored row whose
	// refusal is gone reads as inconsistent, not as a submission.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM image_upkeep_refusals WHERE id = ?`, repair.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); !errors.Is(err, ErrRowInconsistent) || !errors.Is(err, ErrDaemonSubmissionUnsourced) {
		t.Fatalf("read without its refusal: err = %v, want ErrRowInconsistent", err)
	}
}

// TestDaemonSubmissionRequiresBumpOccurrence: an agent-base bump submission
// names the agent_base_bump occurrence that opened it and carries the
// daemon's fixed task text, so the store refuses one naming any other
// occurrence or text, and fails closed on a stored one whose occurrence is
// gone.
func TestDaemonSubmissionRequiresBumpOccurrence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openProjectImageStore(t)
	taskID, runID := seedOriginTestTarget(t, ctx, st)
	nominal := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	interval := int64(7 * 24 * 60 * 60)
	for _, kind := range []domain.ScheduleKind{domain.ScheduleAgentBaseBump, domain.ScheduleDoctor} {
		id := domain.ScheduleID("schedule-" + string(kind))
		schedule, err := domain.NewSchedule(domain.ScheduleInput{
			ID: id, ProjectID: "project-system", Kind: kind,
			Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
			CreatedAt: nominal.Add(-time.Hour), IntervalSeconds: &interval,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutSchedule(ctx, schedule) }); err != nil {
			t.Fatal(err)
		}
		if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.CreatePendingScheduleOccurrence(ctx, domain.ScheduleOccurrence{
				ScheduleID: id, Generation: 1, NominalFireAt: nominal,
				Status: domain.OccurrencePending, CreatedAt: nominal,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	bump := domain.ScheduleID("schedule-" + string(domain.ScheduleAgentBaseBump))
	submission := func(scheduleID domain.ScheduleID, generation int64, source domain.Digest) domain.TaskSubmission {
		return domain.TaskSubmission{
			CommandID: domain.AgentBaseBumpSubmissionIdentity(scheduleID, generation, nominal),
			Origin:    domain.TaskSubmissionOriginDaemon, ProjectID: "proj-1",
			SourceDigest: source, TaskID: taskID, SpecificationRunID: runID,
			Name: domain.DisplayName{Text: domain.AgentBaseBumpTaskName, Source: domain.DisplayNameSourceName},
		}
	}
	put := func(s domain.TaskSubmission) error {
		return st.Write(ctx, func(tx *WriteTx) error { return tx.PutTaskSubmission(ctx, s) })
	}
	fixed := domain.AgentBaseBumpSourceDigest()
	for name, s := range map[string]domain.TaskSubmission{
		"unrecorded occurrence":     submission(bump, 2, fixed),
		"another kind's occurrence": submission("schedule-"+domain.ScheduleID(domain.ScheduleDoctor), 1, fixed),
		"other task text":           submission(bump, 1, domain.Digest("sha256:"+strings.Repeat("0", 64))),
	} {
		if err := put(s); !errors.Is(err, ErrDaemonSubmissionUnsourced) {
			t.Fatalf("%s: put = %v, want ErrDaemonSubmissionUnsourced", name, err)
		}
	}

	want := submission(bump, 1, fixed)
	if err := put(want); err != nil {
		t.Fatalf("bump occurrence: put = %v", err)
	}
	list := func(project domain.ProjectID) ([]domain.TaskSubmission, error) {
		var got []domain.TaskSubmission
		err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			got, err = tx.ListAgentBaseBumpSubmissions(ctx, project)
			return err
		})
		return got, err
	}
	if got, err := list("proj-1"); err != nil || len(got) != 1 || got[0].CommandID != want.CommandID {
		t.Fatalf("list = %+v err=%v, want the bump submission", got, err)
	}
	if got, err := list("proj-2"); err != nil || len(got) != 0 {
		t.Fatalf("another project's list = %+v err=%v, want none", got, err)
	}

	// A row keeps the text of the build that wrote it: one an earlier build
	// worded differently still decodes, so rewording the text never strands
	// a recorded bump.
	earlier := domain.Digest("sha256:" + strings.Repeat("1", 64))
	if _, err := st.db.ExecContext(ctx, `UPDATE task_submission_commands SET body = replace(body, ?, ?)
		WHERE command_id = ?`, string(fixed), string(earlier), want.CommandID); err != nil {
		t.Fatal(err)
	}
	if got, err := list("proj-1"); err != nil || len(got) != 1 || got[0].SourceDigest != earlier {
		t.Fatalf("list of an earlier build's bump = %+v err=%v, want it decoded", got, err)
	}

	// The occurrence is the authority, re-read on decode.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM schedule_occurrences WHERE schedule_id = ?`, bump); err != nil {
		t.Fatal(err)
	}
	if _, err := list("proj-1"); !errors.Is(err, ErrRowInconsistent) || !errors.Is(err, ErrDaemonSubmissionUnsourced) {
		t.Fatalf("list without its occurrence: err = %v, want ErrRowInconsistent", err)
	}
}
