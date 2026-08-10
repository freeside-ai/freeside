package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestFakePublicationTerminalItemIDsUseWorkflowNamespace(t *testing.T) {
	ids := []domain.ItemID{
		FakePublicationReadyItemID("run-1"),
		FakePublicationBlockedItemID("run-1"),
		FakePublicationReadyItemID("run-2"),
		FakePublicationBlockedItemID("run-2"),
	}
	seen := map[domain.ItemID]bool{}
	for _, id := range ids {
		if !strings.HasPrefix(string(id), "fake-publication-") {
			t.Fatalf("terminal item id %q is outside workflow namespace", id)
		}
		if seen[id] {
			t.Fatalf("terminal item id collision: %q", id)
		}
		seen[id] = true
	}
}

type fakePublicationReconciliationFixture struct {
	run     domain.Run
	policy  domain.ResolvedPolicy
	entries map[string]store.QueueEntry
}

func (f fakePublicationReconciliationFixture) GetRun(
	_ context.Context,
	runID domain.RunID,
) (domain.Run, error) {
	if f.run.ID != runID {
		return domain.Run{}, store.ErrNotFound
	}
	return f.run, nil
}

func (f fakePublicationReconciliationFixture) GetResolvedPolicy(
	_ context.Context,
	runID domain.RunID,
) (domain.ResolvedPolicy, error) {
	if f.policy.RunID != runID {
		return domain.ResolvedPolicy{}, store.ErrNotFound
	}
	return f.policy, nil
}

func (f fakePublicationReconciliationFixture) GetOutbox(
	_ context.Context,
	key string,
) (store.QueueEntry, error) {
	entry, ok := f.entries[key]
	if !ok {
		return store.QueueEntry{}, store.ErrNotFound
	}
	return entry, nil
}

func TestFakePublicationReconciliationRevalidatesInvocationOwners(t *testing.T) {
	task := validFakePublicationTask(t)
	owners, err := expectedFakePublicationInvocationOwners(task)
	if err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]store.QueueEntry, len(owners))
	for _, owner := range owners {
		if owner.Role == "verification" {
			owner.RunID = "run-first-verifier"
		}
		payload, err := json.Marshal(owner)
		if err != nil {
			t.Fatal(err)
		}
		key := fakePublicationInvocationOwnerKey(owner.InvocationID)
		entries[key] = store.QueueEntry{
			IdempotencyKey: key, Kind: fakePublicationInvocationOwnerKind,
			Payload: payload, Status: "dispatched",
		}
	}
	valid := fakePublicationReconciliationFixture{
		run: publicationRun(task), policy: publicationPolicy(task), entries: entries,
	}
	if err := validateFakePublicationReconciliation(t.Context(), valid, task); err != nil {
		t.Fatalf("valid reconciliation bindings: %v", err)
	}

	for _, owner := range owners {
		t.Run(owner.Role+" missing", func(t *testing.T) {
			fixture := valid
			fixture.entries = cloneFakePublicationEntries(entries)
			delete(fixture.entries, fakePublicationInvocationOwnerKey(owner.InvocationID))
			if err := validateFakePublicationReconciliation(t.Context(), fixture, task); err == nil {
				t.Fatal("missing invocation owner passed reconciliation validation")
			}
		})
		t.Run(owner.Role+" substituted", func(t *testing.T) {
			fixture := valid
			fixture.entries = cloneFakePublicationEntries(entries)
			substitute := owner
			substitute.BindingDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
			payload, err := json.Marshal(substitute)
			if err != nil {
				t.Fatal(err)
			}
			key := fakePublicationInvocationOwnerKey(owner.InvocationID)
			entry := fixture.entries[key]
			entry.Payload = payload
			fixture.entries[key] = entry
			if err := validateFakePublicationReconciliation(t.Context(), fixture, task); err == nil ||
				!errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("substituted invocation owner error = %v", err)
			}
		})
		t.Run(owner.Role+" incomplete", func(t *testing.T) {
			fixture := valid
			fixture.entries = cloneFakePublicationEntries(entries)
			key := fakePublicationInvocationOwnerKey(owner.InvocationID)
			entry := fixture.entries[key]
			entry.Status = "pending"
			fixture.entries[key] = entry
			if err := validateFakePublicationReconciliation(t.Context(), fixture, task); err == nil ||
				!errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("incomplete invocation owner error = %v", err)
			}
		})
	}
}

