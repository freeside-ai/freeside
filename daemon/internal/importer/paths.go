package importer

import (
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// gatePaths enforces the structural path gates over a validated
// manifest, before any entry can influence the import: no representable
// path may smuggle a git-metadata component in any disguise a case- or
// normalization-insensitive filesystem would honor, and no path may be
// both a file and a directory. The workspace's own top-level .git entry
// (kind git_dir) is exempt: the contract records it, and it never
// enters the tree. invalid_path entries carry no representable path and
// are handled as findings by derivation.
func gatePaths(m export.Manifest) error {
	paths := make(map[string]struct{}, len(m.Entries))
	for _, e := range m.Entries {
		if e.Kind == export.EntryInvalidPath || e.Kind == export.EntryGitDir {
			continue
		}
		for _, comp := range strings.Split(e.Path, "/") {
			if gitUnsafeComponent(comp) {
				return fmt.Errorf("path %q component %q: %w", e.Path, comp, ErrGitPathInjection)
			}
		}
		paths[e.Path] = struct{}{}
	}
	for _, e := range m.Entries {
		if e.Kind == export.EntryInvalidPath || e.Kind == export.EntryGitDir {
			continue
		}
		for dir := parentDir(e.Path); dir != ""; dir = parentDir(dir) {
			if _, ok := paths[dir]; ok {
				return fmt.Errorf("entry %q is also a directory of %q: %w", dir, e.Path, ErrPathConflict)
			}
		}
	}
	return nil
}

// parentDir returns the slash-separated parent of p, or "" at the root.
func parentDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

// gitUnsafeComponent reports whether one path component could name git
// metadata on any filesystem a checkout or downstream working tree
// might use: exact or case-folded ".git" after an NTFS trailing
// dot/space trim, the NTFS 8.3 short form "git~1", a backslash (an
// alternate separator there), or an HFS-ignorable-code-point disguise.
// The plumbing later runs with core.protectHFS and core.protectNTFS as
// a backstop; this gate exists so a forged manifest fails closed on the
// importer's terms with a typed error, not git's.
func gitUnsafeComponent(c string) bool {
	if strings.ContainsRune(c, '\\') {
		return true
	}
	return isDotGitVariant(normalizeComponentAliases(c))
}

// isDotGitVariant reports whether a component, after trimming the
// trailing dots and spaces NTFS ignores, case-folds to ".git" or to
// git's 8.3 short name "git~1".
func isDotGitVariant(c string) bool {
	c = strings.TrimRight(c, ". ")
	return strings.EqualFold(c, ".git") || strings.EqualFold(c, "git~1")
}

// normalizeComponentAliases folds one component through deterministic
// aliases a downstream NTFS/HFS checkout collapses: HFS-ignorable code
// points, an NTFS alternate-data-stream suffix (everything from the
// first colon), and trailing dots/spaces. On NTFS, "name:stream" names a
// stream of "name", and "name::$DATA" is its unnamed data stream.
func normalizeComponentAliases(c string) string {
	if strings.ContainsFunc(c, hfsIgnorable) {
		var b strings.Builder
		for _, r := range c {
			if !hfsIgnorable(r) {
				b.WriteRune(r)
			}
		}
		c = b.String()
	}
	if i := strings.IndexByte(c, ':'); i >= 0 {
		c = c[:i]
	}
	return strings.TrimRight(c, ". ")
}

// normalizeAliases folds each path component the way a downstream
// checkout filesystem would collapse an alias to a protected name:
// HFS-ignorable code points stripped, NTFS ADS suffixes stripped, and
// trailing dots/spaces trimmed. A candidate path is canonical per the
// manifest, but ".gitmodules " or ".gitmodules::$DATA" materializes as
// the protected name downstream, so mandatory policy classes must match
// this normalized form. Case folding is left to matchAny.
func normalizeAliases(path string) string {
	comps := strings.Split(path, "/")
	for i, c := range comps {
		comps[i] = normalizeComponentAliases(c)
	}
	return strings.Join(comps, "/")
}

// hfsIgnorable reports the code points HFS+ filename comparison
// ignores, matching git's own protectHFS set.
func hfsIgnorable(r rune) bool {
	switch {
	case r >= 0x200c && r <= 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x206a && r <= 0x206f,
		r == 0xfeff:
		return true
	}
	return false
}
