package wardstore_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

func TestAdaptersRoundTripAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	identity := domain.AuthIdentity{
		ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
		AuthStoreVolume:       "provider-cred",
		MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, at)
	}); err != nil {
		t.Fatalf("RecordAuthIdentity: %v", err)
	}
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatalf("wardstore.New: %v", err)
	}
	rec := ward.HandoffJournalRecord{
		RunID:          "adapter-run",
		OwnershipToken: "00112233445566778899aabbccddeeff",
		SpecDigest:     strings.Repeat("ab", 32),
		OpenedAt:       at,
	}
	lease, err := adapters.Journal.BeginLeased(
		ctx, rec,
		ward.AuthStoreLeaseClaim{AuthIdentityID: identity.ID, Holder: "inv-1"},
		at, at.Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("BeginLeased: %v", err)
	}
	if lease.Fence != 1 {
		t.Fatalf("lease fence = %d, want 1", lease.Fence)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	adapters, err = wardstore.New(reopened)
	if err != nil {
		t.Fatalf("wardstore.New(reopened): %v", err)
	}
	if volume, err := adapters.Leaser.AuthStoreVolume(ctx, identity.ID); err != nil || volume != identity.AuthStoreVolume {
		t.Fatalf("AuthStoreVolume = %q, %v; want %q", volume, err, identity.AuthStoreVolume)
	}
	got, err := adapters.Journal.Get(ctx, rec.RunID)
	if err != nil {
		t.Fatalf("Journal.Get after reopen: %v", err)
	}
	if got.Lease == nil || got.Lease.Fence != lease.Fence ||
		got.Lease.AuthIdentityID != identity.ID || got.Outcome != nil {
		t.Fatalf("reopened record = %+v, want the same open leased record", got)
	}
	current, err := adapters.Leaser.Get(ctx, identity.ID)
	if err != nil {
		t.Fatalf("Leaser.Get after reopen: %v", err)
	}
	if current != lease {
		t.Fatalf("reopened lease = %+v, want %+v", current, lease)
	}

	base := strings.Repeat("12", 20)
	pre := strings.Repeat("cd", 32)
	exportDir := filepath.Join(t.TempDir(), "export")
	if err := adapters.Journal.MarkSeedObserved(ctx, rec.RunID, base); err != nil {
		t.Fatalf("MarkSeedObserved: %v", err)
	}
	if err := adapters.Journal.MarkCredentialObserved(ctx, rec.RunID, pre); err != nil {
		t.Fatalf("MarkCredentialObserved: %v", err)
	}
	if err := adapters.Journal.MarkWriterComplete(ctx, rec.RunID); err != nil {
		t.Fatalf("MarkWriterComplete: %v", err)
	}
	if err := adapters.Journal.MarkExportMaterialized(ctx, rec.RunID, exportDir); err != nil {
		t.Fatalf("MarkExportMaterialized: %v", err)
	}
	if err := adapters.Journal.Close(ctx, rec.RunID, ward.HandoffCompleted); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := adapters.Journal.Get(ctx, rec.RunID)
	if err != nil {
		t.Fatalf("Get closed: %v", err)
	}
	if closed.Outcome == nil || *closed.Outcome != ward.HandoffCompleted {
		t.Fatalf("closed outcome = %v, want completed", closed.Outcome)
	}
}

func TestJournalMapsMissingRecordToWardSentinel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatalf("wardstore.New: %v", err)
	}
	_, err = adapters.Journal.Get(ctx, "missing-run")
	if !errors.Is(err, ward.ErrJournalRecordNotFound) {
		t.Fatalf("Journal.Get missing = %v, want ErrJournalRecordNotFound", err)
	}
}

