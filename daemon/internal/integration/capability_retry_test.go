package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The issue-#1102 scenario: a selected capability manifest must survive all
// the way to the admission gate. Only the production branch of admitAttempt
// re-derives the manifest from the retry run's policy, re-gates it against the
// live composition, and overrides the egress profile it starts under, so the
// proof is a stored admission and a driver start spec carrying the manifest's
// profile — never a helper call or an attended hold short of admission.

const (
	capabilityRetryProject   domain.ProjectID = "project-capability-retry"
	capabilityRetryRunID     domain.RunID     = "run-implementation-capability-retry"
	capabilityRetryCommandID                  = "command-capability-retry"
)

var (
	capabilityRetrySpecificationPromptBody = []byte("Specify the work item into a bounded specification.\n")
	capabilityRetrySpecificationPrompt     = domain.Digest(
		contentaddr.Sum(capabilityRetrySpecificationPromptBody))
)

// capabilityRetryManifest is the manifest the operator selects. Its egress
// profile differs from the composition's default, which is exactly what makes
// the failure producer offer it and the admission gate change anything.
func capabilityRetryManifest(t *testing.T) domain.CapabilityManifest {
	t.Helper()
	manifest, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
	if err != nil {
		t.Fatalf("NewCapabilityManifest: %v", err)
	}
	return manifest
}

// capabilityRetryPolicy is the specification run's resolved policy: the
// specification workflow's own keys plus the manifest set a retry may select
// from. The implementation run inherits these keys at approval and the retry
// run copies its parent's, so one authored policy governs the whole campaign.
func capabilityRetryPolicy(
	t *testing.T, manifest domain.CapabilityManifest, runID domain.RunID,
) []domain.PolicyKey {
	t.Helper()
	manifests, err := json.Marshal([]domain.CapabilityManifest{manifest})
	if err != nil {
		t.Fatalf("marshal manifest set: %v", err)
	}
	provenance := domain.KeyProvenance{
		Source: domain.ProvenancePreset, Digest: submissionDigest(string(runID), "policy-source"),
	}
	return []domain.PolicyKey{
		{Key: domain.CapabilityManifestPolicyKey, Value: string(manifests), Provenance: provenance},
		{Key: specify.PolicySpecApproval, Value: "true", Provenance: provenance},
		{Key: specify.PolicyMaxIterations, Value: "1", Provenance: provenance},
		{Key: specify.PolicyStageActiveTime, Value: "1m", Provenance: provenance},
		{Key: specify.PolicyApprovalWait, Value: "1m", Provenance: provenance},
		{Key: specify.PolicyResearchAllowlist, Value: "https://docs.example", Provenance: provenance},
		{Key: specify.PolicyResearchMaxBytes, Value: "1024", Provenance: provenance},
	}
}

// openCapabilityRetryFixture is the unattended fixture composed for capability
// retry: enforceable declares both egress profiles the scenario needs, and the
// specification workflow is composed in because specification approval is the
// only thing that creates the production campaign a retry attempt extends.
func openCapabilityRetryFixture(
	t *testing.T, root string, seed bool, enforceable ...domain.EgressProfile,
) *workflowFixture {
	t.Helper()
	return openUnattendedFixtureWith(t, root, seed, testIdentity,
		func(env *engine.AdmissionEnvironment) { env.EnforceableEgressProfiles = enforceable },
		func(st *store.Store, blobs *signet.BlobStore) []engine.Option {
			fetcher, err := specify.NewFetcher(st, blobs, nil)
			if err != nil {
				t.Fatalf("specify.NewFetcher: %v", err)
			}
			return []engine.Option{engine.WithSpecification(engine.SpecificationConfig{
				Fetcher: fetcher, Blobs: blobs, Now: func() time.Time { return admittedAt },
				PromptPackageDigest: capabilityRetrySpecificationPrompt,
			})}
		})
}

// openWideCapabilityRetryFixture is the composition the scenario admits under:
// both the failed attempt's profile and the selected manifest's are
// enforceable.
func openWideCapabilityRetryFixture(t *testing.T, root string, seed bool) *workflowFixture {
	t.Helper()
	return openCapabilityRetryFixture(t, root, seed,
		domain.EgressProviderOnly, domain.EgressProviderWebRead)
}

