package verify

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

// The variables a history command sees, besides git's ownership override.
const (
	// EnvVerifyBaseSHA names the range's base: the single merge base of
	// the enforced base and the candidate head.
	EnvVerifyBaseSHA = "FREESIDE_VERIFY_BASE_SHA"
	// EnvVerifyHeadSHA names the candidate head, never the prospective
	// merge a base-advance re-entry verifies the tree of.
	EnvVerifyHeadSHA = "FREESIDE_VERIFY_HEAD_SHA"
)

// historyConfig is the whole of the written repository's config. Nothing
// in it comes from the daemon checkout or the candidate.
const historyConfig = "[core]\n\trepositoryformatversion = 0\n\tbare = false\n"

// history is the verifier-written .git a history command gets, built once
// per verification in the scratch directory and copied into each history
// command's workspace after that workspace passes its exact-tree check.
//
// It is a shallow repository: the commits in base..head, the boundary
// commits listed in shallow, and every object in those commits' trees, in
// one pack. HEAD is detached at the head. There are no refs, hooks,
// info/attributes, index, or anything else, and nothing is read from the
// candidate's tree. Both of #3121's scripts call `git merge-base`, which
// returns the base on this shape because the base is a boundary commit.
type history struct {
	gitDir  string
	baseSHA string
	// env is what the room adds to the command's environment: the two
	// SHAs and safe.directory=*, since the room may run the command as a
	// uid other than the workspace's owner and git refuses a repository
	// another user owns. GIT_CONFIG_COUNT entries count as command-line
	// config, which git reads safe.directory from; "*" because the
	// verifier doesn't know the in-room path, and the only repository it
	// writes is its own.
	env []string
}

// buildHistory writes the history for headSHA against baseSHA under
// scratch. treeSHA is the commit whose tree each workspace holds (the
// head, or the prospective merge on re-entry); its paths are checked so
// the written .git can't collide with or be shadowed by a tree entry.
func buildHistory(ctx context.Context, g *gitRunner, scratch, treeSHA, baseSHA, headSHA string) (*history, error) {
	if err := rejectGitUnsafePaths(ctx, g, treeSHA); err != nil {
		return nil, err
	}
	// The base is the merge base, not baseSHA itself: on re-entry baseSHA
	// is the moved tip, and base..head must stay exactly the candidate's
	// commits without carrying every mainline commit in between.
	out, err := g.run(ctx, nil, "merge-base", "--all", baseSHA, headSHA)
	if err != nil {
		return nil, fmt.Errorf("merge base of %s and %s: %w: %w", baseSHA, headSHA, ErrHistoryUnavailable, err)
	}
	bases, err := shaLines(out)
	if err != nil {
		return nil, err
	}
	if len(bases) != 1 {
		return nil, fmt.Errorf("%s and %s have %d merge bases, want exactly one: %w", baseSHA, headSHA, len(bases), ErrHistoryUnavailable)
	}
	base := bases[0]
	commits, boundary, err := rangeCommits(ctx, g, base, headSHA)
	if err != nil {
		return nil, err
	}
	// List the objects of each commit on its own, not by a walk that
	// excludes the base: such a walk drops a blob a candidate commit
	// restores from history older than the base.
	listed := strings.Join(append(slices.Clone(commits), boundary...), "\n") + "\n"
	objects, err := g.run(ctx, strings.NewReader(listed),
		"rev-list", "--objects", "--no-object-names", "--no-walk=unsorted", "--stdin")
	if err != nil {
		return nil, fmt.Errorf("list history objects: %w", err)
	}
	gitDir := filepath.Join(scratch, "history", ".git")
	packDir := filepath.Join(gitDir, "objects", "pack")
	for _, dir := range []string{packDir, filepath.Join(gitDir, "refs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create history directory: %w", err)
		}
	}
	if err := writePack(ctx, g, packDir, objects); err != nil {
		return nil, err
	}
	shallow := strings.Join(boundary, "\n") + "\n"
	for name, content := range map[string]string{
		"HEAD":    headSHA + "\n",
		"config":  historyConfig,
		"shallow": shallow,
	} {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte(content), 0o600); err != nil {
			return nil, fmt.Errorf("write history %s: %w", name, err)
		}
	}
	return &history{
		gitDir:  gitDir,
		baseSHA: base,
		env: []string{
			EnvVerifyBaseSHA + "=" + base,
			EnvVerifyHeadSHA + "=" + headSHA,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=safe.directory",
			"GIT_CONFIG_VALUE_0=*",
		},
	}, nil
}

