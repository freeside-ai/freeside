package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestResumeTargetsExactLiveRunAndRefusesTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, _, err := openStoreWithTopicKey(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	liveID := domain.RunID("run-live")
	terminalID := domain.RunID("run-terminal")
	terminalInvocation := domain.InvocationID("inv-terminal")
	terminalStatus := domain.ObservedStatusFailed
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, id := range []domain.RunID{liveID, terminalID} {
			if err := tx.PutRun(ctx, domain.Run{
				ID: id, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}); err != nil {
				return err
			}
		}
		liveInvocation := domain.InvocationID("inv-live")
		if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: liveID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &liveInvocation, RecordedAt: now,
		}); err != nil {
			return err
		}
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: terminalID, Kind: domain.MilestoneTerminalRecorded,
			InvocationID: &terminalInvocation, Terminal: &terminalStatus,
			RecordedAt: now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = runResumeCommand(ctx, []string{"-db", dbPath, "-run", string(terminalID), "-once"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `terminal in state "failed"`) ||
		!strings.Contains(err.Error(), "freesided reattempt") {
		t.Fatalf("terminal resume = %v, want state and reattempt refusal", err)
	}
	stdout.Reset()
	err = runResumeCommand(ctx, []string{"-db", dbPath, "-run", string(liveID), "-once"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), string(liveID)) || !strings.Contains(stdout.String(), "outcome  pending") {
		t.Fatalf("live resume output = %q, want exact run pending snapshot", stdout.String())
	}

	st, _, err = openStoreWithTopicKey(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		runs, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		if len(runs) != 2 {
			t.Fatalf("resume changed run count to %d, want 2", len(runs))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResumeReportsAuthenticatedPublishedOutcome(t *testing.T) {
	err := terminalResumeError("run-published-after-rerun", domain.RunConclusion{
		Outcome: domain.RunOutcomePublished, Final: true,
	})
	if err == nil || !strings.Contains(err.Error(), `terminal in state "published"`) ||
		strings.Contains(err.Error(), `terminal in state "blocked"`) {
		t.Fatalf("published resume refusal = %v", err)
	}
}

func TestResumeTaskUsesNewestRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, _, err := openStoreWithTopicKey(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var taskID domain.TaskID
	addRun := func(id domain.RunID) {
		t.Helper()
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutRun(ctx, domain.Run{
				ID: id, TaskID: taskID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}); err != nil {
				return err
			}
			run, err := tx.GetRun(ctx, id)
			if err != nil {
				return err
			}
			taskID = run.TaskID
			invocation := domain.InvocationID("inv-" + string(id))
			return tx.AppendRunMilestone(ctx, domain.RunMilestone{
				RunID: id, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation,
				RecordedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	addRun("run-z-older")
	addRun("run-a-newer")
	var stdout bytes.Buffer
	args := []string{"-db", dbPath, "--task", string(taskID), "-once"}
	if err := runResumeCommand(ctx, args, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "run-a-newer") || strings.Contains(stdout.String(), "run-z-older") {
		t.Fatalf("task snapshot = %q, want newest ordinal run", stdout.String())
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		terminal := domain.ObservedStatusFailed
		invocation := domain.InvocationID("inv-run-a-newer")
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: "run-a-newer", Kind: domain.MilestoneTerminalRecorded, InvocationID: &invocation,
			Terminal: &terminal, RecordedAt: time.Date(2026, 9, 12, 12, 1, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := runResumeCommand(ctx, args, io.Discard, io.Discard); err == nil ||
		!strings.Contains(err.Error(), `run "run-a-newer" is terminal`) || !strings.Contains(err.Error(), "use freesided reattempt") {
		t.Fatalf("terminal newest run = %v, want refusal instead of following older live run", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		runs, err := tx.TaskRunIDs(ctx, taskID)
		if err == nil && len(runs) != 2 {
			t.Fatalf("resume changed task run count to %d", len(runs))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResumeTaskBeforeSpecificationApproval(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	dbPath := filepath.Join(root, "state.db")
	submitted, err := runSubmitCommand(t.Context(), submitCommandConfig{
		DBPath: dbPath, TaskPath: taskPath, PolicyPath: policyPath,
		PublicationPath: publicationPath, ProjectID: "project-resume",
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var taskID domain.TaskID
	readErr := st.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), submitted.SpecificationRunID)
		taskID = run.TaskID
		return err
	})
	if err := errors.Join(readErr, st.Close()); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := runResumeCommand(t.Context(), []string{
		"-db", dbPath, "--task", string(taskID), "-once",
	}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), string(submitted.SpecificationRunID)) {
		t.Fatalf("pre-approval task snapshot = %q, want specification run %q", stdout.String(), submitted.SpecificationRunID)
	}
}

func TestResumeTaskWithoutRuns(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var task domain.Task
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(t.Context(), "project-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 42},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = runResumeCommand(t.Context(), []string{"-db", dbPath, "-task", string(task.ID), "-once"}, io.Discard, io.Discard)
	if !errors.Is(err, observe.ErrUsage) || !strings.Contains(err.Error(), "has no runs") {
		t.Fatalf("empty task = %v, want no-runs usage error", err)
	}
	err = runResumeCommand(t.Context(), []string{"-db", dbPath, "-task", "task-missing", "-once"}, io.Discard, io.Discard)
	if !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), `task "task-missing"`) {
		t.Fatalf("missing task = %v, want identified not-found error", err)
	}
}

func TestResumeSelectorsFailBeforeOpeningStore(t *testing.T) {
	t.Parallel()
	for _, selectors := range [][]string{nil, {"-task", "task-1", "-run", "run-1"}} {
		dbPath := filepath.Join(t.TempDir(), "state.db")
		args := append([]string{"-db", dbPath}, selectors...)
		err := runResumeCommand(t.Context(), args, io.Discard, io.Discard)
		if !errors.Is(err, observe.ErrUsage) || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("selectors %v = %v, want usage error", selectors, err)
		}
		if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid command created database: %v", err)
		}
	}
}
