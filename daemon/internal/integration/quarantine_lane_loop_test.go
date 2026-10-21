package integration_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestTaskMarkerMismatchHoldsUnderOneNotice: a publication task that
// disagrees with its run's remediation record, while the record itself
// authenticates, is refused by the publication lane alone. The production
// lane accepts the record every pass, so a shared notice was retired and
// re-raised each pass: the Wave 8 exit run minted 89 notices in two minutes
// and notified every device each time (#2102). The refusal keeps one notice
// of its own, and repairing the task retires it.
func TestTaskMarkerMismatchHoldsUnderOneNotice(t *testing.T) {
	t.Parallel()
	p, _ := prepareDispatchedRemediationPublicationTask(t)
	key := "production-publication/" + string(p.runID)
	original := readOutboxPayload(t, p, key)
	// A title the remediation never recorded decodes cleanly, so only the
	// publication lane's comparison with the remediation record refuses it.
	title := []byte(`"title":"`)
	if bytes.Count(original, title) != 1 {
		t.Fatalf("publication task carries %d titles, want one", bytes.Count(original, title))
	}
	writeOutboxPayload(t, p, key, bytes.Replace(
		original, title, []byte(`"title":"Tampered `), 1))

	const passes = 4
	for range passes {
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		if result.PublicationTasksCompleted != 0 || result.ReadyItemsCreated != 0 {
			t.Fatalf("mismatched task advanced = %#v", result)
		}
	}
	notices := quarantineNotices(t, p)
	want := "production-task-marker-quarantined-1-" + string(p.runID)
	if len(notices) != 1 || notices[0].ID != domain.ItemID(want) ||
		notices[0].Status != domain.StatusOpen {
		t.Fatalf("notices after %d passes = %v, want one open %s", passes, noticeStates(notices), want)
	}
	held := notices[0]

	writeOutboxPayload(t, p, key, original)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	released := productionItemRecord(t, p, want)
	if released.Status != domain.StatusSuperseded || released.ItemVersion <= held.ItemVersion {
		t.Fatalf("task-marker notice after repair = %s at version %d", released.Status, released.ItemVersion)
	}
	if notices := quarantineNotices(t, p); len(notices) != 1 {
		t.Fatalf("notices after repair = %v, want only the released one", noticeStates(notices))
	}
}

func quarantineNotices(t *testing.T, p *productionPublicationHarness) []domain.AttentionItem {
	t.Helper()
	items, err := p.attention.ListAttentionItems(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var notices []domain.AttentionItem
	for _, item := range items {
		if strings.Contains(string(item.Item.ID), "-quarantined-") {
			notices = append(notices, item.Item)
		}
	}
	return notices
}

func noticeStates(items []domain.AttentionItem) []string {
	states := make([]string, 0, len(items))
	for _, item := range items {
		states = append(states, string(item.ID)+"="+string(item.Status))
	}
	return states
}
