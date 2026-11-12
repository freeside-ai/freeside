package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// scenarioWaitBound bounds each wait on the scenario daemon's loops, so it
// catches a hang, not a slow run. The longest wait, the quota park's from the
// allowed reading to the parked run's terminal, takes about 2s without -race,
// 17s with it, and about 30s with it on a heavily loaded host: the engine's
// serial reconcile pass slows several times under the race detector.
//
// The bound exceeds the quota-park scenario's 2-minute reset
// (daemoncompose.QuotaParkScenario), so it no longer proves the allowed
// reading, rather than the reset, released the park;
// TestScenarioQuotaParkResumes checks that against the start time.
// It stays under go test's default 10-minute timeout, so a hang still fails
// with the waiting test's own message.
const scenarioWaitBound = 3 * time.Minute

func TestScenarioRefusedBeforeStoreOpens(t *testing.T) {
	t.Parallel()
	clean := daemoncompose.CleanScenario()
	for _, tc := range []struct {
		name string
		cfg  config
		want string
	}{
		{"prod", config{Scenario: &clean, Environment: environmentProd}, "-driver scenario requires -environment ephemeral"},
		{"dev", config{Scenario: &clean, Environment: environmentDev}, "-driver scenario requires -environment ephemeral"},
		{"unset tier", config{Scenario: &clean}, `environment "" is not prod, dev, or ephemeral`},
		{"beside fake", config{Scenario: &clean, Environment: environmentEphemeral, FakeDriverEnabled: true}, "-driver scenario cannot run beside another driver"},
		{"beside claude", config{Scenario: &clean, Environment: environmentEphemeral, Claude: &daemoncompose.ClaudeDriverConfig{}}, "-driver scenario cannot run beside another driver"},
		{"seed fixture", config{Scenario: &clean, Environment: environmentEphemeral, SeedFixture: "representative"}, "-seed-fixture requires -driver disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dbPath := filepath.Join(t.TempDir(), "freeside.db")
			tc.cfg.DBPath = dbPath
			tc.cfg.ListenAddr = "127.0.0.1:0"
			_, err := run(t.Context(), nil, tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %v, want an error containing %q", err, tc.want)
			}
			if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused start touched the store: %v", err)
			}
		})
	}
}

func TestScenarioFromFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		execution executionMode
		scenario  string
		want      string
		err       string
	}{
		{name: "scenario driver", execution: executionScenario, scenario: "clean", want: "clean"},
		{name: "quota scenario", execution: executionScenario, scenario: "quota-park", want: "quota-park"},
		{name: "other driver", execution: executionFake},
		{name: "no name", execution: executionScenario, err: "-driver scenario requires -scenario NAME"},
		{name: "unknown name", execution: executionScenario, scenario: "nope", err: "not one of: clean"},
		{name: "name without driver", execution: executionDisabled, scenario: "clean", err: "-scenario requires -driver scenario"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			played, err := scenarioFromFlags(tc.execution, tc.scenario)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("scenarioFromFlags = %v, want an error containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if played != nil {
				got = played.Name
			}
			if got != tc.want {
				t.Fatalf("scenarioFromFlags played %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScenarioCleanRunPublishes plays the clean scenario on the composed
// daemon: submit the inputs it wrote, approve the specification, and wait for
// the run to publish its pull request to the in-process forge and record a
// completed terminal. A restart on the same root then starts clean, because
// seeding finds its repository and records in place.
func TestScenarioCleanRunPublishes(t *testing.T) {
	t.Parallel()
	clean := daemoncompose.CleanScenario()
	cfg := scenarioConfig(t, &clean)
	h := startDaemon(t, cfg)
	submitted := submitAndApproveScenario(t, h, cfg)

	ready := waitForScenarioItem(t, h.Attention, domain.ItemID("production-ready-"+string(submitted.RunID)))
	if ready.Item.Type != domain.AttentionReadyForFinalReview {
		t.Fatalf("publication item type = %q, want %q", ready.Item.Type, domain.AttentionReadyForFinalReview)
	}
	assertScenarioPublished(t, h.Attention, submitted.TaskID, submitted.RunID)
	// A read that names the scenario's recipe goes through the running
	// daemon's control route, which must approve what its store approves.
	source, err := openObservation(t.Context(), cfg.DBPath, verify.RecipeDigest(daemoncompose.ScenarioRecipe))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ObserveSnapshot(t.Context(), submitted.RunID); err != nil {
		t.Fatalf("observe through the daemon with the scenario recipe: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	restarted := startDaemon(t, cfg)
	assertScenarioPublished(t, restarted.Attention, submitted.TaskID, submitted.RunID)
	if err := restarted.Close(); err != nil {
		t.Fatalf("close restarted daemon: %v", err)
	}
}

// TestScenarioQuotaParkResumes plays the quota-park scenario: the refused
// first attempt parks the task with a quota window hold on its second
// attempt, and the second attempt publishes once the mark clears. A
// contrary reading clears it here, so the test never waits out the reset;
// the engine's synctest cases cover the reset itself.
func TestScenarioQuotaParkResumes(t *testing.T) {
	t.Parallel()
	quota := daemoncompose.QuotaParkScenario()
	cfg := scenarioConfig(t, &quota)
	h := startDaemon(t, cfg)
	submitted := submitAndApproveScenario(t, h, cfg)

	first := waitForScenarioRun(t, h.Store, submitted.RunID)
	parked, err := engine.ProductionAttemptRunID(first.CampaignID, 2)
	if err != nil {
		t.Fatal(err)
	}
	hold := waitForScenarioHold(t, h.Store, parked)
	window := hold.QuotaWindow
	if hold.Reason != domain.HoldQuotaWindowExhausted || window == nil ||
		window.Window != quota.QuotaRefusal.Window || window.Source != domain.ExhaustedMarkProviderRefusal {
		t.Fatalf("parked hold = %+v, want quota_window_exhausted on the refused window", hold)
	}

	recordScenarioAllowedReading(t, h.Store, window.Pool, window.Window)
	assertScenarioPublished(t, h.Attention, submitted.TaskID, parked)
	assertStartedBeforeReset(t, h.Attention, submitted.TaskID, parked, window.ResetsAt)
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestScenarioQuotaMoveMovesTheParkedTask plays the quota-move scenario: the
// refused first attempt parks the task under a hold that offers the other
// subscription, the operator moves the task there through the daemon's own
// command hook, and the parked attempt starts on that enrollment before the
// refused window's reset and publishes.
func TestScenarioQuotaMoveMovesTheParkedTask(t *testing.T) {
	t.Parallel()
	quota := daemoncompose.QuotaMoveScenario()
	cfg := scenarioConfig(t, &quota)
	h := startDaemon(t, cfg)
	submitted := submitAndApproveScenario(t, h, cfg)

	first := waitForScenarioRun(t, h.Store, submitted.RunID)
	parked, err := engine.ProductionAttemptRunID(first.CampaignID, 2)
	if err != nil {
		t.Fatal(err)
	}
	hold := waitForScenarioHold(t, h.Store, parked)
	window := hold.QuotaWindow
	if hold.Reason != domain.HoldQuotaWindowExhausted || window == nil || window.Window != quota.QuotaRefusal.Window {
		t.Fatalf("parked hold = %+v, want quota_window_exhausted on the refused window", hold)
	}
	target := otherSubscription(t, quota, window.Pool)
	offer := domain.AlternateAgentOffer{
		Enrollment: target.Enrollment.ID, Pool: target.Identity.EffectiveUsagePool(),
		Harness: domain.HarnessClientClaudeCode, Model: daemoncompose.ScenarioWriterModel,
		Qualification: domain.AlternateAgentDifferentPool,
	}
	if !slices.Contains(window.AlternateAgents, offer) {
		t.Fatalf("parked hold offers %+v, want %+v among them", window.AlternateAgents, offer)
	}

	if _, err := h.Attention.Submit(t.Context(), signet.ClientCommand{
		CommandID: "move-scenario", DeviceID: "device-scenario", Kind: domain.CommandKindMoveParkedTask,
		MoveParkedTask: signet.MoveParkedTaskPayload{
			TaskID: submitted.TaskID, Choice: &signet.AlternateAgentChoice{EnrollmentID: target.Enrollment.ID},
		},
	}); err != nil {
		t.Fatalf("move_parked_task: %v", err)
	}

	assertScenarioPublished(t, h.Attention, submitted.TaskID, parked)
	assertStartedBeforeReset(t, h.Attention, submitted.TaskID, parked, window.ResetsAt)
	timeline, err := h.Attention.GetTaskTimeline(t.Context(), submitted.TaskID)
	if err != nil {
		t.Fatalf("task timeline: %v", err)
	}
	var moved *domain.TaskEventTaskLine
	for _, event := range timeline.Events {
		if line := event.Line; line != nil && line.Source == domain.TaskLineSourceAlternateAgentMove &&
			line.Role == domain.RoleImplementer && line.Selection != nil &&
			line.Selection.EnrollmentID == target.Enrollment.ID {
			moved = line
		}
	}
	if moved == nil {
		t.Fatalf("task timeline shows no move of the implementer to %s; events: %+v", target.Enrollment.ID, timeline.Events)
	}
	admission := scenarioAdmission(t, h.Store, parked)
	if binding := admission.AgentBinding; binding == nil || binding.EnrollmentID != target.Enrollment.ID ||
		binding.SelectionSource != domain.AgentSelectionSourceTaskLine {
		t.Fatalf("parked attempt admission binds %+v, want %s selected by its task line", binding, target.Enrollment.ID)
	}
	assertScenarioLineupServed(t, h.Attention, quota, target.Enrollment.ID)
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestScenarioQuotaNoResetRaisesTheAlternateAgentCard plays the
// quota-no-reset scenario: the refusal names no reset and no reading
// supplies one, so nothing parks and the failure card offers the other
// subscription beside readings on both pools. The operator retries there
// from the card through the daemon's own command hook, and the retry is
// admitted under that enrollment and publishes.
func TestScenarioQuotaNoResetRaisesTheAlternateAgentCard(t *testing.T) {
	t.Parallel()
	quota := daemoncompose.QuotaNoResetScenario()
	cfg := scenarioConfig(t, &quota)
	h := startDaemon(t, cfg)
	submitted := submitAndApproveScenario(t, h, cfg)

	failed := scenarioInvocation(t, h.Store, submitted.RunID)
	card := waitForScenarioItem(t, h.Attention, domain.ExecutionFailureItemID(failed))
	item := card.Item
	if item.Type != domain.AttentionExecutionFailure || item.ExecutionFailure == nil ||
		item.ExecutionFailure.Cause != domain.FailureCauseQuota {
		t.Fatalf("failure card = %+v, want an execution_failure with cause quota", item)
	}
	for _, action := range []domain.Action{domain.ActionRetryWithAlternateAgent, domain.ActionStop} {
		if !item.Offers(action) {
			t.Fatalf("failure card requests %v, want %s among them", item.RequestedDecision, action)
		}
	}
	refused := scenarioAdmission(t, h.Store, submitted.RunID)
	if refused.AuthIdentityID == nil {
		t.Fatalf("refused admission %+v names no identity", refused)
	}
	target := otherSubscription(t, quota, identityPool(t, h.Store, *refused.AuthIdentityID))
	offer := domain.AlternateAgentOffer{
		Enrollment: target.Enrollment.ID, Pool: target.Identity.EffectiveUsagePool(),
		Harness: domain.HarnessClientClaudeCode, Model: daemoncompose.ScenarioWriterModel,
		Qualification: domain.AlternateAgentDifferentPool,
	}
	if !slices.Contains(item.AlternateAgents, offer) {
		t.Fatalf("failure card offers %+v, want %+v among them", item.AlternateAgents, offer)
	}
	retry, err := engine.ProductionAttemptRunID(waitForScenarioRun(t, h.Store, submitted.RunID).CampaignID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Store.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, found, err := tx.GetRunHold(t.Context(), submitted.RunID); err != nil || found {
			t.Errorf("refused run hold: found %v, err %v; want none", found, err)
		}
		if _, err := tx.GetRun(t.Context(), retry); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("attempt 2 before the decision: err %v, want not found (nothing parks)", err)
		}
		for _, subscription := range quota.Subscriptions() {
			record, err := tx.GetPoolUsageObservation(t.Context(), subscription.Identity.EffectiveUsagePool())
			if err != nil {
				return err
			}
			shown := slices.ContainsFunc(record.Windows, func(reading domain.WindowReading) bool {
				return reading.Window == quota.QuotaRefusal.Window && reading.Status == domain.WindowStatusAllowed &&
					reading.ResetsAt == nil
			})
			if !shown {
				t.Errorf("pool %s readings %+v, want an allowed %s reading with no reset to show beside the card",
					record.Pool, record.Windows, quota.QuotaRefusal.Window)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Attention.Submit(t.Context(), signet.ClientCommand{
		CommandID: "retry-scenario", DeviceID: "device-scenario", ExpectedEntityVersion: card.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionRetryWithAlternateAgent, ItemVersion: item.ItemVersion,
			AlternateAgentChoice: &signet.AlternateAgentChoice{EnrollmentID: target.Enrollment.ID},
		},
	}); err != nil {
		t.Fatalf("retry_with_alternate_agent: %v", err)
	}

	assertScenarioPublished(t, h.Attention, submitted.TaskID, retry)
	admission := scenarioAdmission(t, h.Store, retry)
	if binding := admission.AgentBinding; binding == nil || binding.EnrollmentID != target.Enrollment.ID ||
		binding.SelectionSource != domain.AgentSelectionSourceCard {
		t.Fatalf("retry admission binds %+v, want %s selected by the card", binding, target.Enrollment.ID)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// assertScenarioLineupServed checks the scenario's lineup reaches the
// client: the agent catalog names its lineup and the second subscription's
// enrollment, and the operator's routing-policy command, which reads the
// scenario's selection through the same hook as the move, takes a new order.
func assertScenarioLineupServed(t *testing.T, attention *signet.Service, sc daemoncompose.Scenario, second domain.ClientEnrollmentID) {
	t.Helper()
	snapshot, err := attention.Bootstrap(t.Context())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	catalog := snapshot.AgentCatalog
	listed := slices.ContainsFunc(catalog.Enrollments, func(enrollment signet.AgentCatalogEnrollment) bool {
		return enrollment.EnrollmentID == second
	})
	if catalog.LineupRevision == nil || len(catalog.Lineup) == 0 || !listed {
		t.Fatalf("agent catalog = %+v, want the scenario lineup and enrollment %s", catalog, second)
	}
	if snapshot.RoutingPolicy == nil {
		t.Fatal("bootstrap carries no routing policy, want the scenario's")
	}
	revision := snapshot.RoutingPolicy.Policy.Revision
	var order []domain.ClientEnrollmentID
	for _, subscription := range slices.Backward(sc.Subscriptions()) {
		order = append(order, subscription.Enrollment.ID)
	}
	if _, err := attention.Submit(t.Context(), signet.ClientCommand{
		CommandID: "route-scenario", DeviceID: "device-scenario", Kind: domain.CommandKindSetRoutingPolicy,
		SetRoutingPolicy: signet.SetRoutingPolicyPayload{
			Roles:            []signet.RoutingPolicyRoleInput{{Role: domain.RoleImplementer, Enrollments: order}},
			ExpectedRevision: &revision,
		},
	}); err != nil {
		t.Fatalf("set_routing_policy: %v", err)
	}
}

// otherSubscription returns the scenario's subscription off the refused
// pool. The router may have put the first attempt on either one, so the test
// reads the refused pool from the record instead of assuming it.
func otherSubscription(t *testing.T, sc daemoncompose.Scenario, refused domain.UsagePoolID) daemoncompose.ScenarioSubscription {
	t.Helper()
	for _, subscription := range sc.Subscriptions() {
		if subscription.Identity.EffectiveUsagePool() != refused {
			return subscription
		}
	}
	t.Fatalf("scenario %s has no subscription off pool %s", sc.Name, refused)
	return daemoncompose.ScenarioSubscription{}
}

func identityPool(t *testing.T, st *store.Store, id domain.AuthIdentityID) domain.UsagePoolID {
	t.Helper()
	var pool domain.UsagePoolID
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		identity, err := tx.GetAuthIdentity(t.Context(), id)
		pool = identity.EffectiveUsagePool()
		return err
	}); err != nil {
		t.Fatalf("read identity %s: %v", id, err)
	}
	return pool
}

// scenarioInvocation waits for the run's first invocation.
func scenarioInvocation(t *testing.T, st *store.Store, id domain.RunID) domain.InvocationID {
	t.Helper()
	deadline := time.Now().Add(scenarioWaitBound)
	for {
		for _, stage := range waitForScenarioRun(t, st, id).Stages {
			for _, attempt := range stage.Attempts {
				return attempt.InvocationID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s started no invocation within %s", id, scenarioWaitBound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// scenarioAdmission reads the admission of the run's first invocation.
func scenarioAdmission(t *testing.T, st *store.Store, id domain.RunID) domain.ExecutionAdmission {
	t.Helper()
	invocation := scenarioInvocation(t, st, id)
	var admission domain.ExecutionAdmission
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(t.Context(), invocation)
		return err
	}); err != nil {
		t.Fatalf("read admission of %s: %v", invocation, err)
	}
	return admission
}

// assertStartedBeforeReset checks that the parked run's invocation started
// before the refused window's reset. Once the reset passes, the park lets the
// attempt start whatever the readings say, so a start at or after it no
// longer proves the allowed reading released the park. The check holds
// whatever the wait bound.
func assertStartedBeforeReset(t *testing.T, attention *signet.Service, task domain.TaskID, run domain.RunID, resetsAt *time.Time) {
	t.Helper()
	if resetsAt == nil {
		t.Fatal("parked hold carries no reset")
	}
	timeline, err := attention.GetTaskTimeline(t.Context(), task)
	if err != nil {
		t.Fatalf("task timeline: %v", err)
	}
	for _, event := range timeline.Events {
		if event.RunID == nil || *event.RunID != run ||
			event.Milestone == nil || event.Milestone.Kind != domain.MilestoneInvocationStarted {
			continue
		}
		if started := event.Milestone.RecordedAt; !started.Before(*resetsAt) {
			t.Fatalf("run %s started at %s, not before the refusal's reset at %s: the reset, not the allowed reading, may have released the park",
				run, started.Format(time.RFC3339Nano), resetsAt.Format(time.RFC3339Nano))
		}
		return
	}
	t.Fatalf("run %s recorded no %s milestone; events: %+v", run, domain.MilestoneInvocationStarted, timeline.Events)
}

func scenarioConfig(t *testing.T, played *daemoncompose.Scenario) config {
	t.Helper()
	return config{
		DBPath: filepath.Join(t.TempDir(), "freeside.db"), ListenAddr: "127.0.0.1:0",
		Environment: environmentEphemeral, Scenario: played,
		ReconcileInterval: 10 * time.Millisecond, SchedulerInterval: 50 * time.Millisecond,
	}
}

// submitAndApproveScenario submits the inputs the scenario wrote beside its
// repository and approves the specification from a paired device.
func submitAndApproveScenario(t *testing.T, h *daemon, cfg config) submitResult {
	t.Helper()
	submitted := submitScenario(t, h, cfg)
	approveScenarioSpecification(t, h, submitted.RunID)
	return submitted
}

// submitScenario submits the inputs the scenario wrote beside its repository
// and records the device that approves its specification.
func submitScenario(t *testing.T, h *daemon, cfg config) submitResult {
	t.Helper()
	inputs := filepath.Join(filepath.Dir(cfg.DBPath), "scenario")
	submitted, err := runSubmitCommand(t.Context(), submitCommandConfig{
		DBPath: cfg.DBPath, TaskPath: filepath.Join(inputs, "task.md"),
		PolicyPath: filepath.Join(inputs, "policy.json"), PublicationPath: filepath.Join(inputs, "publication.json"),
		WorkUnitPath: filepath.Join(inputs, "work-unit.json"), ProjectID: daemoncompose.ScenarioProjectID,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if err := h.Store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutDevice(t.Context(), domain.Device{
			ID: "device-scenario", DisplayName: "Scenario approver",
			Status: domain.DeviceActive, PairedAt: time.Now().UTC(),
		})
	}); err != nil {
		t.Fatalf("put device: %v", err)
	}
	return submitted
}

// approveScenarioSpecification approves the run's specification from the
// device submitScenario recorded, once its approval item is open.
func approveScenarioSpecification(t *testing.T, h *daemon, run domain.RunID) {
	t.Helper()
	approval := waitForScenarioItem(t, h.Attention, scenarioSpecApprovalItem(run))
	if _, err := h.Attention.Submit(t.Context(), signet.ClientCommand{
		CommandID: "approve-scenario", DeviceID: "device-scenario", ExpectedEntityVersion: approval.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: approval.Item.ID, Action: domain.ActionApprove,
			ItemVersion: approval.Item.ItemVersion, ArtifactDigests: approval.Item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("approve specification: %v", err)
	}
}

// scenarioSpecApprovalItem names the run's first specification-approval item.
func scenarioSpecApprovalItem(run domain.RunID) domain.ItemID {
	return domain.ItemID("spec-approval-" + string(run) + "-1")
}

// waitForScenarioRun waits for the run the approved specification starts.
func waitForScenarioRun(t *testing.T, st *store.Store, id domain.RunID) domain.Run {
	t.Helper()
	deadline := time.Now().Add(scenarioWaitBound)
	for {
		var run domain.Run
		err := st.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			run, err = tx.GetRun(t.Context(), id)
			return err
		})
		if err == nil {
			return run
		}
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("read run %s: %v", id, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not appear within %s", id, scenarioWaitBound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForScenarioHold waits for the run's current hold.
func waitForScenarioHold(t *testing.T, st *store.Store, run domain.RunID) domain.RunHoldObservation {
	t.Helper()
	deadline := time.Now().Add(scenarioWaitBound)
	for {
		var (
			hold  domain.RunHoldObservation
			found bool
		)
		if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			hold, found, err = tx.GetRunHold(t.Context(), run)
			return err
		}); err != nil {
			t.Fatalf("read hold on %s: %v", run, err)
		}
		if found {
			return hold
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s recorded no hold within %s", run, scenarioWaitBound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// recordScenarioAllowedReading records a collection that reads the window
// allowed, after the refusal's mark, which clears the mark before its reset.
// A reading names the collector's current generation, which seeding adopted.
func recordScenarioAllowedReading(t *testing.T, st *store.Store, pool domain.UsagePoolID, window string) {
	t.Helper()
	ctx := t.Context()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		at := time.Now().UTC()
		record, err := tx.GetPoolUsageObservation(ctx, pool)
		if err != nil {
			return err
		}
		generation, err := tx.CurrentEnrollmentGeneration(ctx, record.Collector)
		if err != nil {
			return err
		}
		reset := at.Add(time.Hour)
		record.LastAttempt = &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: at}
		record.Windows = []domain.WindowReading{{
			Window: window, EnrollmentID: record.Collector, Generation: generation.Ordinal,
			Status: domain.WindowStatusAllowed, ResetsAt: &reset, ObservedAt: at,
		}}
		return tx.RecordPoolUsageObservation(ctx, record, at)
	}); err != nil {
		t.Fatalf("record allowed reading: %v", err)
	}
}

// assertScenarioPublished waits for the implementation run's completed
// terminal, then checks that the run opened its pull request and recorded a
// clean verification verdict on the way.
func assertScenarioPublished(t *testing.T, attention *signet.Service, task domain.TaskID, run domain.RunID) {
	t.Helper()
	deadline := time.Now().Add(scenarioWaitBound)
	for {
		timeline, err := attention.GetTaskTimeline(t.Context(), task)
		if err != nil {
			t.Fatalf("task timeline: %v", err)
		}
		var terminal *domain.ObservedInvocationStatus
		var pr *int
		var verdict domain.ReadinessVerdictClass
		for _, event := range timeline.Events {
			if event.RunID == nil || *event.RunID != run {
				continue
			}
			switch {
			case event.Milestone != nil && event.Milestone.Kind == domain.MilestoneTerminalRecorded:
				terminal = event.Milestone.Terminal
			case event.Kind == domain.TaskEventPROpened:
				pr = event.PRNumber
			case event.Kind == domain.TaskEventVerificationRecorded && event.Verification != nil:
				verdict = event.Verification.Class
			}
		}
		if terminal != nil {
			if *terminal != domain.ObservedStatusCompleted || pr == nil || verdict != domain.ReadinessReadyClean {
				t.Fatalf("run %s: terminal %q, pull request %v, verdict %q; want completed, opened, ready_clean",
					run, *terminal, pr, verdict)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s recorded no terminal within %s; events: %+v", run, scenarioWaitBound, timeline.Events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForScenarioItem(t *testing.T, attention *signet.Service, id domain.ItemID) signet.AttentionItemSnapshot {
	t.Helper()
	deadline := time.Now().Add(scenarioWaitBound)
	for {
		item, err := attention.GetAttentionItem(t.Context(), id)
		if err == nil {
			return item
		}
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get item %q: %v", id, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("item %q did not appear within %s", id, scenarioWaitBound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
