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

// TestEmitEvidenceWriteFailureLeavesNoManifest proves the manifest-last commit
// ordering: a source that passes resolution (lstat sees a regular file) but
// fails the copy (open denied, modeling an infrastructure failure like ENOSPC
// while emitting declared evidence after the repo blobs are written) leaves no
// manifest.json, so the ward gate fails the whole handoff closed rather than
// shipping a repo-only export that silently dropped the declared evidence.
func TestEmitEvidenceWriteFailureLeavesNoManifest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the unreadable-source permission check")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "README.md", "repo\n")
	mustWrite(t, dir, ".freeside-evidence/shot.png", "unreadable")
	if err := os.Chmod(filepath.Join(dir, ".freeside-evidence", "shot.png"), 0o000); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, dir, ".freeside-evidence/evidence.json", oneSource(".freeside-evidence/shot.png"))

	out := t.TempDir()
	if _, err := export.Export(os.DirFS(dir), out, export.Options{}); err == nil {
		t.Fatal("expected export to fail on an unreadable evidence source")
	}
	if _, statErr := os.Stat(filepath.Join(out, export.ManifestFilename)); statErr == nil {
		t.Error("manifest.json written despite an evidence-emission failure; it must be written last")
	}
	if _, statErr := os.Stat(filepath.Join(out, export.EvidenceFilename)); statErr == nil {
		t.Error("evidence.json written despite an evidence-emission failure")
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
