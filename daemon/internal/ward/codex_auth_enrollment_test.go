package ward

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

type fakeCodexAuthEnrollmentJournal struct {
	leaser        *fakeLeaser
	beginCalls    int
	failCalls     int
	verifyCalls   int
	projectCalls  int
	failure       CodexAuthEnrollmentFailure
	verified      domain.CodexReenrollmentRecoveryBinding
	recoverable   bool
	finished      bool
	initial       bool
	item          domain.AttentionItem
	projectMutate func(*domain.AttentionItem)
	bootstrap     *EnrollmentBootstrap
	generation    *domain.EnrollmentGeneration
}

func (j *fakeCodexAuthEnrollmentJournal) Begin(
	_ context.Context,
	identity domain.AuthIdentity,
	_ domain.ProjectID,
	holder domain.InvocationID,
	bootstrap *EnrollmentBootstrap,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	j.beginCalls++
	j.bootstrap = bootstrap
	j.leaser.identity = identity
	j.leaser.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: identity.ID, Holder: holder, Fence: 1,
		AcquiredAt: now, ExpiresAt: expiresAt,
	}
	if bootstrap != nil {
		binding := bootstrap.Binding
		j.leaser.lease.GenerationBinding = &binding
	}
	return j.leaser.lease, nil
}

func (j *fakeCodexAuthEnrollmentJournal) Fail(
	_ context.Context,
	_ domain.AuthIdentityID,
	_ domain.InvocationID,
	_ int64,
	class CodexAuthEnrollmentFailure,
	_ time.Time,
) error {
	j.failCalls++
	j.failure = class
	return nil
}

func (j *fakeCodexAuthEnrollmentJournal) Verify(
	_ context.Context,
	id domain.AuthIdentityID,
	_ domain.InvocationID,
	fence int64,
	digest domain.Digest,
	expiresAt, _ time.Time,
	generation *domain.EnrollmentGeneration,
) error {
	j.verifyCalls++
	j.generation = generation
	j.verified = domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: id, LeaseFence: fence,
		AuthStoreDigest: digest, AccessTokenExpiresAt: expiresAt,
	}
	return nil
}

func (j *fakeCodexAuthEnrollmentJournal) RecoverableVerified(
	_ context.Context, _ domain.AuthIdentity,
) (CodexAuthRecovery, bool, error) {
	return CodexAuthRecovery{Binding: j.verified, Finished: j.finished}, j.recoverable, nil
}

func (j *fakeCodexAuthEnrollmentJournal) ProjectVerified(
	_ context.Context, _ domain.AuthIdentityID,
) (domain.AttentionItem, bool, error) {
	j.projectCalls++
	posture := domain.HealthPostureAdvisory
	binding := j.verified
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: j.item.ID, ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: "Codex auth identity requires verified re-enrollment.",
		RequestedDecision: []domain.Action{
			domain.ActionAcknowledge, domain.ActionResolveReenrollment,
		},
		CodexReenrollmentRecoveryBinding: &binding,
		ItemVersion:                      j.item.ItemVersion, InterruptionClass: domain.InterruptionExceptional,
		Posture: &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return domain.AttentionItem{}, false, err
	}
	if j.initial {
		item, err = item.ConcludeByDaemon(domain.ConcludedByDaemon, codexReviewEpoch,
			"First enrollment verified.", nil)
		if err != nil {
			return domain.AttentionItem{}, false, err
		}
	}
	if j.projectMutate != nil {
		j.projectMutate(&item)
	}
	j.item = item
	return item, j.initial, nil
}

