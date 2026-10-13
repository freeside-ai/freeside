package main

import (
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
	untouched := []domain.AttentionItem{
		seed("other-approval", "abandon-other", domain.AttentionSpecApproval),
		seed("execution-failure-other", "abandon-other", domain.AttentionExecutionFailure),
		seed("production-quarantined-abandon", "abandon-selected", domain.AttentionExecutionFailure),
		seed("abandon-dispute", "abandon-selected", domain.AttentionReviewDispute),
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
	if !result.Released || !slices.Equal(result.RetiredItems, []domain.ItemID{approval.ID, failure.ID}) {
		t.Fatalf("abandon result = %+v", result)
	}
	for _, initial := range []domain.AttentionItem{approval, failure} {
		got := read(initial.ID).Item
		if got.Status != domain.StatusExpired || got.ItemVersion != initial.ItemVersion+1 || got.DecidedAt != nil || !strings.Contains(got.Reason, "task was abandoned") {
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
	late := seed("execution-failure-late", "abandon-selected", domain.AttentionExecutionFailure)
	result, err = runAbandonCommand(ctx, cfg)
	if err != nil || result.Released || !slices.Equal(result.RetiredItems, []domain.ItemID{late.ID}) {
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
	_, err = runAbandonCommand(ctx, abandonCommandConfig{DBPath: dbPath, TaskID: taskID})
	if err == nil || !strings.Contains(err.Error(), "slot-release step committed") || !strings.Contains(err.Error(), "execution-failure-z") {
		t.Fatalf("retirement error = %v", err)
	}
	st, err = store.OpenExisting(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
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