func TestCodexReviewJournalRoundTripsLifecycleAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	runID := "review-run-1"
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: exec.ReviewVerificationEvidence{
			Outcome:                domain.VerificationPassed,
			RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
			EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		}, Instructions: instructions, RequestedAt: when,
	}
	workspace := ward.CodexReviewWorkspaceBinding{
		SourceRunID: runID, Volume: "candidate-volume",
		OwnershipToken: strings.Repeat("a", 32),
	}
	intent := ward.CodexReviewLaunchIntent{
		RunID: runID, SpecDigest: strings.Repeat("a", 64), OwnershipToken: strings.Repeat("b", 32),
		ShadowVolume: "shadow", Network: "network", ReviewContainer: "review",
		Resources: []ward.CodexReviewIntentResource{{Name: "shadow", OwnershipToken: strings.Repeat("b", 32)}},
		State:     ward.CodexReviewIntentPreparing,
	}
	if err := adapters.Journal.PutCodexReviewRequest(ctx, runID, request); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.PutCodexReviewWorkspaceBinding(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if got, err := adapters.Journal.ListCodexReviewWorkspaceIDs(ctx); err != nil ||
		!slices.Equal(got, []string{runID}) {
		t.Fatalf("review workspace ids = %#v, %v", got, err)
	}
	workspace.CreationFingerprint = "workspace-fingerprint"
	if err := adapters.Journal.PutCodexReviewWorkspaceBinding(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	conflictingWorkspace := workspace
	conflictingWorkspace.CreationFingerprint = "replacement-fingerprint"
	if err := adapters.Journal.PutCodexReviewWorkspaceBinding(ctx, conflictingWorkspace); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("workspace replacement = %v, want immutable conflict", err)
	}
	if err := adapters.Journal.DeleteCodexReviewWorkspaceBinding(ctx, conflictingWorkspace); !errors.Is(err, ward.ErrConformance) {
		t.Fatalf("delete replaced workspace = %v, want conformance failure", err)
	}
	if err := adapters.Journal.DeleteCodexReviewWorkspaceBinding(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if got, err := adapters.Journal.ListCodexReviewWorkspaceIDs(ctx); err != nil || len(got) != 0 {
		t.Fatalf("deleted review workspace ids = %#v, %v", got, err)
	}
	if _, err := adapters.Journal.GetCodexReviewWorkspaceBinding(ctx, runID); !errors.Is(err, ward.ErrCodexReviewWorkspaceNotFound) {
		t.Fatalf("deleted review workspace = %v", err)
	}
	if err := adapters.Journal.PutCodexReviewWorkspaceBinding(ctx, workspace); err != nil {
		t.Fatalf("recreate review workspace binding: %v", err)
	}
	if err := adapters.Journal.BeginCodexReviewIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	resource := intent.Resources[0]
	resource.Fingerprint = "shadow-fingerprint"
	if err := adapters.Journal.MarkCodexReviewIntentResource(ctx, runID, resource); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.MarkCodexReviewIntentPrepared(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.MarkCodexReviewIntentStarting(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.MarkCodexReviewIntentStarted(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if got, err := adapters.Journal.ListCodexReviewIntentIDs(ctx); err != nil ||
		!slices.Equal(got, []string{runID}) {
		t.Fatalf("review intent ids = %#v, %v", got, err)
	}
	if err := adapters.Journal.PutCodexReviewBinding(ctx,
		ward.CodexReviewJournalBinding{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	result := exec.ReviewResult{
		InvocationID: domain.InvocationID(runID), BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   instructions.ResultDigest, CostOwner: "owner",
		CompletedAt: when,
	}
	collectionEvidence := domain.Digest("sha256:" + strings.Repeat("c", 64))
	result.CompletionEvidence, err = ward.CodexReviewResultEvidence(result, collectionEvidence)
	if err != nil {
		t.Fatal(err)
	}
	outcome := ward.CodexReviewSourceOutcome{
		InvocationID: domain.InvocationID(runID), Result: &result, CollectionEvidence: collectionEvidence,
	}
	if err := adapters.Journal.PutCodexReviewOutcome(ctx, runID, outcome); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := adapters.Journal.GetCodexReviewOutcome(ctx, runID); err != nil || ready {
		t.Fatalf("collected outcome = ready %v, %v", ready, err)
	}
	if err := adapters.Journal.CloseCodexReviewIntent(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.MarkCodexReviewOutcomeReady(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.PutCodexReviewOutcome(ctx, runID, outcome); err != nil {
		t.Fatalf("replay outcome after ready transition: %v", err)
	}
	if got, err := adapters.Journal.ListCodexReviewIntentIDs(ctx); err != nil ||
		!slices.Equal(got, []string{runID}) {
		t.Fatalf("closed review intent ids = %#v, %v", got, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	adapters, err = wardstore.New(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := adapters.Journal.GetCodexReviewIntent(ctx, runID); err != nil || got.State != ward.CodexReviewIntentClosed {
		t.Fatalf("reopened intent = %#v, %v", got, err)
	}
	if got, err := adapters.Journal.GetCodexReviewWorkspaceBinding(ctx, runID); err != nil || got != workspace {
		t.Fatalf("reopened workspace = %#v, %v", got, err)
	}
	if got, ready, err := adapters.Journal.GetCodexReviewOutcome(ctx, runID); err != nil || !ready || got.Result == nil {
		t.Fatalf("reopened outcome = %#v, ready %v, %v", got, ready, err)
	}
	restartedIntent := intent
	restartedIntent.SpecDigest = "restarted-spec"
	if err := adapters.Journal.BeginCodexReviewIntent(ctx, restartedIntent); err != nil {
		t.Fatalf("restart closed intent: %v", err)
	}
	if got, err := adapters.Journal.GetCodexReviewIntent(ctx, runID); err != nil ||
		got.State != ward.CodexReviewIntentPreparing || got.SpecDigest != restartedIntent.SpecDigest {
		t.Fatalf("restarted intent = %#v, %v", got, err)
	}
	if _, err := adapters.Journal.GetCodexReviewBinding(ctx, runID); !errors.Is(err, ward.ErrCodexReviewBindingNotFound) {
		t.Fatalf("restarted intent retained old binding: %v", err)
	}
}

func TestLeaserExcludesConcurrentHolderAndMapsEndedWindow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	identity := domain.AuthIdentity{
		ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
		AuthStoreVolume:       "provider-cred",
		MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, at)
	}); err != nil {
		t.Fatalf("RecordAuthIdentity: %v", err)
	}
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatalf("wardstore.New: %v", err)
	}
	lease, err := adapters.Leaser.Acquire(ctx, identity.ID, "inv-1", at, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := adapters.Leaser.Acquire(
		ctx, identity.ID, "inv-2", at.Add(time.Second), at.Add(2*time.Minute),
	); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("concurrent holder acquire = %v, want %v", err, store.ErrLeaseHeld)
	}
	err = adapters.Leaser.Release(
		ctx, identity.ID, lease.Holder, lease.Fence, lease.ExpiresAt.Add(time.Second),
	)
	if !errors.Is(err, ward.ErrLeaseWindowEnded) {
		t.Fatalf("late release = %v, want %v", err, ward.ErrLeaseWindowEnded)
	}
}

func TestNewRejectsNilStore(t *testing.T) {
	if _, err := wardstore.New(nil); err == nil {
		t.Fatal("wardstore.New(nil) succeeded")
	}
}

func TestCodexAuthStateIsIdentityScopedAndAdvisory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatalf("wardstore.New: %v", err)
	}
	run := domain.Run{
		ID: "run-codex-auth", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatalf("PutRun: %v", err)
	}

	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, "codex-a"); err != nil {
		t.Fatalf("first mark: %v", err)
	}
	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, "codex-a"); err != nil {
		t.Fatalf("convergent mark: %v", err)
	}
	needs, err := adapters.AuthState.NeedsCodexAuthReenrollment(ctx, "codex-a")
	if err != nil || !needs {
		t.Fatalf("codex-a needs re-enrollment = %t, %v", needs, err)
	}
	needs, err = adapters.AuthState.NeedsCodexAuthReenrollment(ctx, "codex-b")
	if err != nil || needs {
		t.Fatalf("codex-b needs re-enrollment = %t, %v", needs, err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(items) != 1 {
			t.Fatalf("attention items = %d, want one converged marker", len(items))
		}
		item := items[0].Value
		if item.Type != domain.AttentionSystemHealth || item.Posture == nil ||
			*item.Posture != domain.HealthPostureAdvisory || item.Status != domain.StatusOpen {
			t.Fatalf("attention item = %#v, want open advisory system health", item)
		}
		if item.ProjectID != run.ProjectID || item.Subject.Type != domain.SubjectSystem {
			t.Fatalf("attention binding = project %q, subject %#v", item.ProjectID, item.Subject)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	decidedAt := time.Date(2026, 8, 10, 19, 0, 0, 0, time.UTC)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		resolved := items[0].Value
		resolved.ItemVersion++
		resolved.Status = domain.StatusResolved
		resolved, err = resolved.WithDecidedAt(decidedAt)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, resolved)
	}); err != nil {
		t.Fatalf("resolve marker: %v", err)
	}
	needs, err = adapters.AuthState.NeedsCodexAuthReenrollment(ctx, "codex-a")
	if err != nil || !needs {
		t.Fatalf("human-only marker resolution refusal = %t, %v", needs, err)
	}
	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, "codex-a"); err != nil {
		t.Fatalf("second occurrence: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(items) != 2 || items[1].Value.Status != domain.StatusOpen {
			t.Fatalf("attention occurrences = %#v, want resolved then open", items)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAuthStateRequiresVerifiedCommandBackedRecovery(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	identity := domain.AuthIdentity{
		ID: "codex-primary", Provider: "codex", AuthStoreMutationLease: true,
		AuthStoreVolume: "codex-auth", MaxParallelExecutions: 1,
		RefreshStrategy: domain.RefreshOnDemand,
	}
	run := domain.Run{
		ID: "run-codex-recovery", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, at)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, identity.ID); err != nil {
		t.Fatal(err)
	}
	var markerID domain.ItemID
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err == nil && len(items) == 1 {
			markerID = items[0].Value.ID
		}
		return err
	}); err != nil || markerID == "" {
		t.Fatalf("read re-enrollment marker = %q, %v", markerID, err)
	}
	var rec store.CodexReenrollmentJournal
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		rec, _, err = tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, markerID, "enroll-1", at, at.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if needs, err := adapters.AuthState.NeedsCodexAuthReenrollment(ctx, identity.ID); err != nil || !needs {
		t.Fatalf("pending admission refusal = %t, %v", needs, err)
	}
	if _, err := adapters.AuthState.ProjectVerifiedCodexReenrollment(ctx, identity.ID); !errors.Is(err, store.ErrCodexReenrollmentNotVerified) {
		t.Fatalf("pending projection = %v, want not verified", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.VerifyCodexReenrollment(
			ctx, identity.ID, rec.Holder, rec.LeaseFence,
			"sha256:replacement", at.Add(24*time.Hour), at.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	projected, err := adapters.AuthState.ProjectVerifiedCodexReenrollment(ctx, identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projected.CodexReenrollmentRecoveryBinding == nil ||
		!projected.Offers(domain.ActionResolveReenrollment) ||
		projected.Offers(domain.ActionStopUnattended) {
		t.Fatalf("projected marker = %+v", projected)
	}
	if needs, err := adapters.AuthState.NeedsCodexAuthReenrollment(ctx, identity.ID); err != nil || !needs {
		t.Fatalf("projected but unresolved refusal = %t, %v", needs, err)
	}

	device := domain.Device{
		ID: "device-1", DisplayName: "operator", Status: domain.DeviceActive, PairedAt: at,
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDevice(ctx, device) }); err != nil {
		t.Fatal(err)
	}
	service := signet.NewService(st,
		signet.WithPairingKey([]byte("wardstore-recovery-test-key")),
		signet.WithClock(func() time.Time { return at.Add(2 * time.Second) }),
	)
	var entityVersion int64
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, snapshot, err := tx.GetAttentionItemSnapshot(ctx, projected.ID)
		entityVersion = snapshot.EntityVersion
		return err
	}); err != nil {
		t.Fatal(err)
	}
	command := signet.ClientCommand{
		CommandID: "command-resolve-reenrollment", DeviceID: device.ID,
		ExpectedEntityVersion: entityVersion,
		Payload: signet.DecisionPayload{
			ItemID: projected.ID, Action: domain.ActionResolveReenrollment,
			ItemVersion: projected.ItemVersion, PRHeadSHA: projected.PRHeadSHA,
			ArtifactDigests: projected.ArtifactDigests,
		},
	}
	if _, err := service.Submit(ctx, command); err != nil {
		t.Fatal(err)
	}
	if needs, err := adapters.AuthState.NeedsCodexAuthReenrollment(ctx, identity.ID); err != nil || needs {
		t.Fatalf("verified command-backed recovery refusal = %t, %v", needs, err)
	}
	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, identity.ID); err != nil {
		t.Fatal(err)
	}
	if needs, err := adapters.AuthState.NeedsCodexAuthReenrollment(ctx, identity.ID); err != nil || !needs {
		t.Fatalf("new occurrence hidden by historical recovery = %t, %v", needs, err)
	}
	var newest domain.AttentionItem
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		for _, snapshot := range items {
			if snapshot.Value.Status == domain.StatusOpen {
				newest = snapshot.Value
			}
		}
		return nil
	}); err != nil || newest.ID == "" {
		t.Fatalf("read newest occurrence = %q, %v", newest.ID, err)
	}
	newest.Status = domain.StatusResolved
	newest.ItemVersion++
	newest, err = newest.WithDecidedAt(at.Add(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, newest)
	}); err != nil {
		t.Fatal(err)
	}
	if needs, err := adapters.AuthState.NeedsCodexAuthReenrollment(ctx, identity.ID); err != nil || !needs {
		t.Fatalf("newer unverified terminal occurrence hidden by historical recovery = %t, %v", needs, err)
	}
}

