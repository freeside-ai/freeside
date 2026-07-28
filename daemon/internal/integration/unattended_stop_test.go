package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// openWaivedUnattendedFixture is the §5.7 Phase 1A.2 test bed: an unattended
// engine admitting under the backup-encryption waiver, against a store whose
// operator configuration holds that waiver, with healthy local backup
// evidence and the approved trust profile the waiver is anchored to.
func openWaivedUnattendedFixture(t *testing.T) *workflowFixture {
	t.Helper()
	ctx := context.Background()
	floor := []exec.Capability{exec.CapPostExitExport}
	waiverRepository := int64(424242)
	profile := waivedTrustProfile(t)

	env := admissionEnvironment()
	env.OperatingMode = domain.ModeUnattended
	env.Base.Repo, env.Base.RepositoryID = profile.Repo, profile.RepositoryID
	env.BackupEncryptionWaiver = &domain.BackupEncryptionWaiver{
		RepositoryID: waiverRepository, Reason: "phase 1a.2 supervised runs",
	}

	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "freeside.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(floor...),
		},
		ApprovedCredentialModes:            []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupEncryptionWaiverRepositoryID: &waiverRepository,
		BackupHealthSource: store.BackupHealthSourceFunc(func(
			context.Context, store.BackupHealthContext,
		) (domain.BackupHealth, error) {
			return domain.BackupHealth{
				CheckpointCurrency: domain.BackupHealthHealthy,
				ArtifactClosure:    domain.BackupHealthHealthy,
				RestoreTestAge:     domain.BackupHealthHealthy,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordAuthIdentity(ctx, testIdentity, admittedAt); err != nil {
			return err
		}
		return tx.RecordTrustProfile(ctx, profile, admittedAt)
	}); err != nil {
		t.Fatalf("seed identity and profile: %v", err)
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
			fake.RunnerBackend{
				BackendName: "fake_runner",
				Caps:        exec.NewCapabilitySet(domain.AllRunnerCapabilities...),
			},
			floor, env, func() time.Time { return admittedAt }))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return &workflowFixture{root: root, store: st, signet: attention, driver: driver, engine: workflow}
}

// stopOperations seeds a decision carrier and accepts stop_unattended on it
// through signet — the production path, and since the store refuses an
// unbacked transition, the only way to record one.
func stopOperations(t *testing.T, f *workflowFixture, commandID string) {
	t.Helper()
	ctx := context.Background()
	carrier, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID("health-carrier-" + commandID), ProjectID: "proj-235",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "operating-state decision carrier",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge, domain.ActionStopUnattended},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	if err := f.signet.PutItem(ctx, carrier); err != nil {
		t.Fatalf("seed carrier: %v", err)
	}
	submitOn(t, f, carrier.ID, commandID, domain.ActionStopUnattended)
}

// resumeOperations accepts resume_unattended on the open stopped notice.
func resumeOperations(t *testing.T, f *workflowFixture, commandID string) {
	t.Helper()
	ctx := context.Background()
	var noticeID domain.ItemID
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		open, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range open {
			if item.Offers(domain.ActionResumeUnattended) {
				noticeID = item.ID
				return nil
			}
		}
		t.Fatal("no open notice offers resume_unattended")
		return nil
	}); err != nil {
		t.Fatalf("find stopped notice: %v", err)
	}
	submitOn(t, f, noticeID, commandID, domain.ActionResumeUnattended)
}

