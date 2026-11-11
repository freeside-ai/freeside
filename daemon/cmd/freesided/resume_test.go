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
	"github.com/freeside-ai/freeside/daemon/internal/seedfixture"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// openSeededResumeStore opens a store at a topic-keyed path holding the
// representative fixture, with the attended_dev floor its admissions were
// recorded under. Its terminals are backed by admissions and outcomes, which
// resume's read authenticates before it shows an invocation's status.
func openSeededResumeStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seedTemplateStore(t, dbPath)
	st, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: seedfixture.AdmissionCapabilities,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedfixture.Seed(t.Context(), st, seedfixture.Representative); err != nil {
		t.Fatal(err)
	}
	return st, dbPath
}

func countRuns(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		runs, err := tx.ListRuns(t.Context())
		count = len(runs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestResumeTargetsExactLiveRunAndRefusesTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath := openSeededResumeStore(t)
	now := time.Now().UTC()
	liveID := domain.RunID("run-live")
	// The fixture's first retry-campaign attempt failed, with its outcome
	// and terminal recorded.
	terminalID := domain.RunID(fixtureActiveRunID)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: liveID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}); err != nil {
			return err
		}
		liveInvocation := domain.InvocationID("inv-live")
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: liveID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &liveInvocation, RecordedAt: now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	runCount := countRuns(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err := runResumeCommand(ctx, []string{"-db", dbPath, "-run", string(terminalID), "-once"}, &stdout, &stderr)
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
	if got := countRuns(t, st); got != runCount {
		t.Fatalf("resume changed run count to %d, want %d", got, runCount)
	}
}

func TestResumeReportsAuthenticatedPublishedOutcome(t *testing.T) {
	t.Parallel()
	err := terminalResumeError("run-published-after-rerun", domain.RunConclusion{
		Outcome: domain.RunOutcomePublished, Final: true,
	})
	if err == nil || !strings.Contains(err.Error(), `terminal in state "published"`) ||
		strings.Contains(err.Error(), `terminal in state "blocked"`) {
		t.Fatalf("published resume refusal = %v", err)
	}
}

// TestResumeRefusesSettledRuns pins the refusal for the settled outcomes
// (#3043): only an abandoned run points to reattempt, since a stopped task's
// cancellation fence refuses one and a handed-off run's work continues in its
// implementation run.
func TestResumeRefusesSettledRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		outcome   domain.RunOutcome
		hint      string
		reattempt bool
	}{
		{domain.RunOutcomeHandedOff, "its implementation run took over", false},
		{domain.RunOutcomeStopped, "the task was stopped", false},
		{domain.RunOutcomeAbandoned, "use freesided reattempt", true},
	} {
		err := terminalResumeError("run-settled", domain.RunConclusion{Outcome: tc.outcome, Final: true})
		if err == nil || !strings.Contains(err.Error(), `terminal in state "`+string(tc.outcome)+`"`) ||
			!strings.Contains(err.Error(), tc.hint) ||
			strings.Contains(err.Error(), "reattempt") != tc.reattempt {
			t.Errorf("%s resume refusal = %v", tc.outcome, err)
		}
	}
}

// TestResumeTaskUsesNewestRun resumes the fixture's retry-campaign task,
// whose failed first attempt (run-freeside-656) sorts after its running
// retry by id, so only the ordinal picks the retry.
func TestResumeTaskUsesNewestRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, dbPath := openSeededResumeStore(t)
	defer func() { _ = st.Close() }()
	taskID, admission, observed := fixtureRunningRun(t, st)
	newest := admission.RunID
	if newest >= fixtureActiveRunID {
		t.Fatalf("retry run %s sorts after %s; the test needs id order to disagree with ordinal order",
			newest, fixtureActiveRunID)
	}
	var runCount int
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		runs, err := tx.TaskRunIDs(ctx, taskID)
		runCount = len(runs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	args := []string{"-db", dbPath, "--task", string(taskID), "-once"}
	if err := runResumeCommand(ctx, args, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), string(newest)) || strings.Contains(stdout.String(), fixtureActiveRunID) {
		t.Fatalf("task snapshot = %q, want newest ordinal run %s", stdout.String(), newest)
	}
	at := observed.ObservedAt.Add(time.Minute)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
			InvocationID: admission.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeFailed, Summary: "failed",
			Cause: domain.FailureCauseUnknown, RecordedAt: at,
		}); err != nil {
			return err
		}
		terminal := domain.ObservedStatusFailed
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: newest, Kind: domain.MilestoneTerminalRecorded, InvocationID: &admission.InvocationID,
			Terminal: &terminal, RecordedAt: at,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := runResumeCommand(ctx, args, io.Discard, io.Discard); err == nil ||
		!strings.Contains(err.Error(), `run "`+string(newest)+`" is terminal`) ||
		!strings.Contains(err.Error(), "use freesided reattempt") {
		t.Fatalf("terminal newest run = %v, want refusal naming %s", err, newest)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		runs, err := tx.TaskRunIDs(ctx, taskID)
		if err == nil && len(runs) != runCount {
			t.Fatalf("resume changed task run count to %d, want %d", len(runs), runCount)
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
	seedTemplateStore(t, dbPath)
	submitted, err := runSubmitCommand(t.Context(), submitCommandConfig{
		DBPath: dbPath, TaskPath: taskPath, PolicyPath: policyPath,
		WorkUnitPath: declaredWorkUnit(t), PublicationPath: publicationPath, ProjectID: "project-resume",
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
	seedTemplateStore(t, dbPath)
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
