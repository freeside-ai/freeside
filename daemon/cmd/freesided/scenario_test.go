package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// The bound exceeds the quota-park scenario's 2-minute reset (scenario.go),
// so it no longer proves the allowed reading, rather than the reset, released
// the park; TestScenarioQuotaParkResumes checks that against the start time.
// It stays under go test's default 10-minute timeout, so a hang still fails
// with the waiting test's own message.
const scenarioWaitBound = 3 * time.Minute

func TestScenarioRefusedBeforeStoreOpens(t *testing.T) {
	clean := cleanScenario()
	for _, tc := range []struct {
		name string
		cfg  config
		want string
	}{
		{"prod", config{Scenario: &clean, Environment: environmentProd}, "-driver scenario requires -environment ephemeral"},
		{"dev", config{Scenario: &clean, Environment: environmentDev}, "-driver scenario requires -environment ephemeral"},
		{"unset tier", config{Scenario: &clean}, `environment "" is not prod, dev, or ephemeral`},
		{"beside fake", config{Scenario: &clean, Environment: environmentEphemeral, FakeDriverEnabled: true}, "-driver scenario cannot run beside another driver"},
		{"beside claude", config{Scenario: &clean, Environment: environmentEphemeral, Claude: &claudeDriverConfig{}}, "-driver scenario cannot run beside another driver"},
		{"seed fixture", config{Scenario: &clean, Environment: environmentEphemeral, SeedFixture: "representative"}, "-seed-fixture requires -driver disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

func TestListScenarios(t *testing.T) {
	var out bytes.Buffer
	if err := listScenarios(&out); err != nil {
		t.Fatal(err)
	}
	want := "clean\t" + cleanScenario().Description + "\n" +
		"quota-park\t" + quotaParkScenario().Description + "\n"
	if got := out.String(); got != want {
		t.Fatalf("listScenarios = %q, want %q", got, want)
	}
	if _, err := lookupScenario("nope"); err == nil || !strings.Contains(err.Error(), "not one of: clean, quota-park") {
		t.Fatalf("lookupScenario(nope) = %v, want the known names", err)
	}
}

// TestSeedScenarioRepositoryRebuildsAPartialStart covers a start that died
// after git init but before the base commit: the next start rebuilds the
// repository instead of failing on the missing ref forever.
func TestSeedScenarioRepositoryRebuildsAPartialStart(t *testing.T) {
	files := cleanScenario().BaseFiles
	fresh, err := seedScenarioRepository(filepath.Join(t.TempDir(), "repository"), files)
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(partial, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := scenarioGit(partial, "init", "-q", "-b", scenarioBaseRef); err != nil {
		t.Fatal(err)
	}
	for pass := range 2 {
		got, err := seedScenarioRepository(partial, files)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got != fresh {
			t.Fatalf("pass %d: base %s, want %s", pass, got, fresh)
		}
	}
}

func TestScenarioFromFlags(t *testing.T) {
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
	clean := cleanScenario()
	cfg := scenarioConfig(t, &clean)
	h := startDaemon(t, cfg)
	submitted := submitAndApproveScenario(t, h, cfg)

	ready := waitForScenarioItem(t, h.attention, domain.ItemID("production-ready-"+string(submitted.RunID)))
	if ready.Item.Type != domain.AttentionReadyForFinalReview {
		t.Fatalf("publication item type = %q, want %q", ready.Item.Type, domain.AttentionReadyForFinalReview)
	}
	assertScenarioPublished(t, h.attention, submitted.TaskID, submitted.RunID)
	// A read that names the scenario's recipe goes through the running
	// daemon's control route, which must approve what its store approves.
	source, err := openObservation(t.Context(), cfg.DBPath, verify.RecipeDigest(scenarioRecipe))
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
	assertScenarioPublished(t, restarted.attention, submitted.TaskID, submitted.RunID)
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
	quota := quotaParkScenario()
	cfg := scenarioConfig(t, &quota)
	h := startDaemon(t, cfg)
	submitted := submitAndApproveScenario(t, h, cfg)

	first := waitForScenarioRun(t, h.store, submitted.RunID)
	parked, err := engine.ProductionAttemptRunID(first.CampaignID, 2)
	if err != nil {
		t.Fatal(err)
	}
	hold := waitForScenarioHold(t, h.store, parked)
	window := hold.QuotaWindow
	if hold.Reason != domain.HoldQuotaWindowExhausted || window == nil ||
		window.Window != quota.QuotaRefusal.Window || window.Source != domain.ExhaustedMarkProviderRefusal {
		t.Fatalf("parked hold = %+v, want quota_window_exhausted on the refused window", hold)
	}

	recordScenarioAllowedReading(t, h.store, window.Pool, window.Window)
	assertScenarioPublished(t, h.attention, submitted.TaskID, parked)
	assertStartedBeforeReset(t, h.attention, submitted.TaskID, parked, window.ResetsAt)
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
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

func scenarioConfig(t *testing.T, played *scenario) config {
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
	inputs := filepath.Join(filepath.Dir(cfg.DBPath), "scenario")
	submitted, err := runSubmitCommand(t.Context(), submitCommandConfig{
		DBPath: cfg.DBPath, TaskPath: filepath.Join(inputs, "task.md"),
		PolicyPath: filepath.Join(inputs, "policy.json"), PublicationPath: filepath.Join(inputs, "publication.json"),
		WorkUnitPath: filepath.Join(inputs, "work-unit.json"), ProjectID: scenarioProjectID,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if err := h.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutDevice(t.Context(), domain.Device{
			ID: "device-scenario", DisplayName: "Scenario approver",
			Status: domain.DeviceActive, PairedAt: time.Now().UTC(),
		})
	}); err != nil {
		t.Fatalf("put device: %v", err)
	}
	approval := waitForScenarioItem(t, h.attention, domain.ItemID("spec-approval-"+string(submitted.RunID)+"-1"))
	if _, err := h.attention.Submit(t.Context(), signet.ClientCommand{
		CommandID: "approve-scenario", DeviceID: "device-scenario", ExpectedEntityVersion: approval.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: approval.Item.ID, Action: domain.ActionApprove,
			ItemVersion: approval.Item.ItemVersion, ArtifactDigests: approval.Item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatalf("approve specification: %v", err)
	}
	return submitted
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
// A reading names a generation of the collecting enrollment, so the test
// adopts one first; the scenario seeds none.
func recordScenarioAllowedReading(t *testing.T, st *store.Store, pool domain.UsagePoolID, window string) {
	t.Helper()
	ctx := t.Context()
	manifest := domain.Digest("sha256:" + strings.Repeat("5", 64))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		at := time.Now().UTC()
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, scenarioIdentityID, "inv-scenario-collect",
			&domain.LeaseGenerationBinding{
				EnrollmentID: scenarioEnrollmentID, AuthStoreVolume: "scenario-collect",
				StoreManifestDigest: manifest,
			}, at, at.Add(10*time.Minute))
		if err != nil {
			return err
		}
		generation, err := tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: scenarioEnrollmentID, AuthStoreVolume: "scenario-collect",
			StoreManifestDigest: manifest, LeaseFence: lease.Fence,
			AccountBinding: scenarioAccountBinding, RecordedAt: at,
		}, at)
		if err != nil {
			return err
		}
		record, err := tx.GetPoolUsageObservation(ctx, pool)
		if err != nil {
			return err
		}
		reset := at.Add(time.Hour)
		record.LastAttempt = &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: at}
		record.Windows = []domain.WindowReading{{
			Window: window, EnrollmentID: scenarioEnrollmentID, Generation: generation.Ordinal,
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
