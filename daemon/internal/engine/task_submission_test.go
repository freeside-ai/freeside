package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// initiatorHolder is a mutable, concurrency-safe per-project initiator lookup so
// a test can change a project's configured policy between submissions and drive
// concurrent submissions.
type initiatorHolder struct {
	mu        sync.Mutex
	byProject map[domain.ProjectID]engine.ManualInitiator
}

func (h *initiatorHolder) lookup(p domain.ProjectID) (engine.ManualInitiator, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.byProject[p]
	return v, ok
}

func (h *initiatorHolder) set(p domain.ProjectID, init engine.ManualInitiator) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byProject[p] = init
}

func submissionInitiator(pathsValue string) engine.ManualInitiator {
	prov := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: domain.Digest(contentaddr.Sum([]byte("submit-task-test-policy")))}
	return engine.ManualInitiator{
		PolicyKeys: []domain.PolicyKey{
			{Key: specify.PolicySpecApproval, Value: "true", Provenance: prov},
			{Key: specify.PolicyMaxIterations, Value: "4", Provenance: prov},
			{Key: specify.PolicyStageActiveTime, Value: "1m", Provenance: prov},
			{Key: specify.PolicyApprovalWait, Value: "1m", Provenance: prov},
			{Key: specify.PolicyResearchAllowlist, Value: "https://docs.example", Provenance: prov},
			{Key: specify.PolicyResearchMaxBytes, Value: "1024", Provenance: prov},
			{Key: "paths", Value: pathsValue, Provenance: prov},
		},
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	}
}

func newSubmitTaskHarness(t *testing.T) (*signet.Service, *store.Store, *initiatorHolder) {
	t.Helper()
	ctx := context.Background()
	s := storetest.Open(t, t.TempDir()+"/engine.db", store.Options{})
	t.Cleanup(func() { _ = s.Close() })
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	holder := &initiatorHolder{byProject: map[domain.ProjectID]engine.ManualInitiator{
		"project-1": submissionInitiator("daemon/**"),
		"project-2": submissionInitiator("daemon/**"),
	}}
	submitter := engine.NewTaskSubmitter(blobs, holder.lookup)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: "device-1", DisplayName: "Mac", Status: domain.DeviceActive,
			PairedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	service := signet.NewService(s, signet.WithBlobStore(blobs), signet.WithTaskSubmitter(submitter))
	return service, s, holder
}

func submitCmd(commandID, project, source, name string) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: commandID, DeviceID: "device-1", Kind: domain.CommandKindSubmitTask,
		SubmitTask: signet.SubmitTaskPayload{ProjectID: domain.ProjectID(project), Source: source, Name: name},
	}
}

