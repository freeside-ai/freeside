//go:build unix

package export_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// TestEmitEvidenceSymlinkEscape proves the emitter never follows a symlink out
// of the reserved subtree: neither a source that is itself a symlink, an
// intermediate symlinked directory, nor a symlinked descriptor can hand the
// helper a file outside .freeside-evidence/. Real symlinks require a real filesystem, so
// this is unix-only, matching the walk's own symlink coverage. It reuses
// mustWrite and oneSource from evidence_emit_test.go (same package).
func TestEmitEvidenceSymlinkEscape(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"source is a symlink", func(t *testing.T, dir string) {
			mustWrite(t, dir, "outside.txt", "secret")
			mustSymlinkAt(t, dir, ".freeside-evidence/leak.png", "../outside.txt")
			mustWrite(t, dir, ".freeside-evidence/evidence.json", oneSource(".freeside-evidence/leak.png"))
		}},
		{"intermediate symlinked directory", func(t *testing.T, dir string) {
			mustWrite(t, dir, "secretdir/passwd", "secret")
			mustSymlinkAt(t, dir, ".freeside-evidence/link", "../secretdir")
			mustWrite(t, dir, ".freeside-evidence/evidence.json", oneSource(".freeside-evidence/link/passwd"))
		}},
		{"descriptor is a symlink", func(t *testing.T, dir string) {
			mustWrite(t, dir, "elsewhere.json", oneSource(".freeside-evidence/x.png"))
			mustSymlinkAt(t, dir, ".freeside-evidence/evidence.json", "../elsewhere.json")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, dir, "README.md", "repo\n")
			tc.setup(t, dir)
			out := t.TempDir()
			_, err := export.Export(os.DirFS(dir), out, export.Options{})
			if !errors.Is(err, export.ErrEvidenceSourceNotRegular) {
				t.Fatalf("export err = %v, want ErrEvidenceSourceNotRegular", err)
			}
			if _, statErr := os.Stat(filepath.Join(out, export.EvidenceFilename)); statErr == nil {
				t.Errorf("evidence.json written despite a symlink escape")
			}
		})
	}
}

func mustSymlinkAt(t *testing.T, dir, rel, target string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatalf("symlink %q: %v", rel, err)
	}
}
