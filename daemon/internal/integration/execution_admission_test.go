package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var (
	admittedAt   = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	agentImage   = domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32))
	testIdentity = domain.AuthIdentity{
		ID: "auth-claude-owner", Provider: "claude", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
	}
)

func admissionEnvironment() engine.AdmissionEnvironment {
	identity := testIdentity.ID
	return engine.AdmissionEnvironment{
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       agentImage,
		PromptPackageDigest: domain.Digest(
			"sha256:037aa38647518d5b7d034a92109df888dda8247b1772d509e7c4d77b517ddacd"),
		Base: domain.BaseRevision{
			Repo: "freeside-ai/candidate-repo", RepositoryID: 424242,
			BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
		},
		Workspace:      "freeside-handoff-run-235-ws",
		AuthIdentityID: &identity,
	}
}

// openAdmittingFixture is openWorkflowFixture with a runner backend to admit
// against: the store carries the policy floor it re-gates recorded admissions
// with, and the engine carries the backend and the floor it admits under.
func openAdmittingFixture(
	t *testing.T,
	declared []exec.Capability,
	engineFloor []exec.Capability,
	storeFloor domain.CapabilitySnapshot,
) *workflowFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "freeside.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: storeFloor,
		},
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, testIdentity, admittedAt)
	}); err != nil {
		t.Fatalf("record auth identity: %v", err)
	}
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatalf("signet.NewBlobStore: %v", err)
	}
	attention := signet.NewService(st, signet.WithBlobStore(blobs))
	driver, err := fake.NewStageDriverAt(filepath.Join(root, "driver"))
	if err != nil {
		t.Fatalf("fake.NewStageDriverAt: %v", err)
	}
	backend := fake.RunnerBackend{
		BackendName: "fake_runner", Caps: exec.NewCapabilitySet(declared...),
	}
	workflow, err := engine.New(st, attention, driver,
		engine.WithAdmission(backend, engineFloor, admissionEnvironment(), func() time.Time { return admittedAt }))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return &workflowFixture{root: root, store: st, signet: attention, driver: driver, engine: workflow}
}

// dispatchOneInvocation drives the fixture to the point where exactly one
// agent invocation has been dispatched, and returns its id.
func dispatchOneInvocation(t *testing.T, f *workflowFixture) (domain.InvocationID, engine.ReconcileResult, error) {
	t.Helper()
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := domain.InvocationID("inv-discuss-235")
	f.scriptCompletion(invocationID, fake.OutcomeComplete)
	if got := f.discuss(t, feedback); got != invocationID {
		t.Fatalf("discussion invocation = %q, want %q", got, invocationID)
	}
	result, err := f.engine.Reconcile(context.Background())
	return invocationID, result, err
}

