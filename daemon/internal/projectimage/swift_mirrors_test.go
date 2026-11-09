package projectimage

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
)

// bakeRun is one run of swiftMirrorBake on the host: the BusyBox applets
// it calls are the host's tools, and git clone only records its arguments.
type bakeRun struct {
	err     error
	output  string
	mirrors string
	clones  []string
}

// runSwiftMirrorBake runs the bake over the list at listPath, with
// SwiftMirrorsPath moved under a fresh directory. The list comes from
// manifest code, so these runs exercise the validation the image relies on.
func runSwiftMirrorBake(t *testing.T, listPath string) bakeRun {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mirrors := filepath.Join(dir, "swiftpm-mirrors")
	clones := filepath.Join(dir, "clones.log")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	busybox := filepath.Join(bin, "busybox")
	execfixture.Write(t, busybox, []byte(`#!/bin/sh
applet=$1
shift
if [ "$applet" = sha256sum ] && ! command -v sha256sum >/dev/null; then
	exec shasum -a 256 "$@"
fi
exec "$applet" "$@"
`), 0o755)
	execfixture.Write(t, filepath.Join(bin, "git"), []byte(`#!/bin/sh
if [ "$1" = clone ]; then
	printf '%s\n' "$*" >>"$BAKE_CLONES"
	for target; do :; done
	mkdir -p "$target"
	exit 0
fi
exec "$REAL_GIT" "$@"
`), 0o755)

	script := strings.ReplaceAll(swiftMirrorBake, "/usr/bin/busybox ", busybox+" ")
	script = strings.Replace(script, "mirrors="+SwiftMirrorsPath+"\n", "mirrors="+mirrors+"\n", 1)
	scriptPath := filepath.Join(dir, "bake")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", scriptPath, listPath) //nolint:gosec // G204: the bake under test, over a test-owned list
	cmd.Env = append(gitenv.Scrubbed(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_GIT="+realGit, "BAKE_CLONES="+clones, "LC_ALL=C",
		"HOME="+dir, "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	run := bakeRun{err: err, output: string(output), mirrors: mirrors}
	if log, readErr := os.ReadFile(clones); readErr == nil { //nolint:gosec // G304: fixed file under the test-owned directory
		run.clones = strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
	}
	return run
}

func writeMirrorList(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "swift-mirror-urls")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mirrorName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".git"
}

func TestSwiftMirrorBakeMirrorsEachValidatedURLOnce(t *testing.T) {
	t.Parallel()
	run := runSwiftMirrorBake(t, writeMirrorList(t,
		"https://example.test/a\nhttps://example.test/a.git\nhttps://example.test/b.git\n"))
	if run.err != nil {
		t.Fatalf("bake failed: %v\n%s", run.err, run.output)
	}
	a := filepath.Join(run.mirrors, mirrorName("https://example.test/a"))
	b := filepath.Join(run.mirrors, mirrorName("https://example.test/b"))
	want := []string{
		"clone --quiet --mirror -- https://example.test/a " + a,
		"clone --quiet --mirror -- https://example.test/b.git " + b,
	}
	if strings.Join(run.clones, "\n") != strings.Join(want, "\n") {
		t.Fatalf("clones = %q, want %q", run.clones, want)
	}
	record, err := os.ReadFile(filepath.Join(run.mirrors, "urls"))
	if err != nil {
		t.Fatal(err)
	}
	if string(record) != "https://example.test/a\nhttps://example.test/a.git\nhttps://example.test/b.git\n" {
		t.Errorf("record = %q, want every accepted line", record)
	}
	config, err := exec.CommandContext(t.Context(), "git", "config", "--file", //nolint:gosec // G204: fixed test git invocation
		filepath.Join(run.mirrors, "gitconfig"), "--list").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{
		"protocol.file.allow=always",
		"url.file://" + a + ".insteadof=https://example.test/a",
		"url.file://" + a + ".insteadof=https://example.test/a.git",
		"url.file://" + b + ".insteadof=https://example.test/b",
		"url.file://" + b + ".insteadof=https://example.test/b.git",
	} {
		if !strings.Contains(string(config), entry+"\n") {
			t.Errorf("gitconfig lacks %q:\n%s", entry, config)
		}
	}
}

func TestSwiftMirrorBakeRefusesAnUntrustedList(t *testing.T) {
	t.Parallel()
	symlinked := func(t *testing.T) string {
		target := writeMirrorList(t, "https://example.test/a\n")
		link := filepath.Join(t.TempDir(), "swift-mirror-urls")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		return link
	}
	for _, tc := range []struct {
		name string
		list func(t *testing.T) string
		want string
	}{
		{"not https", func(t *testing.T) string {
			return writeMirrorList(t, "https://example.test/a\nhttp://example.test/b\n")
		}, "is not an https URL"},
		{"a git transport", func(t *testing.T) string {
			return writeMirrorList(t, "ext::sh -c id\n")
		}, "is not an https URL"},
		{"a space", func(t *testing.T) string {
			return writeMirrorList(t, "https://example.test/a b\n")
		}, "space or control character"},
		{"a control character", func(t *testing.T) string {
			return writeMirrorList(t, "https://example.test/a\tb\n")
		}, "space or control character"},
		{"a directory", func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), "swift-mirror-urls")
			if err := os.Mkdir(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			return dir
		}, "not a regular file"},
		{"a symlink", symlinked, "not a regular file"},
		{"missing", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "swift-mirror-urls")
		}, "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runSwiftMirrorBake(t, tc.list(t))
			if run.err == nil {
				t.Fatalf("bake accepted the list:\n%s", run.output)
			}
			if !strings.Contains(run.output, tc.want) {
				t.Errorf("bake output = %q, want %q", run.output, tc.want)
			}
			for _, clone := range run.clones {
				if !strings.Contains(clone, "https://example.test/a ") {
					t.Errorf("bake cloned past the refused line: %q", clone)
				}
			}
		})
	}
}