// submitOn accepts one action on the identified item as deviceA, using the
// item's current durable snapshot for every binding.
func submitOn(t *testing.T, f *workflowFixture, itemID domain.ItemID, commandID string, action domain.Action) {
	t.Helper()
	ctx := context.Background()
	item, err := f.signet.GetAttentionItem(ctx, itemID)
	if err != nil {
		t.Fatalf("get item %q: %v", itemID, err)
	}
	if _, err := f.signet.Submit(ctx, signet.ClientCommand{
		CommandID: commandID, DeviceID: deviceA, ExpectedEntityVersion: item.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.Item.ID, Action: action, ItemVersion: item.Item.ItemVersion,
			PRHeadSHA: item.Item.PRHeadSHA, ArtifactDigests: item.Item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("submit %s on %q: %v", action, itemID, err)
	}
}

// TestStopUnattendedHoldsDispatchUntilResume is the #319 acceptance flow end
// to end: a waived unattended admission surfaces its notice, the notice's
// offered stop_unattended durably closes admission (the pending intent is
// held with no error, so attended reconciliation keeps running), and only the
// explicit resume_unattended reopens dispatch — after which the held intent
// is admitted on its own merits.
func TestStopUnattendedHoldsDispatchUntilResume(t *testing.T) {
	ctx := context.Background()
	f := openWaivedUnattendedFixture(t)

	first, _, err := dispatchOneInvocation(t, f)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	// A second pass accepts the completed turn so the conversation is idle
	// for the next discuss.
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("accept first turn: %v", err)
	}

	// The waived-posture notice now honestly offers stop_unattended (#319).
	noticeID := domain.ItemID("system-health-backup-waiver-" + string(first))
	notice, err := f.signet.GetAttentionItem(ctx, noticeID)
	if err != nil {
		t.Fatalf("get waived-posture notice: %v", err)
	}
	if !notice.Item.Offers(domain.ActionStopUnattended) {
		t.Fatalf("waived notice offers %v, want stop_unattended offered", notice.Item.RequestedDecision)
	}
	if notice.Item.BlockingSupersession == nil ||
		notice.Item.BlockingSupersession.RepositoryID != 424242 {
		t.Fatalf("waived notice condition = %+v, want the waived repository", notice.Item.BlockingSupersession)
	}

	// Enqueue the next agent turn, then stop before it dispatches.
	feedback, err := f.signet.GetAttentionItem(ctx, domain.ItemID("feedback-"+string(testRunID)))
	if err != nil {
		t.Fatalf("get feedback item: %v", err)
	}
	secondCommand := "discuss-235-second"
	second := domain.InvocationID("inv-" + secondCommand)
	f.scriptCompletion(second, fake.OutcomeComplete)
	if _, err := f.signet.Submit(ctx, signet.ClientCommand{
		CommandID: secondCommand, DeviceID: deviceA, ExpectedEntityVersion: feedback.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: feedback.Item.ID, Action: domain.ActionDiscuss, ItemVersion: feedback.Item.ItemVersion,
			PRHeadSHA: feedback.Item.PRHeadSHA, ArtifactDigests: feedback.Item.ArtifactDigests,
			Message: "Queue one more turn before the operator stops.",
		},
	}); err != nil {
		t.Fatalf("second discuss: %v", err)
	}
	submitOn(t, f, noticeID, "stop-1", domain.ActionStopUnattended)

	// The held pass: no error (the Run loop must keep ticking), nothing
	// started, nothing admitted, the intent still pending.
	result, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile while stopped: %v", err)
	}
	if result.InvocationsStarted != 0 {
		t.Fatalf("Reconcile while stopped started %d invocations, want 0", result.InvocationsStarted)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, found, err := tx.LookupExecutionAdmission(ctx, second); err != nil {
			return err
		} else if found {
			t.Error("an admission was recorded while stopped")
		}
		pending, err := tx.ListPendingOutbox(ctx, signet.AgentInvocationRequestedKind)
		if err != nil {
			return err
		}
		if len(pending) != 1 {
			t.Errorf("pending intents while stopped = %d, want the held one", len(pending))
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect held state: %v", err)
	}
	if _, ok := f.driver.StartSpec(second); ok {
		t.Fatal("driver started an invocation while stopped")
	}

	// Resume rides the stopped notice; the next pass admits and starts the
	// held intent.
	var stoppedNotice domain.ItemID
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		open, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range open {
			if item.Offers(domain.ActionResumeUnattended) {
				stoppedNotice = item.ID
				return nil
			}
		}
		t.Fatal("no open notice offers resume_unattended while stopped")
		return nil
	}); err != nil {
		t.Fatalf("find stopped notice: %v", err)
	}
	submitOn(t, f, stoppedNotice, "resume-1", domain.ActionResumeUnattended)

	resumed, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after resume: %v", err)
	}
	if resumed.InvocationsStarted != 1 {
		t.Fatalf("Reconcile after resume started %d invocations, want the held one", resumed.InvocationsStarted)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, found, err := tx.LookupExecutionAdmission(ctx, second)
		if err != nil {
			return err
		}
		if !found {
			t.Error("the resumed dispatch recorded no admission")
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect resumed admission: %v", err)
	}
}

