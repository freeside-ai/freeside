package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseEnvironment(t *testing.T) {
	t.Parallel()
	for _, want := range AllEnvironments {
		t.Run(string(want), func(t *testing.T) {
			t.Parallel()
			got, err := parseEnvironment(string(want))
			if err != nil || got != want {
				t.Fatalf("parseEnvironment(%q) = %q, %v", want, got, err)
			}
		})
	}
	for _, raw := range []string{"", "staging"} {
		t.Run("invalid-"+raw, func(t *testing.T) {
			t.Parallel()
			if _, err := parseEnvironment(raw); err == nil || !strings.Contains(err.Error(), "-environment") {
				t.Fatalf("parseEnvironment(%q) error = %v", raw, err)
			}
		})
	}
}

// TestEnvironmentDatabaseLocking: the supervised tiers hold their live
// database exclusively and ephemeral does not, and the daemon's store
// options follow its tier.
func TestEnvironmentDatabaseLocking(t *testing.T) {
	t.Parallel()
	want := map[environment]bool{
		environmentProd:      true,
		environmentDev:       true,
		environmentEphemeral: false,
	}
	for _, env := range AllEnvironments {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			got := env.locksDatabaseExclusively()
			if got != want[env] {
				t.Fatalf("locksDatabaseExclusively() = %v, want %v", got, want[env])
			}
			opts, err := config{Environment: env}.storeOptions()
			if err != nil || opts.ExclusiveLocking != want[env] {
				t.Fatalf("storeOptions().ExclusiveLocking = %v, %v; want %v", opts.ExclusiveLocking, err, want[env])
			}
		})
	}
}

func TestEnvironmentPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flag  string
		paths func(*testing.T, string) environmentPaths
	}{
		{"database", "-db", func(_ *testing.T, root string) environmentPaths {
			return environmentPaths{DB: filepath.Join(root, "daemon", "freeside.db")}
		}},
		{"state root", "-state-dir", func(_ *testing.T, root string) environmentPaths {
			return environmentPaths{StateDir: root}
		}},
		{"case alias", "-state-dir", func(t *testing.T, root string) environmentPaths {
			t.Helper()
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			return environmentPaths{StateDir: filepath.Join(filepath.Dir(root), strings.ToLower(filepath.Base(root)))}
		}},
		{"publication state", "-publication-state-dir", func(_ *testing.T, root string) environmentPaths {
			return environmentPaths{PublicationStateDir: filepath.Join(root, "daemon")}
		}},
		{"publication credentials", "-publication-credentials-dir", func(_ *testing.T, root string) environmentPaths {
			return environmentPaths{PublicationCredentialsDir: filepath.Join(root, "credentials")}
		}},
		{"review input", "-review-input-root", func(_ *testing.T, root string) environmentPaths {
			return environmentPaths{ReviewInputRoot: filepath.Join(root, "review")}
		}},
		{"dangling WAL symlink", "-db", func(t *testing.T, root string) environmentPaths {
			t.Helper()
			db := filepath.Join(t.TempDir(), "test.db")
			if err := os.Symlink(filepath.Join(root, "daemon", "freeside.db-wal"), db+"-wal"); err != nil {
				t.Fatal(err)
			}
			return environmentPaths{DB: db}
		}},
		{"SHM symlink", "-db", func(t *testing.T, root string) environmentPaths {
			t.Helper()
			db := filepath.Join(t.TempDir(), "test.db")
			if err := os.Symlink(filepath.Join(root, "daemon", "freeside.db-shm"), db+"-shm"); err != nil {
				t.Fatal(err)
			}
			return environmentPaths{DB: db}
		}},
		{"hard-linked WAL", "-db", func(t *testing.T, root string) environmentPaths {
			t.Helper()
			sidecar := filepath.Join(root, "daemon", "freeside.db-wal")
			if err := os.MkdirAll(filepath.Dir(sidecar), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sidecar, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(t.TempDir(), "test.db")
			if err := os.Link(sidecar, db+"-wal"); err != nil {
				t.Fatal(err)
			}
			return environmentPaths{DB: db}
		}},
		{"hard-linked SHM", "-db", func(t *testing.T, root string) environmentPaths {
			t.Helper()
			sidecar := filepath.Join(root, "daemon", "freeside.db-shm")
			if err := os.MkdirAll(filepath.Dir(sidecar), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sidecar, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(t.TempDir(), "test.db")
			if err := os.Link(sidecar, db+"-shm"); err != nil {
				t.Fatal(err)
			}
			return environmentPaths{DB: db}
		}},
		{"symlinked parent", "-db", func(t *testing.T, root string) environmentPaths {
			t.Helper()
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(t.TempDir(), "linked-root")
			if err := os.Symlink(root, link); err != nil {
				t.Fatal(err)
			}
			return environmentPaths{DB: filepath.Join(link, "daemon", "freeside.db")}
		}},
		{"relative symlink parent", "-publication-credentials-dir", func(t *testing.T, root string) environmentPaths {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(root, "daemon"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "credentials"), 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Symlink(filepath.Join(root, "daemon"), filepath.Join(outside, "pivot")); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(outside, "credential-link")
			if err := os.Symlink("pivot/../credentials", link); err != nil {
				t.Fatal(err)
			}
			return environmentPaths{PublicationCredentialsDir: link}
		}},
		{"missing parent reconstructed symlink", "-publication-credentials-dir", func(t *testing.T, root string) environmentPaths {
			t.Helper()
			credentials := filepath.Join(root, "credentials")
			if err := os.MkdirAll(credentials, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Symlink(credentials, filepath.Join(outside, "credential-link")); err != nil {
				t.Fatal(err)
			}
			return environmentPaths{PublicationCredentialsDir: filepath.Join(outside, "missing", "..", "credential-link")}
		}},
	}
	for _, tier := range []environment{environmentProd, environmentDev} {
		for _, tc := range cases {
			t.Run(string(tier)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				root := supervisedRoots(home)[tier]
				paths := tc.paths(t, root)
				if err := checkEnvironment(environmentEphemeral, home, paths, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), tc.flag) || !strings.Contains(err.Error(), string(tier)) {
					t.Fatalf("ephemeral guard error = %v, want %s and %s", err, tc.flag, tier)
				}
				outside := t.TempDir()
				if err := checkEnvironment(environmentEphemeral, home, tc.paths(t, outside), "127.0.0.1:0"); err != nil {
					t.Fatalf("outside supervised roots: %v", err)
				}
			})
		}
	}
}