// TestAdmissionSnapshotPersistsWithTheAttempt closes #39's deferred acceptance:
// the snapshot exec.CheckCapabilities produced at spawn is durable, bound to
// the attempt it admitted, and carries the environment the stage ran under.
func TestAdmissionSnapshotPersistsWithTheAttempt(t *testing.T) {
	ctx := context.Background()
	declared := []exec.Capability{
		exec.CapDetachableWorkspace, exec.CapPostExitExport, exec.CapReadOnlyRemount,
	}
	floor := []exec.Capability{exec.CapPostExitExport}
	f := openAdmittingFixture(t, declared, floor, domain.NewCapabilitySnapshot(floor...))

	invocationID, result, err := dispatchOneInvocation(t, f)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.InvocationsStarted != 1 {
		t.Fatalf("Reconcile result = %#v, want one start", result)
	}

	var admission domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmission(ctx, invocationID)
		return err
	}); err != nil {
		t.Fatalf("GetExecutionAdmission: %v", err)
	}

	// The recorded class is exactly what the gate returned, not the floor and
	// not the backend's live map.
	want := exec.NewCapabilitySet(declared...).Snapshot()
	if !admission.Capabilities.Has(exec.CapReadOnlyRemount) || len(admission.Capabilities) != len(want) {
		t.Errorf("recorded capabilities = %v, want %v", admission.Capabilities, want)
	}
	if admission.Backend != "fake_runner" {
		t.Errorf("recorded backend = %q, want fake_runner", admission.Backend)
	}
	if admission.AttemptID != "attempt-"+domain.AttemptID(invocationID) {
		t.Errorf("recorded attempt = %q, want the dispatched attempt", admission.AttemptID)
	}
	env := admissionEnvironment()
	if admission.ImageRef != env.ImageRef || admission.Base != env.Base ||
		admission.CredentialMode != env.CredentialMode || admission.EgressProfile != env.EgressProfile {
		t.Errorf("recorded environment = %+v, want %+v", admission, env)
	}
	if admission.AuthIdentityID == nil || *admission.AuthIdentityID != testIdentity.ID {
		t.Errorf("recorded auth identity = %v, want %q", admission.AuthIdentityID, testIdentity.ID)
	}
	if admission.StageInputs == nil {
		t.Fatal("recorded admission has no materializable stage inputs")
	}
	if admission.StageInputs.PromptPackageDigest != env.PromptPackageDigest ||
		admission.StageInputs.SpecificationDigest != admission.SpecDigest ||
		admission.StageInputs.PolicyDigest != admission.PolicyDigest ||
		admission.StageInputs.InputDigest != admission.InputDigest {
		t.Errorf("recorded stage inputs = %+v, want the admission's frozen input roles",
			admission.StageInputs)
	}
	if admission.StageInputs.ConversationDigest == nil {
		t.Fatal("conversation-bound admission has no materialized conversation prefix")
	}
	blobs, err := signet.NewBlobStore(filepath.Join(f.root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := blobs.Open(*admission.StageInputs.ConversationDigest)
	if err != nil {
		t.Fatalf("open admitted conversation prefix: %v", err)
	}
	if err := prefix.Close(); err != nil {
		t.Fatalf("close admitted conversation prefix: %v", err)
	}

	// The attempt the record claims is the attempt the run carries.
	run, err := f.signet.GetRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	attempt := run.Run.Stages[0].Attempts[0]
	if attempt.InvocationID != invocationID || attempt.ID != admission.AttemptID {
		t.Fatalf("attempt %+v disagrees with admission %q/%q",
			attempt, admission.InvocationID, admission.AttemptID)
	}

	// The driver was started under the spec the record authorizes.
	spec, ok := f.driver.StartSpec(invocationID)
	if !ok {
		t.Fatal("driver recorded no start spec")
	}
	if !reflect.DeepEqual(spec, exec.StartSpecFromAdmission(admission)) {
		t.Fatalf("start spec = %+v, want the admission's spec %+v", spec, exec.StartSpecFromAdmission(admission))
	}
}

// TestAdmissionConfigurationIsDetached proves the configured environment and
// floor cannot be edited out from under the engine after engine.New: a caller
// reusing its slice or pointers must not be able to weaken the gate or
// retarget the credential and waiver bindings a record attests to.
func TestAdmissionConfigurationIsDetached(t *testing.T) {
	ctx := context.Background()
	floor := []exec.Capability{exec.CapPostExitExport}
	identity := testIdentity.ID
	env := admissionEnvironment()
	env.AuthIdentityID = &identity

	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "freeside.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(floor...),
		},
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, testIdentity, admittedAt)
	}); err != nil {
		t.Fatalf("record auth identity: %v", err)
	}
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatalf("signet.NewBlobStore: %v", err)
	}
	attention := signet.NewService(st, signet.WithBlobStore(blobs))
	driver, err := fake.NewStageDriverAt(filepath.Join(root, "driver"))
	if err != nil {
		t.Fatalf("fake.NewStageDriverAt: %v", err)
	}
	workflow, err := engine.New(st, attention, driver,
		engine.WithAdmission(
			fake.RunnerBackend{BackendName: "fake_runner", Caps: exec.NewCapabilitySet(floor...)},
			floor, env, func() time.Time { return admittedAt }))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	// The caller reuses everything it passed in.
	floor[0] = exec.CapNetworklessExport
	identity = "auth-somebody-else"
	env.Workspace = "elsewhere"

	f := &workflowFixture{root: root, store: st, signet: attention, driver: driver, engine: workflow}
	invocationID, _, err := dispatchOneInvocation(t, f)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var admission domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmission(ctx, invocationID)
		return err
	}); err != nil {
		t.Fatalf("GetExecutionAdmission: %v", err)
	}
	if admission.AuthIdentityID == nil || *admission.AuthIdentityID != testIdentity.ID {
		t.Errorf("recorded auth identity = %v, want the configured %q", admission.AuthIdentityID, testIdentity.ID)
	}
	if admission.Workspace != admissionEnvironment().Workspace {
		t.Errorf("recorded workspace = %q, want the configured %q",
			admission.Workspace, admissionEnvironment().Workspace)
	}
}

