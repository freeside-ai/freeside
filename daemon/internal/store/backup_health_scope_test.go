package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestScopedCheckpointHealthPreservesRecipePolicyAndLiveGap(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := tempDBPath(t)
	files, err := store.NewDefaultLocalBackupFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newFixtures(t).artifact
	recipeA := domain.Digest("sha256:recipe-a")
	approved := map[domain.Digest]bool{recipeA: true, fixtureRecipe: true}
	source, err := files.NewCheckpointHealthSource(backupArtifactSet{fixture.Digest: true}, approved, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := openStoreAt(t, path, store.Options{ApprovedRecipes: approved, BackupHealthSource: source})
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutArtifact(ctx, fixture) }); err != nil {
		t.Fatal(err)
	}
	producer, err := files.NewProducer(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		recipes map[domain.Digest]bool
		healthy bool
	}{
		{name: "none"},
		{name: "A", recipes: map[domain.Digest]bool{recipeA: true}},
		{name: "B", recipes: map[domain.Digest]bool{fixtureRecipe: true}, healthy: true},
		{name: "A+B", recipes: approved, healthy: true},
	} {
		scoped, err := files.NewScopedCheckpointHealthSource(test.recipes)
		if err != nil {
			t.Fatal(err)
		}
		health, err := st.BackupHealthWithSource(ctx, scoped)
		if !test.healthy && !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
			t.Fatalf("scope %s should reject recipe B: %+v, %v", test.name, health, err)
		}
		if test.healthy && (err != nil || health.ArtifactClosure != domain.BackupHealthHealthy) {
			t.Fatalf("scope %s: %+v, %v", test.name, health, err)
		}
		assertHealthyLocalBackup(t, st)
	}
	scoped, err := files.NewScopedCheckpointHealthSource(approved)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, "scope-gap", "unknown.scope", []byte(`{}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := producer.Maintain(ctx); !errors.Is(err, store.ErrBackupClosureIncomplete) {
		t.Fatalf("live closure gap: %v", err)
	}
	health, err := st.BackupHealthWithSource(ctx, scoped)
	if err != nil || health.ArtifactClosure != domain.BackupHealthUnhealthy || health.Encryption != domain.BackupHealthHealthy {
		t.Fatalf("scoped source lost live gap over valid checkpoint: %+v, %v", health, err)
	}
}

func TestBackupHealthWithSourceValidatesResultWithoutReplacingDefault(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	valid := domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}
	st := openStore(t, store.Options{BackupHealthSource: store.BackupHealthSourceFunc(func(context.Context, store.BackupHealthContext) (domain.BackupHealth, error) {
		return valid, nil
	})})
	expectedState := backupHealthContext(t, st)
	sourceError := errors.New("request source failed")
	for _, test := range []struct {
		name   string
		source store.BackupHealthSource
		want   error
	}{
		{name: "missing", want: domain.ErrBackupHealthUnavailable},
		{name: "invalid", source: store.BackupHealthSourceFunc(func(_ context.Context, state store.BackupHealthContext) (domain.BackupHealth, error) {
			if state != expectedState {
				t.Fatalf("source state = %+v, want %+v", state, expectedState)
			}
			return domain.BackupHealth{}, nil
		}), want: domain.ErrInvalidBackupHealthStatus},
		{name: "failed", source: store.BackupHealthSourceFunc(func(context.Context, store.BackupHealthContext) (domain.BackupHealth, error) {
			return domain.BackupHealth{}, sourceError
		}), want: sourceError},
	} {
		if _, err := st.BackupHealthWithSource(ctx, test.source); !errors.Is(err, test.want) {
			t.Fatalf("%s source: %v", test.name, err)
		}
		assertHealthyLocalBackup(t, st)
	}
}
