package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestGetProjectRejectsTamperedRow proves scanProject cross-checks the extracted
// repository_id column against the canonical body: a direct-SQL row whose column
// disagrees with its body (the body decodes cleanly on its own) is refused as
// inconsistent, so a tamper of one side alone cannot rebind the project. Written
// through raw SQL because no store write path emits a mismatched pair.
func TestGetProjectRejectsTamperedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	err := st.Write(ctx, func(tx *WriteTx) error {
		// The body is a self-consistent, valid Project bound to repository 1; the
		// extracted column says 2. decode accepts the body, the cross-check rejects
		// the pair.
		body := `{"id":"project-tamper","repo":"owner/repo","repository_id":1}`
		if _, err := tx.tx.ExecContext(ctx,
			`INSERT INTO projects (project_id, repository_id, body) VALUES (?, ?, ?)`,
			"project-tamper", 2, body); err != nil {
			return err
		}
		if _, err := tx.GetProject(ctx, "project-tamper"); !errors.Is(err, errRowInconsistent) {
			return errors.New("tampered projects row was not rejected as inconsistent")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGetProjectRejectsUndecodableRow proves a stored body that decodes as JSON
// but fails domain re-validation (here an empty repository name) surfaces as the
// durable errRowInconsistent, not an undifferentiated error. This is the same
// trust class as a column mismatch, and the read re-gate relies on that sentinel
// to hold rather than retry the corrupt authority.
func TestGetProjectRejectsUndecodableRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	err := st.Write(ctx, func(tx *WriteTx) error {
		// A column that satisfies the table CHECK, but a body Project.Validate
		// rejects (empty repository name).
		body := `{"id":"project-invalid","repo":"","repository_id":5}`
		if _, err := tx.tx.ExecContext(ctx,
			`INSERT INTO projects (project_id, repository_id, body) VALUES (?, ?, ?)`,
			"project-invalid", 5, body); err != nil {
			return err
		}
		if _, err := tx.GetProject(ctx, "project-invalid"); !errors.Is(err, errRowInconsistent) {
			return errors.New("undecodable projects row was not classified as durable corruption")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRegisterProjectRejectsReplayOverCorruptColumn proves register-or-verify
// fails closed on a converging replay whose existing row has a corrupted copied
// repository_id column. putImmutable compares only the body, which is unchanged,
// so without the reconstruction step RegisterProject would report success over a
// row GetProject and intake admission reject; the verify makes it fail closed.
func TestRegisterProjectRejectsReplayOverCorruptColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	err := st.Write(ctx, func(tx *WriteTx) error {
		project, err := domain.NewProject("project-x", "owner/repo", 5)
		if err != nil {
			return err
		}
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		// Corrupt the copied column while leaving the canonical body intact.
		if _, err := tx.tx.ExecContext(ctx,
			`UPDATE projects SET repository_id = repository_id + 1 WHERE project_id = ?`,
			"project-x"); err != nil {
			return err
		}
		// A legitimate, body-identical replay must now fail closed rather than
		// converge on the unreadable row.
		if err := tx.RegisterProject(ctx, project); !errors.Is(err, errRowInconsistent) {
			return fmt.Errorf("replay over a corrupt column did not fail closed, got %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