// TestStopHoldsAReplayedRecordedAdmission covers the crash window between
// recording an admission and starting its driver: an admission recorded
// before the stop is honest history, but its dispatch is still new
// unattended operation, so the replay path holds it while stopped and starts
// it only after the explicit resume.
func TestStopHoldsAReplayedRecordedAdmission(t *testing.T) {
	ctx := context.Background()
	f := openWaivedUnattendedFixture(t)

	// An unscripted driver makes the first dispatch record the attempt and
	// its admission, then fail the start: exactly the recorded-but-unstarted
	// state a daemon crash between the two leaves behind.
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocation := f.discuss(t, feedback)
	if _, err := f.engine.Reconcile(ctx); err == nil {
		t.Fatal("unscripted start unexpectedly succeeded")
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, found, err := tx.LookupExecutionAdmission(ctx, invocation)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("the failed dispatch recorded no admission")
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect recorded admission: %v", err)
	}

	// The operator stops; the driver is now scripted, so only the
	// operating-state gate can be what holds the replay.
	stopOperations(t, f, "stop-cmd-1")
	f.scriptCompletion(invocation, fake.OutcomeComplete)

	held, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile while stopped: %v", err)
	}
	if held.InvocationsStarted != 0 {
		t.Fatalf("replayed dispatch started %d invocations while stopped, want 0", held.InvocationsStarted)
	}
	if _, ok := f.driver.StartSpec(invocation); ok {
		t.Fatal("driver started a recorded admission while stopped")
	}

	resumeOperations(t, f, "resume-cmd-1")
	resumed, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after resume: %v", err)
	}
	if resumed.InvocationsStarted != 1 {
		t.Fatalf("Reconcile after resume started %d invocations, want the held one", resumed.InvocationsStarted)
	}
}

// TestStopDoesNotHoldAcceptanceOfStartedWork pins the boundary of the hold:
// a stop halts new starts, never the acceptance of work already running. The
// crash window here is Start succeeding and the daemon dying before
// MarkOutboxDispatched, leaving the intent pending while the driver runs; a
// stop recorded before restart must not strand that completed invocation
// until resume — the driver's own answer, not the outbox bookkeeping, is
// what distinguishes an unstarted launch from an unmarked one.
func TestStopDoesNotHoldAcceptanceOfStartedWork(t *testing.T) {
	ctx := context.Background()
	f := openWaivedUnattendedFixture(t)

	// Record the attempt and admission with the start failing (unscripted
	// driver), then start the driver directly under the stored admission:
	// exactly the state a crash between Start and MarkOutboxDispatched
	// leaves behind — driver running, intent still pending.
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocation := f.discuss(t, feedback)
	if _, err := f.engine.Reconcile(ctx); err == nil {
		t.Fatal("unscripted start unexpectedly succeeded")
	}
	f.scriptCompletion(invocation, fake.OutcomeComplete)
	var stored domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetExecutionAdmission(ctx, invocation)
		return err
	}); err != nil {
		t.Fatalf("read stored admission: %v", err)
	}
	if err := f.driver.Start(ctx, invocation, exec.StartSpecFromAdmission(stored)); err != nil {
		t.Fatalf("driver start: %v", err)
	}

	stopOperations(t, f, "stop-cmd-2")

	held, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile while stopped: %v", err)
	}
	if held.InvocationsStarted != 0 {
		t.Fatalf("Reconcile while stopped started %d invocations, want 0", held.InvocationsStarted)
	}
	if held.ResultsAccepted != 1 {
		t.Fatalf("Reconcile while stopped accepted %d results, want the completed pre-stop work", held.ResultsAccepted)
	}
}

// TestStopHoldsAFreshDispatchUnderAnUnconfiguredEngine covers the other
// unconfigured crash window: the intent is pending but no attempt or
// admission was ever recorded, so neither the store's in-transaction gate nor
// the replay-branch check can run — the per-pass stop check is the only
// barrier, and an engine with no admission configuration must treat its
// unknowable operating mode as unattended (fail closed) rather than skip it.
func TestStopHoldsAFreshDispatchUnderAnUnconfiguredEngine(t *testing.T) {
	ctx := context.Background()
	f := openWaivedUnattendedFixture(t)

	// Enqueue the intent without any dispatch pass touching it: no attempt,
	// no admission, just the pending outbox row.
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocation := f.discuss(t, feedback)
	f.scriptCompletion(invocation, fake.OutcomeComplete)

	stopOperations(t, f, "stop-cmd-3")

	restarted, err := engine.New(f.store, f.signet, f.driver)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	held, err := restarted.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile while stopped: %v", err)
	}
	if held.InvocationsStarted != 0 {
		t.Fatalf("unconfigured fresh dispatch started %d invocations while stopped, want 0", held.InvocationsStarted)
	}
	if _, ok := f.driver.StartSpec(invocation); ok {
		t.Fatal("driver started an unrecorded intent while stopped")
	}

	resumeOperations(t, f, "resume-cmd-2")
	resumed, err := restarted.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after resume: %v", err)
	}
	if resumed.InvocationsStarted != 1 {
		t.Fatalf("Reconcile after resume started %d invocations, want the held one", resumed.InvocationsStarted)
	}
}

