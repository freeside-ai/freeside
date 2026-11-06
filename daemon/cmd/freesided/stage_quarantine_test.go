package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func stageIntentItems(t *testing.T, st *store.Store) []domain.AttentionItem {
	t.Helper()
	var items []domain.AttentionItem
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		open, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		for _, item := range open {
			if strings.HasPrefix(string(item.ID), stageIntentItemPrefix) {
				items = append(items, item)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

// TestConvergeStageIntentItems checks the quarantine item's life (#386): one
// advisory item per quarantined intent that names the file, the invocation,
// and the cause but never the error; the same item kept by a start that
// still quarantines the intent; and the item resolved by a start that no
// longer does.
func TestConvergeStageIntentItems(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dir := "/state/claude-driver"
	errText := "decoder saw record-content-from-the-file"
	corrupt := claude.IntentFailure{
		Entry: "tcorrupt.json", Path: dir + "/tcorrupt.json",
		Cause: claude.QuarantineUnreadable, Err: errors.New(errText),
	}
	conflict := claude.IntentFailure{
		Entry: "tconflict.json", Path: dir + "/tconflict.json", Invocation: "inv-implement-run-2",
		Cause: claude.QuarantineExportConflict, Err: errors.New(errText),
	}

	if err := convergeStageIntentItems(ctx, st, []claude.IntentFailure{corrupt, conflict}, now); err != nil {
		t.Fatal(err)
	}
	items := stageIntentItems(t, st)
	if len(items) != 2 {
		t.Fatalf("items = %+v, want one per quarantined intent", items)
	}
	byEntry := map[string]domain.AttentionItem{}
	for _, item := range items {
		if item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory ||
			item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != "stage_intent_quarantined" ||
			item.HealthDiagnostic.Impairs != domain.ImpairedCapabilityNone {
			t.Fatalf("item = %+v, want an advisory stage_intent_quarantined item", item)
		}
		if strings.Contains(item.Reason, "record-content") || strings.Contains(item.Reason, "decoder saw") {
			t.Fatalf("item carries the error text: %q", item.Reason)
		}
		for _, failure := range []claude.IntentFailure{corrupt, conflict} {
			if strings.HasPrefix(string(item.ID), stageIntentItemPrefix+stageIntentCauseKey(failure)+"-") {
				byEntry[failure.Entry] = item
			}
		}
	}
	if reason := byEntry[corrupt.Entry].Reason; !strings.Contains(reason, `"tcorrupt.json" in /state/claude-driver/tcorrupt.json`) ||
		!strings.Contains(reason, "an invocation the file does not name readably") ||
		!strings.Contains(reason, "could not be read or decoded") {
		t.Fatalf("corrupt intent's reason = %q", reason)
	}
	if reason := byEntry[conflict.Entry].Reason; !strings.Contains(reason, "invocation inv-implement-run-2") ||
		!strings.Contains(reason, "no longer matches the export record") ||
		!strings.Contains(reason, "ward-exports") {
		t.Fatalf("conflicting intent's reason = %q", reason)
	}

	// A start that still quarantines the conflict keeps its item; the
	// corrupt intent, no longer quarantined, has its item resolved.
	if err := convergeStageIntentItems(ctx, st, []claude.IntentFailure{conflict}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := stageIntentItems(t, st); len(got) != 1 || got[0].ID != byEntry[conflict.Entry].ID {
		t.Fatalf("items after the corrupt intent is repaired = %+v", got)
	}
	// A start that quarantines the same file for another class replaces the
	// item, so its class and repair text are current.
	reclassified := conflict
	reclassified.Cause = claude.QuarantineUnreadable
	if err := convergeStageIntentItems(ctx, st, []claude.IntentFailure{reclassified}, now.Add(90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := stageIntentItems(t, st); len(got) != 1 || got[0].ID == byEntry[conflict.Entry].ID ||
		!strings.Contains(got[0].Reason, "could not be read or decoded") {
		t.Fatalf("items after the class changes = %+v", got)
	}
	if err := convergeStageIntentItems(ctx, st, nil, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := stageIntentItems(t, st); len(got) != 0 {
		t.Fatalf("items after a clean start = %+v", got)
	}
}

func TestStageIntentCauseCoversEveryQuarantineCause(t *testing.T) {
	t.Parallel()
	for _, cause := range claude.AllQuarantineCauses {
		if stageIntentFailureText(cause) == stageIntentFailureText("") || stageIntentRepairText(cause) == "" {
			t.Fatalf("cause %q has no item text of its own", cause)
		}
	}
}
