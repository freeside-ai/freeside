package importer

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func testEntry(kind export.EntryKind, path string) export.Entry {
	e := export.Entry{Path: path, Kind: kind}
	switch kind {
	case export.EntryRegular:
		mode, size, digest := "0644", int64(0), export.Digest(testDigest)
		e.Mode, e.Size, e.Digest = &mode, &size, &digest
	case export.EntrySymlink:
		target := "t"
		e.Target = &target
	case export.EntrySubmodule, export.EntrySpecial,
		export.EntryUnusualMode, export.EntryGitDir, export.EntryInvalidPath:
	}
	return e
}

func TestGatePathsInjection(t *testing.T) {
	cases := []struct {
		name string
		path string
		want error // nil means the gate must pass
	}{
		{"plain dotgit dir", ".git/config", ErrGitPathInjection},
		{"case variant", ".GIT/config", ErrGitPathInjection},
		{"nested dotgit", "a/.git/hooks/pre-commit", ErrGitPathInjection},
		{"dotgit leaf", "a/.git", ErrGitPathInjection},
		{"ntfs short name", "git~1/config", ErrGitPathInjection},
		{"ntfs short name cased", "GIT~1/config", ErrGitPathInjection},
		{"ntfs trailing dot", ".git./config", ErrGitPathInjection},
		{"ntfs trailing space and dot", ".git . /config", ErrGitPathInjection},
		{"backslash component", `a\b/c`, ErrGitPathInjection},
		{"hfs zero width non-joiner", ".g\u200cit/config", ErrGitPathInjection},
		{"hfs rtl override prefix", "\u202e.git/x", ErrGitPathInjection},
		{"hfs bom suffix", ".git\ufeff/x", ErrGitPathInjection},
		{"dotgit-prefixed name", ".gitx/config", nil},
		{"dotgit-suffixed name", "x.git/config", nil},
		{"gitignore", ".gitignore", nil},
		{"short-name lookalike", "agit~1/x", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := export.Manifest{
				Version: export.ManifestVersion,
				Entries: []export.Entry{testEntry(export.EntryRegular, tc.path)},
			}
			err := gatePaths(m)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("gatePaths(%q) = %v, want nil", tc.path, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("gatePaths(%q) = %v, want %v", tc.path, err, tc.want)
			}
		})
	}
}

func TestGatePathsExemptsRecordedGitDir(t *testing.T) {
	m := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{testEntry(export.EntryGitDir, ".git")},
	}
	if err := gatePaths(m); err != nil {
		t.Fatalf("gatePaths = %v, want nil for the recorded workspace .git", err)
	}
}

func TestGatePathsPrefixConflict(t *testing.T) {
	cases := []struct {
		name    string
		entries []export.Entry
		want    error
	}{
		{
			name: "file also a directory",
			entries: []export.Entry{
				testEntry(export.EntryRegular, "a"),
				testEntry(export.EntryRegular, "a/b"),
			},
			want: ErrPathConflict,
		},
		{
			name: "symlink also a directory",
			entries: []export.Entry{
				testEntry(export.EntrySymlink, "link"),
				testEntry(export.EntryRegular, "link/escape"),
			},
			want: ErrPathConflict,
		},
		{
			name: "submodule with smuggled children",
			entries: []export.Entry{
				testEntry(export.EntrySubmodule, "sub"),
				testEntry(export.EntryRegular, "sub/inner"),
			},
			want: ErrPathConflict,
		},
		{
			name: "deep conflict",
			entries: []export.Entry{
				testEntry(export.EntryRegular, "a/b"),
				testEntry(export.EntryRegular, "a/b/c/d"),
			},
			want: ErrPathConflict,
		},
		{
			name: "shared prefixes without conflict",
			entries: []export.Entry{
				testEntry(export.EntryRegular, "a/b"),
				testEntry(export.EntryRegular, "a/c"),
				testEntry(export.EntryRegular, "ab"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := gatePaths(export.Manifest{Version: export.ManifestVersion, Entries: tc.entries})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("gatePaths = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("gatePaths = %v, want %v", err, tc.want)
			}
		})
	}
}
