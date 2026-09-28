package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
)

// prodAppDirs creates prod's two App directories under a temporary home.
func prodAppDirs(t *testing.T, home string) (state, credentials string) {
	t.Helper()
	root := supervisedRoots(home)[environmentProd]
	state = filepath.Join(root, "daemon")
	credentials = filepath.Join(root, "credentials")
	for _, dir := range []string{state, credentials} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return state, credentials
}

func TestProdAppAuthorityAcceptsOnlyProdAppDirectories(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	state, credentials := prodAppDirs(t, home)
	prodRoot := supervisedRoots(home)[environmentProd]
	devRoot := supervisedRoots(home)[environmentDev]
	for _, dir := range []string{filepath.Join(devRoot, "daemon"), filepath.Join(devRoot, "credentials"), filepath.Join(state, "nested")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	links := t.TempDir()
	stateLink := filepath.Join(links, "state")
	if err := os.Symlink(state, stateLink); err != nil {
		t.Fatal(err)
	}
	copyOutside := t.TempDir()
	exact := environmentPaths{PublicationStateDir: state, PublicationCredentialsDir: credentials, ProdAppAuthority: true}

	for _, tc := range []struct {
		name  string
		paths environmentPaths
	}{
		{"exact pair", exact},
		{"symlink to the exact directory", environmentPaths{PublicationStateDir: stateLink, PublicationCredentialsDir: credentials, ProdAppAuthority: true}},
	} {
		if err := checkEnvironment(environmentEphemeral, home, tc.paths, "127.0.0.1:0"); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}

	for _, tc := range []struct {
		name string
		env  environment
		edit func(*environmentPaths)
		want string
	}{
		{"flag unset", environmentEphemeral, func(p *environmentPaths) { p.ProdAppAuthority = false }, "-publication-state-dir"},
		{"subdirectory of daemon", environmentEphemeral, func(p *environmentPaths) { p.PublicationStateDir = filepath.Join(state, "nested") }, "not prod's App directory"},
		{"prod root itself", environmentEphemeral, func(p *environmentPaths) { p.PublicationStateDir = prodRoot }, "not prod's App directory"},
		{"swapped pair", environmentEphemeral, func(p *environmentPaths) {
			p.PublicationStateDir, p.PublicationCredentialsDir = credentials, state
		}, "not prod's App directory"},
		{"dev root", environmentEphemeral, func(p *environmentPaths) {
			p.PublicationStateDir = filepath.Join(devRoot, "daemon")
			p.PublicationCredentialsDir = filepath.Join(devRoot, "credentials")
		}, "not prod's App directory"},
		{"copy outside the root", environmentEphemeral, func(p *environmentPaths) { p.PublicationStateDir = copyOutside }, "not prod's App directory"},
		{"state without credentials", environmentEphemeral, func(p *environmentPaths) { p.PublicationCredentialsDir = "" }, "-publication-credentials-dir"},
		{"credentials without state", environmentEphemeral, func(p *environmentPaths) { p.PublicationStateDir = "" }, "-publication-state-dir"},
		{"other paths stay refused", environmentEphemeral, func(p *environmentPaths) { p.StateDir = state }, "-state-dir"},
		{"database stays refused", environmentEphemeral, func(p *environmentPaths) { p.DB = filepath.Join(state, "freeside.db") }, "-db"},
		{"prod tier", environmentProd, func(*environmentPaths) {}, "requires -environment ephemeral"},
		{"dev tier", environmentDev, func(*environmentPaths) {}, "requires -environment ephemeral"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			paths := exact
			tc.edit(&paths)
			if err := checkEnvironment(tc.env, home, paths, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkEnvironment() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProdAppAuthorityRequiresMissingDirectoriesToExist(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := supervisedRoots(home)[environmentProd]
	paths := environmentPaths{
		PublicationStateDir:       filepath.Join(root, "daemon"),
		PublicationCredentialsDir: filepath.Join(root, "credentials"),
		ProdAppAuthority:          true,
	}
	if err := checkEnvironment(environmentEphemeral, home, paths, "127.0.0.1:0"); err == nil {
		t.Fatal("checkEnvironment() accepted prod App directories that don't exist")
	}
}

func TestRequireOneProdRoot(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	prodAppDirs(t, home)
	if err := requireOneProdRoot(home, home); err != nil {
		t.Fatalf("same home: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if err := requireOneProdRoot(alias, home); err != nil {
		t.Fatalf("symlinked home: %v", err)
	}
	other := t.TempDir()
	prodAppDirs(t, other)
	if err := requireOneProdRoot(home, other); err == nil || !strings.Contains(err.Error(), "one prod root") {
		t.Fatalf("mismatched homes error = %v", err)
	}
	if err := requireOneProdRoot(home, t.TempDir()); err == nil || !strings.Contains(err.Error(), "prod's root") {
		t.Fatalf("missing account prod root error = %v", err)
	}
}

// acquireTestRig holds a real rig lease under a temporary lease root and
// writes the acquisition file rig hold would emit.
func acquireTestRig(t *testing.T) (lease *daemonlock.RigLease, leaseRoot, tokenFile string) {
	t.Helper()
	root := t.TempDir()
	leaseRoot = filepath.Join(root, "rig-locks")
	stateRoot := filepath.Join(root, "state")
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "rig-host", PID: os.Getpid()},
		StateRoot: stateRoot, DatabasePath: filepath.Join(stateRoot, "freeside.db"),
		ListenAddress: "127.0.0.1:8677", SeedRoot: filepath.Join(root, "seed"),
		LeaseRoot: leaseRoot,
		Now:       func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	tokenFile = writeAcquisition(t, lease.Token(), lease.Manifest())
	return lease, leaseRoot, tokenFile
}

func writeAcquisition(t *testing.T, token string, manifest daemonlock.RigManifest) string {
	t.Helper()
	body, err := json.Marshal(rigHoldOutput{Token: token, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rig.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthenticateProductionRigLease(t *testing.T) {
	t.Parallel()
	lease, leaseRoot, tokenFile := acquireTestRig(t)
	if err := authenticateProductionRigLease(tokenFile, leaseRoot); err != nil {
		t.Fatalf("live lease: %v", err)
	}
	if err := authenticateProductionRigLease("", leaseRoot); err == nil || !strings.Contains(err.Error(), "-rig-token-file") {
		t.Fatalf("missing token file error = %v", err)
	}
	wrongToken := writeAcquisition(t, strings.Repeat("0", 64), lease.Manifest())
	if err := authenticateProductionRigLease(wrongToken, leaseRoot); err == nil {
		t.Fatal("wrong token accepted")
	}
	otherRoot := t.TempDir()
	if err := authenticateProductionRigLease(tokenFile, otherRoot); err == nil || !strings.Contains(err.Error(), "not the production rig lease") {
		t.Fatalf("lease under another root error = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := authenticateProductionRigLease(tokenFile, leaseRoot); err == nil {
		t.Fatal("released lease accepted")
	}
}

func TestAuthenticateProductionRigLeaseRequiresTheManifest(t *testing.T) {
	t.Parallel()
	_, leaseRoot, tokenFile := acquireTestRig(t)
	// prod reads only the manifest, so a live lease without one would not
	// keep prod stopped.
	if err := os.Remove(filepath.Join(leaseRoot, "production-rig.json")); err != nil {
		t.Fatal(err)
	}
	if err := authenticateProductionRigLease(tokenFile, leaseRoot); err == nil || !strings.Contains(err.Error(), "no manifest") {
		t.Fatalf("lease without a manifest error = %v", err)
	}
}

func TestHoldProdDatabaseLock(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	prodDaemonDir, _ := prodAppDirs(t, home)
	prodLock, err := daemonlock.Acquire(filepath.Join(prodDaemonDir, "freeside.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holdProdDatabaseLock(home); err == nil || !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("running prod error = %v", err)
	}
	if err := prodLock.Close(); err != nil {
		t.Fatal(err)
	}
	runLock, err := holdProdDatabaseLock(home)
	if err != nil {
		t.Fatalf("stopped prod: %v", err)
	}
	defer func() { _ = runLock.Close() }()
	if _, err := daemonlock.Acquire(filepath.Join(prodDaemonDir, "freeside.db")); !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("prod starting during the run error = %v, want ErrAlreadyRunning", err)
	}
}

func TestHoldProdLockForProdAppDirectories(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	state, credentials := prodAppDirs(t, home)
	homes := []string{t.TempDir(), home}

	lock, err := holdProdLockForProdAppDirectoriesIn(homes, t.TempDir(), t.TempDir())
	if err != nil || lock != nil {
		t.Fatalf("other directories: lock = %v, err = %v; want no lock", lock, err)
	}
	for name, dirs := range map[string][2]string{
		"state":       {state, t.TempDir()},
		"credentials": {t.TempDir(), credentials},
	} {
		lock, err := holdProdLockForProdAppDirectoriesIn(homes, dirs[0], dirs[1])
		if err != nil || lock == nil {
			t.Fatalf("prod's %s directory: lock = %v, err = %v; want the lock", name, lock, err)
		}
		if _, err := daemonlock.Acquire(filepath.Join(state, "freeside.db")); !errors.Is(err, daemonlock.ErrAlreadyRunning) {
			t.Fatalf("prod starting during preflight error = %v, want ErrAlreadyRunning", err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	}
	prodLock, err := daemonlock.Acquire(filepath.Join(state, "freeside.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prodLock.Close() }()
	if _, err := holdProdLockForProdAppDirectoriesIn(homes, state, credentials); !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("running prod error = %v, want ErrAlreadyRunning", err)
	}
}

func TestRequireOtherDaemon(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := requireOtherDaemon(self); err == nil || !strings.Contains(err.Error(), "is this daemon") {
		t.Fatalf("this daemon error = %v", err)
	}
	if err := requireOtherDaemon(fakeProdDaemon(t, "exit 0")); err != nil {
		t.Fatalf("another daemon: %v", err)
	}
	if err := requireOtherDaemon(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing daemon accepted")
	}
}

func TestCheckProdRigLease(t *testing.T) {
	t.Parallel()
	lease, leaseRoot, _ := acquireTestRig(t)
	if err := checkProdRigLease(leaseRoot); err == nil || !strings.Contains(err.Error(), "production rig lease") {
		t.Fatalf("live lease error = %v", err)
	}
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	if err := checkProdRigLease(leaseRoot); err == nil || !strings.Contains(err.Error(), "rig recover") {
		t.Fatalf("stale lease error = %v", err)
	}
	if err := checkProdRigLease(t.TempDir()); err != nil {
		t.Fatalf("no lease: %v", err)
	}
	if err := checkProdRigLease(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("missing lease root: %v", err)
	}
}

func TestCheckProdAppAuthorityRefusesBeforeTheLease(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	for _, tc := range []struct {
		name       string
		env        environment
		prodDaemon string
		want       string
	}{
		{"prod tier", environmentProd, "/bin/true", "requires -environment ephemeral"},
		{"dev tier", environmentDev, "/bin/true", "requires -environment ephemeral"},
		{"no prod daemon", environmentEphemeral, "", "-prod-daemon"},
		{"no rig token", environmentEphemeral, "/usr/bin/true", "-rig-token-file"},
	} {
		err := checkProdAppAuthority(context.Background(), tc.env, home, home, "", t.TempDir(), tc.prodDaemon)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// fakeProdDaemon writes an executable that answers publication-formats with
// script.
func fakeProdDaemon(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "freesided")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // G306: executable fixture in an owner-only test directory.
		t.Fatal(err)
	}
	return path
}

func TestPublicationFormatsOutput(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	if err := runPublicationFormats(nil, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "{\"installation_authority\":{\"writes\":1,\"accepts\":[1]},\"installation_janitor_journal\":{\"writes\":1,\"accepts\":[1]}}\n"; got != want {
		t.Fatalf("publication-formats output = %q, want %q", got, want)
	}
	if err := runPublicationFormats([]string{"extra"}, &out); err == nil {
		t.Fatal("publication-formats accepted an argument")
	}
}

func TestCheckProdPublicationFormats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		script string
		want   string
	}{
		{"accepts", `echo '{"installation_authority":{"writes":1,"accepts":[1]},"installation_janitor_journal":{"writes":1,"accepts":[1]}}'`, ""},
		{"accepts a wider list", `echo '{"installation_authority":{"writes":1,"accepts":[1,2]},"installation_janitor_journal":{"writes":1,"accepts":[2,1]}}'`, ""},
		{"predates the subcommand", `echo 'flag provided but not defined' >&2; exit 2`, "cannot report its publication formats"},
		{"malformed", `echo 'not json'`, "malformed"},
		{"the first shape", `echo '{"installation_authority":[1],"installation_janitor_journal":[1]}'`, "malformed"},
		{"unknown field", `echo '{"installation_authority":{"writes":1,"accepts":[1]},"installation_janitor_journal":{"writes":1,"accepts":[1]},"other":1}'`, "malformed"},
		{"omits authority", `echo '{"installation_janitor_journal":{"writes":1,"accepts":[1]}}'`, "installation authority version 1"},
		{"omits journal accepts", `echo '{"installation_authority":{"writes":1,"accepts":[1]},"installation_janitor_journal":{"writes":1}}'`, "installation janitor journal version 1"},
		{"does not accept the journal version", `echo '{"installation_authority":{"writes":1,"accepts":[1]},"installation_janitor_journal":{"writes":1,"accepts":[2]}}'`, "does not accept installation janitor journal version 1"},
		{"writes a version this build can't read", `echo '{"installation_authority":{"writes":2,"accepts":[1,2]},"installation_janitor_journal":{"writes":1,"accepts":[1]}}'`, "writes installation authority version 2"},
		{"omits journal writes", `echo '{"installation_authority":{"writes":1,"accepts":[1]},"installation_janitor_journal":{"accepts":[1]}}'`, "writes installation janitor journal version 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkProdPublicationFormats(context.Background(), fakeProdDaemon(t, tc.script))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkProdPublicationFormats() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkProdPublicationFormats() error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := checkProdPublicationFormats(context.Background(), "freesided"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path error = %v", err)
	}
	if err := checkProdPublicationFormats(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing installed daemon accepted")
	}
}
