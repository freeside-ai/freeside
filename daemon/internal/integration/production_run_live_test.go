package integration_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

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
	backupFiles, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("open local backup files: %v", err)
	}
	blobs, err := signet.NewBlobStore(dbPath + ".blobs")
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

	st := storetest.Open(t, dbPath, opts)
	defer func() { _ = st.Close() }()

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
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		at := time.Now().UTC()
		if err := tx.RecordAuthIdentity(ctx, identity, at); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, reviewIdentity, at)
	}); err != nil {
		t.Fatalf("record auth identity: %v", err)
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

	t.Log("preconditions recorded; submit the work item with `freesided submit` " +
		"and run the daemon with -driver=claude against this state root")

	// The harness asserts the durable outcome rather than driving the daemon
	// in-process: the production composition lives in package main, and a
	// second in-process wiring of it here would be a different composition
	// than the one shipped, which is exactly what this test exists to check.
	// scripts/run-real-work.sh performs the run; this test is its verifier
	// and can also be pointed at a state root a manual run produced.
	if !bindingSet {
		t.Skip("set " + realRunImplementationRunIDEnv + " and " +
			realRunImplementationInvocationEnv + " to the submitted implementation run " +
			"to verify a completed run; scripts/run-real-work.sh sets them")
	}
	specificationRunID := domain.RunID(os.Getenv(realRunSpecificationRunIDEnv))
	invocationID := binding.invocationID
	runID := binding.runID

	var (
		admission            domain.ExecutionAdmission
		export               domain.ExecutionExport
		terminal             *domain.ExecutionOutcome
		ready                domain.AttentionItem
		blocked              domain.AttentionItem
		specificationFailure domain.AttentionItem
		outcome              publish.Outcome
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmission(ctx, invocationID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) || specificationRunID == "" {
				return err
			}
			items, listErr := tx.ListAttentionItems(ctx)
			if listErr != nil {
				return listErr
			}
			specificationFailure = realRunSpecificationFailure(items, specificationRunID)
			if specificationFailure.ID != "" {
				return nil
			}
			return err
		}
		if _, _, err := validateRealRunImplementationBinding(identityInput, &admission.RunID); err != nil {
			return err
		}
		executionOutcome, outcomeErr := tx.GetExecutionOutcomeRecord(ctx, invocationID)
		switch {
		case outcomeErr == nil:
			terminal = &executionOutcome
			return nil
		case !errors.Is(outcomeErr, store.ErrNotFound):
			return outcomeErr
		}
		export, err = tx.GetExecutionExport(ctx, invocationID)
		if err != nil {
			return err
		}
		if _, err := tx.GetInbox(ctx, string(invocationID)); err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		ready, blocked, err = realRunAttentionState(items, runID)
		if err != nil {
			return err
		}
		if blocked.ID != "" {
			return nil
		}
		if ready.ID == "" {
			return store.ErrNotFound
		}
		artifactDigests := make([]domain.Digest, len(ready.EvidenceSnapshot))
		for index, artifact := range ready.EvidenceSnapshot {
			artifactDigests[index] = artifact.Digest
		}
		identity, err := publish.DeriveIdentity(publish.IdentityInput{
			Repo: env.repo, BaseRef: env.baseRef, SourceHeadSHA: ready.PRHeadSHA,
			ArtifactDigests: artifactDigests, RecipeDigest: &env.approvedRecipe,
		})
		if err != nil {
			return err
		}
		entry, err := tx.GetInbox(ctx, publish.OutcomeKey(identity))
		if err != nil {
			return err
		}
		if entry.Kind != publish.IntentKindOutcome {
			return fmt.Errorf("publication outcome row has kind %q", entry.Kind)
		}
		outcome, err = publish.DecodeOutcome(entry.Payload)
		return err
	}); err != nil {
		t.Fatalf("read durable execution record: %v", err)
	}
	if specificationFailure.ID != "" {
		t.Fatalf(
			"real run specification failed: run=%s item=%s reason=%q",
			specificationRunID,
			specificationFailure.ID,
			specificationFailure.Reason,
		)
	}
	if blocked.ID != "" {
		t.Fatalf("real run publication blocked: %s", blocked.Reason)
	}
	if terminal != nil {
		t.Fatalf(
			"real run terminal outcome: status=%s summary=%q",
			terminal.Status,
			terminal.Summary,
		)
	}
	if ready.Status != domain.StatusOpen || ready.PRHeadSHA != export.HeadSHA ||
		len(ready.EvidenceSnapshot) == 0 {
		t.Errorf("ready item = %#v, want open with verifier evidence at export head", ready)
	}
	if outcome.Repo != env.repo || outcome.BaseRef != env.baseRef ||
		outcome.HeadSHA != export.HeadSHA || outcome.PRNumber <= 0 || !outcome.EvidenceEligible {
		t.Errorf("publication outcome = %#v, want the exact verified export head", outcome)
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

	// Acceptance happened exactly once after publication, and the run reported
	// no failure or publish-blocked item.
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetInbox(ctx, string(invocationID)); err != nil {
			return err
		}
		_, err := tx.GetAttentionItem(ctx, domain.ItemID("execution-failure-"+string(invocationID)))
		if err == nil {
			t.Error("the run raised an execution_failure item")
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
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
			t.Error("the run retains an open production publish-blocked item")
		}
		return nil
	}); err != nil {
		t.Fatalf("read acceptance state: %v", err)
	}

	t.Logf("real production pipeline verified: PR #%d at head %s over base %s",
		outcome.PRNumber, export.HeadSHA, export.ObservedBaseSHA)
}

func realRunBackupPayloadExtractors() map[string]store.BackupPayloadDigestExtractor {
	return map[string]store.BackupPayloadDigestExtractor{
		engine.FakePublicationTaskKind:              engine.FakePublicationBackupPayloadDigests,
		engine.FakePublicationInvocationOwnerKind:   engine.FakePublicationInvocationOwnerBackupPayloadDigests,
		signet.AgentInvocationRequestedKind:         signet.AgentInvocationBackupPayloadDigests,
		signet.PublicationReevaluationRequestedKind: signet.PublicationReevaluationBackupPayloadDigests,
		signet.PublicationReevaluationCompletedKind: signet.PublicationReevaluationCompletionBackupPayloadDigests,
		engine.KindProductionInvocationRequested:    engine.ProductionInvocationBackupPayloadDigests,
		engine.KindProductionPublicationRequested:   engine.ProductionPublicationBackupPayloadDigests,
		engine.KindSpecificationInvocationRequested: engine.SpecificationInvocationBackupPayloadDigests,
		engine.KindSpecificationImplementationClaim: engine.SpecificationImplementationClaimBackupPayloadDigests,
		publish.IntentKindReservation:               publish.ReservationBackupPayloadDigests,
		publish.IntentKindPublication:               publish.PublicationBackupPayloadDigests,
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
		case item.Value.Type == domain.AttentionReadyForFinalReview:
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
