package publish

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
)

// stubGit writes a fake git binary that appends every invocation's
// argument vector, full environment, and working directory to recPath,
// copies the file GIT_CONFIG_GLOBAL names, mode included, to
// recPath+".config" when it names a regular file, then exits with the
// given code. The record path is baked into the script body because the
// runner replaces the child environment entirely — which is itself one
// of the properties under test.
func stubGit(t *testing.T, recPath string, extra string, exit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nrec='" + recPath + "'\n{\nprintf 'BEGIN\\n'\nfor a in \"$@\"; do printf 'ARG %s\\n' \"$a\"; done\nenv | sed 's/^/ENV /'\nprintf 'CWD %s\\n' \"$PWD\"\n} >> \"$rec\"\n" +
		"if [ -f \"$GIT_CONFIG_GLOBAL\" ]; then cp -p \"$GIT_CONFIG_GLOBAL\" \"$rec.config\"; fi\n" +
		extra + "exit " + strconv.Itoa(exit) + "\n"
	execfixture.Write(t, path, []byte(script), 0o700)
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

// stubRecord is one stub invocation's recorded argv, environment, and
// working directory.
type stubRecord struct {
	args, envs []string
	cwd        string
}

func readStubRecord(t *testing.T, rec string) stubRecord {
	t.Helper()
	record, err := os.ReadFile(rec) //nolint:gosec // G304: test-owned record file
	if err != nil {
		t.Fatal(err)
	}
	var out stubRecord
	for _, line := range strings.Split(string(record), "\n") {
		switch {
		case strings.HasPrefix(line, "ARG "):
			out.args = append(out.args, strings.TrimPrefix(line, "ARG "))
		case strings.HasPrefix(line, "ENV "):
			out.envs = append(out.envs, strings.TrimPrefix(line, "ENV "))
		case strings.HasPrefix(line, "CWD "):
			out.cwd = strings.TrimPrefix(line, "CWD ")
		}
	}
	if len(out.args) == 0 || len(out.envs) == 0 {
		t.Fatalf("stub recorded nothing usable:\n%s", record)
	}
	return out
}

// TestRunAuthedTokenPlacement proves the token's one crossing at the
// runner boundary: git receives it only in the header file its
// GIT_CONFIG_GLOBAL names, mode 0600 inside the credential directory,
// in a fully replaced child environment — never as an argument, never
// in any environment entry, never inherited from the daemon's own
// environment, and never retained by the runner between invocations.
// The file is gone once the invocation returns, whether git succeeded
// or failed.
func TestRunAuthedTokenPlacement(t *testing.T) {
	// Serial: t.Setenv mutates the process-global environment, which the
	// Go test runner forbids in a parallel test (it panics).
	for _, exit := range []int{0, 3} {
		t.Run(fmt.Sprintf("git exits %d", exit), func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "record")
			scratch, credentialDir := t.TempDir(), t.TempDir()
			r, err := newNetRunner(stubGit(t, rec, "", exit), scratch, credentialDir, "https")
			if err != nil {
				t.Fatal(err)
			}
			// A canary in the daemon's environment: a leaked os.Environ()
			// would carry it into the child.
			t.Setenv("FREESIDE_TRANSPORT_CANARY", "canary-value")

			_, _, runErr := r.runAuthed(t.Context(), stubToken(), "ls-remote", "https://example.invalid/o/r.git", "refs/heads/b")
			if (runErr != nil) != (exit != 0) {
				t.Fatalf("runAuthed = %v with git exiting %d", runErr, exit)
			}

			got := readStubRecord(t, rec)
			for _, form := range stubTokenForms() {
				for _, entry := range append(append([]string(nil), got.args...), got.envs...) {
					if strings.Contains(entry, form) {
						t.Errorf("token bytes in argv or environment: %q", entry)
					}
				}
			}
			config := ""
			for _, e := range got.envs {
				switch {
				case strings.HasPrefix(e, "GIT_CONFIG_GLOBAL="):
					config = strings.TrimPrefix(e, "GIT_CONFIG_GLOBAL=")
				case credentialConfigEnv(e):
					t.Errorf("child carried a config environment entry: %q", e)
				case strings.HasPrefix(e, "FREESIDE_TRANSPORT_CANARY="):
					t.Error("daemon environment leaked into the child (canary present)")
				}
			}
			wantDir, err := filepath.EvalSymlinks(credentialDir)
			if err != nil {
				t.Fatal(err)
			}
			if gotDir, err := filepath.EvalSymlinks(filepath.Dir(config)); err != nil || gotDir != wantDir {
				t.Errorf("GIT_CONFIG_GLOBAL = %q, want a file in the credential directory %q", config, credentialDir)
			}
			copied, err := os.ReadFile(rec + ".config") //nolint:gosec // G304: test-owned record file
			if err != nil {
				t.Fatalf("git saw no header file: %v", err)
			}
			wantHeader := "[http]\n\textraHeader = Authorization: Basic " +
				base64.StdEncoding.EncodeToString([]byte("x-access-token:"+stubTokenValue)) + "\n"
			if string(copied) != wantHeader {
				t.Errorf("header file = %q, want %q", copied, wantHeader)
			}
			if info, err := os.Stat(rec + ".config"); err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("header file mode = %v, %v; want 0600", info, err)
			}
			if _, err := os.Lstat(config); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("header file outlived the invocation: %v", err)
			}
			if left, err := os.ReadDir(credentialDir); err != nil || len(left) != 0 {
				t.Errorf("credential directory after the invocation = %v, %v; want empty", left, err)
			}

			wantCwd, err := filepath.EvalSymlinks(scratch)
			if err != nil {
				t.Fatal(err)
			}
			if gotCwd, err := filepath.EvalSymlinks(got.cwd); err != nil || gotCwd != wantCwd {
				t.Errorf("child cwd = %q (%v), want the scratch %q", got.cwd, err, scratch)
			}
			if !slices.Contains(r.env, "GIT_CONFIG_GLOBAL="+os.DevNull) {
				t.Errorf("runner retained the header file between invocations: %q", r.env)
			}
		})
	}
}