func taskRunCount(t *testing.T, s *store.Store, id domain.TaskID) int {
	t.Helper()
	var n int
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		ids, err := tx.TaskRunIDs(context.Background(), id)
		n = len(ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSubmitTaskCreatesTaskRunAndRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, s, _ := newSubmitTaskHarness(t)
	const source = "# Add a health endpoint\n\nExpose GET /health.\n"
	res, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, ""))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Submission == nil {
		t.Fatalf("no submission in result: %+v", res)
	}
	sub := *res.Submission
	wantDigest := domain.Digest(contentaddr.Sum([]byte(source)))
	if sub.SourceDigest != wantDigest || sub.TaskID == "" || sub.SpecificationRunID == "" {
		t.Fatalf("submission = %+v", sub)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetTask(ctx, sub.TaskID)
		if err != nil {
			return err
		}
		// The heading names the task with source operator (the namer never runs).
		if task.Name != (domain.DisplayName{Text: "Add a health endpoint", Source: domain.DisplayNameSourceOperator}) {
			t.Fatalf("task name = %+v", task.Name)
		}
		if len(task.CampaignIDs) != 1 {
			t.Fatalf("task campaigns = %v, want one", task.CampaignIDs)
		}
		run, err := tx.GetRun(ctx, sub.SpecificationRunID)
		if err != nil {
			return err
		}
		if run.TaskID != sub.TaskID {
			t.Fatalf("run task = %s, want %s", run.TaskID, sub.TaskID)
		}
		byKey, err := tx.GetTaskByIntakeKey(ctx, "project-1", "submission:client:cmd-1")
		if err != nil {
			return err
		}
		if byKey.ID != sub.TaskID {
			t.Fatalf("intake key task = %s, want %s", byKey.ID, sub.TaskID)
		}
		// The attempt-1 record carries the campaign's initial specification and
		// implementation runs; the work-unit declaration it carries is recorded
		// downstream when implementation begins, not at submission.
		attempt, err := tx.GetProductionAttempt(ctx, task.CampaignIDs[0], 1)
		if err != nil {
			return err
		}
		if attempt.SpecificationRunID != sub.SpecificationRunID || attempt.ImplementationRunID == "" {
			t.Fatalf("attempt = %+v", attempt)
		}
		if _, err := tx.GetArtifact(ctx, domain.ArtifactID("artifact-specification-"+contentaddr.Hex(string(wantDigest)))); err != nil {
			return err
		}
		if _, err := tx.GetArtifact(ctx, domain.ArtifactID("artifact-policy-"+contentaddr.Hex(string(run.PolicyDigest)))); err != nil {
			return err
		}
		recorded, err := tx.GetTaskSubmission(ctx, "cmd-1")
		if err != nil {
			return err
		}
		if recorded != sub {
			t.Fatalf("recorded submission = %+v, want %+v", recorded, sub)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := taskRunCount(t, s, sub.TaskID); got != 1 {
		t.Fatalf("task runs = %d, want 1", got)
	}
}

func TestSubmitTaskReplayAndDistinctNewTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, s, _ := newSubmitTaskHarness(t)
	const source = "Add retries to the uploader."
	first, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, ""))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// A retried command_id returns the recorded result and starts no second run.
	replay, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, ""))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if *replay.Submission != *first.Submission || replay.Revision != first.Revision {
		t.Fatalf("replay = %+v, want %+v", replay, first)
	}
	// A distinct command is deliberate new work, even with identical input.
	second, err := service.Submit(ctx, submitCmd("cmd-2", "project-1", source, ""))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Submission.TaskID == first.Submission.TaskID ||
		second.Submission.SpecificationRunID == first.Submission.SpecificationRunID {
		t.Fatalf("second submission reused identity: %+v vs %+v", *second.Submission, *first.Submission)
	}
	if got := taskRunCount(t, s, first.Submission.TaskID); got != 1 {
		t.Fatalf("task runs after two submissions = %d, want 1", got)
	}
	// The same source in another project creates a distinct task.
	other, err := service.Submit(ctx, submitCmd("cmd-3", "project-2", source, ""))
	if err != nil {
		t.Fatalf("other project: %v", err)
	}
	if other.Submission.TaskID == first.Submission.TaskID {
		t.Fatalf("other project reused task %s", other.Submission.TaskID)
	}
}

func TestSubmitTaskConcurrentProducesDistinctTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, s, _ := newSubmitTaskHarness(t)
	const source = "Two callers submit this at once."
	var wg sync.WaitGroup
	results := make([]signet.CommandResult, 2)
	errs := make([]error, 2)
	for i, id := range []string{"cmd-a", "cmd-b"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			results[i], errs[i] = service.Submit(ctx, submitCmd(id, "project-1", source, ""))
		}(i, id)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submission %d: %v", i, err)
		}
	}
	if results[0].Submission.TaskID == results[1].Submission.TaskID {
		t.Fatalf("concurrent submissions reused a task: %s vs %s",
			results[0].Submission.TaskID, results[1].Submission.TaskID)
	}
	if got := taskRunCount(t, s, results[0].Submission.TaskID); got != 1 {
		t.Fatalf("task runs = %d, want 1", got)
	}
}

func TestSubmitTaskOperatorNamesBelongToSeparateTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, _ := newSubmitTaskHarness(t)
	const source = "# Heading fallback\n\nBody."
	first, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, "Operator chose this"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Submission.Name != (domain.DisplayName{Text: "Operator chose this", Source: domain.DisplayNameSourceOperator}) {
		t.Fatalf("name = %+v, want the operator name", first.Submission.Name)
	}
	// The next submission owns its own name.
	second, err := service.Submit(ctx, submitCmd("cmd-2", "project-1", source, "A different name"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Submission.Name.Text != "A different name" || second.Submission.TaskID == first.Submission.TaskID {
		t.Fatalf("new submission did not own its name: %+v", second.Submission)
	}
}

func TestSubmitTaskConfigChangeMovesNoExistingIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, holder := newSubmitTaskHarness(t)
	const source = "Fetch reuses recorded identity."
	first, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, ""))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Change the project's configured policy: a fresh derivation would produce a
	// different run id, but the intake-key fetch reuses the recorded task and run.
	holder.set("project-1", submissionInitiator("app/**"))
	second, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, ""))
	if err != nil {
		t.Fatalf("second after config change: %v", err)
	}
	if second.Submission.TaskID != first.Submission.TaskID ||
		second.Submission.SpecificationRunID != first.Submission.SpecificationRunID {
		t.Fatalf("config change moved identity: %+v vs %+v", *second.Submission, *first.Submission)
	}
}

