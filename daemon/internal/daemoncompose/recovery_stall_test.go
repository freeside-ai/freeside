package daemoncompose

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// A stalled recovery files one advisory notice however often it is
// reported, the deferral's end resolves it, another invocation's notice is
// left alone, and the startup sweep resolves whatever a restart left open.
func TestStageRecoveryNoticeLifecycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	now := func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) }
	report := StageRecoveryNotice(st, now)
	const (
		inv   domain.InvocationID = "inv-implement-run-1"
		other domain.InvocationID = "inv-implement-run-1-b"
	)
	stalled := func(id domain.InvocationID, cause string) stage.RecoveryStall {
		return stage.RecoveryStall{
			InvocationID: id, RunID: "freeside-run-1", Phase: "import_pending",
			Elapsed: 5*time.Minute + 300*time.Millisecond, Grace: 30 * time.Minute,
			Error: cause, Stalled: true,
		}
	}
	openIDs := func() []string {
		t.Helper()
		var ids []string
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
			for _, item := range items {
				ids = append(ids, string(item.ID))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	step := func(r stage.RecoveryStall) {
		t.Helper()
		if err := report(ctx, r); err != nil {
			t.Fatalf("report(%+v): %v", r, err)
		}
	}

	step(stalled(inv, "finish released export: fetch import base: network down"))
	step(stalled(inv, "a different error"))
	ids := openIDs()
	if len(ids) != 1 || !IsStageRecoveryNotice(ids[0], inv) {
		t.Fatalf("open notices after two stall reports = %v, want one for %s", ids, inv)
	}
	var first domain.AttentionItem
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		first, err = tx.GetAttentionItem(ctx, domain.ItemID(ids[0]))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if first.HealthDiagnostic == nil || first.HealthDiagnostic.Code != "stage_recovery_stalled" ||
		first.HealthDiagnostic.Impairs != domain.ImpairedCapabilityNone ||
		first.Posture == nil || *first.Posture != domain.HealthPostureAdvisory ||
		first.Subject.Type != domain.SubjectSystem ||
		!slices.Equal(first.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) {
		t.Fatalf("recovery notice = %+v", first)
	}
	for _, want := range []string{string(inv), "freeside-run-1", "import_pending", "5m0s", "30m0s", "network down"} {
		if !strings.Contains(first.Reason, want) {
			t.Errorf("reason %q does not name %q", first.Reason, want)
		}
	}

	step(stalled(other, "network down"))
	step(stage.RecoveryStall{InvocationID: inv})
	if ids := openIDs(); len(ids) != 1 || !IsStageRecoveryNotice(ids[0], other) {
		t.Fatalf("open notices after the deferral ended = %v, want only the other invocation's", ids)
	}
	if IsStageRecoveryNotice(openIDs()[0], inv) {
		t.Fatalf("the notice of %s matches %s", other, inv)
	}

	if err := concludeInvocationStallNotices(ctx, st); err != nil {
		t.Fatal(err)
	}
	if ids := openIDs(); len(ids) != 0 {
		t.Fatalf("open notices after the startup sweep = %v", ids)
	}
}
