package projectimage

import (
	"os"
	"slices"
	"strings"
	"testing"
)

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
	if err := source.Fetch(t.Context(), "freeasinbird/gh-imgup", testCommit,
		t.TempDir()+"/repository.git"); err != nil {
		t.Fatal(err)
	}
	if len(runner.specs) != 1 {
		t.Fatalf("clone invocations = %d, want 1", len(runner.specs))
	}
	spec := runner.specs[0]
	wantConfig := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	}
	if !slices.Equal(spec.Args[:len(wantConfig)], wantConfig) {
		t.Fatalf("clone config = %q, want %q", spec.Args[:len(wantConfig)], wantConfig)
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
