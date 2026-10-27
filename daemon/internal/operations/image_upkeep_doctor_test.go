package operations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// An upkeep host fault files one advisory item, so unattended admission keeps
// running work whose image still serves its base; a rerun with another detail
// updates it, and the next pass that reaches a verdict clears it.
func TestDoctorConvergesTheImageUpkeepHostFaultAsAdvisory(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/freeside.db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // Test cleanup cannot affect the assertion.
	attention := signet.NewService(st)
	doctor := operations.Doctor{Store: st, Attention: attention, ProjectID: "project-system"}
	openItems := func() []domain.AttentionItem {
		t.Helper()
		snapshots, err := attention.ListAttentionItems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var open []domain.AttentionItem
		for _, snapshot := range snapshots {
			if snapshot.Item.Type == domain.AttentionSystemHealth && snapshot.Item.Status == domain.StatusOpen {
				open = append(open, snapshot.Item)
			}
		}
		return open
	}

	for _, fault := range []string{"the project-image build failed", "the project-image build failed", "the room failed"} {
		if err := doctor.ConvergeImageUpkeep(ctx, fault); err != nil {
			t.Fatal(err)
		}
	}
	open := openItems()
	if len(open) != 1 {
		t.Fatalf("open items = %d, want 1", len(open))
	}
	item := open[0]
	if !strings.HasPrefix(string(item.ID), "system-health-doctor-image_upkeep_host_fault-") ||
		item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory ||
		item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != "image_upkeep_host_fault" ||
		!strings.Contains(item.Reason, "the room failed") {
		t.Fatalf("item = %+v, want one advisory image_upkeep_host_fault item naming the latest fault", item)
	}

	if err := doctor.ConvergeImageUpkeep(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if open := openItems(); len(open) != 0 {
		t.Fatalf("open items after a verdict = %+v, want none", open)
	}
}
