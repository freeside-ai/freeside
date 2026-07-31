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
)

// The #237 real execution-export harness: one work item submitted through the
// production path and driven to a durable ExecutionExport, against the real
// Apple container runtime, the pinned agent image, and the operator's managed
// repository. Clean verification and publication remain #318's slice.
//
// Opt-in and CI-blind: GitHub's macOS runners have no Apple container, and
// the run spends real inference. CI records it as Not run; the ward live
// suite (FREESIDE_WARD_LIVE_TEST) is the same tradition.
//
// What it proves that the in-process tests cannot: that the composition the
// daemon actually builds admits, hands off, exports, imports, and records,
// with every gate present rather than stubbed.

const realRunLiveEnv = "FREESIDE_REAL_RUN_LIVE_TEST"

type realRunEnv struct {
	stateRoot      string
	agentImage     domain.ImageRef
	exporterImage  string
	seedRoot       string
	authIdentityID domain.AuthIdentityID
	authVolume     string
	repo           string
	repositoryID   int64
	baseRef        string
	baseSHA        string
	promptPackage  string
	instructions   string
	approvedRecipe domain.Digest
}

func realRunEnvironment(t *testing.T) realRunEnv {
	t.Helper()
	if os.Getenv(realRunLiveEnv) != "1" {
		t.Skip("the real unattended run is opt-in: set " + realRunLiveEnv + "=1 with " +
			"FREESIDE_REAL_RUN_STATE_ROOT, FREESIDE_REAL_RUN_AGENT_IMAGE (digest-pinned), " +
			"FREESIDE_WARD_EXPORTER_IMAGE (digest-pinned), FREESIDE_REAL_RUN_SEED_ROOT, " +
			"FREESIDE_REAL_RUN_AUTH_IDENTITY, FREESIDE_REAL_RUN_AUTH_VOLUME, " +
			"FREESIDE_REAL_RUN_REPO (owner/name), FREESIDE_REAL_RUN_REPOSITORY_ID, " +
			"FREESIDE_REAL_RUN_BASE_REF, FREESIDE_REAL_RUN_BASE_SHA, " +
			"FREESIDE_REAL_RUN_PROMPT_PACKAGE (file), FREESIDE_REAL_RUN_INSTRUCTIONS (file), " +
			"FREESIDE_REAL_RUN_APPROVED_RECIPE (sha256 digest)")
	}
	env := realRunEnv{
		stateRoot:      requireEnv(t, "FREESIDE_REAL_RUN_STATE_ROOT"),
		agentImage:     domain.ImageRef(requireEnv(t, "FREESIDE_REAL_RUN_AGENT_IMAGE")),
		exporterImage:  requireEnv(t, "FREESIDE_WARD_EXPORTER_IMAGE"),
		seedRoot:       requireEnv(t, "FREESIDE_REAL_RUN_SEED_ROOT"),
		authIdentityID: domain.AuthIdentityID(requireEnv(t, "FREESIDE_REAL_RUN_AUTH_IDENTITY")),
		authVolume:     requireEnv(t, "FREESIDE_REAL_RUN_AUTH_VOLUME"),
		repo:           requireEnv(t, "FREESIDE_REAL_RUN_REPO"),
		baseRef:        requireEnv(t, "FREESIDE_REAL_RUN_BASE_REF"),
		baseSHA:        requireEnv(t, "FREESIDE_REAL_RUN_BASE_SHA"),
		promptPackage:  requireEnv(t, "FREESIDE_REAL_RUN_PROMPT_PACKAGE"),
		instructions:   requireEnv(t, "FREESIDE_REAL_RUN_INSTRUCTIONS"),
		approvedRecipe: domain.Digest(requireEnv(t, "FREESIDE_REAL_RUN_APPROVED_RECIPE")),
	}
	id, err := strconv.ParseInt(requireEnv(t, "FREESIDE_REAL_RUN_REPOSITORY_ID"), 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("FREESIDE_REAL_RUN_REPOSITORY_ID must be a positive integer: %v", err)
	}
	env.repositoryID = id
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

// TestRealWorkItemProducesExecutionExport verifies that a work item driven by
// scripts/run-real-work.sh reached a terminal production outcome and produced
// the durable pre-publication record this slice owns.
func TestRealWorkItemProducesExecutionExport(t *testing.T) {
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
	health, err := backupFiles.NewCheckpointHealthSource(blobs, opts.ApprovedRecipes,
		map[string]store.BackupPayloadDigestExtractor{
			engine.FakePublicationTaskKind:            engine.FakePublicationBackupPayloadDigests,
			engine.FakePublicationInvocationOwnerKind: engine.FakePublicationInvocationOwnerBackupPayloadDigests,
			signet.AgentInvocationRequestedKind:       signet.AgentInvocationBackupPayloadDigests,
			engine.KindProductionInvocationRequested:  engine.ProductionInvocationBackupPayloadDigests,
			publish.IntentKindReservation:             publish.ReservationBackupPayloadDigests,
			publish.IntentKindPublication:             publish.PublicationBackupPayloadDigests,
		})
	if err != nil {
		t.Fatalf("build checkpoint health source: %v", err)
	}
	opts.BackupHealthSource = health

	st, err := store.Open(ctx, dbPath, opts)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	// The identity binding the ward gate compares the writable mount
	// against. It is an operator precondition in production; the harness
	// records it so a fresh state root is runnable.
	identity := domain.AuthIdentity{
		ID: env.authIdentityID, Provider: "claude", AuthStoreMutationLease: true,
		AuthStoreVolume: env.authVolume, MaxParallelExecutions: 1,
		RefreshStrategy: domain.RefreshOnDemand,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, time.Now().UTC())
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
		return nil
	}); err != nil {
		t.Fatalf("no usable activated AutomationTrustProfile for %q: %v; "+
			"activate one for this repository before the run, or unattended "+
			"admission holds and the daemon never starts the writer", env.repo, err)
	}

	t.Log("preconditions recorded; submit the work item with `freesided submit` " +
		"and run the daemon with -driver=claude against this state root")

	// The harness asserts the durable outcome rather than driving the daemon
	// in-process: the production composition lives in package main, and a
	// second in-process wiring of it here would be a different composition
	// than the one shipped, which is exactly what this test exists to check.
	// scripts/run-real-work.sh performs the run; this test is its verifier
	// and can also be pointed at a state root a manual run produced.
	invocationID := domain.InvocationID(os.Getenv("FREESIDE_REAL_RUN_INVOCATION"))
	if invocationID == "" {
		t.Skip("set FREESIDE_REAL_RUN_INVOCATION to the submitted run's invocation id " +
			"to verify a completed run; scripts/run-real-work.sh sets it")
	}

	var (
		admission domain.ExecutionAdmission
		export    domain.ExecutionExport
		terminal  *domain.ExecutionOutcome
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		outcome, outcomeErr := tx.GetExecutionOutcomeRecord(ctx, invocationID)
		switch {
		case outcomeErr == nil:
			terminal = &outcome
			return nil
		case !errors.Is(outcomeErr, store.ErrNotFound):
			return outcomeErr
		}
		var err error
		admission, err = tx.GetExecutionAdmission(ctx, invocationID)
		if err != nil {
			return err
		}
		export, err = tx.GetExecutionExport(ctx, invocationID)
		return err
	}); err != nil {
		t.Fatalf("read durable execution record: %v", err)
	}
	if terminal != nil {
		t.Fatalf(
			"real run terminal outcome: status=%s summary=%q",
			terminal.Status,
			terminal.Summary,
		)
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

	// Export: the binding #318 will refuse a publication without.
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

	// Acceptance happened exactly once, and the run reported no failure item.
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
		return nil
	}); err != nil {
		t.Fatalf("read acceptance state: %v", err)
	}

	t.Logf("real execution export verified: head %s over base %s", export.HeadSHA, export.ObservedBaseSHA)
}
