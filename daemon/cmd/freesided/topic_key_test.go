package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestOpenStoreWithTopicKey(t *testing.T) {
	t.Run("fresh store mints key before opening", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		st, key, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
		if err != nil {
			t.Fatalf("open store with topic key: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if len(key) != sha256.Size {
			t.Fatalf("open store returned a %d-byte topic key, want %d", len(key), sha256.Size)
		}
		if _, err := os.Stat(dbPath + topicKeySuffix); err != nil {
			t.Fatalf("stat topic key: %v", err)
		}
	})

	t.Run("preexisting store without key fails closed", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		seed, err := store.Open(t.Context(), dbPath, store.Options{})
		if err != nil {
			t.Fatalf("seed store: %v", err)
		}
		if err := seed.Close(); err != nil {
			t.Fatalf("close seed store: %v", err)
		}
		if _, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{}); !errors.Is(err, errTopicKeyMissing) {
			t.Fatalf("open error = %v, want errTopicKeyMissing", err)
		}
	})

	t.Run("failed store open leaves reusable key before database", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		if st, _, err := openStoreWithTopicKey(
			t.Context(), dbPath, store.Options{BusyTimeout: -time.Second},
		); err == nil {
			_ = st.Close()
			t.Fatal("open store accepted a negative busy timeout")
		}
		if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("database stat error = %v, want ErrNotExist", err)
		}
		path := dbPath + topicKeySuffix
		want, err := os.ReadFile(path) //nolint:gosec // test-owned credential path
		if err != nil {
			t.Fatalf("read topic key after failed open: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat topic key after failed open: %v", err)
		}
		if len(want) != sha256.Size || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("topic key length/mode = %d/%v, want %d/0600 regular", len(want), info.Mode(), sha256.Size)
		}
		st, got, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
		if err != nil {
			t.Fatalf("retry open store: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if !bytes.Equal(got, want) {
			t.Fatal("retry replaced the orphan topic key")
		}
	})

	t.Run("existing key is loaded byte-identical", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		want, err := loadOrCreateTopicKey(dbPath, false)
		if err != nil {
			t.Fatalf("create topic key: %v", err)
		}
		st, got, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
		if err != nil {
			t.Fatalf("open store with existing topic key: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if !bytes.Equal(got, want) {
			t.Fatal("open store loaded different topic key bytes")
		}
	})
}

func TestProductionStoreOpenSitesUseTopicKeyHelper(t *testing.T) {
	var directOpens []string
	var productionTestImports []string
	daemonRoot := filepath.Clean(filepath.Join("..", ".."))
	err := filepath.WalkDir(daemonRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relativePath, err := filepath.Rel(daemonRoot, path)
		if err != nil {
			return err
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", relativePath, err)
		}
		storeAliases := map[string]bool{}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("parse import in %s: %w", relativePath, err)
			}
			if importPath == "github.com/freeside-ai/freeside/daemon/internal/store/storetest" {
				productionTestImports = append(productionTestImports, filepath.ToSlash(relativePath))
			}
			if importPath != "github.com/freeside-ai/freeside/daemon/internal/store" {
				continue
			}
			if spec.Name != nil && spec.Name.Name == "." {
				directOpens = append(directOpens, filepath.ToSlash(relativePath)+": dot import")
				continue
			}
			alias := "store"
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			storeAliases[alias] = true
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Open" {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Obj != nil || !storeAliases[pkg.Name] {
				return true
			}
			position := fileSet.Position(call.Pos())
			directOpens = append(directOpens,
				fmt.Sprintf("%s:%d", filepath.ToSlash(relativePath), position.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan production Go files: %v", err)
	}
	if len(productionTestImports) != 0 {
		t.Fatalf("production files import test-only store fixtures: %v", productionTestImports)
	}
	sort.Strings(directOpens)
	wantPrefixes := []string{
		"cmd/freeside-signet-dev/main.go:",
		// Shared test support needs a non-_test.go file, but production files
		// cannot import it (checked above) and bypass topic-key setup.
		"internal/store/storetest/storetest.go:",
		"internal/topicstore/topicstore.go:",
	}
	if len(directOpens) != len(wantPrefixes) {
		t.Fatalf("production store.Open sites = %v, want prefixes %v", directOpens, wantPrefixes)
	}
	for i := range wantPrefixes {
		if !strings.HasPrefix(directOpens[i], wantPrefixes[i]) {
			t.Fatalf("production store.Open sites = %v, want prefixes %v", directOpens, wantPrefixes)
		}
	}
}
