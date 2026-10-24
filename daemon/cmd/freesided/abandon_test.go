package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestAbandonPartialResultsAndExactEpisodeRepair(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{false, true} {
		for _, failure := range []string{"closure", "cleanup"} {
			name := failure + "/offline"
			if live {
				name = failure + "/control"
			}
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				dbPath := filepath.Join(t.TempDir(), "freeside.db")
				seedTemplateStore(t, dbPath)
				h, err := run(ctx, nil, config{Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = h.Close() })
				var taskID domain.TaskID
				if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
					if err := tx.PutRun(ctx, domain.Run{ID: "partial-run", ProjectID: "partial-project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}); err != nil {
						return err
					}
					run, err := tx.GetRun(ctx, "partial-run")
					if err != nil {
						return err
					}
					taskID = run.TaskID
					if err := tx.RecordTaskStart(ctx, run.ID, time.Now().UTC()); err != nil {
						return err
					}
					item, err := domain.NewAttentionItem(domain.AttentionItemInput{
						ID: "execution-failure-partial", ProjectID: run.ProjectID,
						Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID},
						Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh, Reason: "Decision pending.",
						RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 1, Status: domain.StatusOpen,
						InterruptionClass: domain.InterruptionExceptional,
					}, nil)
					if err != nil {
						return err
					}
					return tx.PutAttentionItem(ctx, item)
				}); err != nil {
					t.Fatal(err)
				}
				if !live {
					if err := h.Close(); err != nil {
						t.Fatal(err)
					}
				}
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				trigger := `CREATE TRIGGER fail_abandon BEFORE INSERT ON task_workflow_closures
					BEGIN SELECT RAISE(ABORT, 'injected closure failure'); END`
				if failure == "cleanup" {
					trigger = `CREATE TRIGGER fail_abandon BEFORE UPDATE ON attention_items
						WHEN NEW.status = 'expired' BEGIN SELECT RAISE(ABORT, 'injected cleanup failure'); END`
				}
				if _, err := db.ExecContext(ctx, trigger); err != nil {
					t.Fatal(err)
				}
				result, err := runAbandonCommand(ctx, abandonCommandConfig{DBPath: dbPath, TaskID: taskID})
				if err == nil || !result.Released || result.Episode != 1 ||
					result.WorkflowClosed != (failure == "cleanup") || result.CleanupComplete ||
					!strings.Contains(err.Error(), "-episode 1") {
					t.Fatalf("partial result = %+v, %v", result, err)
				}
				// A newer admission can occur before a delayed repair. The
				// explicit selector must neither release nor close that start.
				st := h.store
				if !live {
					st, err = store.OpenExisting(ctx, dbPath, store.Options{})
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := st.Write(ctx, func(tx *store.WriteTx) error {
					if err := tx.PutRun(ctx, domain.Run{
						ID: "new-partial-run", TaskID: taskID,
						ProjectID: "partial-project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
					}); err != nil {
						return err
					}
					return tx.AdmitTaskStart(ctx, "new-partial-run", time.Now().UTC(), 1)
				}); err != nil {
					t.Fatal(err)
				}
				if !live {
					if err := st.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_abandon`); err != nil {
					t.Fatal(err)
				}
				repair, err := runAbandonCommand(ctx, abandonCommandConfig{DBPath: dbPath, TaskID: taskID, Episode: 1})
				if err != nil || repair.Released || repair.Episode != 1 || !repair.WorkflowClosed || !repair.CleanupComplete {
					t.Fatalf("repair = %+v, %v", repair, err)
				}
				if !live {
					st, err = store.OpenExisting(ctx, dbPath, store.Options{})
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = st.Close() }()
				}
				if err := st.Read(ctx, func(tx *store.ReadTx) error {
					task, err := tx.GetTask(ctx, taskID)
					if err != nil {
						return err
					}
					if !domain.TaskWIP(task) || task.CurrentStart().RunID != "new-partial-run" {
						t.Fatalf("repair released newer episode: %+v", task)
					}
					if err := tx.RequireRunWorkflowOpen(ctx, "partial-run"); !errors.Is(err, store.ErrTaskWorkflowClosed) {
						t.Fatalf("old episode fence = %v", err)
					}
					return tx.RequireRunWorkflowOpen(ctx, "new-partial-run")
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestAbandonRetiresAttentionItems(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h, err := run(ctx, nil, config{Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	var taskID domain.TaskID
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, runID := range []domain.RunID{"abandon-selected", "abandon-other"} {
			if err := tx.PutRun(ctx, domain.Run{ID: runID, ProjectID: "abandon-project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}); err != nil {
				return err
			}
			if err := tx.RecordTaskStart(ctx, runID, time.Now().UTC()); err != nil {
				return err
			}
			if runID == "abandon-selected" {
				run, err := tx.GetRun(ctx, runID)
				if err != nil {
					return err
				}
				taskID = run.TaskID
			}
		}
		return tx.PutDevice(ctx, domain.Device{ID: "abandon-device", DisplayName: "Test device", Status: domain.DeviceActive, PairedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	seed := func(id domain.ItemID, runID domain.RunID, kind domain.AttentionType) domain.AttentionItem {
		t.Helper()
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: id, ProjectID: "abandon-project", Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
			Type: kind, Priority: domain.PriorityHigh, Reason: "Operator decision needed.",
			RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
			t.Fatal(err)
		}
		return item
	}
	approval := seed("abandon-approval", "abandon-selected", domain.AttentionSpecApproval)
	failure := seed("execution-failure-abandon", "abandon-selected", domain.AttentionExecutionFailure)
	quarantine := seed("production-quarantined-abandon", "abandon-selected", domain.AttentionExecutionFailure)
	dispute := seed("abandon-dispute", "abandon-selected", domain.AttentionReviewDispute)
	retiring := []domain.AttentionItem{approval, failure, quarantine, dispute}
	untouched := []domain.AttentionItem{
		seed("other-approval", "abandon-other", domain.AttentionSpecApproval),
		seed("execution-failure-other", "abandon-other", domain.AttentionExecutionFailure),
	}
	service := signet.NewService(h.store)
	read := func(id domain.ItemID) signet.AttentionItemSnapshot {
		t.Helper()
		item, err := service.GetAttentionItem(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	before := read(approval.ID)
	beforeUntouched := make([]signet.AttentionItemSnapshot, len(untouched))
	for i, item := range untouched {
		beforeUntouched[i] = read(item.ID)
	}
	cfg := abandonCommandConfig{DBPath: dbPath, TaskID: taskID}
	result, err := runAbandonCommand(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	expectedIDs := []domain.ItemID{approval.ID, failure.ID, quarantine.ID, dispute.ID}
	slices.Sort(expectedIDs)
	if !result.Released || !result.WorkflowClosed || !result.CleanupComplete || result.Episode != 1 ||
		!slices.Equal(result.RetiredItems, expectedIDs) {
		t.Fatalf("abandon result = %+v", result)
	}
	for _, initial := range retiring {
		got := read(initial.ID).Item
		if got.Status != domain.StatusExpired || got.ItemVersion != initial.ItemVersion+1 || got.DecidedAt != nil || got.Reason != initial.Reason {
			t.Fatalf("retired item = %+v", got)
		}
	}
	for i, item := range untouched {
		if got := read(item.ID); !reflect.DeepEqual(got, beforeUntouched[i]) {
			t.Fatalf("unrelated card changed: %s", item.ID)
		}
	}
	if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItemRecordsForTask(ctx, taskID)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.ID == approval.ID || item.ID == failure.ID {
				t.Fatalf("retired item still listed open: %s", item.ID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after := read(approval.ID)
	_, err = service.Submit(ctx, signet.ClientCommand{
		CommandID: "abandoned-decision", DeviceID: "abandon-device", ExpectedEntityVersion: before.EntityVersion,
		Payload: signet.DecisionPayload{ItemID: approval.ID, ItemVersion: before.Item.ItemVersion, Action: domain.ActionStop},
	})
	var closed *signet.ClosedItemError
	if !errors.As(err, &closed) {
		t.Fatalf("decision error = %v, want closed item", err)
	}
	if got := read(approval.ID); !reflect.DeepEqual(got, after) {
		t.Fatal("rejected decision changed item")
	}
	if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetCommand(ctx, "abandoned-decision")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected command recorded: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	late := failure
	late.ID = "execution-failure-late"
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, late)
	}); !errors.Is(err, store.ErrTaskWorkflowClosed) {
		t.Fatalf("late card = %v, want closed workflow", err)
	}
	result, err = runAbandonCommand(ctx, cfg)
	if err != nil || result.Released || !result.WorkflowClosed || len(result.RetiredItems) != 0 {
		t.Fatalf("repeat abandon = %+v, %v", result, err)
	}
	stateBefore, err := h.store.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err = runAbandonCommand(ctx, cfg)
	if err != nil || result.Released || result.RetiredItems == nil || len(result.RetiredItems) != 0 {
		t.Fatalf("empty repeat = %+v, %v", result, err)
	}
	stateAfter, err := h.store.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stateAfter != stateBefore {
		t.Fatalf("empty repeat changed revision: %+v -> %+v", stateBefore, stateAfter)
	}
	body, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(body), `"retired_items":[]`) {
		t.Fatalf("empty result JSON = %s, %v", body, err)
	}
}

func TestAbandonKeepsReleaseWhenRetirementFails(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	recipe := domain.Digest("sha256:abandon-recipe")
	approved := map[domain.Digest]bool{recipe: true}
	st, _, err := openStoreWithTopicKey(ctx, dbPath, store.Options{ApprovedRecipes: approved})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "abandon-evidence", Type: domain.ArtifactKindVerifyLog, Digest: "sha256:abandon-evidence",
		Provenance: domain.Provenance{ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "abandon-invocation", HeadBinding: domain.HeadBound, SourceHeadSHA: "cafebabe", VerificationRecipeDigest: &recipe, SensitivityClass: domain.SensitivityNormal},
		Metadata:   domain.EvidenceMetadata{MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1, CreatedAt: at, Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable},
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("abandon-failure-run")
	var taskID domain.TaskID
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{ID: runID, ProjectID: "abandon-project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}); err != nil {
			return err
		}
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		taskID = run.TaskID
		if err := tx.RecordTaskStart(ctx, runID, at); err != nil {
			return err
		}
		for _, id := range []domain.ItemID{"execution-failure-a", "execution-failure-z"} {
			input := domain.AttentionItemInput{
				ID: id, ProjectID: run.ProjectID, Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
				Type: domain.AttentionExecutionFailure, Priority: domain.PriorityHigh, Reason: "Recovery needed.",
				RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 1, Status: domain.StatusOpen, InterruptionClass: domain.InterruptionExceptional,
			}
			if id == "execution-failure-z" {
				input.EvidenceSnapshot = []domain.Artifact{artifact}
			}
			item, err := domain.NewAttentionItem(input, approved)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// The offline command opens with current policy, which no longer approves
	// the second card's recipe. The first card must roll back with the second.
	result, err := runAbandonCommand(ctx, abandonCommandConfig{DBPath: dbPath, TaskID: taskID})
	if err == nil || !result.Released || !result.WorkflowClosed || result.CleanupComplete ||
		!strings.Contains(err.Error(), "-episode 1") || !strings.Contains(err.Error(), "execution-failure-z") {
		t.Fatalf("retirement error = %v", err)
	}
	st, err = store.OpenExisting(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if err := tx.RequireRunWorkflowOpen(ctx, runID); !errors.Is(err, store.ErrTaskWorkflowClosed) {
			t.Fatalf("cleanup failure reopened workflow: %v", err)
		}
		task, err := tx.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		if state := task.DisplayLifecycle(nil); state == nil || *state != domain.TaskAbandoned || domain.TaskWIP(task) {
			t.Fatalf("release rolled back: %+v", task)
		}
		for _, id := range []domain.ItemID{"execution-failure-a", "execution-failure-z"} {
			item, err := tx.GetAttentionItemRecord(ctx, id)
			if err != nil {
				return err
			}
			if item.Status != domain.StatusOpen || item.ItemVersion != 1 {
				t.Fatalf("partial retirement persisted: %+v", item)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
