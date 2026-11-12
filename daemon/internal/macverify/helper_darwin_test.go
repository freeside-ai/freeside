//go:build darwin

package macverify

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestHelperRefusesDirectoryRootDoesNotOwn runs the real helper from a
// directory the test user owns, as a hard link outside the executor root
// would be, and checks it refuses before reading any slot list.
func TestHelperRefusesDirectoryRootDoesNotOwn(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bin", "slot-run")
	installHelper(t, path, helperScript)
	if err := os.WriteFile(filepath.Join(dir, "bin", "slots.conf"), []byte("root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", path, "reap", "root") //nolint:gosec // G204: fixed shell running a test-owned copy of the helper
	cmd.Env = []string{"SUDO_UID=" + strconv.Itoa(os.Getuid())}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != helperRefused || !strings.Contains(string(out), "is not owned by root") {
		t.Fatalf("helper = %v\n%s\nwant a refusal with status %d", err, out, helperRefused)
	}
}

func TestUIDOwnsProcess(t *testing.T) {
	t.Parallel()
	if live, err := uidOwnsProcess(os.Getuid()); err != nil || !live {
		t.Fatalf("uidOwnsProcess(own uid) = %v, %v; want true", live, err)
	}
	// No account on a test host has this uid.
	if live, err := uidOwnsProcess(424242); err != nil || live {
		t.Fatalf("uidOwnsProcess(unused uid) = %v, %v; want false", live, err)
	}
}

// TestHelperEmptiesUserDirKeepingOnlyAllowedProtectedEntries runs slot-run's
// empty_user_dir on a directory built to look like a slot's temporary
// directory after a job (#3009). Read-only parents stand in for the
// protection that keeps root from removing an entry, so each case controls
// what survives the rm; the gate is what the function then accepts.
func TestHelperEmptiesUserDirKeepingOnlyAllowedProtectedEntries(t *testing.T) {
	t.Parallel()
	script := string(helperScript)
	start, end := strings.Index(script, "\nlisting() {"), strings.Index(script, "\nreset_home() {")
	if start < 0 || end < start {
		t.Fatal("slot-run.sh has no listing through empty_user_dir block")
	}
	functions := script[start:end]
	// The functions run inside prepare and cleanup, and sh has no local
	// variables, so every variable the rest of the script assigns gets a
	// sentinel the call must leave alone. prepare once read its Swift cache
	// entry from a slot's directory after a collision on entry.
	assigned := regexp.MustCompile(`(?m)^\s*(?:for\s+)?([a-z_][a-z0-9_]*)(?:=|\s+in\s)`)
	var globals []string
	for _, m := range assigned.FindAllStringSubmatch(script[:start]+script[end:], -1) {
		if m[1] != "slot" && !slices.Contains(globals, m[1]) {
			globals = append(globals, m[1])
		}
	}
	if !slices.Contains(globals, "entry") || !slices.Contains(globals, "tmp") {
		t.Fatalf("found globals %q, want entry and tmp among them", globals)
	}
	var keep, check strings.Builder
	for _, g := range globals {
		fmt.Fprintf(&keep, "%s=keep-%s\n", g, g)
		fmt.Fprintf(&check, "[ \"$%s\" = keep-%s ] || { echo 'empty_user_dir changed %s' >&2; exit 99; }\n", g, g, g)
	}

	type layout struct {
		dirs, files, symlinks []string
		// readOnly are made 0500 after the layout is built, so rm cannot
		// remove what they hold.
		readOnly []string
		// sealed are made 0000, so, like the host's TemporaryItems, they can
		// be neither listed nor emptied.
		sealed []string
	}
	protected := layout{
		dirs:     []string{"com.apple.trustd", "com.apple.trustd/TemporaryItems", "com.apple.MTLCompilerService", "TemporaryItems"},
		readOnly: []string{".", "com.apple.trustd"},
		sealed:   []string{"TemporaryItems", "com.apple.trustd/TemporaryItems"},
	}
	with := func(base layout, change func(*layout)) layout {
		l := layout{
			dirs: slices.Clone(base.dirs), files: slices.Clone(base.files),
			symlinks: slices.Clone(base.symlinks), readOnly: slices.Clone(base.readOnly),
			sealed: slices.Clone(base.sealed),
		}
		change(&l)
		return l
	}
	for name, tc := range map[string]struct {
		layout layout
		// cache runs the reset as for the slot's cache directory, not its
		// temporary one.
		cache bool
		// left is what a passing case leaves, sorted.
		left    []string
		refusal string
	}{
		"removable entries": {layout: layout{
			dirs:  []string{"com.apple.trustd", "clang/ModuleCache"},
			files: []string{"clang/ModuleCache/a.pcm", "job-file"},
		}},
		"empty protected entries": {
			layout: protected, left: []string{"TemporaryItems", "com.apple.MTLCompilerService", "com.apple.trustd"},
		},
		"no protected entries": {layout: layout{}},
		// A SwiftPM atomic write can leave an entry here that no one can
		// remove; the owner accepted it (#3009), so the slot stays usable.
		"an entry left in the top-level TemporaryItems": {
			layout: with(protected, func(l *layout) {
				l.dirs = append(l.dirs, "TemporaryItems/NSIRD_swift-build_left")
			}),
			left: []string{"TemporaryItems", "com.apple.MTLCompilerService", "com.apple.trustd"},
		},
		"an entry left in the cache directory's TemporaryItems": {
			layout: with(protected, func(l *layout) {
				l.dirs = append(l.dirs, "TemporaryItems/NSIRD_swift-build_left")
			}),
			cache:   true,
			refusal: "holds entries no one can list or remove",
		},
		"a directory in trustd's TemporaryItems": {
			layout: with(protected, func(l *layout) {
				l.dirs = append(l.dirs, "com.apple.trustd/TemporaryItems/carried")
			}),
			refusal: "holds entries no one can list or remove",
		},
		"a file in MTLCompilerService": {
			layout: with(protected, func(l *layout) {
				l.files = append(l.files, "com.apple.MTLCompilerService/carried")
				l.readOnly = append(l.readOnly, "com.apple.MTLCompilerService")
			}),
			refusal: "cannot empty",
		},
		"a second entry in trustd": {
			layout:  with(protected, func(l *layout) { l.files = append(l.files, "com.apple.trustd/carried") }),
			refusal: "cannot empty",
		},
		"an unlisted survivor": {
			layout:  with(protected, func(l *layout) { l.dirs = append(l.dirs, "carried") }),
			refusal: "cannot empty",
		},
		"a symlink named TemporaryItems": {
			layout:  layout{symlinks: []string{"TemporaryItems"}, readOnly: []string{"."}},
			refusal: "is not a directory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range tc.layout.dirs {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range tc.layout.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("state"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, l := range tc.layout.symlinks {
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, l)); err != nil {
					t.Fatal(err)
				}
			}
			for _, r := range tc.layout.readOnly {
				if err := os.Chmod(filepath.Join(dir, r), 0o500); err != nil { //nolint:gosec // G302: a directory rm cannot write
					t.Fatal(err)
				}
			}
			for _, s := range tc.layout.sealed {
				if err := os.Chmod(filepath.Join(dir, s), 0); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, _ error) error {
					if d != nil && d.IsDir() {
						_ = os.Chmod(path, 0o700) //nolint:gosec // G302: restored so TempDir can remove it
					}
					return nil
				})
			})

			// sudo is stubbed: the slot's chflags has nothing to clear here.
			body := "set -eu\nIFS='\n'\nslot=fvslot1\nsudo() { :; }\n" +
				"refuse() { printf 'slot-run: %s\\n' \"$*\" >&2; exit 120; }\n" + functions + keep.String() +
				"\nempty_user_dir \"$1\" \"$2\"\n" + check.String()
			kind := "temp"
			if tc.cache {
				kind = "cache"
			}
			cmd := exec.Command("/bin/sh", "-c", body, "slot-run", dir, kind) //nolint:gosec // G204: fixed shell running the embedded helper's functions
			cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
			out, err := cmd.CombinedOutput()
			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("empty_user_dir = %v\n%s", err, out)
				}
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				var left []string
				for _, e := range entries {
					left = append(left, e.Name())
				}
				if !slices.Equal(left, tc.left) {
					t.Fatalf("left %q, want %q", left, tc.left)
				}
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != helperRefused || !strings.Contains(string(out), tc.refusal) {
				t.Fatalf("empty_user_dir = %v\n%s\nwant a refusal naming %q", err, out, tc.refusal)
			}
		})
	}
}
