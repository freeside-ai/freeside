package projectimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: the Httpoxy advisory covers Go before 1.6.3; this serves a test repository on loopback
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
)

// bakeRun is one run of swiftMirrorBake on the host: the BusyBox applets
// it calls are the host's tools, and git clone only records its arguments
// unless bakeOptions.server sends it to real git.
type bakeRun struct {
	err     error
	output  string
	mirrors string
	clones  []string
}

// bakeOptions adjusts one bake run. The zero value runs the bake with no
// pause between attempts and every recorded clone succeeding.
type bakeOptions struct {
	// failures makes the first failures clone calls fail without creating
	// their target.
	failures int
	// server, when set, runs each clone with real git, rewriting
	// https://example.test/ to this base URL.
	server string
	// lowSpeedTime replaces the bake's low_speed_time when nonzero.
	lowSpeedTime int
}

// runSwiftMirrorBake runs the bake over the list at listPath, with
// SwiftMirrorsPath moved under a fresh directory. The list comes from
// manifest code, so these runs exercise the validation the image relies on.
func runSwiftMirrorBake(t *testing.T, listPath string, options bakeOptions) bakeRun {
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
	// The bake passes -c options before clone, so any argument that is
	// clone marks one; the bake's other git calls name no such argument.
	execfixture.Write(t, filepath.Join(bin, "git"), []byte(`#!/bin/sh
for arg; do
	[ "$arg" = clone ] || continue
	printf '%s\n' "$*" >>"$BAKE_CLONES"
	if [ -n "$BAKE_SERVER" ]; then
		exec "$REAL_GIT" -c "url.$BAKE_SERVER/.insteadOf=https://example.test/" "$@"
	fi
	if [ $(($(wc -l <"$BAKE_CLONES"))) -le "$BAKE_FAILURES" ]; then
		echo "fatal: injected clone failure" >&2
		exit 1
	fi
	for target; do :; done
	mkdir -p "$target"
	exit 0
done
exec "$REAL_GIT" "$@"
`), 0o755)

	script := strings.ReplaceAll(swiftMirrorBake, "/usr/bin/busybox ", busybox+" ")
	script = replaceBakeLine(t, script, "mirrors="+SwiftMirrorsPath, "mirrors="+mirrors)
	script = replaceBakeLine(t, script, "retry_pause=5", "retry_pause=0")
	if options.lowSpeedTime != 0 {
		script = replaceBakeLine(t, script, "low_speed_time=60", "low_speed_time="+strconv.Itoa(options.lowSpeedTime))
	}
	scriptPath := filepath.Join(dir, "bake")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	// A bound turns a bake that hangs into a failure instead of a stuck run.
	// The bake runs in its own process group so the bound also kills the
	// clones it started, which would otherwise hold its output open.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", scriptPath, listPath) //nolint:gosec // G204: the bake under test, over a test-owned list
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(gitenv.Scrubbed(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_GIT="+realGit, "BAKE_CLONES="+clones, "LC_ALL=C",
		"BAKE_FAILURES="+strconv.Itoa(options.failures), "BAKE_SERVER="+options.server,
		"HOME="+dir, "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bake did not finish within its bound:\n%s", output)
	}
	run := bakeRun{err: err, output: string(output), mirrors: mirrors}
	if log, readErr := os.ReadFile(clones); readErr == nil { //nolint:gosec // G304: fixed file under the test-owned directory
		run.clones = strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
	}
	return run
}

