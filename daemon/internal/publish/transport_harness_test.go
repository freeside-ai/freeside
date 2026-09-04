package publish

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// writeExecutable prevents the ETXTBSY race described in go.dev/issue/22315:
// a parallel fork can inherit a script's write descriptor until its own exec.
// forkExec holds ForkLock for writing, so this read lock prevents forks while
// the write descriptor is open, including the interval before Close.
func writeExecutable(t *testing.T, path string, body []byte) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, body, 0o700) //nolint:gosec // G306: test-owned executable
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteExecutableSurvivesParallelForks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux enforces ETXTBSY for these scripts")
	}
	t.Parallel()
	var stop atomic.Bool
	var forks sync.WaitGroup
	for range 16 {
		forks.Go(func() {
			for !stop.Load() {
				if err := exec.CommandContext(t.Context(), "/bin/true").Run(); err != nil {
					t.Errorf("parallel fork: %v", err)
					return
				}
			}
		})
	}
	defer func() {
		stop.Store(true)
		forks.Wait()
	}()
	path := filepath.Join(t.TempDir(), "script.sh")
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		writeExecutable(t, path, []byte("#!/bin/sh\nexit 0\n"))
		if err := exec.CommandContext(t.Context(), path).Run(); err != nil { //nolint:gosec // G204: test-owned executable
			t.Fatal(err)
		}
	}
}

