package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

// ignoredAdditions identifies content that is neither tracked at the trusted
// base nor requested by the work unit. Only base-owned ignore rules can exclude
// it from construction. All returned bytes still pass the ordinary intake and
// security checks; this is not a finding-profile exemption.
func ignoredAdditions(ctx context.Context, g *gitRunner, base map[string]treeEntry, changes []plannedChange, opts Options, scratch string) (map[string]bool, error) {
	if opts.ExpectNoChanges || (opts.Policy.FindingProfile != nil && *opts.Policy.FindingProfile == FindingProfileSpecification) {
		return nil, nil
	}
	eligible := make(map[string]bool)
	var input bytes.Buffer
	for _, c := range changes {
		if c.kind == ChangeAdded && c.mode != "" && opts.Policy.Allowlist != nil && !MatchesAllowlist(opts.Policy.Allowlist, c.path) {
			eligible[c.path] = true
			input.WriteString(c.path)
			input.WriteByte(0)
		}
	}
	if len(eligible) == 0 {
		return nil, nil
	}
	rules := selectIgnoreRules(base, eligible)
	if len(rules) == 0 {
		return nil, nil
	}
	if err := validateIgnoreSpellings(rules, eligible); err != nil {
		return nil, err
	}
	root := filepath.Join(scratch, "base-ignore-tree")
	matcher, err := gitrun.New(gitrun.Options{
		GitPath: opts.GitPath, Scratch: filepath.Join(scratch, "base-ignore-control"), Class: ErrGitPlumbing,
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

func validateIgnoreSpellings(rules []string, eligible map[string]bool) error {
	type spelling struct{ raw, folded string }
	names := make([]spelling, 0, len(rules)+len(eligible))
	for _, name := range rules {
		names = append(names, spelling{name, strings.ReplaceAll(pathfold.FoldPath(name), "/", "\x00")})
	}
	for name := range eligible {
		dir := parentDir(name)
		names = append(names, spelling{dir, strings.ReplaceAll(pathfold.FoldPath(dir), "/", "\x00")})
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
