package verify

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // G505: sha1 is git's object identity under the enforced sha1 format, not a cryptographic protection; content trust rides the importer's sha256 audit
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// verifyHead enforces the head binding (§11 exit criterion: verification
// binds to the exact head): the requested candidate SHA must resolve to
// exactly itself as a commit in the daemon-owned checkout. A head the
// checkout does not hold, or one that resolves elsewhere, fails closed
// before any recipe command runs.
func (g *gitRunner) verifyHead(ctx context.Context, headSHA string) error {
	out, err := g.run(ctx, nil, "rev-parse", "--verify", headSHA+"^{commit}")
	if err != nil {
		return fmt.Errorf("head %s is not a commit in the checkout: %w: %w", headSHA, ErrHeadMismatch, err)
	}
	if got := strings.TrimSpace(string(out)); got != headSHA {
		return fmt.Errorf("head %s resolved to %s: %w", headSHA, got, ErrHeadMismatch)
	}
	return nil
}

// materialize writes the verified head's tree into dest as the fresh
// verification workspace (§5.6: fresh checkout, never the agent's
// workspace). It extracts each blob with cat-file and writes it
// directly, deliberately NOT `git checkout-index`: checkout-index runs
// any smudge/clean filter the checkout's attributes and config define,
// which would execute host code outside the Room during materialization
// (and GIT_ATTR_SOURCE does not suppress .git/info/attributes). Direct
// blob extraction is pure data with no filter, attribute, or
// line-ending processing, so materialization runs no candidate- or
// config-driven command and the bytes are exactly the tree's.
// verifyMaterialized re-checks the result as the backstop, and the
// destination is cleared first so a pre-created sibling workspace
// (verify.go per-command paths) cannot survive.
func (g *gitRunner) materialize(ctx context.Context, headSHA, dest string) error {
	if err := g.verifyHead(ctx, headSHA); err != nil {
		return err
	}
	entries, gitlinks, err := g.listTree(ctx, headSHA)
	if err != nil {
		return err
	}
	// Reject a malformed tree before writing anything: if an entry's
	// ancestor is itself an entry (a symlink `a` alongside a blob `a/b`,
	// which a well-formed tree cannot contain), writing `a/b` would
	// follow the symlink `a` and escape the workspace to an arbitrary
	// host path. git's own tree builder never produces this; a fetched
	// or crafted malformed tree fails closed here.
	if err := rejectPrefixConflicts(entries, gitlinks); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("clear verification workspace: %w", err)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("create verification workspace: %w", err)
	}
	// Every write goes through an os.Root bound to dest, which refuses to
	// traverse a symlink component whose target escapes the root (and any
	// absolute symlink), per-component and independent of write order.
	// rejectPrefixConflicts above is the exact-string early guard; this is
	// the containment that also closes the case-folding variant a fetched
	// malformed tree can craft on a case-insensitive FS (a symlink `link`
	// plus a blob `LINK/pwned`, which fold onto one path at runtime but not
	// in the dedup key). It is robust beyond ASCII case (APFS also
	// normalizes Unicode) precisely because it never lets the kernel
	// silently resolve an escaping component, rather than modeling one
	// normalization in the key.
	root, err := os.OpenRoot(dest)
	if err != nil {
		return fmt.Errorf("open verification workspace root: %w", err)
	}
	defer func() { _ = root.Close() }()
	for p, e := range entries {
		if err := g.writeTreeEntry(ctx, root, p, e); err != nil {
			return err
		}
	}
	// A gitlink materializes as an empty directory, matching a fresh
	// clone without submodule init and the shape verifyMaterialized
	// pins.
	for _, gl := range gitlinks {
		if err := root.MkdirAll(filepath.FromSlash(gl), 0o700); err != nil {
			return fmt.Errorf("create gitlink dir %s: %w", gl, err)
		}
	}
	return g.verifyMaterialized(ctx, headSHA, dest)
}

// verifyMaterialized proves the workspace holds exactly the head tree's
// bytes: every tree entry exists on disk with content hashing to its
// blob object name, and nothing else exists. This is the decisive
// backstop for the refute-pass finding that an in-tree .gitattributes
// (ident, text/eol, filter) can make checkout-index write bytes other
// than the committed blob's: on a git without GIT_ATTR_SOURCE support
// the conversion still happens, and this comparison turns it into a
// fail-closed ErrWorkspaceMismatch instead of a silent verification of
// content no commit holds.
func (g *gitRunner) verifyMaterialized(ctx context.Context, headSHA, dest string) error {
	want, gitlinks, err := g.listTree(ctx, headSHA)
	if err != nil {
		return err
	}
	for path, entry := range want {
		got, err := materializedBlobOID(filepath.Join(dest, filepath.FromSlash(path)), entry.mode)
		if err != nil {
			return fmt.Errorf("workspace %s: %w: %w", path, err, ErrWorkspaceMismatch)
		}
		if got != entry.oid {
			return fmt.Errorf("workspace %s holds %s, head tree has %s: %w", path, got, entry.oid, ErrWorkspaceMismatch)
		}
	}
	for _, path := range gitlinks {
		if err := verifyGitlinkShape(filepath.Join(dest, filepath.FromSlash(path))); err != nil {
			return fmt.Errorf("workspace %s: %w: %w", path, err, ErrWorkspaceMismatch)
		}
	}
	return walkForStrays(dest, want, gitlinks)
}

