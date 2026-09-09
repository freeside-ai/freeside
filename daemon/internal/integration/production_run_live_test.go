package integration_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func realRunOpenStore(ctx context.Context, path string, opts store.Options, final bool) (*store.Store, error) {
	if final {
		return store.OpenReadOnly(ctx, path, opts)
	}
	return store.Open(ctx, path, opts)
}

func realRunBlobStore(dbPath string, final bool) (*signet.BlobStore, error) {
	// NewBlobStore only syncs an existing directory. Require it first so the
	// final pass cannot manufacture a missing artifact store.
	if final {
		info, err := os.Stat(dbPath + ".blobs")
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("existing blob directory required")
		}
	}
	return signet.NewBlobStore(dbPath + ".blobs")
}

// Final verification must neither initialize prerequisites nor compete for the
// daemon's writer lock. These paths are the existing local-backup layout.
func realRunBackupFiles(dbPath string, final bool) (*store.LocalBackupFiles, error) {
	if !final {
		return store.NewDefaultLocalBackupFiles(dbPath)
	}
	keyPath := dbPath + ".backup-encryption.key"
	info, err := os.Lstat(keyPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("backup key must be an owner-only regular file")
	}
	key, err := os.ReadFile(keyPath) // #nosec G304 -- fixed backup-key sibling in the operator-supplied state root.
	if err != nil {
		return nil, err
	}
	return store.NewEncryptedLocalBackupFiles(filepath.Join(dbPath+".checkpoints", "latest.backup"), key)
}