// TestBlockingItemHoldsAFreshDispatchUnderAnUnconfiguredEngine is the other
// half of the unknown-mode rule: the per-pass check is the one shared
// operating-state predicate, so an unconfigured engine fails closed against
// a blocking system_health item exactly as it does against a stop — not just
// against whichever half was remembered. A superseded notice (the waived
// posture, whose condition validates against the store's live policy) does
// not hold it.
func TestBlockingItemHoldsAFreshDispatchUnderAnUnconfiguredEngine(t *testing.T) {
	ctx := context.Background()
	f := openWaivedUnattendedFixture(t)

	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocation := f.discuss(t, feedback)
	f.scriptCompletion(invocation, fake.OutcomeComplete)

	blocker, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "health-blocker", ProjectID: "proj-235",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "diagnostic finding",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, blocker)
	}); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	restarted, err := engine.New(f.store, f.signet, f.driver)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	held, err := restarted.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile under a blocking item: %v", err)
	}
	if held.InvocationsStarted != 0 {
		t.Fatalf("unconfigured dispatch started %d invocations under a blocking item, want 0", held.InvocationsStarted)
	}
	if _, ok := f.driver.StartSpec(invocation); ok {
		t.Fatal("driver started an intent under a blocking item")
	}

	// The diagnostic clears: conclude the item; the next pass dispatches.
	resolved := blocker
	resolved.ItemVersion = 2
	resolved.Status = domain.StatusResolved
	if resolved, err = resolved.WithDecidedAt(admittedAt); err != nil {
		t.Fatalf("WithDecidedAt: %v", err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, resolved)
	}); err != nil {
		t.Fatalf("resolve blocker: %v", err)
	}
	released, err := restarted.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after the diagnostic cleared: %v", err)
	}
	if released.InvocationsStarted != 1 {
		t.Fatalf("Reconcile after clearing started %d invocations, want the held one", released.InvocationsStarted)
	}
}

// TestStopHoldsAReplayUnderAnUnconfiguredEngine pins the replay-branch gate
// itself: an engine restarted without admission configuration runs no
// dispatch pre-check (it has no operating mode to key one on), so the
// in-transaction check on the stored admission is the only thing standing
// between a recorded unattended attempt and a driver start the operator
// stopped. Without it, exactly one last launch would leak here.
func TestStopHoldsAReplayUnderAnUnconfiguredEngine(t *testing.T) {
	ctx := context.Background()
	f := openWaivedUnattendedFixture(t)

	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocation := f.discuss(t, feedback)
	// Record the attempt and admission with the start failing (unscripted
	// driver): the crash window between the admitting transaction and Start.
	if _, err := f.engine.Reconcile(ctx); err == nil {
		t.Fatal("unscripted start unexpectedly succeeded")
	}

	stopOperations(t, f, "stop-cmd-4")
	f.scriptCompletion(invocation, fake.OutcomeComplete)

	// The restarted daemon lost its admission configuration; the stored
	// record still binds the replay (the #301 rule), and the stop must
	// still hold it.
	restarted, err := engine.New(f.store, f.signet, f.driver)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	held, err := restarted.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile while stopped: %v", err)
	}
	if held.InvocationsStarted != 0 {
		t.Fatalf("unconfigured replay started %d invocations while stopped, want 0", held.InvocationsStarted)
	}
	if _, ok := f.driver.StartSpec(invocation); ok {
		t.Fatal("driver started a recorded admission while stopped")
	}

	resumeOperations(t, f, "resume-cmd-3")
	resumed, err := restarted.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after resume: %v", err)
	}
	if resumed.InvocationsStarted != 1 {
		t.Fatalf("Reconcile after resume started %d invocations, want the held one", resumed.InvocationsStarted)
	}
}