// TestDispatchReplayReusesThePersistedAdmission is the crash-between-commit-
// and-start case: the attempt and its admission are durable, the outbox row is
// still pending, and the next pass builds a fresh admission whose instant (and
// therefore identity) has moved. Starting under that fresh id would hand the
// driver an admission no reader can reconstruct, so the replay must start
// under the record that is actually stored.
func TestDispatchReplayReusesThePersistedAdmission(t *testing.T) {
	ctx := context.Background()
	floor := []exec.Capability{exec.CapPostExitExport}
	f := openAdmittingFixture(t,
		[]exec.Capability{exec.CapPostExitExport}, floor, domain.NewCapabilitySnapshot(floor...))

	// The first pass commits the attempt and its admission, then fails to
	// start (the invocation is unscripted), leaving the outbox row pending:
	// the same durable state a kill between the commit and Start leaves.
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := f.discuss(t, feedback)
	if _, err := f.engine.Reconcile(ctx); err == nil {
		t.Fatal("first pass should fail to start an unscripted invocation")
	}
	if _, started := f.driver.StartSpec(invocationID); started {
		t.Fatal("unscripted start should not have recorded an intent")
	}

	var stored domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetExecutionAdmission(ctx, invocationID)
		return err
	}); err != nil {
		t.Fatalf("the first pass must have committed an admission: %v", err)
	}

	// The daemon restarts later, so its clock has moved: a freshly built
	// admission would carry a different instant and a different identity.
	later := admittedAt.Add(3 * time.Hour)
	replayed, err := engine.New(f.store, f.signet, f.driver,
		engine.WithAdmission(
			fake.RunnerBackend{BackendName: "fake_runner", Caps: exec.NewCapabilitySet(exec.CapPostExitExport)},
			floor, admissionEnvironment(), func() time.Time { return later }))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	f.scriptCompletion(invocationID, fake.OutcomeComplete)
	if _, err := replayed.Reconcile(ctx); err != nil {
		t.Fatalf("replay Reconcile: %v", err)
	}

	spec, ok := f.driver.StartSpec(invocationID)
	if !ok {
		t.Fatal("replay did not start the driver")
	}
	if spec.AdmissionID != stored.ID {
		t.Fatalf("started under admission %q, stored admission is %q", spec.AdmissionID, stored.ID)
	}
	var count int
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		listed, err := tx.ListRunExecutionAdmissions(ctx, testRunID)
		count = len(listed)
		return err
	}); err != nil {
		t.Fatalf("ListRunExecutionAdmissions: %v", err)
	}
	if count != 1 {
		t.Fatalf("run carries %d admissions, want exactly one", count)
	}
}

