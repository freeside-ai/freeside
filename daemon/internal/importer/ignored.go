package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

// ignoredAdditions identifies additions the exact trusted base ignores and
// does not track: a new regular file, including one whose blob the export caps
// omitted, or a new nested repository (a submodule entry, such as a package
// manager's checkout), with nothing tracked at or beneath its path. Only
// base-owned ignore rules can exclude it from construction. Every returned
// byte still passes the ordinary intake and security checks first; the caller
// drops only the findings that describe content no commit will hold
// (FindingKind.inapplicableToExcludedAddition). Handoffs that must carry no
// repository change keep every addition, so ignored debris still refuses them.
func ignoredAdditions(ctx context.Context, g *gitRunner, base map[string]treeEntry, changes []plannedChange, opts Options, scratch string) (map[string]bool, error) {
	if opts.ExpectNoChanges {
		return nil, nil
	}
	// An addition over tracked base content (a directory replaced by a file or
	// a nested repository) is not new untracked content: excluding it would
	// hide that replacement, and a nested repository's opaque subtree would
	// retain the tracked files beneath it unseen.
	var tracked map[string]bool
	files, directories := make(map[string]bool), make(map[string]bool)
	for _, c := range changes {
		regular := c.mode == "100644" || c.mode == "100755"
		// check-ignore reads its input as pathspecs: a leading ':' is magic
		// that would match a different path than the candidate's.
		if c.kind != ChangeAdded || (!regular && !c.nestedRepository) || strings.HasPrefix(c.path, ":") {
			continue
		}
		if tracked == nil {
			tracked = trackedDirectories(base)
		}
		if tracked[c.path] {
			continue
		}
		if c.nestedRepository {
			directories[c.path] = true
		} else {
			files[c.path] = true
		}
	}
	ignored, err := matchIgnored(ctx, g, base, files, false, opts, filepath.Join(scratch, "base-ignore-files"))
	if err != nil {
		return nil, err
	}
	ignoredDirectories, err := matchIgnored(ctx, g, base, directories, true, opts, filepath.Join(scratch, "base-ignore-directories"))
	if err != nil {
		return nil, err
	}
	if ignored == nil {
		return ignoredDirectories, nil
	}
	maps.Copy(ignored, ignoredDirectories)
	return ignored, nil
}

// matchIgnored asks Git which eligible paths the exact base's rules ignore, in
// a fresh scratch repository under scratch. Git judges a directory by its own
// name only when that name is a directory on disk, so for nested repositories
// (directories true) it creates each as an empty directory; files and
// directories match in separate trees so a created directory never changes
// how a candidate file is judged.
func matchIgnored(ctx context.Context, g *gitRunner, base map[string]treeEntry, eligible map[string]bool, directories bool, opts Options, scratch string) (map[string]bool, error) {
	if len(eligible) == 0 {
		return nil, nil
	}
	rules := selectIgnoreRules(base, eligible)
	if len(rules) == 0 {
		return nil, nil
	}
	if err := validateIgnoreSpellings(rules, eligible, directories); err != nil {
		return nil, err
	}
	root := filepath.Join(scratch, "tree")
	matcher, err := gitrun.New(gitrun.Options{
		GitPath: opts.GitPath, Scratch: filepath.Join(scratch, "control"), Class: ErrGitPlumbing,
		ConfigExtra: []string{"-c", "core.excludesFile=/dev/null", "-c", "core.ignoreCase=false"},
	})
	if err != nil {
		return nil, err
	}
	// A fresh repository has no checkout-local config, info/exclude, hooks or
	// index. The real checkout and workspace are never the matcher's worktree.
	if _, err := matcher.Run(ctx, nil, "init", "--quiet", "--template=", root); err != nil {
		return nil, err
	}
	if err := materializeIgnoreRules(ctx, g, base, rules, root, opts.Policy); err != nil {
		return nil, err
	}
	var input bytes.Buffer
	for name := range eligible {
		if directories {
			if err := materializeCandidateDirectory(root, name, opts.Policy); err != nil {
				return nil, err
			}
		}
		input.WriteString(name)
		input.WriteByte(0)
	}
	var out bytes.Buffer
	err = matcher.RunTo(ctx, &input, &out, "-C", root, "check-ignore", "--stdin", "-z", "--verbose", "--non-matching", "--no-index")
	if err != nil {
		var exit interface{ ExitCode() int }
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, err
		}
	}
	return decodeIgnoredAdditions(out.Bytes(), eligible, rules)
}

// materializeCandidateDirectory creates an empty directory at a nested
// repository's path. Its name is untrusted: it must be canonical and free of
// Git metadata components, and spelling validation has already refused any
// alias of a rule's directory.
func materializeCandidateDirectory(root, name string, pol Policy) error {
	if !export.ValidCanonicalPath(name) || int64(len(name)) > pol.MaxPathBytes {
		return fmt.Errorf("nested repository path is not representable: %w", ErrUnsupportedRepo)
	}
	components := strings.Split(name, "/")
	if len(components) > pol.MaxPathDepth {
		return fmt.Errorf("nested repository path exceeds depth cap: %w", ErrUnsupportedRepo)
	}
	for _, component := range components {
		if GitUnsafeComponent(component) {
			return fmt.Errorf("nested repository path contains Git metadata: %w", ErrUnsupportedRepo)
		}
	}
	return os.MkdirAll(filepath.Join(root, filepath.FromSlash(name)), 0o700)
}