func realRunIdentities(ctx context.Context, st *store.Store, final bool, identities ...domain.AuthIdentity) error {
	if !final {
		return st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			for _, identity := range identities {
				if err := tx.RecordAuthIdentity(ctx, identity, time.Now().UTC()); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return st.Read(ctx, func(tx *store.ReadTx) error {
		for _, expected := range identities {
			actual, err := tx.GetAuthIdentity(ctx, expected.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(actual, expected) {
				return fmt.Errorf("recorded auth identity %s differs from the run inputs", expected.ID)
			}
		}
		return nil
	})
}

// The §11 1A.2 real-run harness: one work item submitted through the complete
// production path, against the real Apple container runtime, the admitted
// project image, and the operator's managed repository.
//
// Opt-in and CI-blind: GitHub's macOS runners have no Apple container, and
// the run spends real inference. CI records it as Not run; the ward live
// suite (FREESIDE_WARD_LIVE_TEST) is the same tradition.
//
// What it proves that the in-process tests cannot: that the composition the
// daemon actually builds admits, hands off, exports, reconstructs, verifies
// networkless, publishes through the strong execution gate, and records a
// durable ready result with every gate present rather than stubbed.

const realRunLiveEnv = "FREESIDE_REAL_RUN_LIVE_TEST"

const (
	realRunImplementationRunIDEnv      = "FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID"
	realRunImplementationInvocationEnv = "FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION"
	realRunSpecificationRunIDEnv       = "FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID"
	realRunLegacyRunIDEnv              = "FREESIDE_REAL_RUN_RUN_ID"
	realRunLegacyInvocationEnv         = "FREESIDE_REAL_RUN_INVOCATION"
)

type realRunImplementationIdentityInput struct {
	implementationRunID           string
	implementationRunIDSet        bool
	implementationInvocationID    string
	implementationInvocationIDSet bool
	legacyRunIDSet                bool
	legacyInvocationIDSet         bool
}

type realRunImplementationBinding struct {
	runID        domain.RunID
	invocationID domain.InvocationID
}

func realRunImplementationIdentityFromEnvironment() realRunImplementationIdentityInput {
	runID, runIDSet := os.LookupEnv(realRunImplementationRunIDEnv)
	invocationID, invocationIDSet := os.LookupEnv(realRunImplementationInvocationEnv)
	_, legacyRunIDSet := os.LookupEnv(realRunLegacyRunIDEnv)
	_, legacyInvocationIDSet := os.LookupEnv(realRunLegacyInvocationEnv)
	return realRunImplementationIdentityInput{
		implementationRunID: runID, implementationRunIDSet: runIDSet,
		implementationInvocationID:    invocationID,
		implementationInvocationIDSet: invocationIDSet,
		legacyRunIDSet:                legacyRunIDSet, legacyInvocationIDSet: legacyInvocationIDSet,
	}
}

func validateRealRunImplementationBinding(
	input realRunImplementationIdentityInput, admittedRunID *domain.RunID,
) (realRunImplementationBinding, bool, error) {
	if input.legacyRunIDSet || input.legacyInvocationIDSet {
		return realRunImplementationBinding{}, false, fmt.Errorf(
			"legacy real-run identity variables are not supported: replace %s with %s and %s with %s",
			realRunLegacyRunIDEnv, realRunImplementationRunIDEnv,
			realRunLegacyInvocationEnv, realRunImplementationInvocationEnv,
		)
	}
	if !input.implementationRunIDSet && !input.implementationInvocationIDSet {
		return realRunImplementationBinding{}, false, nil
	}
	if !input.implementationRunIDSet || !input.implementationInvocationIDSet ||
		input.implementationRunID == "" || input.implementationInvocationID == "" {
		return realRunImplementationBinding{}, false, fmt.Errorf(
			"%s and %s must be set together to non-empty implementation-lane identities",
			realRunImplementationRunIDEnv, realRunImplementationInvocationEnv,
		)
	}
	binding := realRunImplementationBinding{
		runID:        domain.RunID(input.implementationRunID),
		invocationID: domain.InvocationID(input.implementationInvocationID),
	}
	if admittedRunID != nil && *admittedRunID != binding.runID {
		return realRunImplementationBinding{}, false, fmt.Errorf(
			"cross-lane real-run identity: implementation invocation %q belongs to admitted run %q, not bound implementation run %q; do not substitute a specification run for %s",
			binding.invocationID, *admittedRunID, binding.runID, realRunImplementationRunIDEnv,
		)
	}
	return binding, true, nil
}

type realRunEnv struct {
	stateRoot            string
	agentImage           domain.ImageRef
	exporterImage        string
	seedRoot             string
	authIdentityID       domain.AuthIdentityID
	authVolume           string
	reviewAuthIdentityID domain.AuthIdentityID
	reviewAuthSnapshot   string
	repo                 string
	repositoryID         int64
	baseRef              string
	baseSHA              string
	promptPackage        string
	instructions         string
	approvedRecipe       domain.Digest
}

func realRunEnvironment(t *testing.T) realRunEnv {
	t.Helper()
	if os.Getenv(realRunLiveEnv) != "1" {
		t.Skip("the real unattended run is opt-in: set " + realRunLiveEnv + "=1 with " +
			"FREESIDE_REAL_RUN_STATE_ROOT, FREESIDE_REAL_RUN_AGENT_IMAGE (digest-pinned), " +
			"FREESIDE_WARD_EXPORTER_IMAGE (digest-pinned), FREESIDE_REAL_RUN_SEED_ROOT, " +
			"FREESIDE_REAL_RUN_AUTH_IDENTITY, FREESIDE_REAL_RUN_AUTH_VOLUME, " +
			"FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY, FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT, " +
			"FREESIDE_REAL_RUN_REPO (owner/name), FREESIDE_REAL_RUN_REPOSITORY_ID, " +
			"FREESIDE_REAL_RUN_BASE_REF, FREESIDE_REAL_RUN_BASE_SHA, " +
			"FREESIDE_REAL_RUN_PROMPT_PACKAGE (file), FREESIDE_REAL_RUN_INSTRUCTIONS (file), " +
			"FREESIDE_REAL_RUN_APPROVED_RECIPE (sha256 digest)")
	}
	env := realRunEnv{
		stateRoot:            requireEnv(t, "FREESIDE_REAL_RUN_STATE_ROOT"),
		agentImage:           domain.ImageRef(requireEnv(t, "FREESIDE_REAL_RUN_AGENT_IMAGE")),
		exporterImage:        requireEnv(t, "FREESIDE_WARD_EXPORTER_IMAGE"),
		seedRoot:             requireEnv(t, "FREESIDE_REAL_RUN_SEED_ROOT"),
		authIdentityID:       domain.AuthIdentityID(requireEnv(t, "FREESIDE_REAL_RUN_AUTH_IDENTITY")),
		authVolume:           requireEnv(t, "FREESIDE_REAL_RUN_AUTH_VOLUME"),
		reviewAuthIdentityID: domain.AuthIdentityID(requireEnv(t, "FREESIDE_REAL_RUN_REVIEW_AUTH_IDENTITY")),
		reviewAuthSnapshot:   requireEnv(t, "FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT"),
		repo:                 requireEnv(t, "FREESIDE_REAL_RUN_REPO"),
		baseRef:              requireEnv(t, "FREESIDE_REAL_RUN_BASE_REF"),
		baseSHA:              requireEnv(t, "FREESIDE_REAL_RUN_BASE_SHA"),
		promptPackage:        requireEnv(t, "FREESIDE_REAL_RUN_PROMPT_PACKAGE"),
		instructions:         requireEnv(t, "FREESIDE_REAL_RUN_INSTRUCTIONS"),
		approvedRecipe:       domain.Digest(requireEnv(t, "FREESIDE_REAL_RUN_APPROVED_RECIPE")),
	}
	id, err := strconv.ParseInt(requireEnv(t, "FREESIDE_REAL_RUN_REPOSITORY_ID"), 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("FREESIDE_REAL_RUN_REPOSITORY_ID must be a positive integer: %v", err)
	}
	env.repositoryID = id
	resolvedReviewAuth, err := filepath.EvalSymlinks(env.reviewAuthSnapshot)
	if err != nil || !filepath.IsAbs(resolvedReviewAuth) {
		t.Fatalf("FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT must resolve to an absolute host file: %v", err)
	}
	env.reviewAuthSnapshot = resolvedReviewAuth
	// Digest pinning is the ward's own refusal, checked here so the harness
	// reports a configuration mistake rather than a gate failure fifteen
	// minutes into a run.
	for _, ref := range []string{string(env.agentImage), env.exporterImage} {
		if !strings.Contains(ref, "@sha256:") {
			t.Fatalf("image reference %q is not digest-pinned", ref)
		}
	}
	encodedRecipe, ok := strings.CutPrefix(string(env.approvedRecipe), "sha256:")
	if !ok || len(encodedRecipe) != 64 {
		t.Fatalf("FREESIDE_REAL_RUN_APPROVED_RECIPE must be a canonical sha256 digest")
	}
	if _, err := hex.DecodeString(encodedRecipe); err != nil {
		t.Fatalf("FREESIDE_REAL_RUN_APPROVED_RECIPE must be a canonical sha256 digest: %v", err)
	}
	return env
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required when %s=1", name, realRunLiveEnv)
	}
	return value
}

// TestRealWorkItemCompletesProductionPipeline verifies the durable output of
// the full production composition driven by scripts/run-real-work.sh.
func TestRealWorkItemCompletesProductionPipeline(t *testing.T) {
	t.Parallel()
	identityInput := realRunImplementationIdentityFromEnvironment()
	binding, bindingSet, bindingErr := validateRealRunImplementationBinding(identityInput, nil)
	if os.Getenv(realRunLiveEnv) == "1" && bindingErr != nil {
		t.Fatal(bindingErr)
	}
	env := realRunEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// The verifier must open the store under the same policy the daemon ran
	// under. Reading an admission re-runs the current policy gate, so a handle
	// missing the backup-health source rejects the very record a real run
	// wrote, and the harness could never report success on a completed run.
	dbPath := filepath.Join(env.stateRoot, "freeside.db")
	opts := store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: {},
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		ApprovedRecipes:         map[domain.Digest]bool{env.approvedRecipe: true},
	}
	backupFiles, err := realRunBackupFiles(dbPath, bindingSet)
	if err != nil {
		t.Fatalf("open local backup files: %v", err)
	}
	blobs, err := realRunBlobStore(dbPath, bindingSet)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	health, err := backupFiles.NewCheckpointHealthSource(
		blobs, opts.ApprovedRecipes, realRunBackupPayloadExtractors(),
	)
	if err != nil {
		t.Fatalf("build checkpoint health source: %v", err)
	}
	opts.BackupHealthSource = health

	st, err := realRunOpenStore(ctx, dbPath, opts, bindingSet)
	if err != nil {
		t.Fatalf("open real-run store (final=%t): %v", bindingSet, err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// The identity binding the ward gate compares the writable mount
	// against. It is an operator precondition in production; the harness
	// records it so a fresh state root is runnable.
	identity := domain.AuthIdentity{
		ID: env.authIdentityID, Provider: "claude", AuthStoreMutationLease: true, MaxParallelExecutions: 1,
		Interim: domain.InterimClientFacts{AuthStoreVolume: env.authVolume, RefreshStrategy: domain.RefreshOnDemand},
	}
	reviewIdentity := domain.AuthIdentity{
		ID: env.reviewAuthIdentityID, Provider: "openai", AuthStoreMutationLease: true, MaxParallelExecutions: 1,
		Interim: domain.InterimClientFacts{AuthStoreVolume: env.reviewAuthSnapshot, RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true},
	}
	if reviewIdentity.ID == identity.ID {
		t.Fatal("writer and Codex reviewer auth identities must be distinct")
	}
	if err := realRunIdentities(ctx, st, bindingSet, identity, reviewIdentity); err != nil {
		t.Fatalf("prepare or verify auth identities: %v", err)
	}

	// The operator-approved trust profile is deliberately not recorded here.
	// Activating one is the human approval unattended dispatch rests on, so a
	// harness that minted its own would be approving on the operator's behalf.
	// Checking it is still the harness's job: unattended admission reads the
	// activated profile, an absent or repository-mismatched one is a mutable
	// policy refusal, the engine holds the invocation rather than failing it,
	// and the run would poll out its entire deadline having never started the
	// writer. Same reason the image digest pinning is checked up front.
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		profile, err := tx.LatestTrustProfile(ctx, env.repo)
		if err != nil {
			return err
		}
		if profile.RepositoryID != env.repositoryID {
			return fmt.Errorf(
				"activated profile names repository %d, FREESIDE_REAL_RUN_REPOSITORY_ID is %d",
				profile.RepositoryID, env.repositoryID)
		}
		images, err := tx.ListProjectImages(ctx, env.repositoryID)
		if err != nil {
			return err
		}
		matching := 0
		for _, image := range images {
			if image.ImageRef != env.agentImage {
				continue
			}
			matching++
			if image.Repository != env.repo || image.RepositoryID != env.repositoryID ||
				image.CommitSHA != env.baseSHA || image.RecipeDigest != env.approvedRecipe {
				return fmt.Errorf("admitted project-image record disagrees with the real-run authority")
			}
		}
		if matching != 1 {
			return fmt.Errorf("found %d project-image records for admitted image %q, want exactly one", matching, env.agentImage)
		}
		return nil
	}); err != nil {
		t.Fatalf("no usable production trust/image authority for %q: %v; "+
			"activate its AutomationTrustProfile and onboard the digest-pinned project image before the run",
			env.repo, err)
	}

	// The harness asserts the durable outcome rather than driving the daemon
	// in-process: the production composition lives in package main, and a
	// second in-process wiring of it here would be a different composition
	// than the one shipped, which is exactly what this test exists to check.
	// scripts/run-real-work.sh performs the run; this test is its verifier
	// and can also be pointed at a state root a manual run produced.
	if !bindingSet {
		t.Log("real run preconditions recorded")
		t.Skip("set " + realRunImplementationRunIDEnv + " and " +
			realRunImplementationInvocationEnv + " to the submitted implementation run " +
			"to verify a completed run; scripts/run-real-work.sh sets them")
	}
	specificationRunID := domain.RunID(os.Getenv(realRunSpecificationRunIDEnv))
	invocationID := binding.invocationID
	runID := binding.runID

	retained := os.Getenv("FREESIDE_REAL_RUN_RETAINED") == "1"
	var admission domain.ExecutionAdmission
	var export domain.ExecutionExport
	var checkpoint realRunCheckpoint
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		anchor, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) && specificationRunID != "" {
				items, listErr := tx.ListAttentionItems(ctx)
				if listErr != nil {
					return listErr
				}
				failure := realRunSpecificationFailure(items, specificationRunID)
				if failure.ID != "" {
					return fmt.Errorf("real run specification failed: run=%s item=%s", specificationRunID, failure.ID)
				}
			}
			return err
		}
		if _, _, err := validateRealRunImplementationBinding(identityInput, &anchor.RunID); err != nil {
			return err
		}
		if anchor.Base.Repo != env.repo || anchor.Base.RepositoryID != env.repositoryID || anchor.Base.BaseSHA != env.baseSHA {
			return fmt.Errorf("retained run differs from configured repository/base")
		}
		checkpoint, err = readRealRunCheckpoint(ctx, tx, runID, retained)
		if err != nil {
			return err
		}
		invocationID = checkpoint.Binding.ProducingInvocationID
		if checkpoint.State == "retained" {
			admission, err = tx.GetExecutionAdmissionRecord(ctx, invocationID)
			if err == nil {
				export, err = tx.GetExecutionExportRecord(ctx, invocationID)
			}
		} else {
			admission, err = tx.GetExecutionAdmission(ctx, invocationID)
			if err == nil {
				export, err = tx.GetExecutionExport(ctx, invocationID)
			}
		}
		return err
	}); err != nil {
		t.Fatalf("read durable execution record: %v", err)
	}
	ready, outcome := checkpoint.ready, checkpoint.outcome
	if ready.PRHeadSHA != export.HeadSHA || len(ready.EvidenceSnapshot) == 0 {
		t.Error("publication evidence does not match its authenticated producer export")
	}
	if outcome.Repo != env.repo || outcome.BaseRef != env.baseRef ||
		outcome.HeadSHA != export.HeadSHA || outcome.PRNumber <= 0 || !outcome.EvidenceEligible {
		t.Error("publication outcome does not name the exact verified export head")
	}

	// Admission: the unattended class the run was actually admitted under.
	if admission.OperatingMode != domain.ModeUnattended {
		t.Errorf("operating mode = %q, want unattended", admission.OperatingMode)
	}
	if admission.CredentialMode != domain.CredentialSubscriptionContained {
		t.Errorf("credential mode = %q, want subscription_contained", admission.CredentialMode)
	}
	if admission.EgressProfile != domain.EgressProviderOnly {
		t.Errorf("egress profile = %q, want provider_only", admission.EgressProfile)
	}
	if admission.ImageRef != env.agentImage {
		t.Errorf("image ref = %q, want the pinned %q", admission.ImageRef, env.agentImage)
	}
	for _, capability := range []domain.RunnerCapability{
		domain.CapDetachableWorkspace, domain.CapPostExitExport, domain.CapReadOnlyRemount,
		domain.CapNetworklessExport, domain.CapEnforcedProviderEgress,
	} {
		if !admission.Capabilities.Has(capability) {
			t.Errorf("admission lacks capability %q", capability)
		}
	}
	// Artifact-bound, not conversation-bound: this is the production lane.
	if admission.StageInputs == nil || admission.StageInputs.ConversationDigest != nil {
		t.Errorf("stage inputs = %#v, want present with no conversation digest", admission.StageInputs)
	}

	// Export: the strong production publisher refuses publication without this
	// exact admission/base/head binding.
	if export.HeadSHA == "" {
		t.Error("execution export carries no head")
	}
	if export.ObservedBaseSHA != admission.Base.BaseSHA {
		t.Errorf("observed base %q, admitted base %q", export.ObservedBaseSHA, admission.Base.BaseSHA)
	}
	if export.AdmissionID != admission.ID {
		t.Errorf("export admission %q, admission %q", export.AdmissionID, admission.ID)
	}
	if err := domain.ValidateExportBinding(admission, export); err != nil {
		t.Errorf("export does not bind to its admission: %v", err)
	}

	if checkpoint.State == "ready" {
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			if _, err := tx.GetInbox(ctx, string(invocationID)); err != nil {
				return err
			}
			items, err := tx.ListAttentionItems(ctx)
			if err != nil {
				return err
			}
			_, blocked, err := realRunAttentionState(items, runID)
			if err != nil {
				return err
			}
			if blocked.ID != "" {
				return fmt.Errorf("run retains an open publish-blocked item")
			}
			return nil
		}); err != nil {
			t.Fatalf("read acceptance state: %v", err)
		}
	}
	if t.Failed() {
		return
	}
	if err := writeRealRunCheckpoint(os.Getenv("FREESIDE_REAL_RUN_CHECKPOINT_PATH"), checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.State == "retained" {
		t.Logf("retained production checkpoint verified: run=%s prior PR #%d at head %s; feedback is not a new ready result", runID, outcome.PRNumber, export.HeadSHA)
		return
	}

	t.Logf("real production pipeline verified: PR #%d at head %s over base %s",
		outcome.PRNumber, export.HeadSHA, export.ObservedBaseSHA)
}