func TestDecodeFakePublicationInvocationOwnerRejectsUppercaseBindingDigest(t *testing.T) {
	task := validFakePublicationTask(t)
	owners, err := expectedFakePublicationInvocationOwners(task)
	if err != nil {
		t.Fatal(err)
	}
	owner := owners[0]
	owner.BindingDigest = domain.Digest("sha256:" + strings.Repeat("A", 64))
	payload, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := decodeFakePublicationInvocationOwner(payload); err == nil {
		t.Fatal("uppercase binding digest passed invocation-owner validation")
	}
}

func TestFakePublicationReconciliationRevalidatesResolvedPolicy(t *testing.T) {
	task := validFakePublicationTask(t)
	valid := fakePublicationReconciliationFixture{
		run: publicationRun(task), policy: publicationPolicy(task),
	}

	tests := []struct {
		name   string
		mutate func(*fakePublicationReconciliationFixture)
	}{
		{"missing", func(f *fakePublicationReconciliationFixture) {
			f.policy = domain.ResolvedPolicy{}
		}},
		{"substituted", func(f *fakePublicationReconciliationFixture) {
			f.policy.Keys[0].Value = "sha256:substituted"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := valid
			fixture.policy.Keys = append([]domain.PolicyKey(nil), valid.policy.Keys...)
			test.mutate(&fixture)
			if err := validateFakePublicationRun(t.Context(), fixture, task); err == nil {
				t.Fatal("invalid publication policy passed reconciliation validation")
			}
		})
	}
}

func TestEnsureFakePublicationPolicyConvergesLegacyV1Run(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	task := validFakePublicationTask(t)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, legacyPublicationRun(task))
	}); err != nil {
		t.Fatal(err)
	}

	if err := ensureFakePublicationPolicy(ctx, st, task); err != nil {
		t.Fatalf("converge legacy publication policy: %v", err)
	}
	if err := ensureFakePublicationPolicy(ctx, st, task); err != nil {
		t.Fatalf("repeat converged publication policy: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		return validateFakePublicationRun(ctx, tx, task)
	}); err != nil {
		t.Fatalf("validate converged publication run: %v", err)
	}
}