func TestEnrollCodexAuthSpendsInputAndProjectsVerifiedRotation(t *testing.T) {
	t.Parallel()
	cfg, journal, leaser, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)

	result, err := EnrollCodexAuth(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if journal.beginCalls != 1 || journal.verifyCalls != 1 || journal.projectCalls != 1 ||
		journal.failCalls != 0 || refresher.calls != 1 || refresher.input != "operator-refresh" {
		t.Fatalf("journal calls = begin %d verify %d project %d fail %d; refresher = %d %q",
			journal.beginCalls, journal.verifyCalls, journal.projectCalls, journal.failCalls,
			refresher.calls, refresher.input)
	}
	if !leaser.released {
		t.Fatal("enrollment lease was not released")
	}
	written, err := os.ReadFile(storePath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	auth, _, err := inspectCodexHostAuth(CodexAuthSubscription, written)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Tokens == nil || auth.Tokens.RefreshToken == nil ||
		*auth.Tokens.RefreshToken != "rotated-refresh" {
		t.Fatalf("stored refresh token was not the rotated family")
	}
	assertNoCodex0167TokenMembers(t, written)
	input, err := os.ReadFile(inputPath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	inputAuth, _, err := inspectCodexHostAuth(CodexAuthSubscription, input)
	if err != nil || inputAuth.Tokens == nil || inputAuth.Tokens.RefreshToken == nil ||
		*inputAuth.Tokens.RefreshToken != "operator-refresh" {
		t.Fatalf("operator input was mutated: %v", err)
	}
	wantDigest := domain.Digest(contentaddr.Sum(written))
	if result.AuthStoreDigest != wantDigest || journal.verified.AuthStoreDigest != wantDigest ||
		result.AttentionItemID != journal.item.ID || result.AttentionItemVersion != journal.item.ItemVersion ||
		result.AttentionItemStatus != domain.StatusOpen {
		t.Fatalf("result = %+v, journal = %+v", result, journal.verified)
	}
}

func TestEnrollCodexAuthKeepsCodex0167TokenMembers(t *testing.T) {
	t.Parallel()
	cfg, _, _, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)
	input := marshalCodexHostAuthDocument(t, codex0167HostAuthDocument(
		t, "operator-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch,
	))
	if err := os.WriteFile(inputPath, input, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := EnrollCodexAuth(context.Background(), cfg); err != nil {
		t.Fatalf("EnrollCodexAuth = %v", err)
	}
	written, err := os.ReadFile(storePath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 || !bytes.Contains(written, []byte("rotated-refresh")) {
		t.Fatalf("refresh calls = %d; store did not hold the rotated family", refresher.calls)
	}
	assertCodex0167TokenMembers(t, written)
}

// TestEnrollCodexAuthAcceptsResolvedInitialProjection proves a first
// enrollment reports its daemon-resolved item, on the fresh path and on the
// retry that recovers a verified operation whose projection never ran.
func TestEnrollCodexAuthAcceptsResolvedInitialProjection(t *testing.T) {
	t.Parallel()
	t.Run("fresh", func(t *testing.T) {
		t.Parallel()
		cfg, journal, _, _, _, _ := codexAuthEnrollmentFixture(t)
		journal.initial = true
		result, err := EnrollCodexAuth(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if result.AttentionItemStatus != domain.StatusResolved || journal.projectCalls != 1 {
			t.Fatalf("result = %+v, project calls = %d", result, journal.projectCalls)
		}
	})
	t.Run("recovered", func(t *testing.T) {
		t.Parallel()
		cfg, journal, _, _, _, storePath := codexAuthEnrollmentFixture(t)
		rotated := codexHostAuthBody(
			t, "already-rotated", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch,
		)
		seedCodexAuthEnrollmentCrash(t, cfg.AuthStoreRoot, storePath, cfg.AuthIdentityID,
			codexHostAuthBody(t, "spent-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch),
			rotated, true)
		journal.verified = domain.CodexReenrollmentRecoveryBinding{
			AuthIdentityID: cfg.AuthIdentityID, LeaseFence: 7,
			AuthStoreDigest:      domain.Digest(contentaddr.Sum(rotated)),
			AccessTokenExpiresAt: codexReviewEpoch.Add(4 * time.Hour),
		}
		journal.recoverable = true
		journal.initial = true
		result, err := EnrollCodexAuth(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if result.AttentionItemStatus != domain.StatusResolved || journal.beginCalls != 0 ||
			journal.projectCalls != 1 {
			t.Fatalf("result = %+v, begin %d project %d", result, journal.beginCalls, journal.projectCalls)
		}
	})
}

func TestEnrollCodexAuthDoesNotReplaceStoreAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	cfg, journal, leaser, refresher, _, storePath := codexAuthEnrollmentFixture(t)
	successorBody := codexHostAuthBody(
		t, "successor-refresh", codexReviewEpoch.Add(5*time.Hour), codexReviewEpoch,
	)
	if err := os.WriteFile(storePath, successorBody, 0o600); err != nil {
		t.Fatal(err)
	}
	gets := 0
	leaser.onGet = func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error) {
		gets++
		if gets == 2 {
			current.Holder = "successor"
			current.Fence++
		}
		return current, nil
	}

	if _, err := EnrollCodexAuth(context.Background(), cfg); err == nil {
		t.Fatal("stale enrollment replaced the successor's auth store")
	}
	got, err := os.ReadFile(storePath) //nolint:gosec // test path is under t.TempDir
	if err != nil || !bytes.Equal(got, successorBody) {
		t.Fatalf("auth store after stale replacement = %q, %v", got, err)
	}
	if refresher.calls != 0 || journal.failure != CodexAuthEnrollmentReplacementFailed {
		t.Fatalf("stale replacement = refresh %d, failure %q", refresher.calls, journal.failure)
	}
}

func TestCodexAuthEnrollmentFailureRegistrationIsValid(t *testing.T) {
	t.Parallel()
	for _, failure := range AllCodexAuthEnrollmentFailures {
		if !failure.valid() {
			t.Fatalf("registered enrollment failure %q is invalid", failure)
		}
	}
	if CodexAuthEnrollmentFailure("unknown").valid() {
		t.Fatal("unknown enrollment failure is valid")
	}
}

func TestEnrollCodexAuthRecoversVerifiedProjectionWithoutInput(t *testing.T) {
	t.Parallel()
	cfg, journal, _, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)
	if err := os.Remove(inputPath); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(cfg.InputRoot, "missing-after-verify"))
	if !errors.Is(err, os.ErrNotExist) || len(body) != 0 {
		t.Fatalf("missing input precondition = %d bytes, %v", len(body), err)
	}
	predecessor := codexHostAuthBody(
		t, "spent-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch,
	)
	rotated := codexHostAuthBody(
		t, "already-rotated", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch,
	)
	seedCodexAuthEnrollmentCrash(t, cfg.AuthStoreRoot, storePath, cfg.AuthIdentityID,
		predecessor, rotated, true)
	journal.verified = domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: cfg.AuthIdentityID, LeaseFence: 7,
		AuthStoreDigest:      domain.Digest(contentaddr.Sum(rotated)),
		AccessTokenExpiresAt: codexReviewEpoch.Add(4 * time.Hour),
	}
	journal.recoverable = true

	result, err := EnrollCodexAuth(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if journal.beginCalls != 0 || journal.verifyCalls != 0 || journal.projectCalls != 1 ||
		refresher.calls != 0 {
		t.Fatalf("recovery calls = begin %d verify %d project %d refresh %d",
			journal.beginCalls, journal.verifyCalls, journal.projectCalls, refresher.calls)
	}
	if result.LeaseFence != 7 || result.AuthStoreDigest != journal.verified.AuthStoreDigest {
		t.Fatalf("recovered result = %+v", result)
	}
	if _, err := os.Lstat(codexAuthRefreshIntentPath(storePath, cfg.AuthIdentityID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified retry left refresh intent: %v", err)
	}
}

// TestCodexAuthAddLeavesPendingRecoveryToEnrollCodex proves auth add never
// finishes an earlier verified replacement whose marker is still open. It
// sends the operator to enroll-codex and back to auth add, and to auth list,
// since an earlier auth add may already have committed the enrollment.
func TestCodexAuthAddLeavesPendingRecoveryToEnrollCodex(t *testing.T) {
	t.Parallel()
	cfg, journal, _, refresher, _, _ := codexAuthEnrollmentFixture(t)
	cfg.Enrollment = &CodexClientEnrollmentRequest{
		EnrollmentID: "codex-primary/codex_cli", Route: "openai-subscription", CostOwner: "operator",
	}
	journal.recoverable = true

	_, err := EnrollCodexAuth(context.Background(), cfg)
	if err == nil || errors.Is(err, ErrCodexAuthAlreadyEnrolled) {
		t.Fatalf("err = %v, want a pending-recovery refusal", err)
	}
	for _, step := range []string{"enroll-codex", "rerun auth add", "auth list"} {
		if !strings.Contains(err.Error(), step) {
			t.Fatalf("err = %v, want a refusal that names %q", err, step)
		}
	}
	if journal.beginCalls != 0 || journal.projectCalls != 0 || refresher.calls != 0 {
		t.Fatalf("refused recovery calls = begin %d project %d refresh %d",
			journal.beginCalls, journal.projectCalls, refresher.calls)
	}
}

// TestCodexAuthAddHandsFinishedFirstEnrollmentToAdoption proves auth add on
// an identity whose first enrollment finished returns the adoption sentinel
// before it reads the input login, which that enrollment already spent, and
// begins, refreshes, and projects nothing.
func TestCodexAuthAddHandsFinishedFirstEnrollmentToAdoption(t *testing.T) {
	t.Parallel()
	cfg, journal, _, refresher, inputPath, _ := codexAuthEnrollmentFixture(t)
	cfg.Enrollment = &CodexClientEnrollmentRequest{
		EnrollmentID: "codex-primary/codex_cli", Route: "openai-subscription", CostOwner: "operator",
	}
	journal.recoverable, journal.finished = true, true
	if err := os.Remove(inputPath); err != nil {
		t.Fatal(err)
	}

	_, err := EnrollCodexAuth(context.Background(), cfg)
	if !errors.Is(err, ErrCodexAuthAlreadyEnrolled) {
		t.Fatalf("err = %v, want %v", err, ErrCodexAuthAlreadyEnrolled)
	}
	if journal.beginCalls != 0 || journal.verifyCalls != 0 || journal.projectCalls != 0 || refresher.calls != 0 {
		t.Fatalf("finished recovery calls = begin %d verify %d project %d refresh %d",
			journal.beginCalls, journal.verifyCalls, journal.projectCalls, refresher.calls)
	}
}

func TestEnrollCodexAuthRotatesAgedVerifiedStoreWithoutInput(t *testing.T) {
	t.Parallel()
	cfg, journal, _, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)
	predecessor := codexHostAuthBody(
		t, "still-refreshable", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch,
	)
	seedCodexAuthEnrollmentCrash(t, cfg.AuthStoreRoot, storePath, cfg.AuthIdentityID,
		predecessor, predecessor, true)
	predecessorDigest := domain.Digest(contentaddr.Sum(predecessor))
	journal.verified = domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: cfg.AuthIdentityID, LeaseFence: 7,
		AuthStoreDigest:      predecessorDigest,
		AccessTokenExpiresAt: codexReviewEpoch.Add(30 * time.Minute),
	}
	journal.recoverable = true
	if err := os.Remove(inputPath); err != nil {
		t.Fatal(err)
	}

	result, err := EnrollCodexAuth(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 || journal.beginCalls != 1 || journal.verifyCalls != 1 ||
		result.AuthStoreDigest == predecessorDigest {
		t.Fatalf("aged recovery = refresh %d begin %d verify %d result %+v", refresher.calls, journal.beginCalls, journal.verifyCalls, result)
	}
}

func TestEnrollCodexAuthRecoversRefreshCrashWithoutRespendingInput(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		name := "pending-before-commit"
		if committed {
			name = "committed-before-journal-verify"
		}
		t.Run(name, func(t *testing.T) {
			cfg, journal, _, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)
			predecessor, err := os.ReadFile(inputPath) //nolint:gosec // test path is under t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			rotated := codexHostAuthBody(
				t, "crash-rotated", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch,
			)
			seedCodexAuthEnrollmentCrash(t, cfg.AuthStoreRoot, storePath, cfg.AuthIdentityID,
				predecessor, rotated, committed)

			result, err := EnrollCodexAuth(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if refresher.calls != 0 || journal.verifyCalls != 1 || journal.projectCalls != 1 {
				t.Fatalf("crash recovery calls = refresh %d verify %d project %d",
					refresher.calls, journal.verifyCalls, journal.projectCalls)
			}
			body, err := os.ReadFile(storePath) //nolint:gosec // test path is under t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != string(rotated) || result.AuthStoreDigest != domain.Digest(contentaddr.Sum(rotated)) {
				t.Fatal("crash recovery did not preserve the response-bound rotated credential")
			}
			for _, path := range []string{
				codexAuthRefreshIntentPath(storePath, cfg.AuthIdentityID),
				codexAuthRefreshPendingPath(storePath, cfg.AuthIdentityID),
			} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("crash recovery left %s: %v", filepath.Base(path), err)
				}
			}
		})
	}
}