// capabilityRetrySelection is the durable state an accepted operator command
// leaves behind, plus the identities every later assertion keys off.
type capabilityRetrySelection struct {
	manifest         domain.CapabilityManifest
	campaignID       domain.CampaignID
	failedInvocation domain.InvocationID
	retryRunID       domain.RunID
	retryInvocation  domain.InvocationID
}

// driveCapabilityRetrySelection runs the real path up to an accepted
// retry_with_capabilities command: a specification run is approved, the
// engine dispatches its implementation attempt, the fake driver fails it, and
// the engine's own failure producer raises the card that offers the manifest.
// Nothing here is hand-seeded — not the attempt, not its admission, not the
// card — so the offer under test is the one production code computes.
func driveCapabilityRetrySelection(t *testing.T, f *workflowFixture) capabilityRetrySelection {
	t.Helper()
	ctx := context.Background()
	f.seedDevices(t)
	manifest := capabilityRetryManifest(t)

	specificationRunID, err := engine.SpecificationRunIDForImplementation(capabilityRetryRunID)
	if err != nil {
		t.Fatalf("derive specification run: %v", err)
	}
	campaignID, err := engine.ProductionCampaignIDForImplementation(capabilityRetryRunID)
	if err != nil {
		t.Fatalf("derive campaign: %v", err)
	}
	source, policyArtifact, resolved := registerSubmissionArtifactsWithPolicyKeys(
		t, f.store, string(specificationRunID), capabilityRetryPolicy(t, manifest, specificationRunID))
	policyBody, err := json.Marshal(resolved.Keys)
	if err != nil {
		t.Fatalf("marshal policy keys: %v", err)
	}
	for _, input := range []struct {
		digest domain.Digest
		body   []byte
	}{
		{source.Digest, submissionSpecification(string(specificationRunID))},
		{policyArtifact.Digest, policyBody},
		{capabilityRetrySpecificationPrompt, capabilityRetrySpecificationPromptBody},
	} {
		if _, err := f.blobs.Put(input.digest, bytes.NewReader(input.body)); err != nil {
			t.Fatalf("put blob %s: %v", input.digest, err)
		}
	}
	specificationInvocation := domain.InvocationID(engine.SpecificationDispatchMarkerKey(specificationRunID))
	if err := specifyfake.Script(f.driver, specificationInvocation, 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary:    "The implementation plan is ready.",
			Body:       "# Approved Specification\n\nRun the implementation stage.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatalf("script specification: %v", err)
	}
	publicationBytes, err := json.Marshal(productionPublicationMetadata())
	if err != nil {
		t.Fatalf("marshal publication: %v", err)
	}
	if _, err := engine.SubmitSpecificationRun(ctx, f.store, engine.SpecificationRunSpec{
		SpecificationRunID: specificationRunID, ImplementationRunID: capabilityRetryRunID,
		ProjectID: capabilityRetryProject, SourceArtifactID: source.ID,
		PolicyArtifactID: policyArtifact.ID, ResolvedPolicy: resolved,
		Publication:       productionPublicationMetadata(),
		PublicationDigest: domain.Digest(contentaddr.Sum(publicationBytes)),
		PublicationBytes:  publicationBytes,
		CampaignID:        campaignID, AttemptNumber: 1,
	}); err != nil {
		t.Fatalf("submit specification run: %v", err)
	}

	// One pass runs the specification stage and accepts its result, which
	// raises the approval card the operator answers.
	reconcileCapabilityRetryPass(t, f, "specification")
	approval, err := f.signet.GetAttentionItem(ctx,
		domain.ItemID("spec-approval-"+string(capabilityRetryRunID)+"-1"))
	if err != nil {
		t.Fatalf("get specification approval: %v", err)
	}
	if _, err := f.signet.Submit(ctx, signet.ClientCommand{
		CommandID: "approve-capability-retry", DeviceID: deviceA,
		ExpectedEntityVersion: approval.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: approval.Item.ID, Action: domain.ActionApprove,
			ItemVersion: approval.Item.ItemVersion, ArtifactDigests: approval.Item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("approve specification: %v", err)
	}

	failedInvocation := productionInvocationForRun(capabilityRetryRunID)
	f.driver.Script(failedInvocation, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "The stage could not reach the provider's documentation."},
	})
	// Approval creates the implementation run, the next pass dispatches it,
	// and the third records its failure terminal and the failure card.
	for _, stage := range []string{"approval", "implementation dispatch", "implementation failure"} {
		reconcileCapabilityRetryPass(t, f, stage)
	}

	card, err := f.signet.GetAttentionItem(ctx,
		domain.ItemID("execution-failure-"+string(failedInvocation)))
	if err != nil {
		t.Fatalf("get failure card: %v", err)
	}
	if !card.Item.Offers(domain.ActionRetryWithCapability) {
		t.Fatalf("failure card requests %v, want retry_with_capabilities", card.Item.RequestedDecision)
	}
	if card.Item.ExecutionFailure == nil {
		t.Fatal("failure card carries no execution-failure facts")
	}
	offered := card.Item.ExecutionFailure.OfferedManifests
	if len(offered) != 1 || offered[0] != manifest.Offer() {
		t.Fatalf("offered manifests = %+v, want the policy manifest %+v", offered, manifest.Offer())
	}

	digest := manifest.Digest
	if _, err := f.signet.Submit(ctx, signet.ClientCommand{
		CommandID: capabilityRetryCommandID, DeviceID: deviceA,
		ExpectedEntityVersion: card.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: card.Item.ID, Action: domain.ActionRetryWithCapability,
			ItemVersion: card.Item.ItemVersion, CapabilityManifestDigest: &digest,
		},
	}); err != nil {
		t.Fatalf("submit retry_with_capabilities: %v", err)
	}

	retryRunID, err := engine.ProductionAttemptRunID(campaignID, 2)
	if err != nil {
		t.Fatalf("derive retry run: %v", err)
	}
	return capabilityRetrySelection{
		manifest: manifest, campaignID: campaignID, failedInvocation: failedInvocation,
		retryRunID: retryRunID, retryInvocation: productionInvocationForRun(retryRunID),
	}
}

