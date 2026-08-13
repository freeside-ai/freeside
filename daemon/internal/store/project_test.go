package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func mustProject(t *testing.T, id domain.ProjectID, repo string, repositoryID int64) domain.Project {
	t.Helper()
	project, err := domain.NewProject(id, repo, repositoryID)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	return project
}

// TestRegisterProjectWriteOnce proves the project authority binding is written
// once: a byte-identical replay converges, a different repository for the same
// project id is an immutable conflict, and GetProject round-trips the stored
// binding.
func TestRegisterProjectWriteOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})

	err := st.Write(ctx, func(tx *store.WriteTx) error {
		project := mustProject(t, "project-alpha", "owner/repo", 84958515)
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		// Replay of the identical binding converges on the existing row.
		if err := tx.RegisterProject(ctx, project); err != nil {
			return errors.New("identical replay did not converge")
		}
		// A different repository for the same project id is an immutable conflict.
		rebind := mustProject(t, "project-alpha", "owner/other", 111)
		if err := tx.RegisterProject(ctx, rebind); !errors.Is(err, store.ErrImmutableConflict) {
			return errors.New("rebinding a project's repository was not refused")
		}
		got, err := tx.GetProject(ctx, "project-alpha")
		if err != nil {
			return err
		}
		if got != project {
			return errors.New("GetProject did not round-trip the registered binding")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRegisterProjectSharedRepository proves two distinct projects may operate
// on one repository: there is no UNIQUE(repository_id), so which project a
// repository's intake mints under stays #659's configuration, not an invariant
// forbidden here.
func TestRegisterProjectSharedRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})

	err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, mustProject(t, "project-a", "owner/repo", 84958515)); err != nil {
			return err
		}
		if err := tx.RegisterProject(ctx, mustProject(t, "project-b", "owner/repo", 84958515)); err != nil {
			return errors.New("second project on the same repository was refused")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGetProjectNotFound proves an unregistered project reports ErrNotFound, the
// fail-closed signal the mint gate and read re-gate propagate.
func TestGetProjectNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})

	err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetProject(ctx, "project-absent"); !errors.Is(err, store.ErrNotFound) {
			return errors.New("absent project did not report ErrNotFound")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