func TestEnrollCodexAuthReplacesUnboundTerminalRefreshIntent(t *testing.T) {
	t.Parallel()
	cfg, journal, _, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)
	predecessor, err := os.ReadFile(inputPath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath, predecessor, 0o600); err != nil { //nolint:gosec // test path is under a hardened t.TempDir root
		t.Fatal(err)
	}
	_, body, metadata, err := readCodexReviewInputWithMetadata(
		cfg.AuthStoreRoot, storePath, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCodexAuthRefreshIntent(
		storePath, cfg.AuthIdentityID, newCodexAuthRefreshPredecessor(body, metadata), codexReviewEpoch,
	); err != nil {
		t.Fatal(err)
	}

	result, err := EnrollCodexAuth(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 || journal.verifyCalls != 1 || result.AuthStoreDigest == "" {
		t.Fatalf("replacement = refresh %d verify %d result %+v", refresher.calls, journal.verifyCalls, result)
	}
	if _, err := os.Lstat(codexAuthRefreshIntentPath(storePath, cfg.AuthIdentityID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement left stale refresh intent: %v", err)
	}
}

func TestEnrollCodexAuthKeepsUnboundIntentAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	cfg, journal, leaser, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)
	predecessor, err := os.ReadFile(inputPath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath, predecessor, 0o600); err != nil { //nolint:gosec // test path is under a hardened t.TempDir root
		t.Fatal(err)
	}
	_, body, metadata, err := readCodexReviewInputWithMetadata(
		cfg.AuthStoreRoot, storePath, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCodexAuthRefreshIntent(
		storePath, cfg.AuthIdentityID, newCodexAuthRefreshPredecessor(body, metadata), codexReviewEpoch,
	); err != nil {
		t.Fatal(err)
	}
	gets := 0
	leaser.onGet = func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error) {
		gets++
		if gets == 1 {
			return current, nil
		}
		current.Holder = "successor"
		current.Fence++
		return current, nil
	}

	if _, err := EnrollCodexAuth(context.Background(), cfg); err == nil {
		t.Fatal("enrollment replaced an unbound refresh intent after lease loss")
	}
	if journal.failure != CodexAuthEnrollmentVerificationFailed || refresher.calls != 0 {
		t.Fatalf("lease-loss result = failure %q refresh %d", journal.failure, refresher.calls)
	}
	if _, err := os.Lstat(codexAuthRefreshIntentPath(storePath, cfg.AuthIdentityID)); err != nil {
		t.Fatalf("lease-loss replacement deleted the predecessor intent: %v", err)
	}
}