func TestEnvironmentPorts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		listen string
		denied bool
	}{
		{"127.0.0.1:7331", true},
		{"127.0.0.1:7332", true},
		{"100.64.0.1:7331", true},
		{"127.0.0.1:0", false},
		{"127.0.0.1:7400", false},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			t.Parallel()
			err := checkEnvironment(environmentEphemeral, t.TempDir(), environmentPaths{}, tc.listen)
			if tc.denied && (err == nil || !strings.Contains(err.Error(), "-listen") || !strings.Contains(err.Error(), "ephemeral")) {
				t.Fatalf("checkEnvironment(%q) error = %v, want refusal", tc.listen, err)
			}
			if !tc.denied && err != nil {
				t.Fatalf("checkEnvironment(%q) = %v", tc.listen, err)
			}
		})
	}
}

func TestUnderRootAcceptsAPFSFirmlinkAlias(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("APFS firmlinks are macOS-specific")
	}
	root := "/Users"
	alias := "/System/Volumes/Data/Users"
	rootInfo, rootErr := os.Stat(root)
	aliasInfo, aliasErr := os.Stat(alias)
	if rootErr != nil || aliasErr != nil || !os.SameFile(rootInfo, aliasInfo) {
		t.Skip("APFS /Users firmlink is unavailable")
	}
	name := "FreesideCaseAlias" + filepath.Base(t.TempDir())
	missingRoot := filepath.Join(root, name)
	if _, err := os.Lstat(missingRoot); !os.IsNotExist(err) {
		t.Skipf("test root %q unexpectedly exists", missingRoot)
	}
	aliasPath := filepath.Join(alias, name, "daemon")
	if !underRoot(aliasPath, missingRoot) {
		t.Fatalf("underRoot(%q, %q) = false, want true", aliasPath, missingRoot)
	}
	caseAliasPath := filepath.Join(alias, strings.ToLower(name), "daemon")
	if !mayBeUnderRoot(caseAliasPath, missingRoot) {
		t.Fatalf("mayBeUnderRoot(%q, %q) = false, want true", caseAliasPath, missingRoot)
	}
}

