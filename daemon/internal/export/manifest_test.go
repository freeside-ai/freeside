package export_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func ptr[T any](v T) *T { return &v }

// digestOf builds a syntactically valid sha256 content address from a
// two-hex-character seed, so fixtures stay readable without real hashing.
func digestOf(seed string) *export.Digest {
	d := export.Digest("sha256:" + strings.Repeat(seed, 32))
	return &d
}

// fullManifest is a fixed, valid manifest carrying one entry of every kind,
// sorted bytewise by raw name; the golden doubles as a validation-positive
// case and pins the v1 wire bytes the importer will parse.
func fullManifest() export.Manifest {
	return export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{
			{Path: ".git", Kind: export.EntryGitDir},
			// Raw bytes "bad\xffname": not valid UTF-8.
			{PathHex: "626164ff6e616d65", Kind: export.EntryInvalidPath},
			{Path: "bin/tool", Kind: export.EntryRegular, Mode: ptr("0755"), Size: ptr(int64(64)), Digest: digestOf("1a")},
			{Path: "docs/readme.md", Kind: export.EntryRegular, Mode: ptr("0644"), Size: ptr(int64(0)), Digest: digestOf("2b")},
			{Path: "legacy/setuid-helper", Kind: export.EntryUnusualMode, Mode: ptr("04755")},
			{Path: "link-to-readme", Kind: export.EntrySymlink, Target: ptr("docs/readme.md")},
			{Path: "media/huge.bin", Kind: export.EntryRegular, Mode: ptr("0644"), Size: ptr(int64(1 << 30)), Digest: digestOf("3c"), BlobOmitted: true},
			{Path: "pipes/queue", Kind: export.EntrySpecial},
			{Path: "vendor/dep", Kind: export.EntrySubmodule},
		},
	}
}

// TestGolden pins the manifest wire format (the gauntlet-internal contract
// the importer consumes). Regenerate with:
// go test ./internal/export -run TestGolden -update.
func TestGolden(t *testing.T) {
	m := fullManifest()
	got, err := m.Encode()
	if err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	golden.Assert(t, "manifest", got)
}