// replaceBakeLine replaces one whole line of the bake script, failing when
// the line is missing so a renamed setting can't leave a test on the
// production value.
func replaceBakeLine(t *testing.T, script, line, replacement string) string {
	t.Helper()
	if !strings.Contains(script, "\n"+line+"\n") {
		t.Fatalf("the bake script lacks the line %q", line)
	}
	return strings.Replace(script, "\n"+line+"\n", "\n"+replacement+"\n", 1)
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

// bakeClonePrefix is the bake's clone arguments before the URL, pinning the
// production stall limit.
const bakeClonePrefix = "-c http.lowSpeedLimit=1000 -c http.lowSpeedTime=60 clone --quiet --mirror -- "

func TestSwiftMirrorBakeMirrorsEachValidatedURLOnce(t *testing.T) {
	t.Parallel()
	run := runSwiftMirrorBake(t, writeMirrorList(t,
		"https://example.test/a\nhttps://example.test/a.git\nhttps://example.test/b.git\n"), bakeOptions{})
	if run.err != nil {
		t.Fatalf("bake failed: %v\n%s", run.err, run.output)
	}
	a := filepath.Join(run.mirrors, mirrorName("https://example.test/a"))
	b := filepath.Join(run.mirrors, mirrorName("https://example.test/b"))
	want := []string{
		bakeClonePrefix + "https://example.test/a " + a,
		bakeClonePrefix + "https://example.test/b.git " + b,
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
			run := runSwiftMirrorBake(t, tc.list(t), bakeOptions{})
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

func TestSwiftMirrorBakeRetriesAFailedCloneOnce(t *testing.T) {
	t.Parallel()
	list := "https://example.test/a\nhttps://example.test/b\n"
	a := mirrorName("https://example.test/a")
	b := mirrorName("https://example.test/b")

	t.Run("one failure", func(t *testing.T) {
		t.Parallel()
		run := runSwiftMirrorBake(t, writeMirrorList(t, list), bakeOptions{failures: 1})
		if run.err != nil {
			t.Fatalf("bake failed after one clone failure: %v\n%s", run.err, run.output)
		}
		want := []string{
			bakeClonePrefix + "https://example.test/a " + filepath.Join(run.mirrors, a),
			bakeClonePrefix + "https://example.test/a " + filepath.Join(run.mirrors, a),
			bakeClonePrefix + "https://example.test/b " + filepath.Join(run.mirrors, b),
		}
		if strings.Join(run.clones, "\n") != strings.Join(want, "\n") {
			t.Fatalf("clones = %q, want %q", run.clones, want)
		}
		config, err := exec.CommandContext(t.Context(), "git", "config", "--file", //nolint:gosec // G204: fixed test git invocation
			filepath.Join(run.mirrors, "gitconfig"), "--get-all",
			"url.file://"+filepath.Join(run.mirrors, a)+".insteadOf").Output()
		if err != nil {
			t.Fatalf("gitconfig lacks the retried mirror's rewrites: %v", err)
		}
		if string(config) != "https://example.test/a\nhttps://example.test/a.git\n" {
			t.Errorf("retried mirror's rewrites = %q", config)
		}
	})

	t.Run("two failures", func(t *testing.T) {
		t.Parallel()
		run := runSwiftMirrorBake(t, writeMirrorList(t, list), bakeOptions{failures: 2})
		if run.err == nil {
			t.Fatalf("bake succeeded after two clone failures:\n%s", run.output)
		}
		if !strings.Contains(run.output, "Swift dependency mirror https://example.test/a failed after 2 attempts") {
			t.Errorf("bake output = %q, want the failed URL named", run.output)
		}
		for _, clone := range run.clones {
			if strings.Contains(clone, "https://example.test/b") {
				t.Errorf("bake cloned past the failed URL: %q", clone)
			}
		}
	})
}

// TestSwiftMirrorBakeFailsAStalledClone runs the bake's clones with real git
// against a git-http-backend that stops sending partway through the pack.
// git's low-speed limit has to end each attempt; the context bound in
// runSwiftMirrorBake fails the test if it does not.
func TestSwiftMirrorBakeFailsAStalledClone(t *testing.T) {
	t.Parallel()
	execPath, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Fatalf("the stall test needs git-http-backend: %v", err)
	}

	root := t.TempDir()
	repo := newBaseInputsRepo(t)
	repo.commit(map[string]string{"README": "mirrored\n"})
	repo.git("clone", "-q", "--bare", repo.dir, filepath.Join(root, "a"))

	var stalled atomic.Int32
	release := make(chan struct{})
	git := &cgi.Handler{
		Path:   backend,
		Env:    []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
		Logger: log.New(io.Discard, "", 0),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte("command=fetch")) {
				stalled.Add(1)
				w = &stallingWriter{ResponseWriter: w, done: r.Context().Done(), release: release}
			}
		}
		git.ServeHTTP(w, r)
	}))
	// Cleanups run last first: release any handler still stalled, so Close
	// doesn't wait on it when the bake failed to end its connections.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	started := time.Now()
	run := runSwiftMirrorBake(t, writeMirrorList(t, "https://example.test/a\n"),
		bakeOptions{server: server.URL, lowSpeedTime: 1})
	if run.err == nil {
		t.Fatalf("bake succeeded over a stalled transfer:\n%s", run.output)
	}
	if !strings.Contains(run.output, "Swift dependency mirror https://example.test/a failed after 2 attempts") {
		t.Errorf("bake output = %q, want the stalled URL named", run.output)
	}
	if got := stalled.Load(); got != 2 {
		t.Errorf("stalled pack requests = %d, want 2 (one retry)\n%s", got, run.output)
	}
	t.Logf("stalled bake failed after %s", time.Since(started))
}

// stallingWriter sends half of the first body chunk it is given, flushes it,
// and then holds the response open until the client goes away or the test
// releases it, as a stalled transfer does.
type stallingWriter struct {
	http.ResponseWriter
	done    <-chan struct{}
	release <-chan struct{}
}

func (w *stallingWriter) Write(p []byte) (int, error) {
	if _, err := w.ResponseWriter.Write(p[:(len(p)+1)/2]); err != nil {
		return 0, err
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return 0, err
	}
	select {
	case <-w.done:
	case <-w.release:
	}
	return 0, errors.New("stalled transfer abandoned")
}
