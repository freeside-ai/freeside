package importer

import (
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
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
		// The exporter reserves this whole namespace for the opaque plan
		// channel. Re-check it here because the handoff manifest is hostile: a
		// forged repo entry must not turn reserved metadata into committed
		// content or poison the next import's trusted-base preflight.
		if export.IsCommitPlanNamespacePath(e.Path) {
			return fmt.Errorf("manifest path %q occupies the reserved commit-plan namespace: %w", e.Path, ErrCommitPlanCollision)
		}
		for _, comp := range strings.Split(e.Path, "/") {
			if GitUnsafeComponent(comp) {
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

// GitUnsafeComponent reports whether one path component could name git
// metadata on any filesystem a checkout or downstream working tree
// might use: exact or case-folded ".git" after an NTFS trailing
// dot/space trim, the NTFS 8.3 short form "git~1", a backslash (an
// alternate separator there), or an HFS-ignorable-code-point disguise.
// The plumbing later runs with core.protectHFS and core.protectNTFS as
// a backstop; this gate exists so a forged manifest fails closed on the
// importer's terms with a typed error, not git's.
func GitUnsafeComponent(c string) bool {
	if strings.ContainsRune(c, '\\') {
		return true
	}
	return isDotGitVariant(pathfold.NormalizeAliases(c))
}

// isDotGitVariant reports whether a component, after trimming the
// trailing dots and spaces NTFS ignores, case-folds to ".git" or to
// git's 8.3 short name "git~1".
func isDotGitVariant(c string) bool {
	c = strings.TrimRight(c, ". ")
	return strings.EqualFold(c, ".git") || strings.EqualFold(c, "git~1")
}