// stubGit writes a fake git binary that appends every invocation's
// argument vector, full environment, and working directory to recPath,
// then exits with the given code. The record path is baked into the
// script body because the runner replaces the child environment
// entirely — which is itself one of the properties under test.
func stubGit(t *testing.T, recPath string, extra string, exit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nrec='" + recPath + "'\n{\nprintf 'BEGIN\\n'\nfor a in \"$@\"; do printf 'ARG %s\\n' \"$a\"; done\nenv | sed 's/^/ENV /'\nprintf 'CWD %s\\n' \"$PWD\"\n} >> \"$rec\"\n" +
		extra + "exit " + strconv.Itoa(exit) + "\n"
	writeExecutable(t, path, []byte(script))
	return path
}

const stubTokenValue = "stub-installation-token-bytes"

// stubRepositoryID is the canonical numeric identity the stub trusted
// binding resolves every fixture repository to.
const stubRepositoryID int64 = 8675309

func stubToken() InstallationToken {
	return InstallationToken{Token: Secret(stubTokenValue), RepositoryID: stubRepositoryID}
}

// stubTokenForms are every rendering of the token that could leak: the
// raw value and the basic-auth base64 the header carries.
func stubTokenForms() []string {
	return []string{
		stubTokenValue,
		base64.StdEncoding.EncodeToString([]byte("x-access-token:" + stubTokenValue)),
	}
}

// TestRunAuthedTokenPlacement proves acceptance 2's argv and
// environment properties at the runner boundary: the token reaches git
// only as the one GIT_CONFIG_VALUE_0 header entry in a fully replaced
// child environment — never as an argument, never inherited from the
// daemon's own environment, and never retained by the runner between
// invocations.
func TestRunAuthedTokenPlacement(t *testing.T) {
	// Serial: t.Setenv mutates the process-global environment, which the
	// Go test runner forbids in a parallel test (it panics).
	rec := filepath.Join(t.TempDir(), "record")
	scratch := t.TempDir()
	r, err := newNetRunner(stubGit(t, rec, "", 0), scratch, "https")
	if err != nil {
		t.Fatal(err)
	}
	// A canary in the daemon's environment: a leaked os.Environ() would
	// carry it into the child.
	t.Setenv("FREESIDE_TRANSPORT_CANARY", "canary-value")

	if _, _, err := r.runAuthed(t.Context(), stubToken(), "ls-remote", "https://example.invalid/o/r.git", "refs/heads/b"); err != nil {
		t.Fatal(err)
	}

	record, err := os.ReadFile(rec) //nolint:gosec // G304: test-owned record file
	if err != nil {
		t.Fatal(err)
	}
	var args, envs []string
	cwd := ""
	for _, line := range strings.Split(string(record), "\n") {
		switch {
		case strings.HasPrefix(line, "ARG "):
			args = append(args, strings.TrimPrefix(line, "ARG "))
		case strings.HasPrefix(line, "ENV "):
			envs = append(envs, strings.TrimPrefix(line, "ENV "))
		case strings.HasPrefix(line, "CWD "):
			cwd = strings.TrimPrefix(line, "CWD ")
		}
	}
	if len(args) == 0 || len(envs) == 0 {
		t.Fatalf("stub recorded nothing usable:\n%s", record)
	}
	for _, form := range stubTokenForms() {
		for _, a := range args {
			if strings.Contains(a, form) {
				t.Errorf("token bytes on argv: %q", a)
			}
		}
	}
	wantHeader := "GIT_CONFIG_VALUE_0=Authorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte("x-access-token:"+stubTokenValue))
	var sawHeader, sawCount, sawKey bool
	for _, e := range envs {
		switch {
		case e == wantHeader:
			sawHeader = true
		case e == "GIT_CONFIG_COUNT=1":
			sawCount = true
		case e == "GIT_CONFIG_KEY_0=http.extraHeader":
			sawKey = true
		case strings.HasPrefix(e, "FREESIDE_TRANSPORT_CANARY="):
			t.Error("daemon environment leaked into the child (canary present)")
		}
		if !strings.HasPrefix(e, "GIT_CONFIG_VALUE_0=") {
			for _, form := range stubTokenForms() {
				if strings.Contains(e, form) {
					t.Errorf("token bytes outside the header entry: %q", e)
				}
			}
		}
	}
	if !sawHeader || !sawCount || !sawKey {
		t.Errorf("header config triple incomplete: header=%v count=%v key=%v", sawHeader, sawCount, sawKey)
	}
	wantCwd, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if gotCwd, err := filepath.EvalSymlinks(cwd); err != nil || gotCwd != wantCwd {
		t.Errorf("child cwd = %q (%v), want the scratch %q", cwd, err, scratch)
	}
	for _, e := range r.env {
		if credentialConfigEnv(e) {
			t.Errorf("runner retained credential env between invocations: %q", e)
		}
	}
}

// TestRunUnauthenticatedCarriesNoCredentialEnv proves the token triple
// exists only on authenticated invocations.
func TestRunUnauthenticatedCarriesNoCredentialEnv(t *testing.T) {
	t.Parallel()
	rec := filepath.Join(t.TempDir(), "record")
	r, err := newNetRunner(stubGit(t, rec, "", 0), t.TempDir(), "https")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.run(t.Context(), nil, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(rec) //nolint:gosec // G304: test-owned record file
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(record), "\n") {
		if credentialConfigEnv(strings.TrimPrefix(line, "ENV ")) {
			t.Errorf("unauthenticated invocation carried credential config env: %q", line)
		}
	}
}

// credentialConfigEnv reports whether an environment entry belongs to
// the per-invocation credential triple (as opposed to the permanent
// GIT_CONFIG_GLOBAL/SYSTEM/NOSYSTEM neutralization, which is expected
// hardening).
func credentialConfigEnv(e string) bool {
	return strings.HasPrefix(e, "GIT_CONFIG_COUNT=") ||
		strings.HasPrefix(e, "GIT_CONFIG_KEY_") ||
		strings.HasPrefix(e, "GIT_CONFIG_VALUE_")
}

// TestTransportErrorRedactsFailureStreams is the stderr-leak
// refutation: the stub prints the whole credential header to stderr
// (the worst realistic leak — git echoing a request or URL), fails,
// and every rendering of the resulting error must still be free of
// the token in any form.
func TestTransportErrorRedactsFailureStreams(t *testing.T) {
	t.Parallel()
	leak := "printf 'fatal: leaked %s\\n' \"$GIT_CONFIG_VALUE_0\" >&2\n"
	for _, tc := range []struct {
		name     string
		extra    string
		exitCode int
		cause    string
	}{
		{name: "ordinary exit", extra: leak, exitCode: 3},
		{name: "signal death", extra: leak + "kill -KILL $$\n", exitCode: -1, cause: "signal: killed"},
		{name: "startup failure", exitCode: -1, cause: "no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "missing-git")
			if tc.name != "startup failure" {
				path = stubGit(t, filepath.Join(t.TempDir(), "record"), tc.extra, 3)
			}
			r, err := newNetRunner(path, t.TempDir(), "https")
			if err != nil {
				t.Fatal(err)
			}
			tok := stubToken()
			_, _, runErr := r.run(t.Context(), &tok, "push", "https://example.invalid/o/r.git", "x:refs/heads/x")
			if !errors.Is(runErr, ErrGitTransport) {
				t.Errorf("error class = %v, want ErrGitTransport", runErr)
			}
			var tge *TransportGitError
			if !errors.As(runErr, &tge) {
				t.Fatalf("error type = %T, want *TransportGitError", runErr)
			}
			if tge.ExitCode != tc.exitCode {
				t.Errorf("exit code = %d, want %d", tge.ExitCode, tc.exitCode)
			}
			if !tge.Refusal.valid() {
				t.Errorf("refusal %q is not a registered class", tge.Refusal)
			}
			if tc.name == "startup failure" {
				var pathErr *os.PathError
				if !errors.As(runErr, &pathErr) || !errors.Is(runErr, os.ErrNotExist) {
					t.Errorf("startup error = %v, want missing-executable PathError", runErr)
				}
			}
			want := "git push https://example.invalid/o/r.git x:refs/heads/x: exit 3: unknown"
			if tc.exitCode == -1 {
				want = fmt.Sprintf("git push https://example.invalid/o/r.git x:refs/heads/x: exit -1 (%v): unknown", tge.Err)
				if !strings.Contains(tge.Error(), tc.cause) {
					t.Errorf("error = %v, want cause %q", tge, tc.cause)
				}
			}
			if tge.Error() != want {
				t.Errorf("error = %q, want %q", tge.Error(), want)
			}
			rendered := fmt.Sprintf("%v | %+v | %#v | %s", runErr, runErr, tge, tge.Args)
			for _, form := range stubTokenForms() {
				if strings.Contains(rendered, form) {
					t.Errorf("token bytes in rendered error: %s", rendered)
				}
			}
			if strings.Contains(rendered, "leaked") {
				t.Errorf("stderr text in rendered error: %s", rendered)
			}
		})
	}
}