// productionInvocationForRun mirrors the engine's production invocation key.
// The derivation is engine-private, so a change to it breaks these tests
// loudly rather than silently asserting about an id nothing dispatches.
func productionInvocationForRun(runID domain.RunID) domain.InvocationID {
	return domain.InvocationID("inv-implement-" + string(runID))
}

func reconcileCapabilityRetryPass(t *testing.T, f *workflowFixture, stage string) engine.ReconcileResult {
	t.Helper()
	result, err := f.engine.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile %s: %v", stage, err)
	}
	return result
}

// scriptCapabilityRetryStage scripts the retry invocation before the pass that
// admits it. An unscripted id fails Start after the admission is already
// persisted, which would turn the proof into a confusing partial state. The
// retry's own outcome is beside the point: this test ends at the start.
func scriptCapabilityRetryStage(f *workflowFixture, selection capabilityRetrySelection) {
	f.driver.Script(selection.retryInvocation, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "The retry ran under the selected profile."},
	})
}

// assertAdmittedUnderManifest is the acceptance proof. CapabilityManifestDigest
// and the overridden egress profile are written only inside the production
// branch of admitAttempt, so a stored admission carrying both is evidence that
// the recheck ran; the driver's start spec is evidence it bound the execution.
func assertAdmittedUnderManifest(
	t *testing.T, f *workflowFixture, selection capabilityRetrySelection,
) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		admission, found, err := tx.LookupExecutionAdmission(ctx, selection.retryInvocation)
		if err != nil {
			return err
		}
		if !found {
			t.Fatalf("retry invocation %q has no admission", selection.retryInvocation)
		}
		if admission.CapabilityManifestDigest == nil ||
			*admission.CapabilityManifestDigest != selection.manifest.Digest {
			t.Errorf("admission manifest digest = %v, want %q",
				admission.CapabilityManifestDigest, selection.manifest.Digest)
		}
		if admission.EgressProfile != domain.EgressProviderWebRead {
			t.Errorf("admission egress profile = %q, want %q",
				admission.EgressProfile, domain.EgressProviderWebRead)
		}
		if admission.OperatingMode != domain.ModeUnattended {
			t.Errorf("admission operating mode = %q, want unattended", admission.OperatingMode)
		}
		return nil
	}); err != nil {
		t.Fatalf("read retry admission: %v", err)
	}
	spec, started := f.driver.StartSpec(selection.retryInvocation)
	if !started {
		t.Fatalf("driver recorded no start for %q", selection.retryInvocation)
	}
	if spec.EgressProfile != domain.EgressProviderWebRead {
		t.Errorf("start spec egress profile = %q, want %q",
			spec.EgressProfile, domain.EgressProviderWebRead)
	}
	assertOneRetryAttempt(t, f, selection)
}

