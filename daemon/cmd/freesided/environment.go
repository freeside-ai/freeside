package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type environment string

const (
	environmentProd      environment = "prod"
	environmentDev       environment = "dev"
	environmentEphemeral environment = "ephemeral"
	defaultEnvironment   environment = environmentEphemeral
)

var AllEnvironments = []environment{
	environmentProd,
	environmentDev,
	environmentEphemeral,
}

func (env environment) valid() bool {
	switch env {
	case environmentProd, environmentDev, environmentEphemeral:
		return true
	default:
		return false
	}
}

// locksDatabaseExclusively reports whether the tier's daemon holds its live
// database against every other process. The supervised tiers do, so nothing
// outside the daemon can open or write their file while it runs; ephemeral
// keeps normal locking for tests and scratch runs.
func (env environment) locksDatabaseExclusively() bool {
	switch env {
	case environmentProd, environmentDev:
		return true
	case environmentEphemeral:
		return false
	}
	return false
}

func parseEnvironment(raw string) (environment, error) {
	env := environment(raw)
	if !env.valid() {
		return "", fmt.Errorf("-environment %q is not prod, dev, or ephemeral", raw)
	}
	return env, nil
}

type environmentPaths struct {
	DB                        string
	StateDir                  string
	PublicationStateDir       string
	PublicationCredentialsDir string
	ReviewInputRoot           string
}

type guardedPath struct {
	flag string
	path string
}

func (paths environmentPaths) guarded() []guardedPath {
	guarded := []guardedPath{
		{"db", paths.DB},
		{"state-dir", paths.StateDir},
		{"publication-state-dir", paths.PublicationStateDir},
		{"publication-credentials-dir", paths.PublicationCredentialsDir},
		{"review-input-root", paths.ReviewInputRoot},
	}
	if paths.DB != "" {
		guarded = append(guarded,
			guardedPath{"db", paths.DB + "-wal"},
			guardedPath{"db", paths.DB + "-shm"})
	}
	return guarded
}

func supervisedRoots(home string) map[environment]string {
	return map[environment]string{
		environmentProd: filepath.Join(home, "Library", "Application Support", "Freeside"),
		environmentDev:  filepath.Join(home, "Library", "Application Support", "Freeside Dev"),
	}
}

// resolveExisting follows symlinks even when the final path or its target has
// not been created yet. A database sidecar can itself be a dangling symlink.
func resolveExisting(path string) (string, error) {
	current := path
	if !filepath.IsAbs(current) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		current = cwd + string(filepath.Separator) + current
	}
	current = strings.TrimRight(current, string(filepath.Separator))
	if current == "" {
		current = string(filepath.Separator)
	}
	original := current
	var missing []string
	symlinks := 0
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				symlinks++
				if symlinks > 40 {
					return "", fmt.Errorf("resolve %q: too many symlinks", path)
				}
				target, err := os.Readlink(current)
				if err != nil {
					return "", err
				}
				if !filepath.IsAbs(target) {
					// Keep .. after a symlink component until EvalSymlinks walks it.
					target = current[:strings.LastIndex(current, string(filepath.Separator))+1] + target
				}
				current = target
				continue
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			if resolved != original {
				// Re-check a path reconstructed after a missing component. Joining
				// can expose a symlink through a later ".." component.
				current = resolved
				original = current
				missing = nil
				continue
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		slash := strings.LastIndex(current, string(filepath.Separator))
		if slash < 0 || current == string(filepath.Separator) {
			return "", fmt.Errorf("resolve %q: no existing ancestor", path)
		}
		missing = append(missing, current[slash+1:])
		current = current[:slash]
		if current == "" {
			current = string(filepath.Separator)
		}
	}
}

func linkedUnderSupervisedRoot(path string, roots map[environment]string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("inspect database sidecar link count")
	}
	if stat.Nlink <= 1 {
		return false, nil
	}
	for _, root := range roots {
		matched := false
		err := filepath.WalkDir(root, func(candidate string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			candidateInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if os.SameFile(info, candidateInfo) {
				matched = true
			}
			return nil
		})
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func underRoot(path, root string) bool {
	if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
		return true
	}
	if caseFoldUnderRoot(path, root) {
		// On a case-insensitive filesystem a differently cased spelling names the
		// same directory. On a case-sensitive filesystem it must not grant a
		// supervised tier access to a separate directory.
		rootInfo, rootErr := os.Stat(root)
		aliasInfo, aliasErr := os.Stat(path[:len(root)])
		if rootErr == nil && aliasErr == nil && os.SameFile(rootInfo, aliasInfo) {
			return true
		}
	}

	return rootIdentityContains(path, root, false)
}

