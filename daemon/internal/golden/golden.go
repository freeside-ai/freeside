// Package golden is the daemon's shared golden-file test helper: a
// single Assert function and a package-level -update flag, so every
// lane's golden tests share one shape and one regeneration switch.
//
// Usage (from any _test.go):
//
//	golden.Assert(t, "case-name", got)
//
// which compares got against testdata/case-name.golden. Run the test
// with -update to (re)write the golden file from the produced value:
//
//	go test ./internal/foo -run TestBar -update
package golden

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update, set by `go test -update`, rewrites golden files from the
// values passed to Assert instead of comparing against them.
var update = flag.Bool("update", false, "rewrite golden files under testdata/ from test output")

// Assert compares got against the golden file testdata/<name>.golden,
// failing the test on any difference. With -update it writes the golden
// file and skips the comparison. name is used verbatim as the file
// stem, so it must be a stable, filesystem-safe case identifier.
func Assert(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name+".golden")

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("golden: create testdata dir: %v", err)
		}
		// 0o600: golden fixtures are non-sensitive committed test data.
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("golden: write %s: %v", path, err)
		}
		return
	}

	// G304: path derives from a caller-controlled fixture name in test
	// code, never from external input.
	want, err := os.ReadFile(path) //nolint:gosec // test-only path from a caller-controlled fixture name
	if err != nil {
		t.Fatalf("golden: read %s (run `go test -update` to create it): %v", path, err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("golden: %s mismatch\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
