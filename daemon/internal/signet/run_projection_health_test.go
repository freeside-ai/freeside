package signet_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runProjectionHealthPrefix mirrors the production const: the item id is
// operator-visible and stable, so the tests pin its format directly rather than
// reaching into the package.
const runProjectionHealthPrefix = "system-health-run-projection-"

func runProjectionHealthItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID(runProjectionHealthPrefix + string(runID))
}

// listSystemHealth returns every system_health item (any status), so a resolve
// is observable after the item leaves the open set.
func (f corpusFixture) listSystemHealth(t *testing.T) []domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	var out []domain.AttentionItem
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Value.Type == domain.AttentionSystemHealth &&
				strings.HasPrefix(string(item.Value.ID), runProjectionHealthPrefix) {
				out = append(out, item.Value)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("list system_health items: %v", err)
	}
	return out
}

func (f corpusFixture) getItem(t *testing.T, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	var item domain.AttentionItem
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, id)
		if err != nil {
			return err
		}
		item = got
		return nil
	}); err != nil {
		t.Fatalf("get item %q: %v", id, err)
	}
	return item
}

// TestRunProjectionHealthMintedOnExclusion is #770's core acceptance: a run
// excluded for a projection integrity contradiction produces exactly one open
// advisory AttentionSystemHealth item, bound to the run so it passes the same
// gate a real observation does, and it rides out on the bootstrap snapshot the
// operator sees.
func TestRunProjectionHealthMintedOnExclusion(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	damaged := domain.RunID("run-damaged")
	invocation := domain.InvocationID("inv-damaged")
	f.seedTerminalRun(t, damaged, invocation, domain.ObservedStatusCompleted, domain.ObservedStatusRunning)

	// The converge runs after the read builds its snapshot, so the item is
	// minted post-read and surfaces on the next poll, not the read that dropped
	// the run. A polling client sees it one cycle later.
	if _, err := f.service.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap (mint) over a damaged run = %v", err)
	}

	items := f.listSystemHealth(t)
	if len(items) != 1 {
		t.Fatalf("run-projection health items = %d, want exactly 1", len(items))
	}
	item := items[0]
	wantID := runProjectionHealthItemID(damaged)
	if item.ID != wantID {
		t.Fatalf("item id = %q, want %q", item.ID, wantID)
	}
	if item.Status != domain.StatusOpen {
		t.Fatalf("item status = %q, want open", item.Status)
	}
	if item.ProjectID != "proj-1" {
		t.Fatalf("item project = %q, want proj-1", item.ProjectID)
	}
	if item.Subject.Type != domain.SubjectRun || item.Subject.ID != domain.SubjectID(damaged) ||
		item.Subject.RunID == nil || *item.Subject.RunID != damaged {
		t.Fatalf("item subject = %+v, want run binding to %q", item.Subject, damaged)
	}
	if item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory {
		t.Fatalf("item posture = %v, want advisory", item.Posture)
	}
	wantCreatedAt := f.at.Add(time.Hour)
	if item.CreatedAt == nil || !item.CreatedAt.Equal(wantCreatedAt) {
		t.Fatalf("item created_at = %v, want injected clock %v", item.CreatedAt, wantCreatedAt)
	}

	// The operator sees the item in the next bootstrap snapshot, not only a log
	// line.
	snapshot, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap (surface) = %v", err)
	}
	var found bool
	for _, snap := range snapshot.AttentionItems {
		if snap.Item.ID == wantID {
			found = true
		}
	}
	if !found {
		t.Fatalf("bootstrap snapshot did not carry the health item %q", wantID)
	}
}

// TestRunProjectionHealthIdempotent pins that repeated listing reads converge on
// one item with no item_version churn: the open-item pre-check skips the mint
// once the item exists.
func TestRunProjectionHealthIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	damaged := domain.RunID("run-damaged")
	f.seedTerminalRun(t, damaged, "inv-damaged", domain.ObservedStatusCompleted, domain.ObservedStatusRunning)

	for i := 0; i < 3; i++ {
		if _, err := f.service.ListRuns(ctx); err != nil {
			t.Fatalf("ListRuns pass %d = %v", i, err)
		}
	}
	if _, err := f.service.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap = %v", err)
	}

	items := f.listSystemHealth(t)
	if len(items) != 1 {
		t.Fatalf("run-projection health items = %d after repeated reads, want 1", len(items))
	}
	if items[0].ItemVersion != 1 {
		t.Fatalf("item_version = %d after repeated reads, want 1 (no churn)", items[0].ItemVersion)
	}
}