// TestRunAuthedWithoutCredentialDirStartsNoGit proves the crossing fails
// closed: a runner with no credential directory never starts git with a
// token.
func TestRunAuthedWithoutCredentialDirStartsNoGit(t *testing.T) {
	t.Parallel()
	rec := filepath.Join(t.TempDir(), "record")
	r, err := newNetRunner(stubGit(t, rec, "", 0), t.TempDir(), "", "https")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.runAuthed(t.Context(), stubToken(), "ls-remote", "https://example.invalid/o/r.git"); !errors.Is(err, ErrGitTransport) {
		t.Fatalf("runAuthed = %v, want ErrGitTransport", err)
	}
	if _, err := os.Stat(rec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git started without a credential directory: %v", err)
	}
}

// TestRunUnauthenticatedCarriesNoCredentialEnv proves the header file
// exists only for authenticated invocations.
func TestRunUnauthenticatedCarriesNoCredentialEnv(t *testing.T) {
	t.Parallel()
	rec := filepath.Join(t.TempDir(), "record")
	credentialDir := t.TempDir()
	r, err := newNetRunner(stubGit(t, rec, "", 0), t.TempDir(), credentialDir, "https")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.run(t.Context(), nil, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	got := readStubRecord(t, rec)
	if !slices.Contains(got.envs, "GIT_CONFIG_GLOBAL="+os.DevNull) {
		t.Errorf("unauthenticated invocation named a global config: %q", got.envs)
	}
	for _, e := range got.envs {
		if credentialConfigEnv(e) {
			t.Errorf("unauthenticated invocation carried credential config env: %q", e)
		}
	}
	if _, err := os.Stat(rec + ".config"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unauthenticated invocation saw a header file: %v", err)
	}
	if left, err := os.ReadDir(credentialDir); err != nil || len(left) != 0 {
		t.Errorf("credential directory = %v, %v; want empty", left, err)
	}
}

// credentialConfigEnv reports whether an environment entry is a
// GIT_CONFIG_{COUNT,KEY,VALUE} entry, the environment config channel no
// transport invocation may use (as opposed to the permanent
// GIT_CONFIG_GLOBAL/SYSTEM/NOSYSTEM entries, which are expected).
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
	leak := "printf 'fatal: leaked %s\\n' \"$(cat \"$GIT_CONFIG_GLOBAL\")\" >&2\n"
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
			r, err := newNetRunner(path, t.TempDir(), t.TempDir(), "https")
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
