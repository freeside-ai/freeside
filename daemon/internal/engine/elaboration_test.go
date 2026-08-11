package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	elaboratefake "github.com/freeside-ai/freeside/daemon/internal/elaborate/fake"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type elaborationRoundTripFunc func(*http.Request) (*http.Response, error)

func (f elaborationRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type elaborationFixture struct {
	store      *store.Store
	blobs      *signet.BlobStore
	signet     *signet.Service
	driverDir  string
	vendorPath string
	now        *time.Time
	policy     domain.ResolvedPolicy
	source     domain.Artifact
	policyArt  domain.Artifact
	prompt     domain.Digest
	fetchCalls *atomic.Int64
}

func newElaborationFixture(t *testing.T, specApproval bool, maxIterations int) elaborationFixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(root, "state.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	attention := signet.NewService(st, signet.WithBlobStore(blobs), signet.WithClock(func() time.Time { return now }))
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutDevice(t.Context(), domain.Device{
			ID: "device-1", DisplayName: "Operator", Status: domain.DeviceActive, PairedAt: now,
		}); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(t.Context(), domain.AuthIdentity{
			ID: "auth-1", Provider: "codex", AuthStoreMutationLease: true,
			AuthStoreVolume: "provider-credentials", MaxParallelExecutions: 1,
			RefreshStrategy: domain.RefreshOnDemand,
		}, now)
	}); err != nil {
		t.Fatal(err)
	}
	provenanceDigest := domain.Digest(contentaddr.Sum([]byte("elaboration-test-policy")))
	provenance := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: provenanceDigest}
	policy, err := domain.NewResolvedPolicy("elaboration-run", []domain.PolicyKey{
		{Key: elaborate.PolicySpecApproval, Value: boolString(specApproval), Provenance: provenance},
		{Key: elaborate.PolicyMaxIterations, Value: intString(maxIterations), Provenance: provenance},
		{Key: elaborate.PolicyStageActiveTime, Value: "1m", Provenance: provenance},
		{Key: elaborate.PolicyApprovalWait, Value: "1m", Provenance: provenance},
		{Key: elaborate.PolicyResearchAllowlist, Value: "https://docs.example", Provenance: provenance},
		{Key: elaborate.PolicyResearchMaxBytes, Value: "1024", Provenance: provenance},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceBody := []byte("Investigate the work item and produce an implementation specification.")
	source := testElaborationArtifact(t, "work-item", domain.ArtifactKindSpecification,
		domain.Digest(contentaddr.Sum(sourceBody)), domain.ProducerAgent, "work-item-importer")
	policyArt := testElaborationArtifact(t, "resolved-policy", domain.ArtifactKindPolicy,
		policy.Digest, domain.ProducerDaemon, "policy-resolver")
	policyBody, err := json.Marshal(policy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(source.Digest, strings.NewReader(string(sourceBody))); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(policyArt.Digest, strings.NewReader(string(policyBody))); err != nil {
		t.Fatal(err)
	}
	promptBody := []byte("Elaborate the work item using only the supplied artifacts.\n")
	promptDigest := domain.Digest(contentaddr.Sum(promptBody))
	if _, err := blobs.Put(promptDigest, strings.NewReader(string(promptBody))); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(t.Context(), source); err != nil {
			return err
		}
		return tx.PutArtifact(t.Context(), policyArt)
	}); err != nil {
		t.Fatal(err)
	}
	vendorPath := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(vendorPath, []byte("Stay within the declared work unit.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return elaborationFixture{
		store: st, blobs: blobs, signet: attention, driverDir: filepath.Join(root, "driver"),
		vendorPath: vendorPath, now: &now, policy: policy, source: source, policyArt: policyArt,
		prompt:     promptDigest,
		fetchCalls: &atomic.Int64{},
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func intString(value int) string {
	return strconv.Itoa(value)
}

func testElaborationArtifact(
	t *testing.T,
	id domain.ArtifactID,
	kind domain.ArtifactKind,
	digest domain.Digest,
	producer domain.ProducerClass,
	invocation domain.InvocationID,
) domain.Artifact {
	t.Helper()
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: id, Type: kind, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: producer, ProducerInvocationID: invocation,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func (f elaborationFixture) newDriver(t *testing.T) *execfake.StageDriver {
	t.Helper()
	driver, err := execfake.NewStageDriverAt(f.driverDir)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func (f elaborationFixture) newEngine(t *testing.T, driver *execfake.StageDriver) *Engine {
	t.Helper()
	transport := elaborationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		f.fetchCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("authoritative research")), Request: request,
		}, nil
	})
	fetcher, err := elaborate.NewFetcher(f.store, f.blobs, transport)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.AuthIdentityID("auth-1")
	engine, err := New(f.store, f.signet, driver,
		WithAdmission(stageInputBackend{}, []exec.Capability{exec.CapPostExitExport}, AdmissionEnvironment{
			OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile:       domain.EgressProviderOnly,
			ImageRef:            domain.ImageRef("agent@sha256:" + strings.Repeat("a", 64)),
			PromptPackageDigest: f.prompt,
			VendorInstructions: VendorInstructionConfig{
				Vendor: domain.AgentVendorCodex, Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: f.vendorPath,
			},
			Base: domain.BaseRevision{
				Repo: "owner/repo", RepositoryID: 1,
				BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
			},
			Workspace: "workspace-1", AuthIdentityID: &identity,
		}, func() time.Time { return *f.now }),
		WithElaboration(ElaborationConfig{Fetcher: fetcher, Blobs: f.blobs, Now: func() time.Time { return *f.now }}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func (f elaborationFixture) submit(t *testing.T) {
	t.Helper()
	if err := SubmitElaborationRun(t.Context(), f.store, ElaborationRunSpec{
		ElaborationRunID: "elaboration-run", ImplementationRunID: "implementation-run",
		ProjectID: "project-1", SourceArtifactID: f.source.ID, PolicyArtifactID: f.policyArt.ID,
		ResolvedPolicy: f.policy,
		Publication: ProductionPublication{
			Title: "Implement approved work item", Body: "Implements the operator-approved specification.",
			CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestElaborationResearchApprovalStartsDigestBoundImplementation(t *testing.T) {
	f := newElaborationFixture(t, true, 4)
	driver := f.newDriver(t)
	firstID := elaborationInvocationID("elaboration-run", 1)
	if err := elaboratefake.Script(driver, firstID, 0, 0, elaborate.Output{FetchRequests: []elaborate.FetchRequest{{
		URL: "https://docs.example/contracts", Purpose: "verify the implementation contract",
	}}}); err != nil {
		t.Fatal(err)
	}
	secondID := elaborationInvocationID("elaboration-run", 2)
	if err := elaboratefake.Script(driver, secondID, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
		Summary: "The implementation plan is ready.", Body: "# Approved Specification\n\nImplement the bounded workflow.",
		Addressals: []elaborate.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	f.submit(t)
	engine := f.newEngine(t, driver)
	var pending []store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(t.Context(), KindElaborationInvocationRequested)
		return err
	}); err != nil || len(pending) != 1 {
		t.Fatalf("pending elaboration intents = %d, %v", len(pending), err)
	}
	if engine.elaboration == nil || engine.admission == nil ||
		engine.admission.environment.OperatingMode != domain.ModeAttendedDev {
		t.Fatalf("engine composition = elaboration %v, admission %+v", engine.elaboration, engine.admission)
	}
	result, err := engine.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultsAccepted != 1 || f.fetchCalls.Load() != 1 {
		t.Fatalf("first reconcile = %+v, fetches = %d", result, f.fetchCalls.Load())
	}

	// Reconstruct both the driver and engine after research has committed but
	// before the next invocation starts. The prior URL must not be fetched
	// again, and the durable next intent must carry the research artifact.
	driver = f.newDriver(t)
	engine = f.newEngine(t, driver)
	result, err = engine.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultsAccepted != 1 || f.fetchCalls.Load() != 1 {
		t.Fatalf("restart reconcile = %+v, fetches = %d", result, f.fetchCalls.Load())
	}
	start, ok := driver.StartSpec(secondID)
	if !ok || start.EgressProfile != domain.EgressProviderOnly || start.StageInputs == nil {
		t.Fatalf("second start = %+v, found = %t", start, ok)
	}
	researchDigest := f.artifact(t, domain.ArtifactID("research-"+string(firstID)+"-1")).Digest
	if !slices.Contains(start.StageInputs.PriorArtifactDigests, researchDigest) {
		t.Fatalf("second stage inputs omit research digest: %+v", start.StageInputs)
	}
	if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("implementation run before approval = %v", err)
	}

	itemID := domain.ItemID("spec-approval-implementation-run-2")
	item, snapshot := f.item(t, itemID)
	if slices.Contains(item.RequestedDecision, domain.ActionDiscuss) {
		t.Fatalf("approval item offers unsupported discussion: %+v", item.RequestedDecision)
	}
	if len(item.AgentClaims) != 1 || item.AgentClaims[0].Text == nil ||
		item.AgentClaims[0].Text.Content != "# Approved Specification\n\nImplement the bounded workflow." {
		t.Fatalf("approval item does not carry the full specification: %+v", item.AgentClaims)
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "approve-spec", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionApprove,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatal(err)
	}
	result, err = engine.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.RunTransitions != 1 {
		t.Fatalf("approval reconcile = %+v", result)
	}
	implementation, err := f.run("implementation-run")
	if err != nil {
		t.Fatal(err)
	}
	if implementation.SpecDigest != item.ArtifactDigests[0] {
		t.Fatalf("implementation spec digest = %s, approved = %s", implementation.SpecDigest, item.ArtifactDigests[0])
	}
	if replay, err := engine.Reconcile(t.Context()); err != nil || replay.RunTransitions != 0 {
		t.Fatalf("approval replay = %+v, %v", replay, err)
	}
}

func TestElaborationRequestChangesCarriesFeedbackAndAddressals(t *testing.T) {
	f := newElaborationFixture(t, true, 3)
	driver := f.newDriver(t)
	firstID := elaborationInvocationID("elaboration-run", 1)
	if err := elaboratefake.Script(driver, firstID, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
		Summary: "First draft.", Body: "# Specification\n\nUse an unbounded request.",
		Addressals: []elaborate.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	firstItem, snapshot := f.item(t, "spec-approval-implementation-run-1")
	comment := "Bound the request body and explain the limit."
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "revise-spec", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: firstItem.ID, Action: domain.ActionRequestChanges,
			ItemVersion: firstItem.ItemVersion, ArtifactDigests: firstItem.ArtifactDigests, Message: comment,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	secondID := elaborationInvocationID("elaboration-run", 2)
	if err := elaboratefake.Script(driver, secondID, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
		Summary: "Revised draft.", Body: "# Specification\n\nLimit the request body to 1 MiB.",
		Addressals: []elaborate.Addressal{{Comment: comment, Response: "Added an explicit 1 MiB bound."}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	secondItem, _ := f.item(t, "spec-approval-implementation-run-2")
	if !strings.Contains(secondItem.Reason, "Diff from last reviewed version:") ||
		!strings.Contains(secondItem.Reason, comment) ||
		!strings.Contains(secondItem.Reason, "Added an explicit 1 MiB bound.") {
		t.Fatalf("revision reason omitted review history:\n%s", secondItem.Reason)
	}
	if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("implementation run after request_changes = %v", err)
	}
}

// TestElaborationSecondRequestChangesDispatches guards #685: a second
// request_changes round must retire the superseded prior specification from
// the next invocation's inputs. Retaining it left a stale Specification-typed
// input that loadElaborationBinding rejects with ErrParentKeyMismatch, so the
// enqueued iteration-3 revision could never dispatch and the reconcile pass
// halted for every run.
func TestElaborationSecondRequestChangesDispatches(t *testing.T) {
	f := newElaborationFixture(t, true, 4)
	driver := f.newDriver(t)
	firstID := elaborationInvocationID("elaboration-run", 1)
	if err := elaboratefake.Script(driver, firstID, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
		Summary: "First draft.", Body: "# Specification\n\nUse an unbounded request.",
		Addressals: []elaborate.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	firstItem, firstSnap := f.item(t, "spec-approval-implementation-run-1")
	firstComment := "Bound the request body and explain the limit."
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "revise-spec-1", DeviceID: "device-1", ExpectedEntityVersion: firstSnap.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: firstItem.ID, Action: domain.ActionRequestChanges,
			ItemVersion: firstItem.ItemVersion, ArtifactDigests: firstItem.ArtifactDigests, Message: firstComment,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	secondID := elaborationInvocationID("elaboration-run", 2)
	if err := elaboratefake.Script(driver, secondID, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
		Summary: "First revision.", Body: "# Specification\n\nLimit the request body to 1 MiB.",
		Addressals: []elaborate.Addressal{{Comment: firstComment, Response: "Added an explicit 1 MiB bound."}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	secondItem, secondSnap := f.item(t, "spec-approval-implementation-run-2")
	secondComment := "Also cap the aggregate response size."
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "revise-spec-2", DeviceID: "device-1", ExpectedEntityVersion: secondSnap.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: secondItem.ID, Action: domain.ActionRequestChanges,
			ItemVersion: secondItem.ItemVersion, ArtifactDigests: secondItem.ArtifactDigests, Message: secondComment,
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Enqueues iteration 3. Before the fix its inputs still carried the
	// iteration-1 specification, so the dispatch below returned
	// ErrParentKeyMismatch and stalled the whole reconcile.
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	thirdID := elaborationInvocationID("elaboration-run", 3)
	if err := elaboratefake.Script(driver, thirdID, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
		Summary: "Second revision.", Body: "# Specification\n\nCap both single and aggregate sizes.",
		Addressals: []elaborate.Addressal{{Comment: secondComment, Response: "Bounded the aggregate response size."}},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile dispatching the second revision: %v", err)
	}

	thirdItem, _ := f.item(t, "spec-approval-implementation-run-3")
	if !strings.Contains(thirdItem.Reason, secondComment) ||
		!strings.Contains(thirdItem.Reason, "Bounded the aggregate response size.") {
		t.Fatalf("second revision reason omitted review history:\n%s", thirdItem.Reason)
	}
	if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("implementation run after second request_changes = %v", err)
	}
}

func TestElaborationClocksCancelActiveWorkAndConsolidateWaiting(t *testing.T) {
	t.Run("stage active time", func(t *testing.T) {
		f := newElaborationFixture(t, true, 2)
		driver := f.newDriver(t)
		id := elaborationInvocationID("elaboration-run", 1)
		if err := elaboratefake.Script(driver, id, 0, 10, elaborate.Output{Specification: &elaborate.Specification{
			Summary: "late", Body: "# Late specification", Addressals: []elaborate.Addressal{},
		}}); err != nil {
			t.Fatal(err)
		}
		f.submit(t)
		engine := f.newEngine(t, driver)
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		*f.now = f.now.Add(2 * time.Minute)
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		item, _ := f.item(t, domain.ItemID("execution-failure-"+string(id)))
		if item.Status != domain.StatusOpen || !strings.Contains(item.Reason, "canceled") {
			t.Fatalf("timeout item = %+v", item)
		}
	})

	t.Run("approval waiting", func(t *testing.T) {
		f := newElaborationFixture(t, true, 2)
		driver := f.newDriver(t)
		id := elaborationInvocationID("elaboration-run", 1)
		if err := elaboratefake.Script(driver, id, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
			Summary: "ready", Body: "# Specification\n\nReady for approval.", Addressals: []elaborate.Addressal{},
		}}); err != nil {
			t.Fatal(err)
		}
		f.submit(t)
		engine := f.newEngine(t, driver)
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		*f.now = f.now.Add(2 * time.Minute)
		result, err := engine.Reconcile(t.Context())
		if err != nil || result.BlockedItemsCreated != 1 {
			t.Fatalf("waiting reconcile = %+v, %v", result, err)
		}
		if replay, err := engine.Reconcile(t.Context()); err != nil || replay.BlockedItemsCreated != 0 {
			t.Fatalf("waiting replay = %+v, %v", replay, err)
		}
		item, _ := f.item(t, "blocked-spec-approval-implementation-run-1")
		if item.Type != domain.AttentionBlocked || item.Status != domain.StatusOpen {
			t.Fatalf("blocked item = %+v", item)
		}
		approval, snapshot := f.item(t, "spec-approval-implementation-run-1")
		if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
			CommandID: "approve-waiting-spec", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
			Payload: signet.DecisionPayload{
				ItemID: approval.ID, Action: domain.ActionApprove,
				ItemVersion: approval.ItemVersion, ArtifactDigests: approval.ArtifactDigests,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		item, _ = f.item(t, item.ID)
		if item.Status != domain.StatusSuperseded {
			t.Fatalf("cleared blocked item = %+v", item)
		}
	})
}

func TestElaborationFailureAndGatePolicies(t *testing.T) {
	t.Run("stop concludes without implementation", func(t *testing.T) {
		f := newElaborationFixture(t, true, 2)
		driver := f.newDriver(t)
		id := elaborationInvocationID("elaboration-run", 1)
		if err := elaboratefake.Script(driver, id, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
			Summary: "ready", Body: "# Specification\n\nDo not start after stop.", Addressals: []elaborate.Addressal{},
		}}); err != nil {
			t.Fatal(err)
		}
		f.submit(t)
		engine := f.newEngine(t, driver)
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		item, snapshot := f.item(t, "spec-approval-implementation-run-1")
		if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
			CommandID: "stop-spec", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
			Payload: signet.DecisionPayload{
				ItemID: item.ID, Action: domain.ActionStop,
				ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("implementation run after stop = %v", err)
		}
	})

	t.Run("approval disabled", func(t *testing.T) {
		f := newElaborationFixture(t, false, 2)
		driver := f.newDriver(t)
		id := elaborationInvocationID("elaboration-run", 1)
		if err := elaboratefake.Script(driver, id, 0, 0, elaborate.Output{Specification: &elaborate.Specification{
			Summary: "ready", Body: "# Specification\n\nGate-free fixture.", Addressals: []elaborate.Addressal{},
		}}); err != nil {
			t.Fatal(err)
		}
		f.submit(t)
		engine := f.newEngine(t, driver)
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.run("implementation-run"); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
			_, err := tx.GetAttentionItem(t.Context(), "spec-approval-implementation-run-1")
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("approval item with gate disabled = %v", err)
		}
	})

	t.Run("iteration budget", func(t *testing.T) {
		f := newElaborationFixture(t, true, 1)
		driver := f.newDriver(t)
		id := elaborationInvocationID("elaboration-run", 1)
		if err := elaboratefake.Script(driver, id, 0, 0, elaborate.Output{FetchRequests: []elaborate.FetchRequest{{
			URL: "https://docs.example/more", Purpose: "request an over-budget iteration",
		}}}); err != nil {
			t.Fatal(err)
		}
		f.submit(t)
		engine := f.newEngine(t, driver)
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		item, _ := f.item(t, domain.ItemID("execution-failure-"+string(id)))
		if !strings.Contains(item.Reason, ErrElaborationIterationsExhausted.Error()) || f.fetchCalls.Load() != 0 {
			t.Fatalf("iteration failure = %+v, fetches = %d", item, f.fetchCalls.Load())
		}
	})

	t.Run("malformed output", func(t *testing.T) {
		f := newElaborationFixture(t, true, 2)
		driver := f.newDriver(t)
		id := elaborationInvocationID("elaboration-run", 1)
		driver.Script(id, execfake.StageScript{
			Outcome: execfake.OutcomeComplete,
			Result:  exec.StageResult{Artifacts: []domain.Digest{}, Summary: "Elaborator returned structured output."},
			Transcript: []byte(
				`{"type":"result","subtype":"success","is_error":false,"result":"{}"}` + "\n",
			),
		})
		f.submit(t)
		engine := f.newEngine(t, driver)
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		item, _ := f.item(t, domain.ItemID("execution-failure-"+string(id)))
		if !strings.Contains(item.Reason, elaborate.ErrInvalidOutput.Error()) {
			t.Fatalf("malformed-output failure = %+v", item)
		}
	})

	t.Run("lost invocation", func(t *testing.T) {
		f := newElaborationFixture(t, true, 2)
		driver := f.newDriver(t)
		id := elaborationInvocationID("elaboration-run", 1)
		driver.Script(id, execfake.StageScript{Outcome: execfake.OutcomeCrashBeforeResult})
		f.submit(t)
		engine := f.newEngine(t, driver)
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		item, _ := f.item(t, domain.ItemID("execution-failure-"+string(id)))
		if !strings.Contains(item.Reason, ErrInvocationLost.Error()) {
			t.Fatalf("lost-invocation failure = %+v", item)
		}
	})
}

func TestElaborationDecisionCommandsRejectDiscussion(t *testing.T) {
	commands := []domain.Command{
		{CommandID: "discuss", Action: domain.ActionDiscuss},
		{CommandID: "approve", Action: domain.ActionApprove},
	}
	if _, err := elaborationDecisionCommands(commands); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("discussion command error = %v, want ErrParentKeyMismatch", err)
	}
}

func TestElaborationReadsPersistedProductionTranscript(t *testing.T) {
	f := newElaborationFixture(t, true, 2)
	driver := f.newDriver(t)
	id := elaborationInvocationID("elaboration-run", 1)
	transcript, err := elaborate.EncodeTranscript(elaborate.Output{Specification: &elaborate.Specification{
		Summary: "Persisted output.", Body: "# Specification\n\nUse the transcript artifact.",
		Addressals: []elaborate.Addressal{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	digest := domain.Digest(contentaddr.Sum(transcript))
	if _, err := f.blobs.Put(digest, strings.NewReader(string(transcript))); err != nil {
		t.Fatal(err)
	}
	driver.Script(id, execfake.StageScript{
		Outcome: execfake.OutcomeComplete,
		Result: exec.StageResult{
			Artifacts: []domain.Digest{digest}, Summary: "Imported candidate deadbeef over base cafebabe.",
		},
	})
	f.submit(t)
	if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	item, _ := f.item(t, "spec-approval-implementation-run-1")
	if item.AgentClaims[0].Text == nil || item.AgentClaims[0].Text.Content != "# Specification\n\nUse the transcript artifact." {
		t.Fatalf("specification claim = %+v", item.AgentClaims)
	}
}

func TestElaborationResearchRefusalBecomesDurableFailure(t *testing.T) {
	f := newElaborationFixture(t, true, 2)
	driver := f.newDriver(t)
	id := elaborationInvocationID("elaboration-run", 1)
	if err := elaboratefake.Script(driver, id, 0, 0, elaborate.Output{
		FetchRequests: []elaborate.FetchRequest{{
			URL: "https://outside.example/research", Purpose: "leave the configured origin",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	item, _ := f.item(t, domain.ItemID("execution-failure-"+string(id)))
	if !strings.Contains(item.Reason, elaborate.ErrResearchURLRefused.Error()) {
		t.Fatalf("research refusal item = %+v", item)
	}
	if replay, err := engine.Reconcile(t.Context()); err != nil || replay.ResultsAccepted != 0 {
		t.Fatalf("research refusal replay = %+v, %v", replay, err)
	}
	if f.fetchCalls.Load() != 0 {
		t.Fatalf("off-allowlist transport calls = %d, want 0", f.fetchCalls.Load())
	}
}

func TestElaborationBackupPayloadDigests(t *testing.T) {
	f := newElaborationFixture(t, true, 2)
	f.submit(t)
	var invocation, claim store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		invocation, err = tx.GetOutbox(t.Context(), string(elaborationInvocationID("elaboration-run", 1)))
		if err != nil {
			return err
		}
		claim, err = tx.GetOutbox(t.Context(), elaborationImplementationClaimKey("implementation-run"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := ElaborationInvocationBackupPayloadDigests(invocation); err != nil || len(got) != 0 {
		t.Fatalf("invocation backup digests = %v, %v", got, err)
	}
	if got, err := ElaborationImplementationClaimBackupPayloadDigests(claim); err != nil || len(got) != 0 {
		t.Fatalf("claim backup digests = %v, %v", got, err)
	}
	claim.Status = "pending"
	if _, err := ElaborationImplementationClaimBackupPayloadDigests(claim); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("pending claim error = %v, want ErrParentKeyMismatch", err)
	}
	invocation.IdempotencyKey = "wrong"
	if _, err := ElaborationInvocationBackupPayloadDigests(invocation); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("retargeted invocation error = %v, want ErrParentKeyMismatch", err)
	}
}

func TestSubmitElaborationRunClaimsFutureImplementationIdentity(t *testing.T) {
	f := newElaborationFixture(t, true, 2)
	f.submit(t)
	otherPolicy, err := domain.NewResolvedPolicy("other-elaboration-run", f.policy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	err = SubmitElaborationRun(t.Context(), f.store, ElaborationRunSpec{
		ElaborationRunID: "other-elaboration-run", ImplementationRunID: "implementation-run",
		ProjectID: "project-1", SourceArtifactID: f.source.ID, PolicyArtifactID: f.policyArt.ID,
		ResolvedPolicy: otherPolicy,
		Publication: ProductionPublication{
			Title: "Implement approved work item", Body: "Implements the operator-approved specification.",
			CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
		},
	})
	if !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("competing implementation claim = %v", err)
	}
	if _, err := f.run("other-elaboration-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("competing elaboration run persisted = %v", err)
	}
}

func TestElaborationReconstructionRejectsChangedRootAndTerminal(t *testing.T) {
	f := newElaborationFixture(t, true, 3)
	f.submit(t)

	var root elaborationRequest
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(t.Context(), string(elaborationInvocationID("elaboration-run", 1)))
		if err != nil {
			return err
		}
		root, err = decodeElaborationRequest(entry)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	changed := root
	changed.Publication.Title = "Retargeted publication"
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		return authenticateElaborationRoot(t.Context(), tx, changed)
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("changed root error = %v, want ErrParentKeyMismatch", err)
	}

	forged, err := encodeElaborationTerminal(elaborationTerminal{
		InvocationID: root.InvocationID, Iteration: root.Iteration,
		Status: exec.StatusCanceled, ResearchArtifactIDs: []domain.ArtifactID{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		_, _, err := tx.RecordInbox(t.Context(), string(root.InvocationID), kindElaborationTerminal, forged)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	engine := f.newEngine(t, f.newDriver(t))
	run, err := f.run("elaboration-run")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.recordElaborationFailure(
		t.Context(), run, root, exec.StatusFailed, "expected failure",
	); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("changed terminal error = %v, want ErrImmutableTransition", err)
	}
}

// TestLoadElaborationBindingRejectsForeignInputProducer guards #685 (Codex
// round 6): the dispatch reconstruction must re-bind every accumulated input
// to this run's own prior elaboration invocations. A retargeted request that
// appends another run's research artifact — well-typed and daemon-produced,
// so it passes the type and feedback-provenance checks — is rejected because
// its producer is not one of this run's invocations, before its bytes could
// reach the elaborator or the approved specification.
func TestLoadElaborationBindingRejectsForeignInputProducer(t *testing.T) {
	f := newElaborationFixture(t, true, 4)
	f.submit(t)

	var root elaborationRequest
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(t.Context(), string(elaborationInvocationID("elaboration-run", 1)))
		if err != nil {
			return err
		}
		root, err = decodeElaborationRequest(entry)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	foreign, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "research-foreign-1", Type: domain.ArtifactKindResearch,
		Digest: domain.Digest(contentaddr.Sum([]byte("foreign research"))),
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerDaemon,
			ProducerInvocationID: elaborationInvocationID("other-run", 1),
			HeadBinding:          domain.HeadIndependent,
			SensitivityClass:     domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	next := root
	next.Iteration = 2
	next.InvocationID = elaborationInvocationID("elaboration-run", 2)
	next.InputArtifactIDs = append(slices.Clone(root.InputArtifactIDs), foreign.ID)
	payload, err := encodeElaborationRequest(next)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := domain.NewAgentInvocation(next.InvocationID, next.InputArtifactIDs, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(t.Context(), foreign); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(t.Context(), invocation); err != nil {
			return err
		}
		_, _, err := tx.EnqueueOutbox(t.Context(), string(next.InvocationID), KindElaborationInvocationRequested, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var entry store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), string(next.InvocationID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	engine := f.newEngine(t, f.newDriver(t))
	_, _, err = engine.loadElaborationBinding(t.Context(), entry)
	if !errors.Is(err, domain.ErrParentKeyMismatch) || !strings.Contains(err.Error(), "is not a prior invocation") {
		t.Fatalf("foreign input producer error = %v, want producer-binding ErrParentKeyMismatch", err)
	}
}

// TestEncodeElaborationRequestRejectsOversizedPayload guards #685 (Codex
// round 7): the encoder enforces the decoder's aggregate byte limit, so an
// oversized but otherwise-valid request fails fast at submission instead of
// persisting a durable row that dispatch can never decode and that halts
// reconciliation for every run.
func TestEncodeElaborationRequestRejectsOversizedPayload(t *testing.T) {
	inputs := make([]domain.ArtifactID, 30000)
	for i := range inputs {
		inputs[i] = domain.ArtifactID("elaboration-input-padding-artifact-" + strconv.Itoa(i))
	}
	request := elaborationRequest{
		Version:             elaborationRequestVersion,
		ElaborationRunID:    "elaboration-run",
		ImplementationRunID: "implementation-run",
		ProjectID:           "project-1",
		InvocationID:        elaborationInvocationID("elaboration-run", 1),
		Iteration:           1,
		InputArtifactIDs:    inputs,
		PolicyArtifactID:    "policy-1",
		Publication: ProductionPublication{
			Title: "Implement approved work item", Body: "Implements the operator-approved specification.",
			CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
		},
	}
	if err := request.validate(); err != nil {
		t.Fatalf("padding request is not valid before the size check: %v", err)
	}
	if _, err := encodeElaborationRequest(request); !errors.Is(err, domain.ErrClaimTextTooLarge) {
		t.Fatalf("oversized request error = %v, want ErrClaimTextTooLarge", err)
	}
}

// TestLoadElaborationBindingRejectsOversizedIteration guards #685 (Codex
// round 8): the dispatch reconstruction bounds the decoded iteration by the
// resolved policy maximum before using it as an allocation capacity and loop
// count, so a canonical but retargeted request with a huge iteration fails
// closed instead of forcing an unbounded allocation.
func TestLoadElaborationBindingRejectsOversizedIteration(t *testing.T) {
	f := newElaborationFixture(t, true, 4)
	f.submit(t)

	var root elaborationRequest
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(t.Context(), string(elaborationInvocationID("elaboration-run", 1)))
		if err != nil {
			return err
		}
		root, err = decodeElaborationRequest(entry)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	next := root
	next.Iteration = 1 << 20
	next.InvocationID = elaborationInvocationID("elaboration-run", next.Iteration)
	payload, err := encodeElaborationRequest(next)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := domain.NewAgentInvocation(next.InvocationID, next.InputArtifactIDs, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutAgentInvocation(t.Context(), invocation); err != nil {
			return err
		}
		_, _, err := tx.EnqueueOutbox(t.Context(), string(next.InvocationID), KindElaborationInvocationRequested, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var entry store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), string(next.InvocationID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	engine := f.newEngine(t, f.newDriver(t))
	_, _, err = engine.loadElaborationBinding(t.Context(), entry)
	if !errors.Is(err, domain.ErrParentKeyMismatch) || !strings.Contains(err.Error(), "exceeds the policy maximum") {
		t.Fatalf("oversized iteration error = %v, want policy-maximum ErrParentKeyMismatch", err)
	}
}

func (f elaborationFixture) artifact(t *testing.T, id domain.ArtifactID) domain.Artifact {
	t.Helper()
	var artifact domain.Artifact
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		artifact, err = tx.GetArtifact(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return artifact
}

func (f elaborationFixture) item(t *testing.T, id domain.ItemID) (domain.AttentionItem, store.Snapshot) {
	t.Helper()
	var item domain.AttentionItem
	var snapshot store.Snapshot
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, snapshot, err = tx.GetAttentionItemSnapshot(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item, snapshot
}

func (f elaborationFixture) run(id domain.RunID) (domain.Run, error) {
	var run domain.Run
	err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(context.Background(), id)
		return err
	})
	return run, err
}