func TestFailedCodexReenrollmentCannotProjectOrClearAdmission(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	identity := domain.AuthIdentity{
		ID: "codex-failed", Provider: "codex", AuthStoreMutationLease: true,
		AuthStoreVolume: "codex-auth-failed", MaxParallelExecutions: 1,
		RefreshStrategy: domain.RefreshOnDemand,
	}
	run := domain.Run{
		ID: "run-codex-failed", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	var rec store.CodexReenrollmentJournal
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, at)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, identity.ID); err != nil {
		t.Fatal(err)
	}
	var markerID domain.ItemID
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err == nil && len(items) == 1 {
			markerID = items[0].Value.ID
		}
		return err
	}); err != nil || markerID == "" {
		t.Fatalf("read failed re-enrollment marker = %q, %v", markerID, err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		rec, _, err = tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, markerID, "enroll-failed", at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		return tx.FailCodexReenrollment(
			ctx, identity.ID, rec.Holder, rec.LeaseFence,
			store.CodexReenrollmentVerificationFailed, at.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	if needs, err := adapters.AuthState.NeedsCodexAuthReenrollment(ctx, identity.ID); err != nil || !needs {
		t.Fatalf("failed admission refusal = %t, %v", needs, err)
	}
	if _, err := adapters.AuthState.ProjectVerifiedCodexReenrollment(ctx, identity.ID); !errors.Is(err, store.ErrCodexReenrollmentNotVerified) {
		t.Fatalf("failed projection = %v, want not verified", err)
	}
}

func TestCodexReenrollmentOperationCannotCrossMarkerOccurrences(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	identity := domain.AuthIdentity{
		ID: "codex-occurrence", Provider: "codex", AuthStoreMutationLease: true,
		AuthStoreVolume: "codex-auth-occurrence", MaxParallelExecutions: 1,
		RefreshStrategy: domain.RefreshOnDemand,
	}
	run := domain.Run{
		ID: "run-codex-occurrence", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, at)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	markers := func() []domain.AttentionItem {
		t.Helper()
		var got []domain.AttentionItem
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			items, err := tx.ListAttentionItems(ctx)
			if err != nil {
				return err
			}
			for _, snapshot := range items {
				if snapshot.Value.Type == domain.AttentionSystemHealth {
					got = append(got, snapshot.Value)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	verify := func(markerID domain.ItemID, holder domain.InvocationID, start time.Time) store.CodexReenrollmentJournal {
		t.Helper()
		var rec store.CodexReenrollmentJournal
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			var err error
			rec, _, err = tx.BeginCodexReenrollmentJournal(
				ctx, identity.ID, markerID, holder, start, start.Add(time.Minute))
			if err != nil {
				return err
			}
			return tx.VerifyCodexReenrollment(
				ctx, identity.ID, holder, rec.LeaseFence,
				domain.Digest("sha256:"+string(holder)), start.Add(24*time.Hour), start.Add(time.Second))
		}); err != nil {
			t.Fatal(err)
		}
		return rec
	}

	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, identity.ID); err != nil {
		t.Fatal(err)
	}
	first := markers()[0]
	firstOp := verify(first.ID, "enroll-first", at)
	// A new revocation after verification but before projection must retire the
	// unbound carrier rather than inheriting its completed operation.
	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, identity.ID); err != nil {
		t.Fatal(err)
	}
	got := markers()
	if len(got) != 2 || got[0].Status != domain.StatusSuperseded || got[1].Status != domain.StatusOpen {
		t.Fatalf("post-verification occurrences = %+v", got)
	}
	if _, err := adapters.AuthState.ProjectVerifiedCodexReenrollment(ctx, identity.ID); !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
		t.Fatalf("old operation projection = %v, want marker mismatch", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(
			ctx, identity.ID, firstOp.Holder, firstOp.LeaseFence, at.Add(2*time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	verify(got[1].ID, "enroll-second", at.Add(3*time.Second))
	projected, err := adapters.AuthState.ProjectVerifiedCodexReenrollment(ctx, identity.ID)
	if err != nil || projected.ID != got[1].ID {
		t.Fatalf("second occurrence projection = %s, %v", projected.ID, err)
	}
	// A revocation while that projected marker is still open must also rotate
	// the occurrence, making its operation unavailable to the new failure.
	if err := adapters.AuthState.MarkCodexAuthNeedsReenrollment(ctx, run.ID, identity.ID); err != nil {
		t.Fatal(err)
	}
	got = markers()
	if len(got) != 3 || got[1].Status != domain.StatusSuperseded || got[2].Status != domain.StatusOpen {
		t.Fatalf("post-projection occurrences = %+v", got)
	}
	if _, err := adapters.AuthState.ProjectVerifiedCodexReenrollment(ctx, identity.ID); !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
		t.Fatalf("projected old operation reuse = %v, want marker mismatch", err)
	}
}

// TestCodexReviewIntentTransitionFromStates pins, for every registered launch
// state, exactly which persistence transitions accept it as a from-state (a
// forward move to the transition's target). The expectations map is keyed by
// ward.AllCodexReviewIntentStates: a state absent from the map fails the test,
// so a future durable state cannot merge without its from-state decision being
// made here rather than silently inherited by the explicit close list.
func TestCodexReviewIntentTransitionFromStates(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatalf("wardstore.New: %v", err)
	}

	type transition struct {
		name string
		to   ward.CodexReviewIntentState
		run  func(runID string) error
	}
	transitions := []transition{
		{"MarkPrepared", ward.CodexReviewIntentPrepared, func(id string) error {
			return adapters.Journal.MarkCodexReviewIntentPrepared(ctx, id)
		}},
		{"MarkStarting", ward.CodexReviewIntentStarting, func(id string) error {
			return adapters.Journal.MarkCodexReviewIntentStarting(ctx, id)
		}},
		{"MarkStarted", ward.CodexReviewIntentStarted, func(id string) error {
			return adapters.Journal.MarkCodexReviewIntentStarted(ctx, id)
		}},
		{"Close", ward.CodexReviewIntentClosed, func(id string) error {
			return adapters.Journal.CloseCodexReviewIntent(ctx, id)
		}},
	}

	// acceptedFrom[state] lists the transitions that move that state forward.
	// Every ward.AllCodexReviewIntentStates member must appear.
	acceptedFrom := map[ward.CodexReviewIntentState][]string{
		ward.CodexReviewIntentPreparing: {"MarkPrepared", "Close"},
		ward.CodexReviewIntentPrepared:  {"MarkStarting", "Close"},
		ward.CodexReviewIntentStarting:  {"MarkStarted", "Close"},
		ward.CodexReviewIntentStarted:   {"Close"},
		ward.CodexReviewIntentClosed:    {},
	}
	for _, s := range ward.AllCodexReviewIntentStates {
		if _, ok := acceptedFrom[s]; !ok {
			t.Fatalf("registered state %q absent from transition expectations; decide which "+
				"Mark*/Close transitions accept it as a from-state", s)
		}
	}

	// driveTo begins a fresh intent and advances it to target through the
	// forward transitions, so each (state, transition) probe starts isolated.
	driveTo := func(id string, target ward.CodexReviewIntentState) {
		t.Helper()
		intent := ward.CodexReviewLaunchIntent{
			RunID: id, SpecDigest: strings.Repeat("a", 64), OwnershipToken: strings.Repeat("b", 32),
			ShadowVolume: "shadow", Network: "network", ReviewContainer: "review",
			Resources: []ward.CodexReviewIntentResource{{Name: "shadow", OwnershipToken: strings.Repeat("b", 32)}},
			State:     ward.CodexReviewIntentPreparing,
		}
		if err := adapters.Journal.BeginCodexReviewIntent(ctx, intent); err != nil {
			t.Fatalf("begin intent %q: %v", id, err)
		}
		var seq []func(string) error
		switch target {
		case ward.CodexReviewIntentPreparing:
		case ward.CodexReviewIntentPrepared:
			seq = []func(string) error{transitions[0].run}
		case ward.CodexReviewIntentStarting:
			seq = []func(string) error{transitions[0].run, transitions[1].run}
		case ward.CodexReviewIntentStarted:
			seq = []func(string) error{transitions[0].run, transitions[1].run, transitions[2].run}
		case ward.CodexReviewIntentClosed:
			seq = []func(string) error{transitions[3].run}
		}
		for _, step := range seq {
			if err := step(id); err != nil {
				t.Fatalf("drive %q to %q: %v", id, target, err)
			}
		}
		got, err := adapters.Journal.GetCodexReviewIntent(ctx, id)
		if err != nil || got.State != target {
			t.Fatalf("seeded %q at %q, got %q (%v)", id, target, got.State, err)
		}
	}

	for _, s := range ward.AllCodexReviewIntentStates {
		want := acceptedFrom[s]
		for _, tr := range transitions {
			id := fmt.Sprintf("run-%s-%s", s, tr.name)
			driveTo(id, s)
			err := tr.run(id)
			got, gerr := adapters.Journal.GetCodexReviewIntent(ctx, id)
			if gerr != nil {
				t.Fatalf("read %q after %s: %v", id, tr.name, gerr)
			}
			acceptedAsFrom := err == nil && s != tr.to && got.State == tr.to
			shouldAccept := slices.Contains(want, tr.name)
			if acceptedAsFrom != shouldAccept {
				t.Errorf("%s from %q: acceptedAsFrom=%v want %v (err=%v, state=%q)",
					tr.name, s, acceptedAsFrom, shouldAccept, err, got.State)
			}
			switch {
			case shouldAccept:
				// forward move already asserted by acceptedAsFrom above.
			case s == tr.to:
				// idempotent self-transition: no error, state unchanged.
				if err != nil || got.State != s {
					t.Errorf("%s on already-%q: err=%v state=%q, want no-op", tr.name, s, err, got.State)
				}
			default:
				// rejected from-state: immutable conflict, state unchanged.
				if !errors.Is(err, store.ErrImmutableConflict) || got.State != s {
					t.Errorf("%s from %q: err=%v state=%q, want ErrImmutableConflict and unchanged",
						tr.name, s, err, got.State)
				}
			}
		}
	}
}