func realRunBackupPayloadExtractors() map[string]store.BackupPayloadDigestExtractor {
	return map[string]store.BackupPayloadDigestExtractor{
		engine.FakePublicationTaskKind:                 engine.FakePublicationBackupPayloadDigests,
		engine.FakePublicationInvocationOwnerKind:      engine.FakePublicationInvocationOwnerBackupPayloadDigests,
		signet.AgentInvocationRequestedKind:            signet.AgentInvocationBackupPayloadDigests,
		signet.PublicationReevaluationRequestedKind:    signet.PublicationReevaluationBackupPayloadDigests,
		signet.PublicationReevaluationCompletedKind:    signet.PublicationReevaluationCompletionBackupPayloadDigests,
		engine.KindProductionInvocationRequested:       engine.ProductionInvocationBackupPayloadDigests,
		engine.KindProductionPublicationRequested:      engine.ProductionPublicationBackupPayloadDigests,
		engine.KindRemediationInvocationRequested:      engine.RemediationInvocationBackupPayloadDigests,
		engine.KindOperatorFeedbackInvocationRequested: engine.OperatorFeedbackInvocationBackupPayloadDigests,
		domain.PublicationSuccessorKind:                engine.PublicationSuccessorBackupPayloadDigests,
		domain.PublicationContinuationRequestedKind:    engine.PublicationContinuationBackupPayloadDigests,
		engine.KindSpecificationInvocationRequested:    engine.SpecificationInvocationBackupPayloadDigests,
		engine.KindSpecificationDiscussionRequested:    engine.SpecificationDiscussionBackupPayloadDigests,
		engine.KindSpecificationImplementationClaim:    engine.SpecificationImplementationClaimBackupPayloadDigests,
		publish.IntentKindReservation:                  publish.ReservationBackupPayloadDigests,
		publish.IntentKindPublication:                  publish.PublicationBackupPayloadDigests,
	}
}