func TestEnrollCodexAuthRecoveryRefusesCommitAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	cfg, journal, leaser, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)
	predecessor, err := os.ReadFile(inputPath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	rotated := codexHostAuthBody(
		t, "must-not-commit", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch,
	)
	seedCodexAuthEnrollmentCrash(t, cfg.AuthStoreRoot, storePath, cfg.AuthIdentityID,
		predecessor, rotated, false)
	gets := 0
	leaser.onGet = func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error) {
		gets++
		if gets == 1 {
			return current, nil
		}
		taken := current
		taken.Holder = "new-holder"
		taken.Fence++
		return taken, nil
	}

	if _, err := EnrollCodexAuth(context.Background(), cfg); err == nil {
		t.Fatal("recovery committed after losing its mutation lease")
	}
	stored, err := os.ReadFile(storePath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(predecessor) {
		t.Fatal("stale recovery holder replaced the predecessor")
	}
	if _, err := os.Lstat(codexAuthRefreshPendingPath(storePath, cfg.AuthIdentityID)); err != nil {
		t.Fatalf("stale recovery did not preserve pending response for the next holder: %v", err)
	}
	if journal.failure != CodexAuthEnrollmentVerificationFailed || refresher.calls != 0 {
		t.Fatalf("lease-loss result = failure %q, refresh calls %d", journal.failure, refresher.calls)
	}
}

