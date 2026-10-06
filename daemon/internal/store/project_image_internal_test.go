package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestProjectImageMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestGetProjectImageByRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openProjectImageStore(t)
	image := testProjectImage(t)
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordProjectImage(ctx, image)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		got, found, err := tx.GetProjectImageByRef(ctx, image.ImageRef)
		if err != nil {
			return err
		}
		if !found || got.ID != image.ID || got.CommitSHA != image.CommitSHA {
			t.Fatalf("by-ref lookup = (%+v, %v), want image %s", got, found, image.ID)
		}
		_, found, err = tx.GetProjectImageByRef(
			ctx, domain.ImageRef("example.test/project@sha256:"+strings.Repeat("e", 64)))
		if err != nil {
			return err
		}
		if found {
			t.Fatal("unrecorded image ref reported as found")
		}
		if _, _, err := tx.GetProjectImageByRef(ctx, ""); err == nil {
			t.Fatal("empty image ref was accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProjectImageImmutableConflictAndTampering(t *testing.T) {
	t.Parallel()
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

func testProjectImageWithEnvironment(t *testing.T) domain.ProjectImage {
	t.Helper()
	legacy := testProjectImage(t)
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: legacy.Repository, RepositoryID: legacy.RepositoryID,
		CommitSHA: legacy.CommitSHA, RecipeDigest: legacy.RecipeDigest,
		PreparationCommand: legacy.PreparationCommand,
		BaseImageRef:       legacy.BaseImageRef,
		ImageRef:           domain.ImageRef("example.test/project@sha256:" + strings.Repeat("d", 64)),
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: strings.Repeat("1", 64),
			PackageLockSHA256: strings.Repeat("2", 64),
			PreparationDigest: domain.Digest("sha256:" + strings.Repeat("3", 64)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func projectImageEnvironmentColumn(t *testing.T, s *Store, id domain.Digest) sql.NullString {
	t.Helper()
	var column sql.NullString
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT environment_digest FROM project_images WHERE id = ?`, id).Scan(&column); err != nil {
		t.Fatal(err)
	}
	return column
}

// 0091 lands on a store that already holds a project image: the row's body is
// untouched, no environment is invented for it, and it reconstructs under its
// original ID as a record without one.
func TestProjectImageEnvironmentMigrationKeepsLegacyRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0091_")

	legacy := testProjectImage(t)
	current, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	// A row written before the environment existed has no such key at all.
	body := strings.Replace(string(current), `,"environment":null`, "", 1)
	if body == string(current) {
		t.Fatal("fixture body carries no environment key to strip")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_images
		(id, repository, repository_id, commit_sha, recipe_digest, base_image_ref, image_ref, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		legacy.ID, legacy.Repository, legacy.RepositoryID, legacy.CommitSHA,
		legacy.RecipeDigest, legacy.BaseImageRef, legacy.ImageRef, body); err != nil {
		t.Fatalf("seed the pre-migration row: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	var stored string
	var column sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT body, environment_digest FROM project_images WHERE id = ?`, legacy.ID,
	).Scan(&stored, &column); err != nil {
		t.Fatal(err)
	}
	if stored != body || column.Valid {
		t.Fatalf("migrated row = body %q, environment_digest %+v; want the body unchanged and NULL",
			stored, column)
	}
	got, err := scanProjectImage(db.QueryRowContext(ctx, getProjectImageByRefSQL, legacy.ImageRef))
	if err != nil {
		t.Fatalf("reconstruct the legacy row: %v", err)
	}
	if got.ID != legacy.ID || got.Environment != nil {
		t.Fatalf("legacy row = %+v, want ID %s with no environment", got, legacy.ID)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE project_images SET environment_digest = '' WHERE id = ?`, legacy.ID); err == nil {
		t.Fatal("the migrated schema admitted an empty environment digest")
	}
}

func TestProjectImageEnvironmentRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openProjectImageStore(t)
	legacy, image := testProjectImage(t), testProjectImageWithEnvironment(t)
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordProjectImage(ctx, legacy); err != nil {
			return err
		}
		if err := tx.RecordProjectImage(ctx, image); err != nil {
			return err
		}
		return tx.RecordProjectImage(ctx, image)
	}); err != nil {
		t.Fatalf("record/replay: %v", err)
	}
	wantDigest, err := image.Environment.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if column := projectImageEnvironmentColumn(t, s, image.ID); !column.Valid ||
		column.String != string(wantDigest) {
		t.Fatalf("environment_digest = %+v, want %s", column, wantDigest)
	}
	if column := projectImageEnvironmentColumn(t, s, legacy.ID); column.Valid {
		t.Fatalf("legacy environment_digest = %+v, want NULL", column)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		got, found, err := tx.GetProjectImageByRef(ctx, image.ImageRef)
		if err != nil {
			return err
		}
		if !found || got.ID != image.ID || got.Environment == nil ||
			*got.Environment != *image.Environment {
			t.Fatalf("round trip = (%+v, %v), want environment %+v", got, found, *image.Environment)
		}
		gotLegacy, err := tx.GetProjectImage(ctx, legacy.ID)
		if err != nil {
			return err
		}
		if gotLegacy.Environment != nil {
			t.Fatalf("legacy round trip gained environment %+v", *gotLegacy.Environment)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Compatibility evidence lives in the body and is mirrored by one column. A
// row altered through either alone must not reconstruct, in either direction:
// evidence stripped from a record that had it, or grafted onto one that did
// not.
func TestProjectImageEnvironmentTamperingFailsClosed(t *testing.T) {
	t.Parallel()
	legacy, image := testProjectImage(t), testProjectImageWithEnvironment(t)
	other := domain.ProjectImageEnvironment{
		PackageJSONSHA256: strings.Repeat("9", 64),
		PackageLockSHA256: image.Environment.PackageLockSHA256,
		PreparationDigest: image.Environment.PreparationDigest,
	}
	otherDigest, err := other.Digest()
	if err != nil {
		t.Fatal(err)
	}
	otherBody, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		target domain.ProjectImage
		query  string
		args   []any
		want   error
	}{
		{
			"column cleared", image,
			`UPDATE project_images SET environment_digest = NULL WHERE id = ?`, nil,
			errRowInconsistent,
		},
		{
			"column replaced", image,
			`UPDATE project_images SET environment_digest = ? WHERE id = ?`,
			[]any{string(otherDigest)},
			errRowInconsistent,
		},
		{
			"column grafted onto a legacy row", legacy,
			`UPDATE project_images SET environment_digest = ? WHERE id = ?`,
			[]any{string(otherDigest)},
			errRowInconsistent,
		},
		{
			"body environment stripped", image,
			`UPDATE project_images SET body = json_set(body, '$.environment', NULL),
				environment_digest = NULL WHERE id = ?`, nil,
			domain.ErrProjectImageInconsistent,
		},
		{
			"body environment replaced with its column", image,
			`UPDATE project_images SET body = json_set(body, '$.environment', json(?)),
				environment_digest = ? WHERE id = ?`,
			[]any{string(otherBody), string(otherDigest)},
			domain.ErrProjectImageInconsistent,
		},
		{
			"body environment grafted onto a legacy row with its column", legacy,
			`UPDATE project_images SET body = json_set(body, '$.environment', json(?)),
				environment_digest = ? WHERE id = ?`,
			[]any{string(otherBody), string(otherDigest)},
			domain.ErrProjectImageInconsistent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openProjectImageStore(t)
			if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				return tx.RecordProjectImage(ctx, tc.target)
			}); err != nil {
				t.Fatal(err)
			}
			result, err := s.db.ExecContext(ctx, tc.query, append(tc.args, tc.target.ID)...)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				t.Fatalf("tamper changed %d rows (%v), want 1", changed, err)
			}
			err = s.Read(ctx, func(tx *ReadTx) error {
				_, _, err := tx.GetProjectImageByRef(ctx, tc.target.ImageRef)
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("tampered read = %v, want %v", err, tc.want)
			}
		})
	}
}
