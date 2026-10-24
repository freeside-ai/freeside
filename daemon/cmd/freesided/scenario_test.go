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
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// scenarioWaitBound bounds each wait on the scenario daemon's loops. A clean
// run takes about a second; the margin is for a loaded CI host.
const scenarioWaitBound = 60 * time.Second

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
	if got, want := out.String(), "clean\t"+cleanScenario().Description+"\n"; got != want {
		t.Fatalf("listScenarios = %q, want %q", got, want)
	}
	if _, err := lookupScenario("nope"); err == nil || !strings.Contains(err.Error(), "not one of: clean") {
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
	dir := t.TempDir()
	cfg := config{
		DBPath: filepath.Join(dir, "freeside.db"), ListenAddr: "127.0.0.1:0",
		Environment: environmentEphemeral, Scenario: &clean,
		ReconcileInterval: 10 * time.Millisecond, SchedulerInterval: 50 * time.Millisecond,
	}
	h := startDaemon(t, cfg)
	inputs := filepath.Join(dir, "scenario")
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