func TestRealRunBackupPayloadExtractorsIncludeSpecificationMarkers(t *testing.T) {
	t.Parallel()
	extractors := realRunBackupPayloadExtractors()
	for _, kind := range []string{
		engine.KindSpecificationInvocationRequested,
		engine.KindSpecificationImplementationClaim,
		signet.PublicationReevaluationRequestedKind,
	} {
		if extractors[kind] == nil {
			t.Errorf("backup payload extractor %q is not registered", kind)
		}
	}
}

func realRunSpecificationFailure(
	items []store.Snapshotted[domain.AttentionItem], runID domain.RunID,
) domain.AttentionItem {
	for _, item := range items {
		if item.Value.Type == domain.AttentionExecutionFailure &&
			item.Value.Subject.RunID != nil && *item.Value.Subject.RunID == runID &&
			realRunSpecificationFailureID(item.Value.ID, runID) {
			return item.Value
		}
	}
	return domain.AttentionItem{}
}

func realRunSpecificationFailureID(id domain.ItemID, runID domain.RunID) bool {
	terminalPrefix := "execution-failure-inv-specify-" + string(runID) + "-"
	if suffix, ok := strings.CutPrefix(string(id), terminalPrefix); ok {
		iteration, err := strconv.ParseUint(suffix, 10, 64)
		return err == nil && iteration > 0 && strconv.FormatUint(iteration, 10) == suffix
	}
	return strings.HasPrefix(string(id), "execution-failure-spec-revision-")
}

func realRunAttentionState(
	items []store.Snapshotted[domain.AttentionItem], runID domain.RunID,
) (domain.AttentionItem, domain.AttentionItem, error) {
	var ready, blocked domain.AttentionItem
	for _, item := range items {
		if item.Value.Subject.RunID == nil || *item.Value.Subject.RunID != runID {
			continue
		}
		switch {
		case item.Value.Type == domain.AttentionReadyForFinalReview && item.Value.Status == domain.StatusOpen:
			if ready.ID != "" {
				return domain.AttentionItem{}, domain.AttentionItem{},
					errors.New("multiple ready items name the real run")
			}
			ready = item.Value
		case item.Value.Type == domain.AttentionPublishBlocked && item.Value.Status == domain.StatusOpen:
			if blocked.ID != "" {
				return domain.AttentionItem{}, domain.AttentionItem{},
					errors.New("multiple open publish-blocked items name the real run")
			}
			blocked = item.Value
		}
	}
	return ready, blocked, nil
}