// TestRunProjectionHealthResolvesOnRepair pins the resolve path and, by serving
// the repaired run, proves the minted item's binding passes
// authenticateRunObservation: were it mis-bound, the still-open item would
// re-exclude the repaired run instead of letting it project and resolve.
func TestRunProjectionHealthResolvesOnRepair(t *testing.T) {
	ctx := context.Background()
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	damaged := domain.RunID("run-damaged")
	invocation := domain.InvocationID("inv-damaged")
	f.seedTerminalRun(t, damaged, invocation, domain.ObservedStatusCompleted, domain.ObservedStatusRunning)

	if _, err := f.service.ListRuns(ctx); err != nil {
		t.Fatalf("ListRuns (mint) = %v", err)
	}
	if items := f.listSystemHealth(t); len(items) != 1 || items[0].Status != domain.StatusOpen {
		t.Fatalf("after mint, items = %+v, want one open item", items)
	}

	// Repair: re-observe the invocation consistent with its recorded terminal
	// (last write wins), so the run projects cleanly again.
	f.observe(t, domain.InvocationObservation{
		InvocationID: invocation, RunID: damaged, Status: domain.ObservedStatusCompleted,
		Live: false, ObservedAt: f.at.Add(3 * time.Hour),
	})

	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns (resolve) = %v", err)
	}
	var served bool
	for _, run := range runs {
		if run.Run.ID == damaged {
			served = true
		}
	}
	if !served {
		t.Fatalf("repaired run %q not served; the health item's binding did not pass the gate", damaged)
	}

	item := f.getItem(t, runProjectionHealthItemID(damaged))
	if item.Status != domain.StatusResolved {
		t.Fatalf("item status = %q after repair, want resolved", item.Status)
	}
	if item.ItemVersion != 2 {
		t.Fatalf("item_version = %d after resolve, want 2", item.ItemVersion)
	}
}

// TestRunProjectionHealthWriteFailureDoesNotFailRead pins #767's isolation
// objective under a mint write failure: a pre-existing resolved item with the
// run's deterministic id makes the version-1 mint an illegal backward
// transition, so PutAttentionItem rejects it, yet the read still serves the
// healthy run and leaves the existing item untouched.
func TestRunProjectionHealthWriteFailureDoesNotFailRead(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	f := newCorpusFixture(t, signet.WithLogger(logger))
	f.seedAuthIdentity(t)
	healthy := domain.RunID("run-healthy")
	damaged := domain.RunID("run-damaged")
	f.seedTerminalRun(t, healthy, "inv-healthy", domain.ObservedStatusCompleted, domain.ObservedStatusCompleted)
	f.seedTerminalRun(t, damaged, "inv-damaged", domain.ObservedStatusCompleted, domain.ObservedStatusRunning)

	// Seed a resolved v2 item at the damaged run's deterministic id. The next
	// mint attempt (v1, open) cannot advance past it and fails closed.
	runID := damaged
	posture := domain.HealthPostureAdvisory
	blocker, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        runProjectionHealthItemID(damaged),
		ProjectID: "proj-1",
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(damaged), RunID: &runID,
		},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityHigh,
		Reason:            "pre-existing resolved incident",
		RequestedDecision: []domain.Action{domain.ActionRunDoctor, domain.ActionAcknowledge},
		ItemVersion:       2,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           &posture,
		Status:            domain.StatusResolved,
	}, nil)
	if err != nil {
		t.Fatalf("build blocker item: %v", err)
	}
	f.mustWrite(t, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, blocker)
	})

	snapshot, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap over a mint failure = %v, want the healthy run served", err)
	}
	if len(snapshot.Runs) != 1 || snapshot.Runs[0].Run.ID != healthy {
		t.Fatalf("served runs = %+v, want only %q", snapshot.Runs, healthy)
	}

	// The pre-existing item is unchanged and the failure was logged, not raised.
	item := f.getItem(t, runProjectionHealthItemID(damaged))
	if item.Status != domain.StatusResolved || item.ItemVersion != 2 {
		t.Fatalf("blocker item = {%q, v%d}, want resolved v2 untouched", item.Status, item.ItemVersion)
	}
	if !strings.Contains(logs.String(), "mint item") {
		t.Fatalf("logs = %q, want a warn recording the swallowed mint failure", logs.String())
	}
}
