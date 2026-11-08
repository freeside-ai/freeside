package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestDecodeFakePublicationInvocationOwnerRejectsUppercaseBindingDigest(t *testing.T) {
	t.Parallel()
	task := validFakePublicationTask(t)
	owner := fakePublicationInvocationOwner{
		Version:      fakePublicationInvocationOwnerVersion,
		InvocationID: task.VerificationInvocationID, Role: "verification",
		BindingDigest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		RunID:         task.RunID,
	}
	valid, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeFakePublicationInvocationOwner(valid); err != nil {
		t.Fatalf("valid invocation owner rejected: %v", err)
	}
	owner.BindingDigest = domain.Digest("sha256:" + strings.Repeat("A", 64))
	payload, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := decodeFakePublicationInvocationOwner(payload); err == nil {
		t.Fatal("uppercase binding digest passed invocation-owner validation")
	}
}

func TestEnsureFakePublicationPolicyConvergesLegacyV1Run(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
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
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	task := validFakePublicationTask(t)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: fakepublication.ReadyItemID(task.RunID), ProjectID: task.ProjectID,
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
	t.Parallel()
	ctx := t.Context()
	tempDir := t.TempDir()
	st := storetest.Open(t, filepath.Join(tempDir, "freeside.db"), store.Options{})
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

	checkpointSource := storetest.Open(t, filepath.Join(tempDir, "checkpoint-source.db"), store.Options{})
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
		OperatingMode:            fakepublication.OperatingModeAttended,
	}
}

func TestFakePublicationBackupPayloadDigests(t *testing.T) {
	t.Parallel()
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

func TestValidSHA256Digest(t *testing.T) {
	t.Parallel()
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

// validateFakePublicationRun checks that a run and its policy row are the
// current, converged shape the task implies.
func validateFakePublicationRun(
	ctx context.Context,
	reader fakePublicationRunReader,
	task fakePublicationTask,
) error {
	run, err := reader.GetRun(ctx, task.RunID)
	if err != nil {
		return fmt.Errorf("load publication run %q: %w", task.RunID, err)
	}
	expected := publicationRun(task)
	expected.TaskID = run.TaskID
	if !reflect.DeepEqual(run, expected) {
		return fmt.Errorf(
			"publication run %q disagrees with task: %w",
			task.RunID, domain.ErrParentKeyMismatch,
		)
	}
	policy, err := reader.GetResolvedPolicy(ctx, task.RunID)
	if err != nil {
		return fmt.Errorf("load publication resolved policy %q: %w", task.RunID, err)
	}
	if !reflect.DeepEqual(policy, publicationPolicy(task)) {
		return fmt.Errorf(
			"publication resolved policy %q disagrees with task: %w",
			task.RunID, domain.ErrParentKeyMismatch,
		)
	}
	return nil
}

// The two tests below keep coverage of fakepublication's task validation,
// which DecodeTask applies to every fake-lane row the converge, the backup
// extractors, and the store migrations still decode.

func TestFakePublicationTaskValidateReappliesAdmissionValidators(t *testing.T) {
	t.Parallel()
	valid := validFakePublicationTask(t)
	if err := fakepublication.Task(valid).Validate(); err != nil {
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
			if err := fakepublication.Task(task).Validate(); err == nil {
				t.Fatal("decoded task passed admission validation")
			}
		})
	}
}

// fakePublicationMaxCommitTimestamp is 2100-01-01T00:00:00Z, the first commit
// date fakepublication.ValidateCommitDate refuses.
const fakePublicationMaxCommitTimestamp = int64(4102444800)

func TestValidateFakePublicationCommitDateBoundaries(t *testing.T) {
	t.Parallel()
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
			err := fakepublication.ValidateCommitDate(test.commitDate)
			if (err != nil) != test.wantErr {
				t.Fatalf("fakepublication.ValidateCommitDate(%v) error = %v, wantErr %t",
					test.commitDate, err, test.wantErr)
			}
		})
	}
}