// assertOneRetryAttempt pins the one-attempt-one-effect half: attempt 2 stays
// the latest attempt, carries the operator bindings the command supplied, and
// its run's implement stage holds exactly one attempt.
func assertOneRetryAttempt(t *testing.T, f *workflowFixture, selection capabilityRetrySelection) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		attempt, err := tx.LatestProductionAttempt(ctx, selection.campaignID)
		if err != nil {
			return err
		}
		if attempt.AttemptNumber != 2 || attempt.Kind != domain.ProductionAttemptRetry ||
			attempt.ImplementationRunID != selection.retryRunID {
			t.Errorf("latest attempt = %+v, want retry attempt 2 on %q", attempt, selection.retryRunID)
		}
		if attempt.OperatorCommandID == nil || *attempt.OperatorCommandID != capabilityRetryCommandID ||
			attempt.RetryOfInvocationID == nil ||
			*attempt.RetryOfInvocationID != selection.failedInvocation ||
			attempt.CapabilityManifestDigest == nil ||
			*attempt.CapabilityManifestDigest != selection.manifest.Digest {
			t.Errorf("attempt operator bindings = %+v, want the accepted command's", attempt)
		}
		run, err := tx.GetRun(ctx, selection.retryRunID)
		if err != nil {
			return err
		}
		attempts := productionStageAttempts(t, run)
		if attempts != 1 {
			t.Errorf("retry run implement-stage attempts = %d, want exactly one", attempts)
		}
		return nil
	}); err != nil {
		t.Fatalf("read retry attempt state: %v", err)
	}
}

func productionStageAttempts(t *testing.T, run domain.Run) int {
	t.Helper()
	for _, stage := range run.Stages {
		if stage.ID == domain.StageID("implement-"+string(run.ID)) {
			return len(stage.Attempts)
		}
	}
	t.Fatalf("run %q has no implement stage: %+v", run.ID, run.Stages)
	return 0
}

// allocateCapabilityRetryAttempt reproduces the reconciler's allocation
// without admitting it: ReattemptProductionRun is exactly what
// reconcileCapabilityRetry calls once the command is accepted, so the durable
// state it leaves is the real crash window between allocation and admission.
func allocateCapabilityRetryAttempt(
	t *testing.T, f *workflowFixture, selection capabilityRetrySelection,
) {
	t.Helper()
	retry, err := engine.ReattemptProductionRun(context.Background(), f.store,
		engine.ProductionReattemptSpec{
			ParentRunID:              capabilityRetryRunID,
			Reason:                   "operator capability retry " + capabilityRetryCommandID,
			OperatorCommandID:        capabilityRetryCommandID,
			RetryOfInvocationID:      selection.failedInvocation,
			CapabilityManifestDigest: selection.manifest.Digest,
		})
	if err != nil {
		t.Fatalf("allocate retry attempt: %v", err)
	}
	if !retry.Created || retry.Run.Run.ID != selection.retryRunID {
		t.Fatalf("allocation = %+v, want the created retry run %q", retry, selection.retryRunID)
	}
}

// TestCapabilityRetryAdmitsUnderTheSelectedManifest is the acceptance proof:
// the selected manifest survives allocation, is re-derived and re-gated at
// admission, and binds the execution the driver starts. Replay on the same
// engine and replay across a restart add no second attempt, admission, or
// start.
func TestCapabilityRetryAdmitsUnderTheSelectedManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := openWideCapabilityRetryFixture(t, root, true)
	selection := driveCapabilityRetrySelection(t, f)
	scriptCapabilityRetryStage(f, selection)

	// One pass allocates attempt 2 and admits and starts its invocation:
	// reconcileCapabilityRetries runs before reconcileInvocations, so there is
	// no pass boundary between the two.
	admitted := reconcileCapabilityRetryPass(t, f, "capability retry")
	if admitted.RunTransitions != 1 || admitted.InvocationsStarted != 1 {
		t.Fatalf("capability retry pass = %+v, want one transition and one start", admitted)
	}
	assertAdmittedUnderManifest(t, f, selection)

	reconcileCapabilityRetryPass(t, f, "capability retry replay")
	assertAdmittedUnderManifest(t, f, selection)

	f.close(t)
	restarted := openWideCapabilityRetryFixture(t, root, false)
	reconcileCapabilityRetryPass(t, restarted, "capability retry restart")
	assertAdmittedUnderManifest(t, restarted, selection)
}