// TestFakePublicationPendingLegacyRecoveryConvergesOwnedSchedule covers the
// crash window after publication watches commit but before finishTask marks
// their owning task dispatched. Startup recovery must authenticate that
// pending owner's schedule before the scheduler can fire it.
func TestFakePublicationPendingLegacyRecoveryConvergesOwnedSchedule(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	task := validFakePublicationTask(t)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: FakePublicationReadyItemID(task.RunID), ProjectID: task.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &task.RunID,
		},
		Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "published and verified", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRReference: &domain.PRReference{Repo: task.Repo, Number: 7},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	itemID, itemVersion := item.ID, item.ItemVersion
	legacyDigest := task.TrustProfileDigest
	fireAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	schedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        PublicationWatchScheduleID(domain.SchedulePRChecksDeadline, item.ID),
		ProjectID: task.ProjectID, Kind: domain.SchedulePRChecksDeadline,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &itemVersion,
		},
		RunID: &task.RunID, PolicyDigest: &legacyDigest,
		CreatedAt: fireAt.Add(-time.Hour), FireAt: &fireAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, legacyPublicationRun(task)); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		return tx.PutSchedule(ctx, schedule)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			ctx, fakePublicationTaskKey(task.RunID),
			fakePublicationTaskKind, mustEncodeFakePublicationTask(task),
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	recovery := &fakePublicationPolicyRecovery{store: st}
	if err := recovery.converge(ctx); err != nil {
		t.Fatalf("pending legacy recovery: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if err := validateFakePublicationRun(ctx, tx, task); err != nil {
			return err
		}
		got, err := tx.GetSchedule(ctx, schedule.ID)
		if err != nil {
			return err
		}
		if got.PolicyDigest == nil || *got.PolicyDigest != publicationPolicy(task).Digest {
			t.Fatalf("recovered schedule policy = %v", got.PolicyDigest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFakePublicationDispatchedLegacyRecoveryRunsOncePerStoreEpoch(t *testing.T) {
	ctx := t.Context()
	tempDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tempDir, "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	recovery := &fakePublicationPolicyRecovery{store: st}
	engine := &Engine{fakePublicationPolicy: recovery}
	if err := engine.ConvergeLegacyFakePublicationPolicies(ctx); err != nil {
		t.Fatalf("initial recovery pass: %v", err)
	}

	const key = "engine.fake_publication/late-invalid-history"
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if _, _, err := tx.EnqueueOutbox(
			ctx, key, fakePublicationTaskKind, []byte(`{}`),
		); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, key)
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.ConvergeLegacyFakePublicationPolicies(ctx); err != nil {
		t.Fatalf("steady-state reconciliation rescanned dispatched history: %v", err)
	}

	checkpointSource, err := store.Open(
		ctx, filepath.Join(tempDir, "checkpoint-source.db"), store.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpointSource.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if _, _, err := tx.EnqueueOutbox(
			ctx, key, fakePublicationTaskKind, []byte(`{}`),
		); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, key)
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(tempDir, "checkpoint.db")
	if err := checkpointSource.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := checkpointSource.Close(); err != nil {
		t.Fatal(err)
	}

	restored, err := st.Restore(ctx, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if restored.SyncEpoch == recovery.epoch {
		t.Fatal("restore did not rotate the recovery epoch")
	}
	if err := engine.ConvergeLegacyFakePublicationPolicies(ctx); err == nil {
		t.Fatal("same workflow skipped dispatched recovery after in-place restore")
	}
	if err := engine.ConvergeLegacyFakePublicationPolicies(ctx); err == nil {
		t.Fatal("failed restored recovery was not retried")
	}
}

func cloneFakePublicationEntries(
	entries map[string]store.QueueEntry,
) map[string]store.QueueEntry {
	cloned := make(map[string]store.QueueEntry, len(entries))
	for key, entry := range entries {
		entry.Payload = append([]byte(nil), entry.Payload...)
		cloned[key] = entry
	}
	return cloned
}

func validFakePublicationTask(t *testing.T) fakePublicationTask {
	t.Helper()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	return fakePublicationTask{
		Version: fakePublicationTaskVersion,
		RunID:   "run-1", ProjectID: "project-1", StoreEpoch: "epoch-1",
		WorkspaceDir: t.TempDir(), HandoffDir: t.TempDir(),
		HandoffDigest:            "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Repo:                     "freeside-ai/freeside",
		BaseRef:                  "main",
		BaseSHA:                  "0123456789012345678901234567890123456789",
		AllowedPaths:             []string{"daemon/**"},
		RecipeDigest:             "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		RecipePath:               ".freeside/verify.json",
		TrustProfileDigest:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		VerificationInvocationID: "verify-1",
		PublicationInvocationID:  "publish-1",
		Title:                    "Publish candidate",
		Body:                     "Verified candidate.",
		CommitDate:               now,
		StartedAt:                now,
		OperatingMode:            OperatingModeAttendedDev,
	}
}

func TestFakePublicationBackupPayloadDigests(t *testing.T) {
	task := validFakePublicationTask(t)
	payload := mustEncodeFakePublicationTask(task)
	entry := store.QueueEntry{
		IdempotencyKey: fakePublicationTaskKey(task.RunID),
		Kind:           FakePublicationTaskKind,
		Payload:        payload,
	}
	got, err := FakePublicationBackupPayloadDigests(entry)
	if err != nil {
		t.Fatalf("FakePublicationBackupPayloadDigests: %v", err)
	}
	if len(got) != 1 || got[0] != task.RecipeDigest {
		t.Fatalf("backup payload digests = %v, want [%s]", got, task.RecipeDigest)
	}
	entry.Payload = []byte(`{}`)
	if _, err := FakePublicationBackupPayloadDigests(entry); err == nil {
		t.Fatal("FakePublicationBackupPayloadDigests accepted an invalid task")
	}
	entry.Payload = payload
	entry.IdempotencyKey = fakePublicationTaskKey("other-run")
	if _, err := FakePublicationBackupPayloadDigests(entry); !errors.Is(
		err, domain.ErrParentKeyMismatch,
	) {
		t.Fatalf("mismatched task key error = %v, want ErrParentKeyMismatch", err)
	}
	entry.IdempotencyKey = fakePublicationTaskKey(task.RunID)
	entry.Kind = FakePublicationInvocationOwnerKind
	if _, err := FakePublicationBackupPayloadDigests(entry); !errors.Is(
		err, domain.ErrParentKeyMismatch,
	) {
		t.Fatalf("mismatched task kind error = %v, want ErrParentKeyMismatch", err)
	}
}

func TestFakePublicationDurabilityHelpers(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one", "two")
	if err := makeFakePublicationDirectory(nested, 0o700); err != nil {
		t.Fatalf("make durable directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "candidate.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncFakePublicationTree(filepath.Join(root, "one")); err != nil {
		t.Fatalf("sync tree: %v", err)
	}
	if err := syncFakePublicationDirectory(root); err != nil {
		t.Fatalf("sync directory: %v", err)
	}
}

func TestFakePublicationDurabilityHelpersRejectNonDirectoryAncestor(t *testing.T) {
	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := makeFakePublicationDirectory(filepath.Join(notDirectory, "child"), 0o700); err == nil {
		t.Fatal("created directory beneath a regular file")
	}
}

func TestOpenValidatedFakePublicationWorkspacePinsOriginalRoot(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "candidate.txt"), []byte("captured\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := openValidatedFakePublicationWorkspace(workspace, []string{t.TempDir()})
	if err != nil {
		t.Fatalf("open validated workspace: %v", err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close workspace root: %v", err)
		}
	})

	moved := filepath.Join(parent, "moved")
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "candidate.txt"), []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := fs.ReadFile(root.FS(), "candidate.txt")
	if err != nil {
		t.Fatalf("read pinned workspace: %v", err)
	}
	if string(body) != "captured\n" {
		t.Fatalf("pinned workspace body = %q, want captured bytes", body)
	}
}

func TestMakeFakePublicationDirectoryRetriesExistingParentSync(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "candidate")
	failedSync := errors.New("injected parent sync failure")
	calls := 0
	err := makeFakePublicationDirectoryWithSync(target, 0o700, func(string) error {
		calls++
		if calls == 2 {
			return failedSync
		}
		return nil
	})
	if !errors.Is(err, failedSync) {
		t.Fatalf("first creation error = %v, want injected sync failure", err)
	}
	if info, statErr := os.Stat(target); statErr != nil || !info.IsDir() {
		t.Fatalf("failed sync did not leave the created directory for retry: %v", statErr)
	}

	var synced []string
	if err := makeFakePublicationDirectoryWithSync(target, 0o700, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatalf("retry existing directory: %v", err)
	}
	if len(synced) != 1 || synced[0] != root {
		t.Fatalf("retry synced %v, want the existing target's parent %s", synced, root)
	}
}

func TestSyncFakePublicationCheckpointDirectoryRetriesExistingEntry(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "candidate.json")
	failedSync := errors.New("injected checkpoint directory sync failure")
	err := syncFakePublicationCheckpointDirectory(path, func(got string) error {
		if got != parent {
			t.Fatalf("synced %q, want checkpoint parent %q", got, parent)
		}
		return failedSync
	})
	if !errors.Is(err, failedSync) {
		t.Fatalf("checkpoint sync error = %v, want injected failure", err)
	}
}

func TestInstallFakePublicationHandoffRollsBackAfterDirectorySyncFailure(t *testing.T) {
	parent := t.TempDir()
	output := filepath.Join(parent, "output")
	destination := filepath.Join(parent, "handoff")
	if err := os.Mkdir(output, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	failedSync := errors.New("injected handoff directory sync failure")
	calls := 0
	err := installFakePublicationHandoff(output, destination, func(path string) error {
		if path != parent {
			t.Fatalf("synced %q, want %q", path, parent)
		}
		calls++
		if calls == 1 {
			return failedSync
		}
		return nil
	})
	if !errors.Is(err, failedSync) {
		t.Fatalf("install error = %v, want injected sync failure", err)
	}
	if calls != 2 {
		t.Fatalf("directory sync calls = %d, want commit and rollback barriers", calls)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed install left destination: %v", err)
	}
}

func TestInstallFakePublicationCheckpointDoesNotReplaceConcurrentWinner(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "candidate.json")
	sources := []string{
		filepath.Join(root, "candidate-one"),
		filepath.Join(root, "candidate-two"),
	}
	for i, source := range sources {
		if err := os.WriteFile(source, []byte{byte('1' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	results := make(chan bool, len(sources))
	errs := make(chan error, len(sources))
	var workers sync.WaitGroup
	for _, source := range sources {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			installed, err := installFakePublicationCheckpoint(source, destination)
			results <- installed
			errs <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errs)

	installed := 0
	for result := range results {
		if result {
			installed++
		}
	}
	if installed != 1 {
		t.Fatalf("installed = %d, want exactly one winner", installed)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("install checkpoint: %v", err)
		}
	}
	body, err := os.ReadFile(destination) //nolint:gosec // test-owned path rooted in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "1" && string(body) != "2" {
		t.Fatalf("checkpoint body = %q, want one complete contender", body)
	}
}

func TestValidateFakePublicationRecipePath(t *testing.T) {
	for _, path := range []string{
		"",
		"/verify.json",
		"../verify.json",
		"./verify.json",
		"recipes//verify.json",
		"recipes/*.json",
		"recipes\\verify.json",
		"recipes:verify.json",
	} {
		t.Run(path, func(t *testing.T) {
			if err := validateFakePublicationRecipePath(path); err == nil {
				t.Fatalf("accepted invalid recipe path %q", path)
			}
		})
	}
	if err := validateFakePublicationRecipePath(".freeside/verify.json"); err != nil {
		t.Fatalf("rejected valid recipe path: %v", err)
	}
}

func TestFakePublicationTaskValidateReappliesAdmissionValidators(t *testing.T) {
	valid := validFakePublicationTask(t)
	if err := valid.validate(); err != nil {
		t.Fatalf("valid task: %v", err)
	}

	tests := map[string]func(*fakePublicationTask){
		"repository": func(task *fakePublicationTask) { task.Repo = "owner/repo/extra" },
		"base ref":   func(task *fakePublicationTask) { task.BaseRef = "release/.candidate" },
		"allowlist":  func(task *fakePublicationTask) { task.AllowedPaths = []string{"["} },
		"recipe path": func(task *fakePublicationTask) {
			task.RecipePath = "../verify.json"
		},
		"publication body": func(task *fakePublicationTask) {
			task.Body = "<!-- freeside:publication-identity=sha256:foreign -->"
		},
		"commit date": func(task *fakePublicationTask) {
			task.CommitDate = time.Unix(-1, 0).UTC()
		},
		"commit date upper bound": func(task *fakePublicationTask) {
			task.CommitDate = time.Unix(fakePublicationMaxCommitTimestamp, 0).UTC()
		},
		"handoff digest case": func(task *fakePublicationTask) {
			task.HandoffDigest = domain.Digest("sha256:" + strings.Repeat("A", 64))
		},
		"utf-8": func(task *fakePublicationTask) {
			task.RunID = domain.RunID("run-\xff")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			task := valid
			task.AllowedPaths = append([]string(nil), valid.AllowedPaths...)
			mutate(&task)
			if err := task.validate(); err == nil {
				t.Fatal("decoded task passed admission validation")
			}
		})
	}
}

func TestValidSHA256Digest(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	tests := []struct {
		name   string
		digest domain.Digest
		want   bool
	}{
		{name: "lowercase hex", digest: domain.Digest("sha256:" + hex64), want: true},
		{name: "uppercase hex", digest: domain.Digest("sha256:" + strings.ToUpper(hex64))},
		{name: "mixed case hex", digest: domain.Digest("sha256:A" + hex64[1:])},
		{name: "empty"},
		{name: "missing prefix", digest: domain.Digest(hex64)},
		{name: "wrong length", digest: domain.Digest("sha256:" + hex64[:63])},
		{name: "non-hex", digest: domain.Digest("sha256:" + strings.Repeat("g", 64))},
		{name: "whitespace", digest: domain.Digest(" sha256:" + hex64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validSHA256Digest(test.digest); got != test.want {
				t.Fatalf("validSHA256Digest(%q) = %t, want %t", test.digest, got, test.want)
			}
		})
	}
}

func TestValidateFakePublicationCommitDateBoundaries(t *testing.T) {
	tests := map[string]struct {
		commitDate time.Time
		wantErr    bool
	}{
		"epoch":              {commitDate: time.Unix(0, 0).UTC()},
		"before epoch":       {commitDate: time.Unix(-1, 0).UTC(), wantErr: true},
		"last accepted time": {commitDate: time.Unix(fakePublicationMaxCommitTimestamp-1, 0).UTC()},
		"2100 boundary": {
			commitDate: time.Unix(fakePublicationMaxCommitTimestamp, 0).UTC(),
			wantErr:    true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateFakePublicationCommitDate(test.commitDate)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateFakePublicationCommitDate(%v) error = %v, wantErr %t",
					test.commitDate, err, test.wantErr)
			}
		})
	}
}

