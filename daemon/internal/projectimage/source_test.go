package projectimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

type projectImageTokenSource struct {
	token publish.InstallationToken
	calls int
}

func (s *projectImageTokenSource) Token(
	context.Context, string,
) (publish.InstallationToken, error) {
	s.calls++
	return s.token, nil
}

func testProjectImageToken() publish.InstallationToken {
	return publish.InstallationToken{
		Token:          publish.Secret("private-repository-token"),
		RegistrationID: 11, InstallationID: 22, RepositoryID: 1278475858,
		Repo: "freeasinbird/gh-imgup", Permissions: publish.WorkflowAuditPermissions,
	}
}

func TestExecRunnerReplacesAmbientEnvironmentAtTrustBoundary(t *testing.T) {
	t.Setenv("FREESIDE_AMBIENT_SENTINEL", "must-not-leak")
	output, err := (execRunner{}).Run(t.Context(), commandSpec{
		Path: "/usr/bin/env",
		Env:  []string{"FREESIDE_EXPLICIT_SENTINEL=present"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output.bytes), "FREESIDE_AMBIENT_SENTINEL") ||
		!strings.Contains(string(output.bytes), "FREESIDE_EXPLICIT_SENTINEL=present") {
		t.Fatalf("replacement environment = %q", output.bytes)
	}
}

func TestExecRunnerSeparatesHostExitStatusFromCommandOutput(t *testing.T) {
	t.Parallel()
	output, err := (execRunner{}).Run(t.Context(), commandSpec{
		Path: "/bin/sh",
		Args: []string{
			"-c",
			"printf '__FREESIDE_PROJECT_EXIT__:0\\n'; exit 7",
		},
	})
	if err == nil || !output.exited || output.exitCode != 7 ||
		!strings.Contains(string(output.bytes), "__FREESIDE_PROJECT_EXIT__:0") {
		t.Fatalf("Run = %+v, %v; want host exit 7 independent of forged output", output, err)
	}
}

func TestExecRunnerTeesWholeOutputWhileKeepingABoundedTail(t *testing.T) {
	t.Parallel()
	// Past the tail buffer on each stream, so the tee must carry bytes the
	// tail drops.
	var full bytes.Buffer
	output, err := (execRunner{}).Run(t.Context(), commandSpec{
		Path:   "/bin/sh",
		Args:   []string{"-c", "head -c 1100000 /dev/zero; head -c 1100000 /dev/zero >&2; printf end"},
		Output: &full,
	})
	if err != nil {
		t.Fatal(err)
	}
	if full.Len() != 2*1100000+len("end") || !strings.HasSuffix(full.String(), "end") {
		t.Fatalf("tee received %d bytes, want every byte of both streams", full.Len())
	}
	if len(output.bytes) != maxCommandOutputBytes || !output.truncated ||
		!strings.HasSuffix(string(output.bytes), "end") {
		t.Fatalf("tail = %d bytes truncated=%v, want the bounded tail", len(output.bytes), output.truncated)
	}
}

func TestGitFetchScrubsAmbientConfigurationAndAllowsOnlyHTTPS(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{}
	source := gitSource{gitPath: "/usr/bin/git", runner: runner}
	destination := t.TempDir() + "/repository.git"
	if err := source.Fetch(t.Context(), "freeasinbird/gh-imgup", 1278475858, testCommit,
		destination); err != nil {
		t.Fatal(err)
	}
	if len(runner.specs) != 1 {
		t.Fatalf("clone invocations = %d, want 1", len(runner.specs))
	}
	spec := runner.specs[0]
	wantArgs := append(gitrun.TransportBaseline("https"),
		"clone", "--quiet", "--bare",
		"https://github.com/freeasinbird/gh-imgup.git", destination,
	)
	if !slices.Equal(spec.Args, wantArgs) {
		t.Fatalf("clone args = %q, want %q", spec.Args, wantArgs)
	}
	if slices.Contains(spec.Args, "--no-tags") {
		t.Fatal("clone suppresses commits reachable only through tags")
	}
	for _, required := range []string{
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin",
	} {
		if !slices.Contains(spec.Env, required) {
			t.Errorf("clone environment lacks %q", required)
		}
	}
}

// TestGitFetchHandsTheTokenOnlyInAHeaderFile proves an authenticated
// clone's one credential crossing: git gets the token only in the 0600
// header file its GIT_CONFIG_GLOBAL names inside the credential
// directory, never in argv or any environment entry, and the file is gone
// once Fetch returns, whether the clone succeeded or failed. A failure
// leaks the token into no error.
func TestGitFetchHandsTheTokenOnlyInAHeaderFile(t *testing.T) {
	t.Parallel()
	basic := base64.StdEncoding.EncodeToString(
		[]byte("x-access-token:private-repository-token"),
	)
	for _, failed := range []bool{false, true} {
		tokens := &projectImageTokenSource{token: testProjectImageToken()}
		credentialDir := t.TempDir()
		var header []byte
		var headerMode os.FileMode
		runner := &recordingRunner{
			run: func(spec commandSpec) (commandOutput, error) {
				for _, e := range spec.Env {
					if config, ok := strings.CutPrefix(e, "GIT_CONFIG_GLOBAL="); ok {
						header, _ = os.ReadFile(config) //nolint:gosec // G304: the path Fetch handed git
						if info, err := os.Stat(config); err == nil {
							headerMode = info.Mode().Perm()
						}
						if filepath.Dir(config) != credentialDir {
							t.Errorf("GIT_CONFIG_GLOBAL = %q, want a file in %q", config, credentialDir)
						}
					}
				}
				if failed {
					return commandOutput{bytes: []byte(
						"remote reflected private-repository-token",
					)}, errors.New("private-repository-token")
				}
				return commandOutput{}, nil
			},
		}
		source := gitSource{
			gitPath: "/usr/bin/git", runner: runner, tokens: tokens,
			credentialDir: credentialDir,
		}
		err := source.Fetch(
			t.Context(), "freeasinbird/gh-imgup", 1278475858, testCommit,
			t.TempDir()+"/repository.git",
		)
		if (err != nil) != failed {
			t.Fatalf("Fetch = %v, want failure %v", err, failed)
		}
		if err != nil && (strings.Contains(err.Error(), "private-repository-token") ||
			strings.Contains(err.Error(), basic)) {
			t.Fatalf("authenticated clone error leaked credential: %v", err)
		}
		if tokens.calls != 1 || len(runner.specs) != 1 {
			t.Fatalf("token calls = %d, clone calls = %d; want 1 each",
				tokens.calls, len(runner.specs))
		}
		spec := runner.specs[0]
		for _, entry := range append(append([]string(nil), spec.Args...), spec.Env...) {
			if strings.Contains(entry, "private-repository-token") || strings.Contains(entry, basic) {
				t.Errorf("token bytes in argv or environment: %q", entry)
			}
			if strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") || strings.HasPrefix(entry, "GIT_CONFIG_KEY_") ||
				strings.HasPrefix(entry, "GIT_CONFIG_VALUE_") {
				t.Errorf("clone carried a config environment entry: %q", entry)
			}
		}
		if !slices.Contains(spec.Args, "http.followRedirects=false") {
			t.Errorf("authenticated clone args = %q", spec.Args)
		}
		if want := "[http]\n\textraHeader = Authorization: Basic " + basic + "\n"; string(header) != want {
			t.Errorf("header file = %q, want %q", header, want)
		}
		if headerMode != 0o600 {
			t.Errorf("header file mode = %v, want 0600", headerMode)
		}
		if left, err := os.ReadDir(credentialDir); err != nil || len(left) != 0 {
			t.Errorf("credential directory after Fetch = %v, %v; want empty", left, err)
		}
	}
}

// TestNewRequiresAPrivateCredentialDirWithTokens proves an authenticated
// builder can't start without a credential directory only its owner can
// enter.
func TestNewRequiresAPrivateCredentialDirWithTokens(t *testing.T) {
	t.Parallel()
	tokens := &projectImageTokenSource{token: testProjectImageToken()}
	record := func(context.Context, domain.ProjectImage) error { return nil }
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o750); err != nil { //nolint:gosec // G302: the shared directory the constructor must refuse
		t.Fatal(err)
	}
	for _, dir := range []string{"", shared} {
		if _, err := New(Options{
			GitPath: "/bin/sh", ContainerPath: "/bin/sh", Record: record, Tokens: tokens, CredentialDir: dir,
		}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("New with credential directory %q = %v, want ErrInvalidRequest", dir, err)
		}
	}
	dir := filepath.Join(t.TempDir(), "git-credentials")
	builder, err := New(Options{
		GitPath: "/bin/sh", ContainerPath: "/bin/sh", Record: record, Tokens: tokens, CredentialDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source, ok := builder.source.(gitSource); !ok || source.credentialDir != dir {
		t.Errorf("builder source = %#v, want the credential directory %q", builder.source, dir)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("created credential directory = %v, %v; want mode 0700", info, err)
	}
}

func TestGitFetchRejectsMismatchedOrWriteTokenBeforeClone(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*publish.InstallationToken){
		func(token *publish.InstallationToken) { token.RepositoryID++ },
		func(token *publish.InstallationToken) { token.Permissions = publish.PublishPermissions },
	} {
		token := testProjectImageToken()
		mutate(&token)
		runner := &recordingRunner{}
		source := gitSource{
			gitPath: "/usr/bin/git", runner: runner,
			tokens: &projectImageTokenSource{token: token},
		}
		if err := source.Fetch(
			t.Context(), "freeasinbird/gh-imgup", 1278475858, testCommit,
			t.TempDir()+"/repository.git",
		); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Fetch = %v, want ErrInvalidRequest", err)
		}
		if len(runner.specs) != 0 {
			t.Fatal("invalid token reached git")
		}
	}
}