// TestCapabilityRetryAdmitsOnceAcrossARestartBeforeAdmission covers the crash
// window the allocation opens: attempt 2 is committed and its production
// intent is pending when the daemon dies. The restarted engine resumes that
// allocation rather than making a second one, and still admits it under the
// selected manifest exactly once.
func TestCapabilityRetryAdmitsOnceAcrossARestartBeforeAdmission(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := openWideCapabilityRetryFixture(t, root, true)
	selection := driveCapabilityRetrySelection(t, f)
	allocateCapabilityRetryAttempt(t, f, selection)
	if _, started := f.driver.StartSpec(selection.retryInvocation); started {
		t.Fatal("allocation started the retry invocation before any admission")
	}

	f.close(t)
	restarted := openWideCapabilityRetryFixture(t, root, false)
	scriptCapabilityRetryStage(restarted, selection)
	reconcileCapabilityRetryPass(t, restarted, "resumed capability retry")
	assertAdmittedUnderManifest(t, restarted, selection)

	reconcileCapabilityRetryPass(t, restarted, "resumed capability retry replay")
	assertAdmittedUnderManifest(t, restarted, selection)
}

// TestCapabilityRetryRefusesAnUnenforceableManifest is the fail-closed half:
// a composition that no longer declares the selected profile enforceable
// refuses the admission outright. There is no fallback to the composition's
// own profile, and the allocated attempt stays unadmitted and unstarted with
// its intent still pending.
func TestCapabilityRetryRefusesAnUnenforceableManifest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	f := openWideCapabilityRetryFixture(t, root, true)
	selection := driveCapabilityRetrySelection(t, f)
	allocateCapabilityRetryAttempt(t, f, selection)

	f.close(t)
	// Same composition apart from the enforceable set, so a refusal can only
	// be the manifest re-gate and not an unrelated admission mismatch.
	narrowed := openCapabilityRetryFixture(t, root, false, domain.EgressProviderOnly)
	scriptCapabilityRetryStage(narrowed, selection)
	_, err := narrowed.engine.Reconcile(ctx)
	if !errors.Is(err, domain.ErrCapabilityManifestInvalid) {
		t.Fatalf("narrowed reconcile = %v, want ErrCapabilityManifestInvalid", err)
	}
	// The reconciler skips its own manifest recheck once the attempt is
	// allocated, so the refusal has to come from the admission gate. Pinning
	// the origin keeps this from passing for the wrong reason if that
	// ordering ever changes.
	if !strings.Contains(err.Error(), "admit invocation") {
		t.Fatalf("refusal = %v, want it raised by the admission gate", err)
	}

	if err := narrowed.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, found, err := tx.LookupExecutionAdmission(ctx, selection.retryInvocation); err != nil {
			return err
		} else if found {
			t.Error("the refused retry recorded an admission")
		}
		run, err := tx.GetRun(ctx, selection.retryRunID)
		if err != nil {
			return err
		}
		if attempts := productionStageAttempts(t, run); attempts != 0 {
			t.Errorf("refused retry run has %d stage attempts, want none", attempts)
		}
		pending, err := tx.ListPendingOutbox(ctx, engine.KindProductionInvocationRequested)
		if err != nil {
			return err
		}
		held := false
		for _, entry := range pending {
			held = held || entry.IdempotencyKey == string(selection.retryInvocation)
		}
		if !held {
			t.Error("the refused retry intent is no longer pending")
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect refused state: %v", err)
	}
	if _, started := narrowed.driver.StartSpec(selection.retryInvocation); started {
		t.Error("driver started the refused retry invocation")
	}
}
