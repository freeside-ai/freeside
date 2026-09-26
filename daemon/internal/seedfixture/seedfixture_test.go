package seedfixture_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/seedfixture"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// openFixtureStore opens a store with the attended_dev floor the fixture's
// admissions require (see the package comment).
func openFixtureStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: seedfixture.AdmissionCapabilities,
		},
	})
}

func TestRepresentativeSeedsDeclaredInventory(t *testing.T) {
	ctx := t.Context()
	st := openFixtureStore(t)
	inventory, err := seedfixture.Seed(ctx, st, seedfixture.Representative)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	var got seedfixture.Inventory
	var tasks []store.Snapshotted[domain.Task]
	var runs []store.Snapshotted[domain.Run]
	openByType := map[domain.AttentionType]int{}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if tasks, err = tx.ListTasks(ctx); err != nil {
			return err
		}
		if runs, err = tx.ListRuns(ctx); err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Value.Status == domain.StatusOpen {
				openByType[item.Value.Type]++
			}
		}
		projects := map[domain.ProjectID]bool{}
		for _, run := range runs {
			if projects[run.Value.ProjectID] {
				continue
			}
			project, err := tx.GetProject(ctx, run.Value.ProjectID)
			if err != nil {
				return err
			}
			if project.ID != run.Value.ProjectID || !strings.HasPrefix(project.Repo, "example/") {
				t.Errorf("project %s = %+v", run.Value.ProjectID, project)
			}
			projects[run.Value.ProjectID] = true
			got.Projects++
		}
		for _, run := range runs {
			_, err := tx.GetWorkUnitPRBinding(ctx, domain.WorkUnitIDForRun(run.Value.ID))
			switch {
			case err == nil:
				got.WorkUnitPRBindings++
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		got.Tasks, got.Runs, got.AttentionItems = len(tasks), len(runs), len(items)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != inventory {
		t.Fatalf("stored inventory = %+v, declared %+v", got, inventory)
	}
	for _, itemType := range seedfixture.OpenItemTypes {
		if openByType[itemType] != 1 {
			t.Errorf("open %s items = %d, want 1", itemType, openByType[itemType])
		}
	}
	if len(openByType) != len(seedfixture.OpenItemTypes) {
		t.Errorf("open item types = %v, want exactly %v", openByType, seedfixture.OpenItemTypes)
	}

	// Every run and task reads back through the same authenticated
	// projections the app's task list, task detail, and inbox use.
	service := signet.NewService(st)
	if _, err := service.ListRuns(ctx); err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	for _, run := range runs {
		if _, err := service.GetRun(ctx, run.Value.ID); err != nil {
			t.Errorf("GetRun %s: %v", run.Value.ID, err)
		}
		if _, err := service.GetRunTimeline(ctx, run.Value.ID); err != nil {
			t.Errorf("GetRunTimeline %s: %v", run.Value.ID, err)
		}
	}
	for _, task := range tasks {
		if task.Value.Name.Source == domain.DisplayNameSourceIdentifier {
			t.Errorf("task %s is unnamed", task.Value.ID)
		}
		if _, err := service.GetTaskTimeline(ctx, task.Value.ID); err != nil {
			t.Errorf("GetTaskTimeline %s: %v", task.Value.ID, err)
		}
	}
	if _, err := service.ListAttentionItems(ctx); err != nil {
		t.Fatalf("ListAttentionItems: %v", err)
	}
}

// TestRepresentativeLeavesNoPendingIntent pins that every dispatch intent the
// fixture records is already dispatched, so an engine a later driver start
// opens on the seeded store has nothing to execute.
func TestRepresentativeLeavesNoPendingIntent(t *testing.T) {
	ctx := t.Context()
	st := openFixtureStore(t)
	if _, err := seedfixture.Seed(ctx, st, seedfixture.Representative); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	kinds := []string{
		string(domain.ProductionInvocationRequestedKind),
		string(domain.SpecificationInvocationRequestedKind),
		"publish.publication",
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		for _, kind := range kinds {
			pending, err := tx.ListPendingOutbox(ctx, kind)
			if err != nil {
				return err
			}
			if len(pending) != 0 {
				t.Errorf("pending %s intents = %d, want 0", kind, len(pending))
			}
			dispatched, err := tx.ListDispatchedOutbox(ctx, kind)
			if err != nil {
				return err
			}
			if len(dispatched) == 0 {
				t.Errorf("dispatched %s intents = 0, want the fixture's records", kind)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSeedRefusesNonEmptyStore(t *testing.T) {
	ctx := t.Context()
	st := openFixtureStore(t)
	if _, err := seedfixture.Seed(ctx, st, seedfixture.Representative); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	if _, err := seedfixture.Seed(ctx, st, seedfixture.Representative); !errors.Is(err, seedfixture.ErrStoreNotEmpty) {
		t.Fatalf("second Seed = %v, want ErrStoreNotEmpty", err)
	}
}

func TestParse(t *testing.T) {
	if name, err := seedfixture.Parse("representative"); err != nil || name != seedfixture.Representative {
		t.Fatalf("Parse(representative) = %q, %v", name, err)
	}
	_, err := seedfixture.Parse("nope")
	if err == nil || !strings.Contains(err.Error(), "representative") {
		t.Fatalf("Parse(nope) = %v, want an error naming the known fixtures", err)
	}
}
