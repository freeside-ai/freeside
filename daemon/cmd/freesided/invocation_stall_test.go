package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// A stall files one advisory notice however often it is reported, recovery
// resolves it, a new stall files a new one, another invocation's notice is
// left alone, and the startup sweep resolves whatever a restart left open.
func TestInvocationStallNoticeLifecycle(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	now := func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
	report := invocationStallNotice(st, 5*time.Minute, now)
	const (
		inv   domain.InvocationID = "inv-implement-run-1"
		other domain.InvocationID = "inv-implement-run-1-b"
	)
	open := func() []domain.AttentionItem {
		t.Helper()
		var items []domain.AttentionItem
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			items, err = tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return items
	}
	openIDs := func() []string {
		t.Helper()
		var ids []string
		for _, item := range open() {
			ids = append(ids, string(item.ID))
		}
		return ids
	}
	step := func(id domain.InvocationID, stalled bool) {
		t.Helper()
		if err := report(ctx, id, stalled); err != nil {
			t.Fatalf("report(%s, %v): %v", id, stalled, err)
		}
	}

	step(inv, true)
	step(inv, true)
	items := open()
	if len(items) != 1 {
		t.Fatalf("open notices after two stall reports = %v, want one", openIDs())
	}
	first := items[0]
	if !strings.HasPrefix(string(first.ID), "invocation-stalled-inv-implement-run-1-") ||
		first.HealthDiagnostic == nil || first.HealthDiagnostic.Code != "invocation_stalled" ||
		first.HealthDiagnostic.Impairs != domain.ImpairedCapabilityNone ||
		first.Posture == nil || *first.Posture != domain.HealthPostureAdvisory ||
		first.Subject.Type != domain.SubjectSystem ||
		!slices.Equal(first.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) ||
		!strings.Contains(first.Reason, string(inv)) || !strings.Contains(first.Reason, "5m0s") {
		t.Fatalf("stall notice = %+v", first)
	}

	step(other, true)
	step(inv, false)
	ids := openIDs()
	if len(ids) != 1 || !strings.HasPrefix(ids[0], "invocation-stalled-inv-implement-run-1-b-") {
		t.Fatalf("open notices after recovery = %v, want only the other invocation's", ids)
	}

	step(inv, true)
	if ids := openIDs(); len(ids) != 2 || slices.Contains(ids, string(first.ID)) {
		t.Fatalf("open notices after a second stall = %v, want a new notice beside the other's", ids)
	}

	if err := concludeInvocationStallNotices(ctx, st); err != nil {
		t.Fatal(err)
	}
	if ids := openIDs(); len(ids) != 0 {
		t.Fatalf("open notices after the startup sweep = %v", ids)
	}
}