// mayBeUnderRoot is deliberately conservative when rejecting access to a
// supervised root. A missing root has no inode to compare, so its suffix is
// folded only after the existing ancestor identity has matched.
func mayBeUnderRoot(path, root string) bool {
	return underRoot(path, root) || rootIdentityContains(path, root, true)
}

func rootIdentityContains(path, root string, foldSuffix bool) bool {
	rootAncestor, err := existingAncestor(root)
	if err != nil {
		return false
	}
	rootInfo, err := os.Stat(rootAncestor)
	if err != nil {
		return false
	}
	rootSuffix, err := filepath.Rel(rootAncestor, root)
	if err != nil {
		return false
	}
	for ancestor := path; ; ancestor = filepath.Dir(ancestor) {
		ancestorInfo, err := os.Stat(ancestor)
		if err == nil && os.SameFile(rootInfo, ancestorInfo) {
			candidateSuffix, err := filepath.Rel(ancestor, path)
			if err == nil && suffixUnderRoot(candidateSuffix, rootSuffix, foldSuffix) {
				return true
			}
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return false
		}
	}
}

func suffixUnderRoot(path, root string, fold bool) bool {
	if root == "." {
		return true
	}
	if fold {
		return strings.EqualFold(path, root) ||
			(len(path) > len(root) && strings.EqualFold(path[:len(root)], root) && path[len(root)] == filepath.Separator)
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func existingAncestor(path string) (string, error) {
	for current := path; ; current = filepath.Dir(current) {
		if _, err := os.Stat(current); err == nil {
			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("%q has no existing ancestor", path)
		}
	}
}

func caseFoldUnderRoot(path, root string) bool {
	return strings.EqualFold(path, root) ||
		(len(path) > len(root) && strings.EqualFold(path[:len(root)], root) && path[len(root)] == filepath.Separator)
}

func checkEnvironment(env environment, home string, paths environmentPaths, listen string) error {
	if !env.valid() {
		return fmt.Errorf("invalid environment %q", env)
	}
	roots := supervisedRoots(home)
	resolvedRoots := make(map[environment]string, len(roots))
	for tier, root := range roots {
		resolved, err := resolveExisting(root)
		if err != nil {
			return fmt.Errorf("resolve %s state root %q: %w", tier, root, err)
		}
		resolvedRoots[tier] = resolved
	}
	if env != environmentEphemeral {
		for _, required := range []guardedPath{{"db", paths.DB}, {"state-dir", paths.StateDir}} {
			if required.path == "" {
				return fmt.Errorf("-%s is required for the %s environment", required.flag, env)
			}
		}
	}
	for _, candidate := range paths.guarded() {
		if candidate.path == "" {
			continue
		}
		resolved, err := resolveExisting(candidate.path)
		if err != nil {
			return fmt.Errorf("-%s %q cannot be resolved for the %s environment: %w", candidate.flag, candidate.path, env, err)
		}
		for tier, root := range resolvedRoots {
			if !caseFoldUnderRoot(resolved, root) && !mayBeUnderRoot(resolved, root) {
				continue
			}
			if env == environmentEphemeral || env != tier {
				return fmt.Errorf("-%s %q resolves under the %s state root %q; the %s environment may not use it", candidate.flag, resolved, tier, root, env)
			}
		}
		if candidate.path == paths.DB+"-wal" || candidate.path == paths.DB+"-shm" {
			linked, err := linkedUnderSupervisedRoot(candidate.path, resolvedRoots)
			if err != nil {
				return fmt.Errorf("inspect -%s %q for supervised hard links: %w", candidate.flag, candidate.path, err)
			}
			if linked {
				return fmt.Errorf("-%s %q is hard linked under a supervised state root; the %s environment may not use it", candidate.flag, candidate.path, env)
			}
		}
		if env != environmentEphemeral && (candidate.flag == "db" || candidate.flag == "state-dir" || candidate.flag == "publication-state-dir") && !underRoot(resolved, resolvedRoots[env]) {
			return fmt.Errorf("-%s %q resolves outside the %s state root %q", candidate.flag, resolved, env, resolvedRoots[env])
		}
	}
	if env == environmentEphemeral {
		resolved, err := net.ResolveTCPAddr("tcp", listen)
		if err == nil {
			for tier, port := range map[environment]int{environmentProd: 7331, environmentDev: 7332} {
				if resolved.Port == port {
					return fmt.Errorf("-listen %q names port %d, reserved for the %s tier; the %s environment may not use it", listen, port, tier, env)
				}
			}
		}
	}
	return nil
}