// TestManifestValidate enumerates the invariants Validate guards, one
// mutation of the valid fixture per case.
func TestManifestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*export.Manifest)
		wantErr error
	}{
		{"unknown version", func(m *export.Manifest) {
			m.Version = "freeside.export.manifest/v0"
		}, export.ErrUnknownManifestVersion},
		{"unsorted entries", func(m *export.Manifest) {
			m.Entries[2], m.Entries[3] = m.Entries[3], m.Entries[2]
		}, export.ErrEntriesNotCanonical},
		{"duplicate path", func(m *export.Manifest) {
			m.Entries[3] = m.Entries[2]
		}, export.ErrEntriesNotCanonical},
		{"unknown kind", func(m *export.Manifest) {
			m.Entries[2].Kind = "hardlink"
		}, export.ErrInvalidEntryKind},
		{"zero-value kind", func(m *export.Manifest) {
			m.Entries[2].Kind = ""
		}, export.ErrInvalidEntryKind},
		{"empty path", func(m *export.Manifest) {
			m.Entries[2].Path = ""
		}, export.ErrInvalidPath},
		{"non-canonical path", func(m *export.Manifest) {
			m.Entries[2].Path = "bin//tool"
		}, export.ErrInvalidPath},
		{"parent-escaping path", func(m *export.Manifest) {
			m.Entries[2].Path = "../tool"
		}, export.ErrInvalidPath},
		{"absolute path", func(m *export.Manifest) {
			m.Entries[2].Path = "/bin/tool"
		}, export.ErrInvalidPath},
		{"root path", func(m *export.Manifest) {
			m.Entries[2].Path = "."
		}, export.ErrInvalidPath},
		{"path with NUL byte", func(m *export.Manifest) {
			// Valid UTF-8 and fs.ValidPath-clean, but no real filesystem
			// path can carry it; the canonical-path gate must reject it.
			m.Entries[2].Path = "bin/to\x00ol"
		}, export.ErrInvalidPath},
		{"regular without mode", func(m *export.Manifest) {
			m.Entries[2].Mode = nil
		}, export.ErrInvalidMode},
		{"regular with unnormalized mode", func(m *export.Manifest) {
			m.Entries[2].Mode = ptr("0600")
		}, export.ErrInvalidMode},
		{"regular without size", func(m *export.Manifest) {
			m.Entries[2].Size = nil
		}, export.ErrKindFieldMismatch},
		{"regular with negative size", func(m *export.Manifest) {
			m.Entries[2].Size = ptr(int64(-1))
		}, export.ErrNegativeSize},
		{"regular without digest", func(m *export.Manifest) {
			m.Entries[2].Digest = nil
		}, export.ErrInvalidDigest},
		{"regular with malformed digest", func(m *export.Manifest) {
			m.Entries[2].Digest = ptr(export.Digest("sha256:xyz"))
		}, export.ErrInvalidDigest},
		{"regular with uppercase digest", func(m *export.Manifest) {
			m.Entries[2].Digest = ptr(export.Digest("sha256:" + strings.Repeat("A1", 32)))
		}, export.ErrInvalidDigest},
		{"regular with target", func(m *export.Manifest) {
			m.Entries[2].Target = ptr("elsewhere")
		}, export.ErrKindFieldMismatch},
		{"regular with path_hex", func(m *export.Manifest) {
			m.Entries[2].PathHex = "6162"
		}, export.ErrKindFieldMismatch},
		{"symlink without target", func(m *export.Manifest) {
			m.Entries[5].Target = nil
		}, export.ErrKindFieldMismatch},
		{"symlink with digest", func(m *export.Manifest) {
			m.Entries[5].Digest = digestOf("4d")
		}, export.ErrKindFieldMismatch},
		{"symlink with blob_omitted", func(m *export.Manifest) {
			m.Entries[5].BlobOmitted = true
		}, export.ErrKindFieldMismatch},
		{"submodule with mode", func(m *export.Manifest) {
			m.Entries[8].Mode = ptr("0755")
		}, export.ErrKindFieldMismatch},
		{"special with size", func(m *export.Manifest) {
			m.Entries[7].Size = ptr(int64(1))
		}, export.ErrKindFieldMismatch},
		{"unusual_mode without special bits", func(m *export.Manifest) {
			m.Entries[4].Mode = ptr("00755")
		}, export.ErrInvalidMode},
		{"unusual_mode with bare git mode", func(m *export.Manifest) {
			m.Entries[4].Mode = ptr("0755")
		}, export.ErrInvalidMode},
		{"git_dir off the workspace root", func(m *export.Manifest) {
			m.Entries[0].Path = "vendor-two/.git"
		}, export.ErrInvalidPath},
		{"invalid_path with path set", func(m *export.Manifest) {
			m.Entries[1].Path = "also-a-path"
		}, export.ErrKindFieldMismatch},
		{"invalid_path with uppercase hex", func(m *export.Manifest) {
			m.Entries[1].PathHex = "626164FF6E616D65"
		}, export.ErrInvalidPathHex},
		{"invalid_path with odd-length hex", func(m *export.Manifest) {
			m.Entries[1].PathHex = "626164f"
		}, export.ErrInvalidPathHex},
		{"invalid_path with non-hex bytes", func(m *export.Manifest) {
			m.Entries[1].PathHex = "zz6164ff6e616d65"
		}, export.ErrInvalidPathHex},
		{"invalid_path decoding to a representable path", func(m *export.Manifest) {
			m.Entries[1].PathHex = "616263" // "abc"
		}, export.ErrInvalidPathHex},
		{"blob_omitted on git_dir", func(m *export.Manifest) {
			m.Entries[0].BlobOmitted = true
		}, export.ErrKindFieldMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fullManifest()
			tc.mutate(&m)
			err := m.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestManifestValidatePositive keeps the golden fixture honest: the exact
// value the golden pins must pass Validate on its own.
func TestManifestValidatePositive(t *testing.T) {
	if err := fullManifest().Validate(); err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
}
