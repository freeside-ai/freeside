package main

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestReattemptArgumentsFailBeforeOpeningStore(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	for _, tc := range []struct {
		name string
		cfg  reattemptCommandConfig
		want string
	}{
		{"no selector", reattemptCommandConfig{DBPath: dbPath, Reason: "repair"}, "exactly one"},
		{"parent and campaign", reattemptCommandConfig{
			DBPath: dbPath, ParentRunID: "run-1", CampaignID: "campaign-1", Reason: "repair",
		}, "exactly one"},
		{"task and parent", reattemptCommandConfig{
			DBPath: dbPath, TaskID: "task-1", ParentRunID: "run-1", Reason: "repair",
		}, "exactly one"},
		{"task and campaign", reattemptCommandConfig{
			DBPath: dbPath, TaskID: "task-1", CampaignID: "campaign-1", Reason: "repair",
		}, "exactly one"},
		{"all selectors", reattemptCommandConfig{
			DBPath: dbPath, TaskID: "task-1", ParentRunID: "run-1", CampaignID: "campaign-1", Reason: "repair",
		}, "exactly one"},
		{"untrimmed reason", reattemptCommandConfig{
			DBPath: dbPath, ParentRunID: "run-1", Reason: " repair ",
		}, "non-empty and trimmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runReattemptCommand(t.Context(), tc.cfg); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runReattemptCommand() = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid command created database: %v", err)
			}
		})
	}
}

func TestReattemptTaskMatchesCampaignAndUsesLatestAttempt(t *testing.T) {
	t.Parallel()
	var campaignResult submitResult
	for _, selector := range []string{"--campaign", "--task"} {
		t.Run(selector, func(t *testing.T) {
			dbPath, st, parent := reattemptTaskFixture(t)
			id := string(parent.CampaignID)
			if selector == "--task" {
				id = string(parent.TaskID)
			}
			cfg, err := parseReattemptCommand([]string{
				"-db", dbPath, selector, id, "-reason", "Repair the fixture",
			}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runReattemptCommand(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "use resume while it is live") {
				t.Fatalf("live parent = %v, want resume refusal", err)
			}
			finish := func(runID domain.RunID, invocationID domain.InvocationID) {
				t.Helper()
				terminal := domain.ObservedStatusFailed
				if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
					return tx.AppendRunMilestone(t.Context(), domain.RunMilestone{
						RunID: runID, Kind: domain.MilestoneTerminalRecorded,
						InvocationID: &invocationID, Terminal: &terminal,
						RecordedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
					})
				}); err != nil {
					t.Fatal(err)
				}
			}
			finish(parent.ID, domain.InvocationID("inv-implement-"+string(parent.ID)))
			first, err := runReattemptCommand(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if first.ParentRunID != parent.ID || first.CampaignID != parent.CampaignID || first.AttemptNumber != 2 {
				t.Fatalf("first retry = %+v, want parent %q in campaign %q", first, parent.ID, parent.CampaignID)
			}
			if _, err := runReattemptCommand(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "use resume while it is live") {
				t.Fatalf("live latest attempt = %v, want refusal instead of retrying the older terminal run", err)
			}
			finish(first.RunID, first.InvocationID)
			latest, err := runReattemptCommand(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if latest.ParentRunID != first.RunID || latest.AttemptNumber != 3 {
				t.Fatalf("latest retry = %+v, want attempt 3 from %q", latest, first.RunID)
			}
			if selector == "--campaign" {
				campaignResult = latest
			} else if latest != campaignResult {
				t.Fatalf("task retry = %+v, want campaign result %+v", latest, campaignResult)
			}
		})
	}
}

func TestReattemptTaskSelectsNewestCampaign(t *testing.T) {
	t.Parallel()
	dbPath, st, parent := reattemptTaskFixture(t)
	implementationID := domain.RunID("newer-campaign-implementation")
	campaignID, err := engine.ProductionCampaignIDForImplementation(implementationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		original, err := tx.GetProductionAttemptByRun(t.Context(), parent.ID)
		if err != nil {
			return err
		}
		specificationID := domain.SpecificationRunIDForImplementation(implementationID)
		if err := tx.PutProductionAttempt(t.Context(), domain.ProductionAttempt{
			CampaignID: campaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
			SourceDigest: original.SourceDigest, PublicationDigest: "sha256:newer-publication",
			SpecificationRunID: specificationID, ImplementationRunID: implementationID,
		}); err != nil {
			return err
		}
		return tx.PutRun(t.Context(), domain.Run{
			ID: specificationID, TaskID: parent.TaskID, ProjectID: parent.ProjectID,
			SpecDigest: original.SourceDigest, PolicyDigest: parent.PolicyDigest,
			CampaignID: campaignID, AttemptNumber: 1,
		})
	}); err != nil {
		t.Fatal(err)
	}
	// The new campaign has no implementation yet. Both selectors must refuse
	// it, rather than falling back to the older campaign's live implementation.
	cfg := reattemptCommandConfig{DBPath: dbPath, CampaignID: campaignID, Reason: "Repair the fixture"}
	_, campaignErr := runReattemptCommand(t.Context(), cfg)
	cfg.CampaignID, cfg.TaskID = "", parent.TaskID
	_, taskErr := runReattemptCommand(t.Context(), cfg)
	if !errors.Is(campaignErr, store.ErrNotFound) || !errors.Is(taskErr, store.ErrNotFound) || taskErr.Error() != campaignErr.Error() {
		t.Fatalf("newest campaign errors: task=%v campaign=%v, want matching missing implementation", taskErr, campaignErr)
	}
}

