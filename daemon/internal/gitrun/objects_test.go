package gitrun

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

var errTestClass = errors.New("test git class")

func fixtureGit(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture running git over a test-owned repo
	cmd.Env = append(gitenv.Scrubbed(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

type objectRepo struct {
	dir                      string
	blob, link, tree, commit string
	large                    string
	largeSize                int64
}

const largeBlobSize = 8 << 20

func newObjectRepo(t *testing.T) objectRepo {
	t.Helper()
	dir := t.TempDir()
	fixtureGit(t, dir, nil, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, nil, "add", ".")
	fixtureGit(t, dir, nil, "commit", "-q", "-m", "init")
	return objectRepo{
		dir:       dir,
		blob:      fixtureGit(t, dir, nil, "rev-parse", "HEAD:file"),
		link:      fixtureGit(t, dir, nil, "rev-parse", "HEAD:link"),
		tree:      fixtureGit(t, dir, nil, "rev-parse", "HEAD^{tree}"),
		commit:    fixtureGit(t, dir, nil, "rev-parse", "HEAD"),
		large:     fixtureGit(t, dir, bytes.Repeat([]byte("x"), largeBlobSize), "hash-object", "-w", "--stdin"),
		largeSize: largeBlobSize,
	}
}

func pinnedRunner(t *testing.T, gitPath, dir string) *Runner {
	t.Helper()
	runner, err := New(Options{GitPath: gitPath, Scratch: t.TempDir(), Class: errTestClass})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.PinCheckout(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	return runner
}

// fakeRunner is a runner whose git is a script, pinned by hand to sha1 so
// the script needs to speak only the batch protocol.
func fakeRunner(t *testing.T, script string) *Runner {
	t.Helper()
	scratch := t.TempDir()
	gitPath := filepath.Join(scratch, "fake-git")
	execfixture.Write(t, gitPath, []byte("#!/bin/sh\n"+script), 0o700)
	runner, err := New(Options{GitPath: gitPath, Scratch: scratch, Class: errTestClass})
	if err != nil {
		t.Fatal(err)
	}
	runner.format = "sha1"
	return runner
}

// requireStopped proves no process the reader started outlives it: each
// was waited on and its whole process group, descendants included, is gone.
func requireStopped(t *testing.T, o *ObjectReader) {
	t.Helper()
	for _, p := range []*batchProcess{o.check, o.content} {
		if p == nil {
			continue
		}
		if err := procbound.ConfirmExit(p.cmd, procbound.DefaultWaitDelay); err != nil {
			t.Fatalf("%v outlived the reader: %v", p.args, err)
		}
	}
}

// requireFailed checks the reader's error carries the runner's class, then
// that the reader stays failed and left no process behind.
func requireFailed(t *testing.T, o *ObjectReader, err error, want error) {
	t.Helper()
	if !errors.Is(err, errTestClass) || (want != nil && !errors.Is(err, want)) {
		t.Fatalf("err = %v, want class %v and %v", err, errTestClass, want)
	}
	requireStopped(t, o)
	if _, again := o.Info(t.Context(), strings.Repeat("a", 40)); !errors.Is(again, err) {
		t.Fatalf("Info after failure = %v, want the first error %v", again, err)
	}
	if _, _, again := o.Open(t.Context(), strings.Repeat("a", 40)); !errors.Is(again, err) {
		t.Fatalf("Open after failure = %v, want the first error %v", again, err)
	}
	if o.Close() != nil {
		t.Fatal("Close after failure returned an error")
	}
}

func TestObjectReaderReadsEachType(t *testing.T) {
	repo := newObjectRepo(t)
	ctx := t.Context()
	o := pinnedRunner(t, "git", repo.dir).Objects(ctx)
	for _, tc := range []struct {
		name, id string
		typ      ObjectType
		content  string
	}{
		{"blob", repo.blob, ObjectBlob, "hello\n"},
		{"symlink blob", repo.link, ObjectBlob, "file"},
		{"tree", repo.tree, ObjectTree, ""},
		{"commit", repo.commit, ObjectCommit, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := fixtureGit(t, repo.dir, nil, "cat-file", "-s", tc.id)
			info, err := o.Info(ctx, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if info.ID != tc.id || info.Type != tc.typ || fmt.Sprint(info.Size) != want {
				t.Fatalf("Info = %+v, want %s of size %s", info, tc.typ, want)
			}
			opened, body, err := o.Open(ctx, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if opened != info || int64(len(content)) != info.Size {
				t.Fatalf("Open = %+v with %d bytes, want %+v", opened, len(content), info)
			}
			if tc.content != "" && string(content) != tc.content {
				t.Fatalf("content = %q, want %q", content, tc.content)
			}
		})
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	requireStopped(t, o)
	if _, err := o.Info(ctx, repo.blob); !errors.Is(err, errTestClass) {
		t.Fatalf("Info after Close = %v, want %v", err, errTestClass)
	}
}

func TestObjectReaderMissingObjectFails(t *testing.T) {
	repo := newObjectRepo(t)
	for name, call := range map[string]func(*ObjectReader, string) error{
		"info": func(o *ObjectReader, id string) error { _, err := o.Info(t.Context(), id); return err },
		"open": func(o *ObjectReader, id string) error { _, _, err := o.Open(t.Context(), id); return err },
	} {
		t.Run(name, func(t *testing.T) {
			o := pinnedRunner(t, "git", repo.dir).Objects(t.Context())
			err := call(o, strings.Repeat("0", 40))
			requireFailed(t, o, err, ErrObjectMissing)
		})
	}
}

// Every refused ID is refused before the reader starts git, so no request
// reaches a process.
func TestObjectReaderRefusesNonHexIDs(t *testing.T) {
	repo := newObjectRepo(t)
	for name, id := range map[string]string{
		"empty":           "",
		"revision":        "HEAD",
		"path expression": repo.commit + ":file",
		"abbreviation":    repo.blob[:39],
		"uppercase":       strings.ToUpper(repo.blob),
		"sha256 length":   repo.blob + repo.blob[:24],
		"embedded line":   repo.blob[:20] + "\n" + repo.blob[21:],
		"trailing space":  repo.blob[:39] + " ",
	} {
		t.Run(name, func(t *testing.T) {
			o := pinnedRunner(t, "git", repo.dir).Objects(t.Context())
			_, _, err := o.Open(t.Context(), id)
			if !errors.Is(err, errTestClass) {
				t.Fatalf("Open(%q) = %v, want %v", id, err, errTestClass)
			}
			if o.check != nil || o.content != nil {
				t.Fatalf("Open(%q) started git before refusing the ID", id)
			}
			requireFailed(t, o, err, nil)
		})
	}
}

func TestObjectReaderRefusesAnUnpinnedRunner(t *testing.T) {
	runner, err := New(Options{Scratch: t.TempDir(), Class: errTestClass})
	if err != nil {
		t.Fatal(err)
	}
	o := runner.Objects(t.Context())
	_, err = o.Info(t.Context(), strings.Repeat("a", 40))
	if o.check != nil {
		t.Fatal("an unpinned reader started git")
	}
	requireFailed(t, o, err, nil)
}

// A size check never makes git stream the object, and content larger than
// the caller's cap streams through a bounded buffer rather than being held.
func TestObjectReaderStreamsALargeBlob(t *testing.T) {
	repo := newObjectRepo(t)
	ctx := t.Context()
	o := pinnedRunner(t, "git", repo.dir).Objects(ctx)
	defer func() { _ = o.Close() }()
	info, err := o.Info(ctx, repo.large)
	if err != nil || info.Size != repo.largeSize {
		t.Fatalf("Info = %+v, %v; want size %d", info, err, repo.largeSize)
	}
	if o.content != nil {
		t.Fatal("Info started the content process")
	}

	info, body, err := o.Open(ctx, repo.large)
	if err != nil || info.Size != repo.largeSize {
		t.Fatalf("Open = %+v, %v; want size %d", info, err, repo.largeSize)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	n, err := io.Copy(io.Discard, body)
	runtime.ReadMemStats(&after)
	if err != nil || n != repo.largeSize {
		t.Fatalf("copied %d bytes, %v; want %d", n, err, repo.largeSize)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > largeBlobSize/8 {
		t.Fatalf("streaming %d bytes allocated %d", repo.largeSize, allocated)
	}
	// The stream stays aligned after a full drain.
	if _, body, err = o.Open(ctx, repo.blob); err != nil {
		t.Fatal(err)
	}
	if content, err := io.ReadAll(body); err != nil || string(content) != "hello\n" {
		t.Fatalf("next object = %q, %v", content, err)
	}

	// A caller that stops at its cap leaves content unread, and the next
	// call fails rather than reading the rest as a header.
	_, body, err = o.Open(ctx, repo.large)
	if err != nil {
		t.Fatal(err)
	}
	const callerCap = 1 << 10
	if n, err := io.Copy(io.Discard, io.LimitReader(body, callerCap)); err != nil || n != callerCap {
		t.Fatalf("capped read = %d, %v", n, err)
	}
	_, err = o.Info(ctx, repo.blob)
	requireFailed(t, o, err, nil)
}

// countingGit wraps the real git and logs each invocation's arguments.
func countingGit(t *testing.T) (string, func() []string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "git")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexec %s \"$@\"\n", quote(log), quote(realGit))
	execfixture.Write(t, script, []byte(body), 0o700)
	return script, func() []string {
		t.Helper()
		data, err := os.ReadFile(log) //nolint:gosec // G304: test-owned invocation log in t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func TestObjectReaderStartsOneProcessPerMode(t *testing.T) {
	repo := newObjectRepo(t)
	gitPath, calls := countingGit(t)
	ctx := t.Context()
	o := pinnedRunner(t, gitPath, repo.dir).Objects(ctx)
	const reads = 50
	for range reads {
		for _, id := range []string{repo.blob, repo.link, repo.tree} {
			if _, err := o.Info(ctx, id); err != nil {
				t.Fatal(err)
			}
			_, body, err := o.Open(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	requireStopped(t, o)
	counts := map[string]int{}
	for _, call := range calls() {
		args := strings.Fields(call)
		counts[strings.Join(args[len(args)-2:], " ")]++
	}
	want := map[string]int{
		"rev-parse --absolute-git-dir": 1, "rev-parse --show-object-format": 1,
		"cat-file --batch-check": 1, "cat-file --batch": 1,
	}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Fatalf("git invocations = %v, want %v", counts, want)
	}
}

// Each fake git breaks the protocol a different way. A fake that must stay
// alive execs sleep rather than forking it: on macOS a child forked while
// the group kill lands can escape it, and an orphaned child lingers as a
// zombie that keeps the group probe from proving absence. Real cat-file
// forks nothing; procbound's own tests cover the group kill.
func TestObjectReaderFailsClosedOnBrokenResponses(t *testing.T) {
	const id = "1111111111111111111111111111111111111111"
	const other = "2222222222222222222222222222222222222222"
	for _, tc := range []struct {
		name, script string
		read         bool
		want         error
	}{
		{"malformed header", "read id; printf 'garbage\\n'; exec sleep 600\n", false, errBatchResponse},
		{"signed size", "read id; printf '%s blob +3\\nabc\\n' \"$id\"; exec sleep 600\n", false, errBatchResponse},
		{"unknown type", "read id; printf '%s note 3\\nabc\\n' \"$id\"; exec sleep 600\n", false, errBatchResponse},
		{"header for another ID", "read id; printf '" + other + " blob 3\\nabc\\n'; exec sleep 600\n", false, errBatchResponse},
		{"missing", "read id; printf '%s missing\\n' \"$id\"; exec sleep 600\n", false, ErrObjectMissing},
		{"overlong header", "read id; printf '%s blob ' \"$id\"; i=0; while [ $i -lt 5000 ]; do printf 9; i=$((i+1)); done; exec sleep 600\n", false, bufio.ErrBufferFull},
		{"no response", "read id; exit 0\n", false, io.ErrUnexpectedEOF},
		{"short body", "read id; printf '%s blob 10\\nabc' \"$id\"; exit 0\n", true, io.ErrUnexpectedEOF},
		{"bad delimiter", "read id; printf '%s blob 3\\nabcX' \"$id\"; exec sleep 600\n", true, errBatchResponse},
		{"missing delimiter", "read id; printf '%s blob 3\\nabc' \"$id\"; exit 0\n", true, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := fakeRunner(t, tc.script).Objects(t.Context())
			_, body, err := o.Open(t.Context(), id)
			if tc.read {
				if err != nil {
					t.Fatalf("Open = %v, want the header accepted", err)
				}
				_, err = io.ReadAll(body)
			}
			requireFailed(t, o, err, tc.want)
		})
	}
}

// A git that answers the first request and exits during the second.
func TestObjectReaderFailsWhenGitExitsMidStream(t *testing.T) {
	const id = "1111111111111111111111111111111111111111"
	o := fakeRunner(t, "read id; printf '%s blob 3\\nabc\\n' \"$id\"; read id; exit 0\n").Objects(t.Context())
	_, body, err := o.Open(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if content, err := io.ReadAll(body); err != nil || string(content) != "abc" {
		t.Fatalf("first object = %q, %v", content, err)
	}
	_, _, err = o.Open(t.Context(), id)
	requireFailed(t, o, err, io.ErrUnexpectedEOF)
}

// hangingGit answers nothing after signalling, through a FIFO, that the
// request arrived, so a test can cancel exactly while a call waits on git.
// prefix is what git writes before it hangs.
func hangingGit(t *testing.T, prefix string) (*Runner, <-chan struct{}) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "request-read")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := fakeRunner(t, fmt.Sprintf("read id; printf '%s' \"$id\"; echo > '%s'; exec sleep 600\n", prefix, fifo))
	arrived := make(chan struct{})
	go func() {
		defer close(arrived)
		_, _ = os.ReadFile(fifo) //nolint:gosec // G304: test-owned FIFO in t.TempDir
	}()
	return runner, arrived
}

func TestObjectReaderCancellation(t *testing.T) {
	const id = "1111111111111111111111111111111111111111"
	t.Run("call context during the header", func(t *testing.T) {
		runner, arrived := hangingGit(t, "")
		o := runner.Objects(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-arrived; cancel() }()
		_, err := o.Info(ctx, id)
		requireFailed(t, o, err, context.Canceled)
	})
	t.Run("call context during the content", func(t *testing.T) {
		runner, arrived := hangingGit(t, "%s blob 10\\nabc")
		o := runner.Objects(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		_, body, err := o.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		go func() { <-arrived; cancel() }()
		_, err = io.ReadAll(body)
		requireFailed(t, o, err, context.Canceled)
	})
	t.Run("session context during the header", func(t *testing.T) {
		runner, arrived := hangingGit(t, "")
		session, cancel := context.WithCancel(t.Context())
		o := runner.Objects(session)
		go func() { <-arrived; cancel() }()
		_, err := o.Info(t.Context(), id)
		requireFailed(t, o, err, context.Canceled)
	})
	// git has answered in full, so only the cancellation check after the
	// read can fail the call; the content and delimiter arrive in one
	// write, so the delimiter is already buffered when the context ends.
	t.Run("call context after git answered", func(t *testing.T) {
		o := fakeRunner(t, "read id; printf '%s blob 3\\nabc\\n' \"$id\"; exec sleep 600\n").Objects(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		_, body, err := o.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		content := make([]byte, 3)
		if _, err := io.ReadFull(body, content); err != nil || string(content) != "abc" {
			t.Fatalf("content = %q, %v", content, err)
		}
		cancel()
		_, err = io.ReadAll(body)
		requireFailed(t, o, err, errCallCancelled)
	})
	t.Run("call context already done", func(t *testing.T) {
		o := fakeRunner(t, "exit 1\n").Objects(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := o.Info(ctx, id)
		if o.check != nil {
			t.Fatal("a cancelled call started git")
		}
		requireFailed(t, o, err, context.Canceled)
	})
}