// legitDirs returns every directory that may legitimately exist in the
// materialized workspace: an ancestor directory of a tree entry, or a
// gitlink path (which materializes as its own empty directory). Any
// other directory is a stray.
func legitDirs(want map[string]treeEntry, gitlinks []string) map[string]struct{} {
	dirs := map[string]struct{}{".": {}}
	add := func(p string) {
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = struct{}{}
		}
	}
	for p := range want {
		add(p)
	}
	for _, p := range gitlinks {
		add(p)
		dirs[p] = struct{}{} // the gitlink itself is an (empty) directory
	}
	return dirs
}

// verifyGitlinkShape pins what a gitlink may look like on disk: absent
// or an empty directory (clone parity). A file, a symlink, or any
// nested entry is content the bound head does not hold and fails
// closed; files nested under a populated directory are additionally
// caught as strays.
func verifyGitlinkShape(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("gitlink entry materialized as %s, want an empty directory", info.Mode())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("gitlink directory holds %d entries, want none", len(entries))
	}
	return nil
}

// treeEntry is one head-tree slot: its index mode and blob object name.
type treeEntry struct {
	mode string // "100644", "100755", or "120000"
	oid  string
}

// listTree lists the head's full recursive tree, splitting regular and
// symlink blob entries (returned by path) from gitlinks (returned as a
// path slice): a gitlink is a submodule pointer with no blob content.
func (g *gitRunner) listTree(ctx context.Context, commitSHA string) (map[string]treeEntry, []string, error) {
	out, err := g.run(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", commitSHA)
	if err != nil {
		return nil, nil, err
	}
	entries := make(map[string]treeEntry)
	var gitlinks []string
	seen := map[string]struct{}{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, pathBytes, ok := bytes.Cut(rec, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 3 {
			return nil, nil, fmt.Errorf("unparseable ls-tree record %q: %w", rec, ErrGitPlumbing)
		}
		p := string(pathBytes)
		if err := validateTreePath(p); err != nil {
			return nil, nil, err
		}
		// Reject a duplicated path before it reaches either the entries
		// map (which would silently keep the last record) or the gitlinks
		// slice (which would append blind): materialization writes one of
		// them and rejectSymlinkEntrypoints reads the first, so a
		// duplicate could make the two disagree on what a path is.
		if _, dup := seen[p]; dup {
			return nil, nil, fmt.Errorf("malformed tree: duplicate path %q: %w", p, ErrMalformedTree)
		}
		seen[p] = struct{}{}
		if fields[0] == "160000" {
			gitlinks = append(gitlinks, p)
			continue
		}
		entries[p] = treeEntry{mode: fields[0], oid: fields[2]}
	}
	return entries, gitlinks, nil
}

// validateTreePath fails closed on a tree path that filepath.Join could
// resolve outside the workspace. git write-tree never emits these, but a
// tree crafted with `hash-object -t tree --literally` (the stated
// malformed-tree threat) can, so every recursive tree path must be a
// clean relative path. An empty component catches an empty path, an
// absolute path (leading "/"), and a "//" or trailing "/" run; a "." or
// ".." component is a traversal segment. Checked before the path becomes
// a map key, hence before rejectPrefixConflicts and any write.
func validateTreePath(p string) error {
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("malformed tree: path %q has an empty or absolute component: %w", p, ErrMalformedTree)
		case ".", "..":
			return fmt.Errorf("malformed tree: path %q has a %q component: %w", p, seg, ErrMalformedTree)
		}
	}
	return nil
}

// rejectPrefixConflicts fails closed if any entry path has an ancestor
// that is itself an entry (a blob, symlink, or gitlink). A well-formed
// git tree cannot represent a name as both a leaf and a directory, so
// this only fires on a malformed tree; catching it before any write
// closes the symlink-prefix escape (writing `a/b` through a symlink
// `a`).
func rejectPrefixConflicts(entries map[string]treeEntry, gitlinks []string) error {
	all := make(map[string]struct{}, len(entries)+len(gitlinks))
	for p := range entries {
		all[p] = struct{}{}
	}
	for _, p := range gitlinks {
		all[p] = struct{}{}
	}
	for p := range all {
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			if _, ok := all[d]; ok {
				return fmt.Errorf("malformed tree: %q is nested under entry %q: %w", p, d, ErrMalformedTree)
			}
		}
	}
	return nil
}