// trackedDirectories returns every directory that holds a base path, so an
// addition at one of them is known to sit over tracked content.
func trackedDirectories(base map[string]treeEntry) map[string]bool {
	dirs := make(map[string]bool)
	for name := range base {
		for dir := parentDir(name); dir != "" && !dirs[dir]; dir = parentDir(dir) {
			dirs[dir] = true
		}
	}
	return dirs
}

// Keep only real base rules, never a map of every candidate ancestor. Substring
// ancestor lookups bound retained path bytes by the input and trusted rule set.
func selectIgnoreRules(base map[string]treeEntry, eligible map[string]bool) []string {
	byDirectory := make(map[string]string)
	for name, entry := range base {
		if path.Base(name) == ".gitignore" && (entry.mode == "100644" || entry.mode == "100755") {
			byDirectory[parentDir(name)] = name
		}
	}
	if len(byDirectory) == 0 {
		return nil
	}
	selected := make(map[string]bool)
	for name := range eligible {
		for dir := parentDir(name); ; dir = parentDir(dir) {
			if rule, exists := byDirectory[dir]; exists {
				selected[rule] = true
			}
			if dir == "" {
				break
			}
		}
	}
	rules := make([]string, 0, len(selected))
	for rule := range selected {
		rules = append(rules, rule)
	}
	slices.Sort(rules)
	return rules
}

// validateIgnoreSpellings refuses directory spellings that alias on the
// checkout filesystem. directories adds each eligible name itself, since
// matching creates it as a directory.
func validateIgnoreSpellings(rules []string, eligible map[string]bool, directories bool) error {
	type spelling struct{ raw, folded string }
	names := make([]spelling, 0, len(rules)+2*len(eligible))
	add := func(name string) {
		names = append(names, spelling{name, strings.ReplaceAll(pathfold.FoldPath(name), "/", "\x00")})
	}
	for _, name := range rules {
		add(name)
	}
	for name := range eligible {
		add(parentDir(name))
		if directories {
			add(name)
		}
	}
	slices.SortFunc(names, func(a, b spelling) int { return strings.Compare(a.folded, b.folded) })
	// NUL sorts before every valid path byte, keeping a directory next to its
	// descendants even with siblings such as "a-" between "a" and "a/x" in
	// ordinary byte order. Compare their raw
	// components without retaining the quadratic set of all ancestor strings.
	for i := 1; i < len(names); i++ {
		a, b := strings.Split(names[i-1].raw, "/"), strings.Split(names[i].raw, "/")
		for j := 0; j < min(len(a), len(b)); j++ {
			if CheckoutFoldComponent(a[j]) != CheckoutFoldComponent(b[j]) {
				break
			}
			if a[j] != b[j] {
				return fmt.Errorf("ignore paths alias on the checkout filesystem: %w", ErrUnsupportedRepo)
			}
		}
	}
	return nil
}

// Materialization copies only canonical regular .gitignore blobs from the
// enforced tree. It never checks out files, follows a base symlink, or reads a
// candidate rule. Ambiguous filesystem aliases fail closed on every platform.
func materializeIgnoreRules(ctx context.Context, g *gitRunner, base map[string]treeEntry, rules []string, root string, pol Policy) error {
	if len(rules) > pol.MaxEntries {
		return fmt.Errorf("too many base ignore files: %w", ErrUnsupportedRepo)
	}
	var total int64
	for _, name := range rules {
		if !export.ValidCanonicalPath(name) || int64(len(name)) > pol.MaxPathBytes {
			return fmt.Errorf("base ignore path is not representable: %w", ErrUnsupportedRepo)
		}
		components := strings.Split(name, "/")
		if len(components) > pol.MaxPathDepth {
			return fmt.Errorf("base ignore path exceeds depth cap: %w", ErrUnsupportedRepo)
		}
		for _, component := range components {
			if GitUnsafeComponent(component) {
				return fmt.Errorf("base ignore path contains Git metadata: %w", ErrUnsupportedRepo)
			}
		}
		size, err := g.blobSize(ctx, base[name].oid)
		if err != nil {
			return err
		}
		if size < 0 || size > pol.MaxBlobBytes || size > pol.MaxTotalBytes-total {
			return fmt.Errorf("base ignore files exceed import byte caps: %w", ErrUnsupportedRepo)
		}
		total += size
		body, err := g.blobContent(ctx, base[name].oid)
		if err != nil {
			return err
		}
		if int64(len(body)) != size {
			return fmt.Errorf("base ignore blob size changed: %w", ErrTreeMismatch)
		}
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func decodeIgnoredAdditions(out []byte, eligible map[string]bool, rules []string) (map[string]bool, error) {
	fields := bytes.Split(out, []byte{0})
	if len(fields) != len(eligible)*4+1 || len(fields[len(fields)-1]) != 0 {
		return nil, fmt.Errorf("incomplete check-ignore result: %w", ErrGitPlumbing)
	}
	ignored := make(map[string]bool)
	seen := make(map[string]bool)
	for i := 0; i < len(fields)-1; i += 4 {
		source, pattern, name := string(fields[i]), string(fields[i+2]), string(fields[i+3])
		if !eligible[name] || seen[name] {
			return nil, fmt.Errorf("check-ignore returned an unknown or repeated path: %w", ErrGitPlumbing)
		}
		seen[name] = true
		if source == "" || pattern == "" || strings.HasPrefix(pattern, "!") {
			continue
		}
		// On a case-insensitive host Git might find an unrelated rule through
		// a directory alias. Its source must be an exact base rule and an exact
		// ancestor of the queried path before it can authorize an exclusion.
		if slices.Contains(rules, source) && (source == ".gitignore" || strings.HasPrefix(name, path.Dir(source)+"/")) {
			ignored[name] = true
		}
	}
	return ignored, nil
}