// TestReplayWithoutAdmissionConfigStillUsesTheRecord covers the restart that
// has lost its admission configuration between the attempt/admission commit
// and Start. The attempt was admitted, so it stays admitted: an engine with no
// admitter must still start it under the stored record rather than downgrading
// it to an unbound start that omits the image, base, credentials, and egress
// profile it was admitted with.
func TestReplayWithoutAdmissionConfigStillUsesTheRecord(t *testing.T) {
	ctx := context.Background()
	floor := []exec.Capability{exec.CapPostExitExport}
	f := openAdmittingFixture(t,
		[]exec.Capability{exec.CapPostExitExport}, floor, domain.NewCapabilitySnapshot(floor...))

	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := f.discuss(t, feedback)
	if _, err := f.engine.Reconcile(ctx); err == nil {
		t.Fatal("first pass should fail to start an unscripted invocation")
	}

	var stored domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetExecutionAdmission(ctx, invocationID)
		return err
	}); err != nil {
		t.Fatalf("the first pass must have committed an admission: %v", err)
	}

	// The daemon comes back with no admitter configured at all.
	unconfigured, err := engine.New(f.store, f.signet, f.driver)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	f.scriptCompletion(invocationID, fake.OutcomeComplete)
	if _, err := unconfigured.Reconcile(ctx); err != nil {
		t.Fatalf("replay Reconcile: %v", err)
	}

	spec, ok := f.driver.StartSpec(invocationID)
	if !ok {
		t.Fatal("replay did not start the driver")
	}
	if !reflect.DeepEqual(spec, exec.StartSpecFromAdmission(stored)) {
		t.Fatalf("started under %+v, want the stored admission's spec %+v",
			spec, exec.StartSpecFromAdmission(stored))
	}
}

// TestReplayUnderADegradedBackendUsesTheRecord is the third shape of the same
// rule: a recorded attempt is governed by its record, not by what this process
// could admit now. A restart whose backend has lost a capability the engine
// floor demands must still replay an already-admitted attempt from its durable
// bindings, rather than refusing it forever because a fresh admission of the
// same work would fail.
func TestReplayUnderADegradedBackendUsesTheRecord(t *testing.T) {
	ctx := context.Background()
	floor := []exec.Capability{exec.CapPostExitExport, exec.CapDetachableWorkspace}
	f := openAdmittingFixture(t,
		[]exec.Capability{exec.CapPostExitExport, exec.CapDetachableWorkspace},
		floor, domain.NewCapabilitySnapshot(exec.CapPostExitExport))

	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := f.discuss(t, feedback)
	if _, err := f.engine.Reconcile(ctx); err == nil {
		t.Fatal("first pass should fail to start an unscripted invocation")
	}

	var stored domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetExecutionAdmission(ctx, invocationID)
		return err
	}); err != nil {
		t.Fatalf("the first pass must have committed an admission: %v", err)
	}

	// The backend comes back declaring less than the engine floor requires, so
	// a fresh admission would be refused outright.
	degraded, err := engine.New(f.store, f.signet, f.driver,
		engine.WithAdmission(
			fake.RunnerBackend{BackendName: "fake_runner", Caps: exec.NewCapabilitySet(exec.CapPostExitExport)},
			floor, admissionEnvironment(), func() time.Time { return admittedAt.Add(time.Hour) }))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	f.scriptCompletion(invocationID, fake.OutcomeComplete)
	if _, err := degraded.Reconcile(ctx); err != nil {
		t.Fatalf("replay under a degraded backend: %v", err)
	}

	spec, ok := f.driver.StartSpec(invocationID)
	if !ok {
		t.Fatal("replay did not start the driver")
	}
	if !reflect.DeepEqual(spec, exec.StartSpecFromAdmission(stored)) {
		t.Fatalf("started under %+v, want the stored admission's spec %+v",
			spec, exec.StartSpecFromAdmission(stored))
	}
}

