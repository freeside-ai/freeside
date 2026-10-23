package claudeinference

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// probeScript answers --version without a credential, and otherwise checks
// that the call is the judgment launch with the probe's fixed conversation
// before printing body and exiting with status.
func probeScript(body, status string) string {
	return `if [ "$1" = --version ]; then
  test -z "$CLAUDE_CODE_OAUTH_TOKEN" || exit 9
  echo '2.1.267 (Claude Code)'
  exit 0
fi
test "$CLAUDE_CODE_OAUTH_TOKEN" = 'synthetic-credential' || exit 2
case "$*" in *synthetic-credential*) exit 3;; esac
case "$*" in *"--model probe-model --effort high --safe-mode --tools  "*"--max-turns 1 --system-prompt ` + ProbePrompt + `") ;; *) exit 4;; esac
test "$(cat)" = '` + ProbeInput + `' || exit 5
printf '%s' '` + body + `'
echo 'stderr line' >&2
exit ` + status + "\n"
}

func TestProbeRunsTheJudgmentLaunchAndReturnsItsOutput(t *testing.T) {
	d := testDriver(t, probeScript(`{"type":"result","is_error":false}`, "0"))
	out, err := d.Probe(t.Context(), "probe-model", "high", inference.Secret("synthetic-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Version) != "2.1.267 (Claude Code)\n" || string(out.Stdout) != `{"type":"result","is_error":false}` ||
		string(out.Stderr) != "stderr line\n" || out.ExitStatus != 0 {
		t.Fatalf("output = %+v", out)
	}
}

func TestProbeReturnsARefusalAsOutput(t *testing.T) {
	refusal := `{"type":"result","is_error":true,"result":"version 2.1.251 or newer is required"}`
	d := testDriver(t, probeScript(refusal, "1"))
	out, err := d.Probe(t.Context(), "probe-model", "high", inference.Secret("synthetic-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitStatus != 1 || string(out.Stdout) != refusal {
		t.Fatalf("output = %+v", out)
	}
}

func TestProbeRefusesWhatItCannotRun(t *testing.T) {
	d := testDriver(t, probeScript(`{}`, "0"))
	for name, call := range map[string]func() error{
		"no model": func() error {
			_, err := d.Probe(t.Context(), "", "high", inference.Secret("synthetic-credential"))
			return err
		},
		"option-shaped model": func() error {
			_, err := d.Probe(t.Context(), "--debug", "high", inference.Secret("synthetic-credential"))
			return err
		},
		"spaced effort": func() error {
			_, err := d.Probe(t.Context(), "probe-model", "high low", inference.Secret("synthetic-credential"))
			return err
		},
		"no credential": func() error {
			_, err := d.Probe(t.Context(), "probe-model", "high", inference.Secret(""))
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: probe ran", name)
		}
	}

	failingVersion := testDriver(t, "exit 1\n")
	if _, err := failingVersion.Probe(t.Context(), "probe-model", "high", inference.Secret("synthetic-credential")); err == nil ||
		!strings.Contains(err.Error(), "--version") {
		t.Fatalf("failed --version: err = %v", err)
	}

	execfixture.Write(t, d.config.Binary, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	if _, err := d.Probe(t.Context(), "probe-model", "high", inference.Secret("synthetic-credential")); err == nil {
		t.Fatal("probe ran a changed CLI")
	}
}

func TestProbeBoundsItsOutput(t *testing.T) {
	d := testDriver(t, `if [ "$1" = --version ]; then echo v; exit 0; fi
head -c 70000 /dev/zero
`)
	if _, err := d.Probe(t.Context(), "probe-model", "", inference.Secret("synthetic-credential")); err == nil {
		t.Fatal("probe accepted output past its bound")
	}
}