func TestValidateNewFakePublicationBindingsRejectsChangedEpochAndProfile(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "freeside-ai/freeside", RepositoryID: 1,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanPlanPreferred,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, profile, now)
	}); err != nil {
		t.Fatal(err)
	}
	state, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := fakePublicationTask{
		StoreEpoch: state.SyncEpoch, Repo: profile.Repo,
		TrustProfileDigest:      profile.ProfileDigest,
		PublicationInvocationID: "publish-bindings",
		RunID:                   "run-bindings",
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return validateNewFakePublicationBindings(ctx, tx, task)
	}); err != nil {
		t.Fatalf("current bindings: %v", err)
	}

	staleEpoch := task
	staleEpoch.StoreEpoch = "restored-epoch"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return validateNewFakePublicationBindings(ctx, tx, staleEpoch)
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("stale epoch error = %v", err)
	}
	staleProfile := task
	staleProfile.TrustProfileDigest = "sha256:stale-profile"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return validateNewFakePublicationBindings(ctx, tx, staleProfile)
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("stale profile error = %v", err)
	}
}

func TestPutTerminalItemAcceptsCompatibleLifecycleAdvance(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	attention := signet.NewService(st)
	runID := domain.RunID("run-terminal")
	expected, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: FakePublicationReadyItemID(runID), ProjectID: "project-terminal",
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID,
		},
		Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "owner/repo#7 is published and ready for final review.",
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionMarkSeen, domain.ActionDismiss, domain.ActionStop,
		},
		PRHeadSHA:   "0123456789012345678901234567890123456789",
		PRReference: &domain.PRReference{Repo: "owner/repo", Number: 7},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := attention.PutItem(ctx, expected); err != nil {
		t.Fatal(err)
	}
	advanced := expected
	advanced.ItemVersion = 2
	advanced.Status = domain.StatusDismissed
	if err := attention.PutItem(ctx, advanced); err != nil {
		t.Fatal(err)
	}

	workflow := &fakePublicationWorkflow{store: st, attention: attention}
	if err := workflow.putTerminalItem(ctx, expected); err != nil {
		t.Fatalf("compatible terminal replay: %v", err)
	}
}