func TestCodexAuthRecoveryDoesNotDeleteSuccessorIntentAfterDiscard(t *testing.T) {
	t.Parallel()
	cfg, _, _, _, inputPath, storePath := codexAuthEnrollmentFixture(t)
	predecessor, err := os.ReadFile(inputPath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	rotated := codexHostAuthBody(
		t, "superseded-rotation", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch,
	)
	seedCodexAuthEnrollmentCrash(t, cfg.AuthStoreRoot, storePath, cfg.AuthIdentityID,
		predecessor, rotated, false)
	external := codexHostAuthBody(
		t, "external-replacement", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch,
	)
	if err := os.WriteFile(storePath, external, 0o600); err != nil {
		t.Fatal(err)
	}
	_, body, metadata, err := readCodexReviewInputWithMetadata(
		cfg.AuthStoreRoot, storePath, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	intentPath := codexAuthRefreshIntentPath(storePath, cfg.AuthIdentityID)
	const successorIntent = "successor-holder-intent"
	checks := 0
	_, err = recoverCodexAuthRefreshTransactionUnderLease(
		cfg.AuthStoreRoot, storePath, cfg.AuthIdentityID, body, metadata,
		cfg.AccessTokenRefreshThreshold, false,
		func(mutation func() error) error {
			checks++
			if err := os.WriteFile(intentPath, []byte(successorIntent), 0o600); err != nil {
				t.Fatal(err)
			}
			return mutation()
		},
	)
	if err == nil || checks != 1 {
		t.Fatalf("discard recovery = %v after %d checks", err, checks)
	}
	if _, err := os.Lstat(codexAuthRefreshPendingPath(storePath, cfg.AuthIdentityID)); err != nil {
		t.Fatalf("failed cleanup did not preserve the pending response: %v", err)
	}
	intent, err := os.ReadFile(intentPath) //nolint:gosec // test path is under t.TempDir
	if err != nil || string(intent) != successorIntent {
		t.Fatalf("stale holder deleted successor intent: %q, %v", intent, err)
	}
}

func TestEnrollCodexAuthRecordsCredentialFreeVerificationFailure(t *testing.T) {
	t.Parallel()
	cfg, journal, leaser, refresher, _, _ := codexAuthEnrollmentFixture(t)
	refresher.err = errors.New("provider unavailable")

	_, err := EnrollCodexAuth(context.Background(), cfg)
	if err == nil {
		t.Fatal("enrollment succeeded despite provider failure")
	}
	if journal.failCalls != 1 || journal.failure != CodexAuthEnrollmentVerificationFailed ||
		journal.verifyCalls != 0 || journal.projectCalls != 0 || !leaser.released {
		t.Fatalf("failure calls = fail %d class %q verify %d project %d released %t",
			journal.failCalls, journal.failure, journal.verifyCalls, journal.projectCalls,
			leaser.released)
	}
}

func TestEnrollCodexAuthRejectsMismatchedProjectedItem(t *testing.T) {
	t.Parallel()
	// initial selects the first-enrollment projection, which must come back
	// resolved by the daemon; any other must come back open.
	tests := map[string]struct {
		initial bool
		mutate  func(*domain.AttentionItem)
	}{
		"not open": {mutate: func(item *domain.AttentionItem) {
			item.Status = domain.StatusResolved
		}},
		"initial left open": {initial: true, mutate: func(item *domain.AttentionItem) {
			item.Status = domain.StatusOpen
			item.Conclusion = nil
		}},
		"missing action": {mutate: func(item *domain.AttentionItem) {
			item.RequestedDecision = []domain.Action{domain.ActionAcknowledge}
		}},
		"missing binding": {mutate: func(item *domain.AttentionItem) {
			item.CodexReenrollmentRecoveryBinding = nil
		}},
		"identity": {mutate: func(item *domain.AttentionItem) {
			item.CodexReenrollmentRecoveryBinding.AuthIdentityID = "other"
		}},
		"fence": {mutate: func(item *domain.AttentionItem) {
			item.CodexReenrollmentRecoveryBinding.LeaseFence++
		}},
		"digest": {mutate: func(item *domain.AttentionItem) {
			item.CodexReenrollmentRecoveryBinding.AuthStoreDigest = "sha256:other"
		}},
		"initial digest": {initial: true, mutate: func(item *domain.AttentionItem) {
			item.CodexReenrollmentRecoveryBinding.AuthStoreDigest = "sha256:other"
		}},
		"expiry": {mutate: func(item *domain.AttentionItem) {
			item.CodexReenrollmentRecoveryBinding.AccessTokenExpiresAt = item.CodexReenrollmentRecoveryBinding.AccessTokenExpiresAt.Add(time.Second)
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, journal, _, _, _, _ := codexAuthEnrollmentFixture(t)
			journal.initial = tc.initial
			journal.projectMutate = tc.mutate
			if _, err := EnrollCodexAuth(context.Background(), cfg); err == nil {
				t.Fatal("enrollment trusted a mismatched projected item")
			}
			if journal.verifyCalls != 1 || journal.projectCalls != 1 || journal.failCalls != 0 {
				t.Fatalf("projection mismatch calls = verify %d project %d fail %d",
					journal.verifyCalls, journal.projectCalls, journal.failCalls)
			}
		})
	}
}

func TestEnrollCodexAuthRejectsOverlappingOrEscapedRoots(t *testing.T) {
	t.Parallel()
	cfg, _, _, _, _, _ := codexAuthEnrollmentFixture(t)
	cfg.AuthStoreRoot = cfg.InputRoot
	cfg.AuthStorePath = filepath.Join(cfg.InputRoot, "live.json")
	if _, err := EnrollCodexAuth(context.Background(), cfg); err == nil ||
		err.Error() != "enrollment input root and auth-store root must be separate trees" {
		t.Fatalf("overlapping roots = %v", err)
	}

	cfg, _, _, _, _, _ = codexAuthEnrollmentFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.AuthStorePath = filepath.Join(outside, "auth.json")
	if _, err := EnrollCodexAuth(context.Background(), cfg); err == nil ||
		err.Error() != "auth-store path resolves outside its private root" {
		t.Fatalf("escaped store target = %v", err)
	}
}

func TestEnrollCodexAuthDoesNotProjectChangedVerifiedStore(t *testing.T) {
	t.Parallel()
	cfg, journal, _, refresher, inputPath, storePath := codexAuthEnrollmentFixture(t)
	if err := os.Remove(inputPath); err != nil {
		t.Fatal(err)
	}
	verified := codexHostAuthBody(
		t, "verified-refresh", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch,
	)
	journal.verified = domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: cfg.AuthIdentityID, LeaseFence: 7,
		AuthStoreDigest:      domain.Digest(contentaddr.Sum(verified)),
		AccessTokenExpiresAt: codexReviewEpoch.Add(4 * time.Hour),
	}
	journal.recoverable = true
	changed := codexHostAuthBody(
		t, "changed-refresh", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch,
	)
	if err := os.WriteFile(storePath, changed, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := EnrollCodexAuth(context.Background(), cfg); err == nil {
		t.Fatal("changed verified store projected without a fresh enrollment input")
	}
	if journal.projectCalls != 0 || journal.beginCalls != 0 || refresher.calls != 0 {
		t.Fatalf("changed-store calls = project %d begin %d refresh %d",
			journal.projectCalls, journal.beginCalls, refresher.calls)
	}
}

func codexAuthEnrollmentFixture(
	t *testing.T,
) (CodexAuthEnrollmentConfig, *fakeCodexAuthEnrollmentJournal, *fakeLeaser,
	*fakeCodexAuthRefresher, string, string,
) {
	t.Helper()
	inputRoot := filepath.Join(t.TempDir(), "input")
	storeRoot := filepath.Join(t.TempDir(), "store")
	for _, root := range []string{inputRoot, storeRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inputPath := filepath.Join(inputRoot, "auth.json")
	storePath := filepath.Join(storeRoot, "auth.json")
	input := codexHostAuthBody(
		t, "operator-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch,
	)
	if err := os.WriteFile(inputPath, input, 0o600); err != nil {
		t.Fatal(err)
	}
	leaser := &fakeLeaser{}
	journal := &fakeCodexAuthEnrollmentJournal{
		leaser: leaser,
		item:   domain.AttentionItem{ID: "reenrollment-item", ItemVersion: 2},
	}
	refresher := &fakeCodexAuthRefresher{tokens: CodexAuthRefreshTokens{
		IDToken:      "rotated-id",
		AccessToken:  codexReviewJWT(t, codexReviewEpoch.Add(4*time.Hour)),
		RefreshToken: "rotated-refresh",
	}}
	cfg := CodexAuthEnrollmentConfig{
		InputRoot: inputRoot, InputFile: inputPath,
		AuthStoreRoot: storeRoot, AuthStorePath: storePath,
		AuthIdentityID: "codex-primary", ProjectID: "project-1",
		Journal: journal, AuthStoreLeaser: leaser, AuthRefresher: refresher,
		Now: func() time.Time { return codexReviewEpoch },
	}
	return cfg, journal, leaser, refresher, inputPath, storePath
}

func seedCodexAuthEnrollmentCrash(
	t *testing.T,
	root, path string,
	id domain.AuthIdentityID,
	predecessorBody, rotatedBody []byte,
	committed bool,
) {
	t.Helper()
	if err := os.WriteFile( //nolint:gosec // test path is under a hardened t.TempDir root
		path, predecessorBody, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	_, body, metadata, err := readCodexReviewInputWithMetadata(
		root, path, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := newCodexAuthRefreshPredecessor(body, metadata)
	if err := writeCodexAuthRefreshIntent(path, id, predecessor, codexReviewEpoch); err != nil {
		t.Fatal(err)
	}
	if err := bindCodexAuthRefreshIntent(
		root, path, id, predecessor, rotatedBody, codexReviewEpoch,
	); err != nil {
		t.Fatal(err)
	}
	pending, err := stageCodexAuthStore(root, path, id, predecessor, rotatedBody)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		if err := commitCodexAuthStore(root, path, pending, predecessor, rotatedBody); err != nil {
			t.Fatal(err)
		}
	}
}
