package gitrun

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBaseline(t *testing.T) {
	want := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
		"-c", "core.protectHFS=true",
		"-c", "core.protectNTFS=true",
	}
	got := Baseline()
	if !slices.Equal(got, want) {
		t.Fatalf("Baseline = %q, want %q", got, want)
	}
	got[0] = "changed"
	if fresh := Baseline(); !slices.Equal(fresh, want) {
		t.Fatalf("Baseline reused caller-mutable storage: %q", fresh)
	}
}

func TestTransportBaselineExtendsBaseline(t *testing.T) {
	wantExtra := []string{
		"-c", "protocol.https.allow=always",
		"-c", "credential.helper=",
		"-c", "http.followRedirects=false",
		"-c", "push.followTags=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "transfer.fsckObjects=true",
	}
	base := Baseline()
	got := TransportBaseline("https")
	if !slices.Equal(got[:len(base)], base) || !slices.Equal(got[len(base):], wantExtra) {
		t.Fatalf("TransportBaseline = %q, want prefix %q plus %q", got, base, wantExtra)
	}
	got[0] = "changed"
	if fresh := TransportBaseline("https"); fresh[0] != "-c" {
		t.Fatalf("TransportBaseline reused caller-mutable storage: %q", fresh)
	}
}

func TestGitErrorPreservesCallerClassAndCause(t *testing.T) {
	class := errors.New("class")
	cause := errors.New("cause")
	err := &GitError{
		Args: []string{"rev-parse", "HEAD"}, Stderr: " failure\n",
		Class: class, Err: cause,
	}
	if !errors.Is(err, class) || !errors.Is(err, cause) {
		t.Fatalf("errors.Is = class:%v cause:%v", errors.Is(err, class), errors.Is(err, cause))
	}
	if got := err.Error(); got != "git rev-parse HEAD: cause: failure" {
		t.Fatalf("Error = %q", got)
	}
}

func TestRunnerPrependsBaselineAndExtras(t *testing.T) {
	scratch := t.TempDir()
	gitPath := filepath.Join(scratch, "record-git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test fixture must be executable
		t.Fatal(err)
	}
	extra := []string{"-c", "core.autocrlf=false"}
	runner, err := New(Options{GitPath: gitPath, Scratch: scratch, ConfigExtra: extra})
	if err != nil {
		t.Fatal(err)
	}
	want := append(Baseline(), extra...)
	want = append(want, "rev-parse", "HEAD")
	assertOutput := func(name string, output []byte) {
		t.Helper()
		got := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
		if !slices.Equal(got, want) {
			t.Fatalf("%s argv = %q, want %q", name, got, want)
		}
	}
	out, err := runner.Run(t.Context(), nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	assertOutput("Run", out)
	var streamed bytes.Buffer
	if err := runner.RunTo(t.Context(), nil, &streamed, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	assertOutput("RunTo", streamed.Bytes())
}

func TestRunnerFailureUsesCallerClass(t *testing.T) {
	scratch := t.TempDir()
	gitPath := filepath.Join(scratch, "fail-git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\nprintf 'rejected\\n' >&2\nexit 7\n"), 0o700); err != nil { //nolint:gosec // G306: test fixture must be executable
		t.Fatal(err)
	}
	class := errors.New("package plumbing failed")
	runner, err := New(Options{GitPath: gitPath, Scratch: scratch, Class: class})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(t.Context(), nil, "rev-parse", "HEAD")
	if !errors.Is(err, class) {
		t.Fatalf("Run error = %v, want class %v", err, class)
	}
	var gitErr *GitError
	if !errors.As(err, &gitErr) || gitErr.Stderr != "rejected\n" ||
		!slices.Equal(gitErr.Args, []string{"rev-parse", "HEAD"}) {
		t.Fatalf("Run error = %#v", err)
	}
}