// TestAdmissionAcceptsAnEmptyFloor covers the policy state where a mode
// requires no minimum capability: WithAdmission is what declares admission
// configured, so an empty floor is expressible here exactly as it is at the
// persistence boundary, which distinguishes a present-but-empty floor from a
// missing one.
func TestAdmissionAcceptsAnEmptyFloor(t *testing.T) {
	ctx := context.Background()
	f := openAdmittingFixture(t,
		[]exec.Capability{exec.CapPostExitExport}, nil, domain.CapabilitySnapshot{})

	invocationID, result, err := dispatchOneInvocation(t, f)
	if err != nil {
		t.Fatalf("Reconcile under an empty floor: %v", err)
	}
	if result.InvocationsStarted != 1 {
		t.Fatalf("Reconcile result = %#v, want one start", result)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, invocationID)
		return err
	}); err != nil {
		t.Fatalf("an admission recorded under an empty floor must read back: %v", err)
	}
}

// TestAcceptanceRegatesTheAdmission covers the other end of the record's life:
// a floor raised while an attempt is in flight makes its class inadmissible,
// and accepting that output anyway would advance the workflow on work produced
// under an isolation class the operator now rejects.
func TestAcceptanceRegatesTheAdmission(t *testing.T) {
	ctx := context.Background()
	floor := []exec.Capability{exec.CapPostExitExport}
	f := openAdmittingFixture(t,
		[]exec.Capability{exec.CapPostExitExport}, floor, domain.NewCapabilitySnapshot(floor...))

	// Start the invocation but leave it unaccepted: the script spends an
	// inspect step before it commits anything, so the first pass starts and
	// the result lands on a later pass, which is where a policy change can
	// overtake an in-flight attempt.
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := f.discuss(t, feedback)
	f.driver.Script(invocationID, fake.StageScript{
		RunningInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "in flight when the floor moved"},
	})
	result, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if result.InvocationsStarted != 1 || result.ResultsAccepted != 0 {
		t.Fatalf("first pass = %#v, want one start and no acceptance", result)
	}

	// The operator raises the floor and the daemon restarts over the same
	// durable state.
	raised := reopenWithFloor(t, f, domain.NewCapabilitySnapshot(exec.CapNetworklessExport))
	blobs, err := signet.NewBlobStore(filepath.Join(f.root, "blobs"))
	if err != nil {
		t.Fatalf("signet.NewBlobStore: %v", err)
	}
	attention := signet.NewService(raised, signet.WithBlobStore(blobs))
	restarted, err := engine.New(raised, attention, f.driver)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	if _, err := restarted.Reconcile(ctx); !errors.Is(err, domain.ErrCapabilityBelowFloor) {
		t.Fatalf("acceptance under a raised floor = %v, want %v", err, domain.ErrCapabilityBelowFloor)
	}

	// The refusal is loud and durable, not a silent skip: the conversation
	// still holds only the user turn, so nothing advanced on it.
	item, err := attention.GetAttentionItem(ctx, domain.ItemID("feedback-"+string(testRunID)))
	if err != nil {
		t.Fatalf("get feedback: %v", err)
	}
	conversation, err := attention.GetConversation(ctx, *item.Item.ConversationID)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if len(conversation.Conversation.Messages) != 1 {
		t.Fatalf("conversation carries %d messages, want the user turn alone",
			len(conversation.Conversation.Messages))
	}
}

