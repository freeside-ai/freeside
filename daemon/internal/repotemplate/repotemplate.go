// Package repotemplate copies small, test-owned repository fixtures from memory.
package repotemplate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// FilesKey returns a deterministic key for file names and contents, preserving
// arbitrary bytes. JSON string encoding alone replaces invalid UTF-8 bytes.
func FilesKey(files map[string]string) string {
	encoded := make(map[string][]byte, len(files))
	for path, content := range files {
		encoded[base64.StdEncoding.EncodeToString([]byte(path))] = []byte(content)
	}
	key, _ := json.Marshal(encoded)
	return string(key)
}

// Cache builds each keyed directory tree once per test binary and gives every
// caller its own copy. Its zero value is ready to use. Metadata must be immutable.
type Cache[M any] struct {
	mu      sync.Mutex
	entries map[string]*template[M]
}

type template[M any] struct {
	mu    sync.Mutex
	built bool
	tree  []entry
	meta  M
}

type entry struct {
	path string
	mode fs.FileMode
	data []byte
	link string
}

// Copy returns a fresh t.TempDir containing the keyed fixture and its metadata.
// Build must finish all writes before returning; Git builders must disable
// automatic maintenance. Leave no absolute paths in the tree, including git
// alternates or worktree links. A plain git init fixture is suitable; a local
// clone or git worktree is not. Failed builds are not cached.
func (c *Cache[M]) Copy(t testing.TB, key string, build func() (string, M)) (string, M) {
	t.Helper()
	tree, meta := c.load(t, key, build)
	dir := t.TempDir()
	if err := restore(dir, tree); err != nil {
		t.Fatalf("copy repository template: %v", err)
	}
	return dir, meta
}

func (c *Cache[M]) load(t testing.TB, key string, build func() (string, M)) ([]entry, M) {
	t.Helper()
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*template[M])
	}
	stored := c.entries[key]
	if stored == nil {
		stored = &template[M]{}
		c.entries[key] = stored
	}
	c.mu.Unlock()
	stored.mu.Lock()
	defer stored.mu.Unlock() // t.Fatal exits the goroutine, so it must release the key.
	if !stored.built {
		dir, meta := build()
		tree, err := snapshot(dir)
		if err != nil {
			t.Fatalf("read repository template: %v", err)
		}
		stored.tree, stored.meta, stored.built = tree, meta, true
	}
	return stored.tree, stored.meta
}

func snapshot(dir string) ([]entry, error) {
	var tree []entry
	err := filepath.WalkDir(dir, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		file := entry{path: rel, mode: info.Mode()}
		switch {
		case info.IsDir():
		case info.Mode().IsRegular():
			file.data, err = os.ReadFile(path) //nolint:gosec // G304: test-owned fixture tree.
		case info.Mode()&fs.ModeSymlink != 0:
			file.link, err = os.Readlink(path)
		default:
			return fmt.Errorf("unsupported file type at %s: %s", rel, info.Mode())
		}
		if err != nil {
			return err
		}
		tree = append(tree, file)
		return nil
	})
	return tree, err
}

func restore(dir string, tree []entry) error {
	for _, file := range tree {
		path := filepath.Join(dir, file.path)
		var err error
		switch {
		case file.mode.IsDir():
			if file.path != "." {
				err = os.Mkdir(path, 0o700)
			}
		case file.mode.IsRegular():
			err = os.WriteFile(path, file.data, 0o600)
		case file.mode&fs.ModeSymlink != 0:
			err = os.Symlink(file.link, path)
		}
		if err != nil {
			return err
		}
	}
	// Apply permissions after writing children, including read-only directories.
	// Chmod explicitly restores bits that the process umask may have removed.
	for i := len(tree) - 1; i >= 0; i-- {
		file := tree[i]
		if file.mode&fs.ModeSymlink == 0 {
			if err := os.Chmod(filepath.Join(dir, file.path), file.mode); err != nil {
				return err
			}
		}
	}
	return nil
}