// writeTreeEntry extracts one blob entry and writes it under root (bound
// to the workspace dest): a regular file with the mode's executable bit,
// or a symlink whose target is the blob's bytes. cat-file emits raw blob
// content, so no filter or attribute processing runs. root refuses to
// traverse an escaping symlink component, so no write can leave dest even
// when a malformed tree crafts a case- or Unicode-folding collision; the
// symlink itself is created with root.Symlink, which does not validate
// the target (a legitimate committed symlink may point outside the tree),
// so only a later path that traverses it fails closed.
func (g *gitRunner) writeTreeEntry(ctx context.Context, root *os.Root, treePath string, e treeEntry) error {
	name := filepath.FromSlash(treePath)
	if err := root.MkdirAll(filepath.FromSlash(path.Dir(treePath)), 0o700); err != nil {
		return fmt.Errorf("create workspace dir for %s: %w", treePath, err)
	}
	content, err := g.run(ctx, nil, "cat-file", "blob", e.oid)
	if err != nil {
		return err
	}
	if e.mode == "120000" {
		if err := root.Symlink(string(content), name); err != nil {
			return fmt.Errorf("materialize symlink %s: %w", treePath, err)
		}
		return nil
	}
	perm := os.FileMode(0o644)
	if e.mode == "100755" {
		perm = 0o755
	}
	if err := root.WriteFile(name, content, perm); err != nil {
		return fmt.Errorf("materialize %s: %w", treePath, err)
	}
	return nil
}

// walkForStrays rejects any workspace entry the head tree does not
// hold, files and directories alike: a stray file would let
// verification exercise content outside the bound head, and a stray
// directory is observable too (a later command's `test -d extra`), so
// the workspace must be exactly the tree, no more. Legitimate
// directories are the tree entries' ancestors and the gitlink
// placeholders; every other directory is a stray.
func walkForStrays(destPath string, want map[string]treeEntry, gitlinks []string) error {
	dirs := legitDirs(want, gitlinks)
	return filepath.WalkDir(destPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk workspace: %w: %w", err, ErrWorkspaceMismatch)
		}
		rel, err := filepath.Rel(destPath, p)
		if err != nil {
			return fmt.Errorf("walk workspace: %w: %w", err, ErrWorkspaceMismatch)
		}
		key := filepath.ToSlash(rel)
		if d.IsDir() {
			if _, ok := dirs[key]; !ok {
				return fmt.Errorf("workspace holds directory %s, which the head tree does not: %w", rel, ErrWorkspaceMismatch)
			}
			return nil
		}
		if _, ok := want[key]; !ok {
			return fmt.Errorf("workspace holds %s, which the head tree does not: %w", rel, ErrWorkspaceMismatch)
		}
		return nil
	})
}

// materializedBlobOID computes the git blob object name of what
// checkout-index wrote at path, requiring the on-disk shape to match
// the tree mode across everything a git tree can express (gitlinks are
// excluded upstream): a 120000 entry must be an actual symlink (under
// core.symlinks=false checkout-index writes the target text as a plain
// file, a type downgrade a recipe can observe via lstat or symlink
// traversal), a regular entry must be a regular file, and the owner
// executable bit must match 100755 vs 100644 (a recipe's ./script or a
// mode-sensitive test observes it; the owner bit is checked because a
// restrictive umask may strip group/other while a downgrade always
// strips owner too). Every mismatch fails closed rather than comparing
// content.
func materializedBlobOID(path, mode string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	isSymlink := info.Mode()&os.ModeSymlink != 0
	var content []byte
	switch mode {
	case "120000":
		if !isSymlink {
			return "", fmt.Errorf("symlink entry materialized as %s (symlinks unsupported here)", info.Mode())
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		content = []byte(target)
	default:
		if isSymlink {
			return "", fmt.Errorf("regular entry materialized as a symlink")
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("regular entry materialized as %s", info.Mode())
		}
		ownerExec := info.Mode().Perm()&0o100 != 0
		if wantExec := mode == "100755"; ownerExec != wantExec {
			return "", fmt.Errorf("mode %s entry materialized with permissions %s", mode, info.Mode().Perm())
		}
		content, err = os.ReadFile(path) //nolint:gosec // G304: path is dest-joined from the head tree's own entries
		if err != nil {
			return "", err
		}
	}
	h := sha1.New() //nolint:gosec // G401: git object identity, not a cryptographic protection (see the import note)
	h.Write([]byte("blob " + strconv.Itoa(len(content))))
	h.Write([]byte{0})
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil)), nil
}
