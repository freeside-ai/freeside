package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TemplateDatabase returns a database file migrated to head without a sync
// epoch. It supports test fixtures; production callers should use Open.
// Each copy seeds its own epoch when Open first opens it.
func TemplateDatabase(ctx context.Context) ([]byte, error) {
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
	if err := migrate(ctx, db, migrations.FS); err != nil {
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
