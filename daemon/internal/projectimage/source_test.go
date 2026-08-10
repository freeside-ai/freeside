package projectimage

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

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

func TestGitFetchScrubsAmbientConfigurationAndAllowsOnlyHTTPS(t *testing.T) {
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

func TestGitFetchUsesExactReadOnlyTokenWithoutLeakingFailures(t *testing.T) {
	tokens := &projectImageTokenSource{token: testProjectImageToken()}
	runner := &recordingRunner{
		run: func(commandSpec) (commandOutput, error) {
			return commandOutput{bytes: []byte(
				"remote reflected private-repository-token",
			)}, errors.New("private-repository-token")
		},
	}
	source := gitSource{gitPath: "/usr/bin/git", runner: runner, tokens: tokens}
	err := source.Fetch(
		t.Context(), "freeasinbird/gh-imgup", 1278475858, testCommit,
		t.TempDir()+"/repository.git",
	)
	if err == nil {
		t.Fatal("authenticated clone failure succeeded")
	}
	basic := base64.StdEncoding.EncodeToString(
		[]byte("x-access-token:private-repository-token"),
	)
	if strings.Contains(err.Error(), "private-repository-token") ||
		strings.Contains(err.Error(), basic) {
		t.Fatalf("authenticated clone error leaked credential: %v", err)
	}
	if tokens.calls != 1 || len(runner.specs) != 1 {
		t.Fatalf("token calls = %d, clone calls = %d; want 1 each",
			tokens.calls, len(runner.specs))
	}
	spec := runner.specs[0]
	if strings.Contains(strings.Join(spec.Args, "\x00"), "private-repository-token") ||
		!slices.Contains(spec.Args, "http.followRedirects=false") ||
		!slices.Contains(spec.Env, "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic) {
		t.Fatalf("authenticated clone invocation = args %q env %q", spec.Args, spec.Env)
	}
}

func TestGitFetchRejectsMismatchedOrWriteTokenBeforeClone(t *testing.T) {
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