// TestWaivedAdmissionSurfacesItsPosture is §5.7's other half: a waived
// admission must not only record the waiver but surface the degraded posture,
// so an operator can see that unattended work is running on the temporary
// encryption exception. The notice lands in the admitting transaction, and a
// replay converges on the same item rather than raising a second one.
func TestWaivedAdmissionSurfacesItsPosture(t *testing.T) {
	ctx := context.Background()
	f := openWaivedUnattendedFixture(t)
	attention := f.signet
	invocationID, _, err := dispatchOneInvocation(t, f)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	notice, err := attention.GetAttentionItem(ctx,
		domain.ItemID("system-health-backup-waiver-"+string(invocationID)))
	if err != nil {
		t.Fatalf("a waived admission must surface its posture: %v", err)
	}
	if notice.Item.Type != domain.AttentionSystemHealth {
		t.Errorf("notice type = %q, want %q", notice.Item.Type, domain.AttentionSystemHealth)
	}
	if notice.Item.Status != domain.StatusOpen {
		t.Errorf("notice status = %q, want it open", notice.Item.Status)
	}
	if !strings.Contains(notice.Item.Reason, "424242") {
		t.Errorf("notice reason %q does not name the waived repository", notice.Item.Reason)
	}

	// A second pass replays the dispatch; the notice converges rather than
	// multiplying.
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("replay Reconcile: %v", err)
	}
	items, err := attention.ListAttentionItems(ctx)
	if err != nil {
		t.Fatalf("ListAttentionItems: %v", err)
	}
	health := 0
	for _, item := range items {
		if item.Item.Type == domain.AttentionSystemHealth {
			health++
		}
	}
	if health != 1 {
		t.Fatalf("system_health items = %d, want exactly one", health)
	}
}

// waivedTrustProfile is the approved profile a waived unattended admission is
// anchored to.
func waivedTrustProfile(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "freeside-ai/candidate-repo", RepositoryID: 424242,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	return profile
}

// TestAdmissionRefusalStartsNothing is §5.7's no-silent-downgrade rule at the
// dispatch boundary: a backend below the floor leaves no attempt, no record,
// and no started invocation, so the intent is still there for a pass under a
// backend that clears the floor.
func TestAdmissionRefusalStartsNothing(t *testing.T) {
	floor := []exec.Capability{exec.CapNetworklessExport}
	f := openAdmittingFixture(t,
		[]exec.Capability{exec.CapPostExitExport}, floor, domain.NewCapabilitySnapshot(floor...))

	invocationID, _, err := dispatchOneInvocation(t, f)
	if !errors.Is(err, exec.ErrCapabilityRefused) {
		t.Fatalf("Reconcile = %v, want %v", err, exec.ErrCapabilityRefused)
	}

	assertNoAttemptRecorded(t, f, invocationID)
	if _, started := f.driver.StartSpec(invocationID); started {
		t.Error("a refused dispatch started the driver")
	}
}

// TestAdmissionAndAttemptShareOneTransaction is the atomicity claim: the store
// re-gate rejecting the record must also roll back the attempt, or a crash-free
// refusal would still leave an attempt with no audited class. The engine floor
// is satisfied here and the store's is not, which is exactly the drift the two
// independent authorities exist to catch.
func TestAdmissionAndAttemptShareOneTransaction(t *testing.T) {
	f := openAdmittingFixture(t,
		[]exec.Capability{exec.CapPostExitExport},
		[]exec.Capability{exec.CapPostExitExport},
		domain.NewCapabilitySnapshot(exec.CapNetworklessExport))

	invocationID, _, err := dispatchOneInvocation(t, f)
	if !errors.Is(err, domain.ErrCapabilityBelowFloor) {
		t.Fatalf("Reconcile = %v, want %v", err, domain.ErrCapabilityBelowFloor)
	}
	assertNoAttemptRecorded(t, f, invocationID)
}

// reopenWithFloor reopens the fixture'"'"'s database under a different admission
// floor, as a restart after an operator policy change would.
func reopenWithFloor(t *testing.T, f *workflowFixture, floor domain.CapabilitySnapshot) *store.Store {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	f.store = nil
	reopened, err := store.Open(context.Background(), filepath.Join(f.root, "freeside.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{domain.ModeAttendedDev: floor},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func assertNoAttemptRecorded(t *testing.T, f *workflowFixture, invocationID domain.InvocationID) {
	t.Helper()
	ctx := context.Background()
	run, err := f.signet.GetRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	for _, stage := range run.Run.Stages {
		if len(stage.Attempts) != 0 {
			t.Fatalf("stage %q recorded %d attempts, want none", stage.ID, len(stage.Attempts))
		}
	}
	err = f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, invocationID)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetExecutionAdmission = %v, want %v", err, store.ErrNotFound)
	}
}