func TestReattemptTaskRefusesAnotherTasksCampaign(t *testing.T) {
	t.Parallel()
	dbPath, st, parent := reattemptTaskFixture(t)
	var requested domain.Task
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		requested, err = tx.GetOrCreateTask(t.Context(), parent.ProjectID, domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 99},
		})
		if err != nil {
			return err
		}
		terminal := domain.ObservedStatusFailed
		invocationID := domain.InvocationID("inv-implement-" + string(parent.ID))
		return tx.AppendRunMilestone(t.Context(), domain.RunMilestone{
			RunID: parent.ID, Kind: domain.MilestoneTerminalRecorded,
			InvocationID: &invocationID, Terminal: &terminal,
			RecordedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, corruptErr := raw.ExecContext(t.Context(),
		`UPDATE tasks SET body = json_set(body, '$.campaign_ids', json_array(?)) WHERE id = ?`,
		parent.CampaignID, requested.ID)
	if err := errors.Join(corruptErr, raw.Close()); err != nil {
		t.Fatal(err)
	}
	result, err := runReattemptCommand(t.Context(), reattemptCommandConfig{
		DBPath: dbPath, TaskID: requested.ID, Reason: "Repair the fixture",
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("cross-task campaign = %+v, %v; want parent-key mismatch for requested task %q, campaign owner %q",
			result, err, requested.ID, parent.TaskID)
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		latest, err := tx.LatestProductionAttempt(t.Context(), parent.CampaignID)
		if err == nil && latest.AttemptNumber != 1 {
			t.Errorf("cross-task refusal allocated attempt %d", latest.AttemptNumber)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReattemptTaskWithoutCampaign(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var task domain.Task
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(t.Context(), "project-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 42},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cfg := reattemptCommandConfig{DBPath: dbPath, TaskID: task.ID, Reason: "Repair the fixture"}
	if _, err := runReattemptCommand(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "has no campaign yet; approve its specification first") {
		t.Fatalf("empty task = %v, want no-campaign refusal", err)
	}
	cfg.TaskID = "task-missing"
	if _, err := runReattemptCommand(t.Context(), cfg); !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), `task "task-missing"`) {
		t.Fatalf("missing task = %v, want identified not-found error", err)
	}
}

// The frozen database supplies an authenticated, approved campaign, including
// its specification root and publication metadata, without launching an agent.
func reattemptTaskFixture(t *testing.T) (string, *store.Store, domain.Run) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	dump, err := os.ReadFile(filepath.Join("..", "..", "internal", "store", "testdata", "pre_rename_specification_vocabulary.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateTopicKey(dbPath, false); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, loadErr := raw.ExecContext(t.Context(), string(dump))
	if err := errors.Join(loadErr, raw.Close()); err != nil {
		t.Fatal(err)
	}
	st, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var parent domain.Run
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		parent, err = tx.GetRun(t.Context(), "implementation-from-submit")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return dbPath, st, parent
}

func TestParseReattemptRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	_, err := parseReattemptCommand([]string{
		"-db", "state.db", "-parent-run", "run-1", "-reason", "repair", "ignored",
	}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
		t.Fatalf("parseReattemptCommand() = %v, want positional-argument refusal", err)
	}
}
