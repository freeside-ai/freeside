package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func testProjectImage(t *testing.T) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA:          "6ab4e3dff2be53f74bde9b8b3150290775152f9f",
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		PreparationCommand: []string{"/usr/local/bin/freeside-project-prepare"},
		BaseImageRef:       domain.ImageRef("example.test/base@sha256:" + strings.Repeat("a", 64)),
		ImageRef:           domain.ImageRef("example.test/project@sha256:" + strings.Repeat("b", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func openProjectImageStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestProjectImageMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0016_")
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_images`).Scan(&rows); err != nil {
		t.Fatalf("count project_images: %v", err)
	}
	if rows != 0 {
		t.Fatalf("project_images = %d rows after migration, want no invented provenance", rows)
	}
}

func TestProjectImageRoundTripAndReplay(t *testing.T) {
	ctx := context.Background()
	s := openProjectImageStore(t)
	image := testProjectImage(t)
	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordProjectImage(ctx, image); err != nil {
			return err
		}
		return tx.RecordProjectImage(ctx, image)
	}); err != nil {
		t.Fatalf("record/replay: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("internal project-image record bumped revision %d -> %d", before.Revision, after.Revision)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetProjectImage(ctx, image.ID)
		if err != nil {
			return err
		}
		if got.ID != image.ID || got.ImageRef != image.ImageRef {
			t.Fatalf("round trip = %+v, want %+v", got, image)
		}
		list, err := tx.ListProjectImages(ctx, image.RepositoryID)
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].ID != image.ID {
			t.Fatalf("list = %+v, want one image %s", list, image.ID)
		}
		recorded, err := tx.ProjectImageRefRecorded(ctx, image.ImageRef)
		if err != nil {
			return err
		}
		if !recorded {
			t.Fatalf("global image ref %q was not reported as recorded", image.ImageRef)
		}
		missing, err := tx.ProjectImageRefRecorded(
			ctx, domain.ImageRef("example.test/project@sha256:"+strings.Repeat("d", 64)))
		if err != nil {
			return err
		}
		if missing {
			t.Fatal("unrecorded global image ref was reported as recorded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectImageImmutableConflictAndTampering(t *testing.T) {
	ctx := context.Background()
	s := openProjectImageStore(t)
	image := testProjectImage(t)
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordProjectImage(ctx, image)
	}); err != nil {
		t.Fatal(err)
	}

	changed, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: image.Repository, RepositoryID: image.RepositoryID,
		CommitSHA:    "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: image.RecipeDigest, PreparationCommand: image.PreparationCommand,
		BaseImageRef: image.BaseImageRef, ImageRef: image.ImageRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordProjectImage(ctx, changed)
	}); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("conflicting provenance for one image ref = %v, want ErrImmutableConflict", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE project_images SET body = json_set(body, '$.repository_id', 7) WHERE id = ?`,
		image.ID); err != nil {
		t.Fatal(err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetProjectImage(ctx, image.ID)
		return err
	})
	if !errors.Is(err, domain.ErrProjectImageInconsistent) {
		t.Fatalf("tampered project image read = %v, want identity rejection", err)
	}
}