// writePack writes the listed objects into packDir as one pack and its
// index, with no reverse index, so the file set is fixed whatever the git
// version's default. pack-objects streams the pack to a file in packDir
// rather than writing it itself: given a destination, it builds its
// temporary pack in the daemon checkout's object directory, which would
// write into the trusted checkout and fail when scratch sits on another
// volume. index-pack then indexes the file where it is and names it.
func writePack(ctx context.Context, g *gitRunner, packDir string, objects []byte) error {
	tmp := filepath.Join(packDir, "incoming.pack")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a fixed name in the verifier's own scratch
	if err != nil {
		return fmt.Errorf("create history pack: %w", err)
	}
	packErr := g.shared.RunTo(ctx, bytes.NewReader(objects), f, "pack-objects", "-q", "--stdout")
	if closeErr := f.Close(); packErr == nil {
		packErr = closeErr
	}
	if packErr != nil {
		return fmt.Errorf("pack history objects: %w", packErr)
	}
	out, err := g.run(ctx, nil, "index-pack", "--no-rev-index", tmp)
	if err != nil {
		return fmt.Errorf("index history pack: %w", err)
	}
	names, err := shaLines(out)
	if err != nil || len(names) != 1 {
		return fmt.Errorf("index-pack named %q, want one pack: %w", out, ErrGitPlumbing)
	}
	for _, ext := range []string{".pack", ".idx"} {
		from := strings.TrimSuffix(tmp, ".pack") + ext
		if err := os.Rename(from, filepath.Join(packDir, "pack-"+names[0]+ext)); err != nil {
			return fmt.Errorf("name history pack: %w", err)
		}
	}
	return nil
}

// rangeCommits returns the commits in base..head and the sorted boundary
// commits: the parents of those commits that the range excludes. The base
// is always a boundary, also when head is the base and the range is empty,
// so HEAD and the shallow file always name a commit the pack holds.
func rangeCommits(ctx context.Context, g *gitRunner, base, head string) (commits, boundary []string, err error) {
	out, err := g.run(ctx, nil, "rev-list", "--boundary", base+".."+head)
	if err != nil {
		return nil, nil, fmt.Errorf("list history commits: %w", err)
	}
	boundary = []string{base}
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		sha, isBoundary := strings.CutPrefix(line, "-")
		if !pathfold.ValidSHA1Hex(sha) {
			return nil, nil, fmt.Errorf("unparseable rev-list line %q: %w", line, ErrGitPlumbing)
		}
		if isBoundary {
			boundary = append(boundary, sha)
		} else {
			commits = append(commits, sha)
		}
	}
	slices.Sort(boundary)
	return commits, slices.Compact(boundary), nil
}

// rejectGitUnsafePaths refuses a tree holding a path component git treats
// as its own directory (.git and its case, NTFS, and 8.3 aliases): the
// workspace's .git must be only the one the verifier writes.
func rejectGitUnsafePaths(ctx context.Context, g *gitRunner, treeSHA string) error {
	entries, gitlinks, err := g.listTree(ctx, treeSHA)
	if err != nil {
		return err
	}
	for _, p := range append(slices.Collect(maps.Keys(entries)), gitlinks...) {
		for _, seg := range strings.Split(p, "/") {
			if importer.GitUnsafeComponent(seg) {
				// Also ErrMalformedTree: a tree no history command can
				// safely get a .git beside, which a retry meets again.
				return fmt.Errorf("tree path %q has component %q, which a history command's .git would collide with: %w: %w", p, seg, ErrHistoryUnavailable, ErrMalformedTree)
			}
		}
	}
	return nil
}

// base returns the range's base, or "" when the recipe asked for no
// history (h is nil).
func (h *history) base() string {
	if h == nil {
		return ""
	}
	return h.baseSHA
}

// install copies the written .git into a workspace whose exact-tree check
// has passed. The workspace's tree holds no .git (rejectGitUnsafePaths),
// and os.CopyFS refuses to overwrite a file.
func (h *history) install(workspace string) error {
	if err := os.CopyFS(filepath.Join(workspace, ".git"), os.DirFS(h.gitDir)); err != nil {
		return fmt.Errorf("install history into workspace: %w", err)
	}
	return nil
}

// shaLines parses git output that must be one full SHA per line.
func shaLines(out []byte) ([]string, error) {
	var shas []string
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		if !pathfold.ValidSHA1Hex(line) {
			return nil, fmt.Errorf("unparseable object name %q: %w", line, ErrGitPlumbing)
		}
		shas = append(shas, line)
	}
	return shas, nil
}