func TestSubmitTaskRejectsInvalidOperatorName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	badNames := []struct{ label, name string }{
		{"blank", "   "},
		{"multiline", "line one\nline two"},
		{"too long", strings.Repeat("é", 61)},
		{"credential", "ghp_" + strings.Repeat("A", 36)},
	}
	t.Run("rejected before any write", func(t *testing.T) {
		for _, bad := range badNames {
			t.Run(bad.label, func(t *testing.T) {
				service, s, _ := newSubmitTaskHarness(t)
				const source = "Work whose operator name is invalid."
				if _, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, bad.name)); !errors.Is(err, signet.ErrInvalidSubmitTaskPayload) {
					t.Fatalf("error = %v, want signet.ErrInvalidSubmitTaskPayload", err)
				}
				// The name is refused before the intake-key fetch and every write, so
				// nothing persisted: no submission record, source artifact, or task.
				digest := domain.Digest(contentaddr.Sum([]byte(source)))
				if err := s.Read(ctx, func(tx *store.ReadTx) error {
					if _, err := tx.GetTaskSubmission(ctx, "cmd-1"); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("submission record error = %v, want ErrNotFound", err)
					}
					if _, err := tx.GetArtifact(ctx, domain.ArtifactID("artifact-specification-"+contentaddr.Hex(string(digest)))); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("source artifact error = %v, want ErrNotFound", err)
					}
					if _, err := tx.GetTaskByIntakeKey(ctx, "project-1", "source:"+string(digest)); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("task intake key error = %v, want ErrNotFound", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("rejected even when the source already fetched a task", func(t *testing.T) {
		service, _, _ := newSubmitTaskHarness(t)
		const source = "# Existing task\n\nBody."
		if _, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, "")); err != nil {
			t.Fatalf("seed submit: %v", err)
		}
		// The same source now fetches the existing task, but the bad name is refused
		// before the fetch, so the command fails and does not return the task.
		if _, err := service.Submit(ctx, submitCmd("cmd-2", "project-1", source, strings.Repeat("é", 61))); !errors.Is(err, signet.ErrInvalidSubmitTaskPayload) {
			t.Fatalf("error = %v, want signet.ErrInvalidSubmitTaskPayload", err)
		}
	})
	t.Run("padded name is stored trimmed", func(t *testing.T) {
		service, s, _ := newSubmitTaskHarness(t)
		const source = "Padded operator name body."
		res, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, "  Padded name  "))
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		// The stored task name is the trimmed name; the composed publication title
		// derives from the same canonical value in SubmitTask, so the
		// reviewer-facing title and the stored name agree.
		want := domain.DisplayName{Text: "Padded name", Source: domain.DisplayNameSourceOperator}
		if res.Submission.Name != want {
			t.Fatalf("result name = %+v, want %+v", res.Submission.Name, want)
		}
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			task, err := tx.GetTask(ctx, res.Submission.TaskID)
			if err != nil {
				return err
			}
			if task.Name != want {
				t.Fatalf("stored task name = %+v, want %+v", task.Name, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSubmitTaskUnconfiguredProjectRefusedBeforeWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, s, _ := newSubmitTaskHarness(t)
	const source = "Work for an unconfigured project."
	if _, err := service.Submit(ctx, submitCmd("cmd-1", "project-unknown", source, "")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unconfigured project error = %v, want store.ErrNotFound", err)
	}
	// The transaction rolled back, so nothing persisted: no submission record and
	// no source artifact for the refused command.
	digest := domain.Digest(contentaddr.Sum([]byte(source)))
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetTaskSubmission(ctx, "cmd-1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("submission record error = %v, want ErrNotFound", err)
		}
		if _, err := tx.GetArtifact(ctx, domain.ArtifactID("artifact-specification-"+contentaddr.Hex(string(digest)))); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("source artifact error = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitTaskConcurrentReplayKeepsOriginalResult(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	service, s, _ := newSubmitTaskHarness(t)
	const deliveries = 8
	results := make([]signet.CommandResult, deliveries)
	errs := make([]error, deliveries)
	var wg sync.WaitGroup
	for i := range deliveries {
		wg.Go(func() {
			results[i], errs[i] = service.Submit(ctx, submitCmd("same-command", "project-1", "Same work", "Same name"))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		if *results[i].Submission != *results[0].Submission || results[i].Revision != results[0].Revision {
			t.Fatalf("delivery %d changed committed result", i)
		}
	}
	if taskRunCount(t, s, results[0].Submission.TaskID) != 1 {
		t.Fatal("replay created another run")
	}
}

func TestSubmitTaskRollbackAllowsFirstCommitWithSameIdentity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	service, s, holder := newSubmitTaskHarness(t)
	command := submitCmd("retry-after-refusal", "later-project", "Same work", "Same name")
	if _, err := service.Submit(ctx, command); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first = %v", err)
	}
	holder.set("later-project", submissionInitiator("daemon/**"))
	result, err := service.Submit(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if taskRunCount(t, s, result.Submission.TaskID) != 1 {
		t.Fatal("manual retry did not commit one run")
	}
}