func TestEnvironmentSupervised(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	roots := supervisedRoots(home)
	for _, tier := range []environment{environmentProd, environmentDev} {
		t.Run(string(tier), func(t *testing.T) {
			t.Parallel()
			root := roots[tier]
			paths := environmentPaths{
				DB:                        filepath.Join(root, "daemon", "freeside.db"),
				StateDir:                  filepath.Join(root, "daemon"),
				PublicationStateDir:       filepath.Join(root, "daemon"),
				PublicationCredentialsDir: t.TempDir(),
				ReviewInputRoot:           t.TempDir(),
			}
			if err := checkEnvironment(tier, home, paths, "127.0.0.1:7331"); err != nil {
				t.Fatalf("own root: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(root, "daemon"), 0o700); err != nil {
				t.Fatal(err)
			}
			aliasRoot := filepath.Join(filepath.Dir(root), strings.ToLower(filepath.Base(root)))
			aliasPaths := paths
			aliasPaths.DB = filepath.Join(aliasRoot, "daemon", "freeside.db")
			aliasPaths.StateDir = filepath.Join(aliasRoot, "daemon")
			aliasPaths.PublicationStateDir = ""
			rootInfo, rootErr := os.Stat(root)
			aliasInfo, aliasErr := os.Stat(aliasRoot)
			aliasIsRoot := rootErr == nil && aliasErr == nil && os.SameFile(rootInfo, aliasInfo)
			if err := checkEnvironment(tier, home, aliasPaths, "127.0.0.1:0"); (err == nil) != aliasIsRoot {
				t.Fatalf("case alias accepted = %t, same directory = %t: %v", err == nil, aliasIsRoot, err)
			}
			other := environmentProd
			if tier == environmentProd {
				other = environmentDev
			}
			paths.DB = filepath.Join(roots[other], "daemon", "freeside.db")
			if err := checkEnvironment(tier, home, paths, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "-db") {
				t.Fatalf("other tier database error = %v", err)
			}
			paths.DB = filepath.Join(t.TempDir(), "test.db")
			if err := checkEnvironment(tier, home, paths, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "-db") {
				t.Fatalf("outside root database error = %v", err)
			}
			paths.DB = filepath.Join(root, "daemon", "freeside.db")
			paths.StateDir = ""
			if err := checkEnvironment(tier, home, paths, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "-state-dir") {
				t.Fatalf("missing state directory error = %v", err)
			}
			paths.StateDir = filepath.Join(root, "daemon")
			paths.PublicationStateDir = t.TempDir()
			if err := checkEnvironment(tier, home, paths, "127.0.0.1:0"); err == nil || !strings.Contains(err.Error(), "-publication-state-dir") {
				t.Fatalf("outside publication state error = %v", err)
			}
		})
	}
}

func TestEnvironmentAccountHomeIgnoresHOME(t *testing.T) {
	accountHome, err := currentAccountHome()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestEnvironmentAccountHomeIgnoresHOMEProcess") //nolint:gosec // G204: execute this package's test binary
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOME=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env,
		"FREESIDE_TEST_ACCOUNT_HOME="+accountHome,
		"HOME="+t.TempDir(),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("account-home subprocess: %v\n%s", err, output)
	}
}

func TestEnvironmentAccountHomeIgnoresHOMEProcess(t *testing.T) {
	accountHome := os.Getenv("FREESIDE_TEST_ACCOUNT_HOME")
	if accountHome == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if home == accountHome {
		t.Fatalf("os.UserHomeDir() = account home %q despite overridden HOME", home)
	}
	resolvedAccountHome, err := currentAccountHome()
	if err != nil {
		t.Fatal(err)
	}
	if resolvedAccountHome != accountHome {
		t.Fatalf("currentAccountHome() = %q, want %q", resolvedAccountHome, accountHome)
	}
	paths := environmentPaths{DB: filepath.Join(accountHome, "Library", "Application Support", "Freeside", "daemon", "freeside.db")}
	if err := checkEnvironment(environmentEphemeral, home, paths, "127.0.0.1:0"); err != nil {
		t.Fatalf("caller-controlled home guard = %v, want no error", err)
	}
	if err := checkEnvironment(environmentEphemeral, resolvedAccountHome, paths, "127.0.0.1:0"); err == nil {
		t.Fatal("account home guard accepted a supervised database path")
	}
}
