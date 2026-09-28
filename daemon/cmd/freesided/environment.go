package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
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
	// ProdAppAuthority exempts exactly prod's two App directories from the
	// ephemeral tier's supervised-root refusal, so an attended real-work run
	// shares prod's one App authority (#1583). main sets it only for
	// -prod-app-authority, after checkProdAppAuthority passes.
	ProdAppAuthority bool
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
	exempt := map[string]bool{}
	if paths.ProdAppAuthority {
		if env != environmentEphemeral {
			return fmt.Errorf("-prod-app-authority requires -environment ephemeral, not %s", env)
		}
		if err := requireProdAppDirectories(roots[environmentProd], paths); err != nil {
			return err
		}
		exempt["publication-state-dir"] = true
		exempt["publication-credentials-dir"] = true
	}
	if env != environmentEphemeral {
		for _, required := range []guardedPath{{"db", paths.DB}, {"state-dir", paths.StateDir}} {
			if required.path == "" {
				return fmt.Errorf("-%s is required for the %s environment", required.flag, env)
			}
		}
	}
	for _, candidate := range paths.guarded() {
		if candidate.path == "" || exempt[candidate.flag] {
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

// requireProdAppDirectories accepts the two publication directories only when
// they are prod's own App directories: the same directories, by identity, as
// <prod root>/daemon and <prod root>/credentials. Nothing else under the root,
// and no copy outside it, qualifies.
func requireProdAppDirectories(prodRoot string, paths environmentPaths) error {
	for _, dir := range []struct {
		flag string
		path string
		want string
	}{
		{"publication-state-dir", paths.PublicationStateDir, filepath.Join(prodRoot, "daemon")},
		{"publication-credentials-dir", paths.PublicationCredentialsDir, filepath.Join(prodRoot, "credentials")},
	} {
		if dir.path == "" {
			return fmt.Errorf("-prod-app-authority requires -%s %q", dir.flag, dir.want)
		}
		got, err := os.Stat(dir.path)
		if err != nil {
			return fmt.Errorf("-%s %q: %w", dir.flag, dir.path, err)
		}
		want, err := os.Stat(dir.want)
		if err != nil {
			return fmt.Errorf("prod App directory %q: %w", dir.want, err)
		}
		if !got.IsDir() || !os.SameFile(got, want) {
			return fmt.Errorf("-%s %q is not prod's App directory %q; -prod-app-authority accepts only that directory", dir.flag, dir.path, dir.want)
		}
	}
	return nil
}

// checkProdAppAuthority gates -prod-app-authority before the path check
// grants its exemption. The run must hold the live production rig lease,
// which also keeps prod stopped (checkProdRigLease); $HOME and the account
// home must name one prod root, so the run and a launchd-started prod can't
// read different authorities; and the installed prod daemon must accept every
// App authority state format this build writes.
func checkProdAppAuthority(ctx context.Context, env environment, home, accountHome, rigTokenFile, leaseRoot, prodDaemon string) error {
	if env != environmentEphemeral {
		return fmt.Errorf("-prod-app-authority requires -environment ephemeral, not %s", env)
	}
	if prodDaemon == "" {
		return errors.New("-prod-app-authority requires -prod-daemon naming the installed prod daemon")
	}
	if err := requireOtherDaemon(prodDaemon); err != nil {
		return err
	}
	if err := authenticateProductionRigLease(rigTokenFile, leaseRoot); err != nil {
		return err
	}
	if err := requireOneProdRoot(home, accountHome); err != nil {
		return err
	}
	return checkProdPublicationFormats(ctx, prodDaemon)
}

// authenticateProductionRigLease proves the caller holds the live rig lease
// published under leaseRoot, the root prod checks at startup.
func authenticateProductionRigLease(rigTokenFile, leaseRoot string) error {
	if rigTokenFile == "" {
		return errors.New("-prod-app-authority requires -rig-token-file naming the live production rig lease")
	}
	acquisition, err := readRigAcquisition(rigTokenFile)
	if err != nil {
		return err
	}
	manifest, err := daemonlock.AuthenticateRig(acquisition.Manifest.Resources.StateRoot, acquisition.Token)
	if err != nil {
		return fmt.Errorf("-prod-app-authority requires a live production rig lease: %w", err)
	}
	resolvedLeaseRoot, err := filepath.EvalSymlinks(leaseRoot)
	if err != nil {
		return fmt.Errorf("resolve production rig lease root %q: %w", leaseRoot, err)
	}
	if manifest.Resources.LeaseRoot != resolvedLeaseRoot {
		return fmt.Errorf("rig lease under %q is not the production rig lease under %q", manifest.Resources.LeaseRoot, resolvedLeaseRoot)
	}
	// prod checks the manifest, not the lock: a lease whose manifest is gone
	// would not keep prod stopped.
	present, err := daemonlock.RigManifestPresent(resolvedLeaseRoot)
	if err != nil {
		return fmt.Errorf("check the production rig manifest under %q: %w", resolvedLeaseRoot, err)
	}
	if !present {
		return fmt.Errorf("the production rig lease under %q has no manifest, so prod would not see it", resolvedLeaseRoot)
	}
	return nil
}

// requireOtherDaemon refuses a -prod-daemon that is this binary: the run's own
// build always accepts its own formats, so the check would prove nothing.
func requireOtherDaemon(prodDaemon string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this daemon: %w", err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("stat this daemon: %w", err)
	}
	prodInfo, err := os.Stat(prodDaemon)
	if err != nil {
		return fmt.Errorf("stat -prod-daemon %q: %w", prodDaemon, err)
	}
	if os.SameFile(selfInfo, prodInfo) {
		return fmt.Errorf("-prod-daemon %q is this daemon, not the installed prod daemon", prodDaemon)
	}
	return nil
}

// requireOneProdRoot compares the two prod roots by directory identity: a
// case-only or firmlinked spelling of one home names the same root.
func requireOneProdRoot(home, accountHome string) error {
	fromHome := supervisedRoots(home)[environmentProd]
	fromAccount := supervisedRoots(accountHome)[environmentProd]
	homeInfo, err := os.Stat(fromHome)
	if err != nil {
		return fmt.Errorf("-prod-app-authority requires prod's root: %w", err)
	}
	accountInfo, err := os.Stat(fromAccount)
	if err != nil {
		return fmt.Errorf("-prod-app-authority requires prod's root: %w", err)
	}
	if !os.SameFile(homeInfo, accountInfo) {
		return fmt.Errorf("$HOME names prod root %q but the account home names %q; -prod-app-authority needs one prod root", fromHome, fromAccount)
	}
	return nil
}

// holdProdDatabaseLock takes prod's own database lock, the one a prod daemon
// takes before it opens its database, and holds it for the run. The rig lease
// check in rig hold sees only a launchd-loaded prod; the lock also refuses a
// prod started by hand, and keeps one from starting while the run holds it.
func holdProdDatabaseLock(home string) (*daemonlock.Lock, error) {
	path := filepath.Join(supervisedRoots(home)[environmentProd], "daemon", "freeside.db")
	lock, err := daemonlock.Acquire(path)
	if err != nil {
		return nil, fmt.Errorf("-prod-app-authority requires prod to be stopped: take prod's database lock %q: %w", path, err)
	}
	return lock, nil
}

// holdProdLockForProdAppDirectories takes prod's database lock when state or
// credentials is one of prod's App directories under $HOME or the passwd home,
// and returns a nil lock otherwise. A command that reads the shared authority
// before the run's daemon starts, such as preflight, holds it so a prod
// started outside launchd can't use the authority at the same time.
func holdProdLockForProdAppDirectories(state, credentials string) (*daemonlock.Lock, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	accountHome, err := currentAccountHome()
	if err != nil {
		return nil, err
	}
	return holdProdLockForProdAppDirectoriesIn([]string{home, accountHome}, state, credentials)
}

func holdProdLockForProdAppDirectoriesIn(homes []string, state, credentials string) (*daemonlock.Lock, error) {
	for _, home := range homes {
		root := supervisedRoots(home)[environmentProd]
		if sameDirectory(state, filepath.Join(root, "daemon")) || sameDirectory(credentials, filepath.Join(root, "credentials")) {
			return holdProdDatabaseLock(home)
		}
	}
	return nil, nil
}

// sameDirectory reports whether both paths name one existing directory.
func sameDirectory(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aInfo, err := os.Stat(a)
	if err != nil {
		return false
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(aInfo, bInfo)
}

// checkProdRigLease keeps prod stopped while a production rig lease, live or
// stale, exists: the run shares prod's App authority, and rig hold checks for
// a loaded prod only when it acquires the lease.
func checkProdRigLease(leaseRoot string) error {
	present, err := daemonlock.RigManifestPresent(leaseRoot)
	if err != nil {
		return fmt.Errorf("check for a production rig lease under %q: %w", leaseRoot, err)
	}
	if present {
		return fmt.Errorf("a production rig lease (live or stale) exists under %q; prod may not start until the real-work run releases it, or until freesided rig recover clears a stale one", leaseRoot)
	}
	return nil
}
