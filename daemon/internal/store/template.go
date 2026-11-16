package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing/fstest"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TemplateDatabase returns a database file migrated to head without a sync
// epoch. It supports test fixtures; production callers should use Open.
// Each copy seeds its own epoch when Open first opens it.
func TemplateDatabase(ctx context.Context) ([]byte, error) {
	return templateDatabase(ctx, migrations.FS)
}

// TemplateDatabaseAt is TemplateDatabase at an older schema version, for
// fixtures that stand in for a database an older build left. Version must
// be between the migration baseline and head: no history below the
// baseline exists to build from. The database records the baseline's own
// row, as one created after the squash does.
func TemplateDatabaseAt(ctx context.Context, version int) ([]byte, error) {
	prefix, err := migrationsThrough(version)
	if err != nil {
		return nil, err
	}
	return templateDatabase(ctx, prefix)
}

func templateDatabase(ctx context.Context, fsys fs.FS) ([]byte, error) {
	dir, err := os.MkdirTemp("", "freeside-store-template-*")
	if err != nil {
		return nil, fmt.Errorf("create template directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "store.db")
	db, err := openDB(path, Options{})
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, fsys); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Closing the last connection checkpoints the WAL into the database file.
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("close template database: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open template directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile("store.db")
}

// migrationsThrough returns the embedded migrations from the baseline up to
// and including version, with the squashed history the baseline needs.
// loadMigrationHistory has pinned the versions as contiguous from the
// baseline, so the first version-baseline+1 files are exactly that history.
func migrationsThrough(version int) (fs.FS, error) {
	history, err := loadMigrationHistory(migrations.FS)
	if err != nil {
		return nil, err
	}
	if version < history.baseline || version > history.head() {
		return nil, fmt.Errorf("schema version %d is outside baseline %d through head %d", version, history.baseline, history.head())
	}
	prefix := fstest.MapFS{}
	names := history.files[:version-history.baseline+1]
	if history.baseline > 1 {
		names = append([]string{squashedHistoryFile}, names...)
	}
	for _, name := range names {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		prefix[name] = &fstest.MapFile{Data: body}
	}
	return prefix, nil
}
