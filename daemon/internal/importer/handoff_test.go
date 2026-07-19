package importer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// testDigest is shape-valid; content binding is verified against blobs,
// not here.
const testDigest = "sha256:" + "abababababababababababababababababababababababababababababababab"

// writeHandoff lays out a handoff directory holding exactly the given
// manifest bytes.
func writeHandoff(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func regularEntryJSON(path string) string {
	return `{"path":"` + path + `","kind":"regular","mode":"0644","size":0,"digest":"` + testDigest + `"}`
}

func manifestJSON(entries ...string) string {
	return `{"version":"` + export.ManifestVersion + `","entries":[` + strings.Join(entries, ",") + `]}`
}

func TestLoadManifestRejects(t *testing.T) {
	// The #180 laundering regression: a raw invalid byte inside a path.
	// encoding/json would replace it with U+FFFD on decode, so without the
	// raw-bytes pre-check it becomes a valid-looking canonical path; the
	// pre-check rejects it before the decoder ever sees it.
	invalidUTF8 := strings.Replace(manifestJSON(regularEntryJSON("a.txt")), "a.txt", "a\xfftxt", 1)
	cases := []struct {
		name     string
		manifest string
		policy   Policy
		wantErr  error
		wantAlso error // additionally expected in the chain, when set
	}{
		{
			name:     "oversized manifest",
			manifest: manifestJSON(regularEntryJSON("a.txt")),
			policy:   Policy{MaxManifestBytes: 16},
			wantErr:  ErrManifestTooLarge,
		},
		{
			name:     "malformed json",
			manifest: `{"version":`,
			wantErr:  ErrManifestInvalid,
		},
		{
			name:     "unknown field",
			manifest: `{"version":"` + export.ManifestVersion + `","entries":[],"base_sha":"trustme"}`,
			wantErr:  ErrManifestInvalid,
		},
		{
			name:     "trailing content",
			manifest: manifestJSON() + `{"version":"evil"}`,
			wantErr:  ErrManifestInvalid,
		},
		{
			name:     "unknown version",
			manifest: `{"version":"freeside.export.manifest/v99","entries":[]}`,
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrUnknownManifestVersion,
		},
		{
			name:     "path traversal",
			manifest: manifestJSON(regularEntryJSON("../escape")),
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrInvalidPath,
		},
		{
			name:     "interior traversal",
			manifest: manifestJSON(regularEntryJSON("a/../b")),
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrInvalidPath,
		},
		{
			name:     "absolute path",
			manifest: manifestJSON(regularEntryJSON("/etc/passwd")),
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrInvalidPath,
		},
		{
			name:     "dot-prefixed path",
			manifest: manifestJSON(regularEntryJSON("./a")),
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrInvalidPath,
		},
		{
			name:     "root path",
			manifest: manifestJSON(regularEntryJSON(".")),
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrInvalidPath,
		},
		{
			name:     "nul in path",
			manifest: manifestJSON(regularEntryJSON(`a\u0000b`)),
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrInvalidPath,
		},
		{
			name:     "duplicate entries",
			manifest: manifestJSON(regularEntryJSON("a.txt"), regularEntryJSON("a.txt")),
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrEntriesNotCanonical,
		},
		{
			name:     "unsorted entries",
			manifest: manifestJSON(regularEntryJSON("b.txt"), regularEntryJSON("a.txt")),
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrEntriesNotCanonical,
		},
		{
			name:     "entry cap",
			manifest: manifestJSON(regularEntryJSON("a.txt"), regularEntryJSON("b.txt")),
			policy:   Policy{MaxEntries: 1},
			wantErr:  ErrManifestTooLarge,
		},
		{
			name:     "invalid utf-8 in path",
			manifest: invalidUTF8,
			wantErr:  ErrManifestInvalid,
			wantAlso: export.ErrInvalidUTF8,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeHandoff(t, tc.manifest)
			_, err := loadManifest(dir, tc.policy.withDefaults())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("loadManifest error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantAlso != nil && !errors.Is(err, tc.wantAlso) {
				t.Fatalf("loadManifest error = %v, want it to also wrap %v", err, tc.wantAlso)
			}
		})
	}
}

// TestLoadManifestPathCaps is the refute-pass regression: a single
// over-long or over-deep path is rejected at intake, before the
// superlinear gate and collision work runs.
func TestLoadManifestPathCaps(t *testing.T) {
	t.Run("over-long path", func(t *testing.T) {
		long := strings.Repeat("a", 5000)
		dir := writeHandoff(t, manifestJSON(regularEntryJSON(long)))
		if _, err := loadManifest(dir, Policy{}.withDefaults()); !errors.Is(err, ErrManifestTooLarge) {
			t.Fatalf("loadManifest = %v, want %v", err, ErrManifestTooLarge)
		}
	})
	t.Run("over-deep path", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 300; i++ {
			if i > 0 {
				b.WriteByte('/')
			}
			b.WriteByte('a')
		}
		dir := writeHandoff(t, manifestJSON(regularEntryJSON(b.String())))
		if _, err := loadManifest(dir, Policy{}.withDefaults()); !errors.Is(err, ErrManifestTooLarge) {
			t.Fatalf("loadManifest = %v, want %v", err, ErrManifestTooLarge)
		}
	})
}

func TestLoadManifestMissingFile(t *testing.T) {
	_, err := loadManifest(t.TempDir(), Policy{}.withDefaults())
	if !errors.Is(err, ErrManifestUnreadable) {
		t.Fatalf("loadManifest error = %v, want %v", err, ErrManifestUnreadable)
	}
}

func TestLoadManifestAccepts(t *testing.T) {
	manifest := manifestJSON(
		`{"path":".git","kind":"git_dir","mode":null,"size":null,"digest":null,"target":null}`,
		regularEntryJSON("a.txt"),
		`{"path":"link","kind":"symlink","mode":null,"size":null,"digest":null,"target":"a.txt"}`,
	)
	m, err := loadManifest(writeHandoff(t, manifest), Policy{}.withDefaults())
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if len(m.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(m.Entries))
	}
}
