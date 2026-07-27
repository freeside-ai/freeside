package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func validArgs() []string {
	return []string{
		"-db", "/tmp/project-image.db",
		"-repository", "freeasinbird/gh-imgup",
		"-repository-id", "1278475858",
		"-commit", "6ab4e3dff2be53f74bde9b8b3150290775152f9f",
		"-recipe", "/tmp/recipe.json",
		"-base-image", "example.test/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"-base-build-ref", "base:local",
		"-local-registry-port", "5100",
	}
}

func TestParseConfig(t *testing.T) {
	args := append(validArgs(), "-dns", "1.1.1.1", "-dns", "8.8.8.8")
	cfg, err := parseConfig(args, new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RepositoryID != 1278475858 || cfg.LocalRegistryPort != 5100 ||
		len(cfg.DNS) != 2 || cfg.DNS[1] != "8.8.8.8" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestImageCheckerFailsWhenCleanupInspectionFails(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required by the image checker")
	}
	stateDir := t.TempDir()
	fakeContainer := filepath.Join(t.TempDir(), "container")
	fake := `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
create)
	shift
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--cidfile) cidfile="$2"; shift 2 ;;
		--label) token="${2#*=}"; shift 2 ;;
		*) shift ;;
		esac
	done
	printf '%s\n' "$token" >"${FAKE_STATE}/token"
	printf '0\n' >"${FAKE_STATE}/inspects"
	printf 'probe-id\n' >"$cidfile"
	;;
inspect)
	count=$(cat "${FAKE_STATE}/inspects")
	count=$((count + 1))
	printf '%s\n' "$count" >"${FAKE_STATE}/inspects"
	if [ "$count" -gt 1 ]; then
		exit 17
	fi
	token=$(cat "${FAKE_STATE}/token")
	printf '[{"id":"probe-id","configuration":{"id":"probe-id","labels":{"ai.freeside.project-image.owner":"%s"},"image":{"reference":"project:test"},"initProcess":{"environment":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],"workingDirectory":"/","executable":"sh","arguments":["-c","true"]},"ssh":false,"publishedPorts":[],"publishedSockets":[],"networks":[],"mounts":[]}}]\n' "$token"
	;;
delete)
	touch "${FAKE_STATE}/deleted"
	;;
*)
	exit 2
	;;
esac
`
	if err := os.WriteFile(fakeContainer, []byte(fake), 0o700); err != nil { //nolint:gosec // executable test fixture contains only fixed script text
		t.Fatal(err)
	}
	checker := filepath.Join("..", "..", "..", "scripts", "check-agent-image.sh")
	command := exec.CommandContext(t.Context(), "bash", checker, "project:test", fakeContainer) //nolint:gosec // fixed shell and checker with a test-owned fake runtime path
	command.Env = append(os.Environ(), "FAKE_STATE="+stateDir, "TMPDIR="+t.TempDir())
	output, err := command.CombinedOutput()
	if err == nil ||
		!strings.Contains(string(output), "could not inspect probe container probe-id for cleanup") {
		t.Fatalf("checker = %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "deleted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checker deleted an unverifiable probe: %v", err)
	}
}

func TestParseConfigRejectsMissingInputsAndPositionals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing db", validArgs()[2:], "-db is required"},
		{"missing repository id", append(validArgs()[:4], validArgs()[6:]...), "-repository-id must be positive"},
		{"positional", append(validArgs(), "extra"), "unexpected positional"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig(tc.args, new(bytes.Buffer))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseConfig = %v, want %q", err, tc.want)
			}
		})
	}
}