func TestFakePublicationTerminalBindingAcceptsPrePRReferenceDigest(t *testing.T) {
	task := validFakePublicationTask(t)
	runID := task.RunID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: FakePublicationReadyItemID(runID), ProjectID: task.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID,
		},
		Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA:         task.BaseSHA,
		PRReference:       &domain.PRReference{Repo: task.Repo, Number: 7},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := fakePublicationTerminalDigestBeforePRReference(task, item)
	if err != nil {
		t.Fatal(err)
	}
	current, err := fakePublicationTerminalDigest(task, item)
	if err != nil {
		t.Fatal(err)
	}
	if legacy == current {
		t.Fatal("legacy terminal digest unexpectedly equals current digest")
	}
	item.Reason += "\n\n" + fakePublicationTerminalBindingPrefix +
		string(legacy) + fakePublicationTerminalBindingSuffix
	got, err := validateFakePublicationTerminalBinding(task, item)
	if err != nil {
		t.Fatalf("validate legacy terminal binding: %v", err)
	}
	if got.Reason != "published and ready" {
		t.Fatalf("unbound reason = %q", got.Reason)
	}
	tampered := item
	tampered.PRHeadSHA = "foreign-head"
	if _, err := validateFakePublicationTerminalBinding(task, tampered); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("tampered legacy terminal error = %v, want ErrParentKeyMismatch", err)
	}
}
