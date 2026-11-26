//go:build darwin && cgo

package cordon

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRunProbeUnconfinedFailsEveryCheck shows the probe can't pass outside
// a launch: every check fails there, so a pass is the rules' doing. A
// refusal for any reason but the sandbox's fails too.
func TestRunProbeUnconfinedFailsEveryCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	standIn := exec.Command("/bin/sleep", "600")
	if err := standIn.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = standIn.Process.Kill(); _ = standIn.Wait() })
	start, err := processStart(standIn.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// check.sh runs the tests under a sandbox of its own, which may deny
	// this process an operation already.
	var ops []string
	if denied, err := sandboxDeniesOperation("job-creation"); err != nil {
		t.Fatal(err)
	} else if !denied {
		ops = append(ops, "job-creation")
	}
	var domains []string
	for _, domain := range deniedPreferenceDomains {
		if denied, err := sandboxDeniesPreference(domain); err != nil {
			t.Fatal(err)
		} else if !denied {
			domains = append(domains, domain)
		}
	}
	spec := probeSpec{
		Reads:       []probeRead{{Path: present, Present: true}, {Path: filepath.Join(dir, "absent")}},
		Creates:     []probeCreate{{Path: filepath.Join(dir, "new")}, {Path: filepath.Join(dir, "newdir"), Dir: true}},
		Sockets:     []string{filepath.Join(dir, "nothing.sock")},
		Unwritable:  []string{present},
		Ports:       []int{freePort(t)},
		Services:    []probeService{{Name: "com.apple.cfprefsd.daemon", Registered: true}},
		Denied:      ops,
		Preferences: domains,
		StandIn:     &probeStandIn{Pid: standIn.Process.Pid, Start: start},
		Commands:    []probeCommand{{Name: "true", Argv: []string{"/usr/bin/true"}}},
	}
	var out strings.Builder
	if code := RunProbe([]string{spec.arg()}, &out); code != 1 {
		t.Fatalf("RunProbe = %d, want 1:\n%s", code, out.String())
	}
	for _, check := range spec.checks() {
		if check == checkRead(filepath.Join(dir, "absent")) {
			// Nothing to read anywhere, which passes; the suite's control
			// is what found it absent.
			continue
		}
		if !strings.Contains(out.String(), "FAIL "+check+": ") {
			t.Errorf("%s did not fail unconfined:\n%s", check, out.String())
		}
	}
	// The exclusive creates removed what they made.
	for _, c := range spec.Creates {
		if _, err := os.Lstat(c.Path); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", c.Path, err)
		}
	}
	if _, err := checkProbeReport(spec, []byte(out.String()), 1); err == nil {
		t.Fatal("an unconfined report passed")
	}
}

// TestRunProbeSignalsOnlyItsStandIn shows a pid whose start time no longer
// matches is never signaled.
func TestRunProbeSignalsOnlyItsStandIn(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	spec := probeSpec{StandIn: &probeStandIn{Pid: os.Getpid(), Start: 1}}
	if code := RunProbe([]string{spec.arg()}, &out); code != 1 ||
		!strings.Contains(out.String(), "is not the suite's stand-in") {
		t.Fatalf("RunProbe = %d:\n%s", code, out.String())
	}
}

// TestProbeFailsWithoutEachPreferenceDomainRule runs the probe under the
// rendered rule file, and under it with each domain's rule removed in
// turn, and shows each domain's check passes only while its own rule
// stands. Under a rule file that denies every domain, the unrelated
// domain's answer fails the checks, since that answer doesn't name a
// domain. The probe only asks the sandbox, so nothing writes a preference.
func TestProbeFailsWithoutEachPreferenceDomainRule(t *testing.T) {
	t.Parallel()
	cfg := newHostFixture(t).cfg
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rules := RenderRules(cfg)
	spec := probeSpec{Preferences: deniedPreferenceDomains}
	// probe runs the probe under ruleFile and returns its report and the
	// report checker's verdict on it.
	probe := func(t *testing.T, name string, ruleFile []byte) (string, error) {
		t.Helper()
		path := filepath.Join(dir, name+".sb")
		if err := os.WriteFile(path, ruleFile, 0o600); err != nil {
			t.Fatal(err)
		}
		argv := append([]string{"-f", path}, sandboxParams(cfg, filepath.Join(dir, "run"), filepath.Join(dir, "tag"))...)
		argv = append(append(argv, self), append(slices.Clone(probeVerb), spec.arg())...)
		cmd := exec.Command(sandboxExec, argv...) //nolint:gosec // G204: this test binary under its own rule file
		var out strings.Builder
		cmd.Stdout = &out
		code := 0
		var exit *exec.ExitError
		if err := cmd.Run(); errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		_, err := checkProbeReport(spec, []byte(out.String()), code)
		return out.String(), err
	}
	if out, err := probe(t, "full", rules); err != nil {
		t.Fatalf("under the rule file: %v\n%s", err, out)
	}
	for _, domain := range deniedPreferenceDomains {
		t.Run(domain, func(t *testing.T) {
			rule := []byte("(deny user-preference-write (preference-domain " + sbString(domain) + "))\n")
			reduced := bytes.Replace(rules, rule, nil, 1)
			if bytes.Equal(reduced, rules) {
				t.Fatalf("the rule file has no %s", rule)
			}
			out, err := probe(t, domain, reduced)
			var f *ConformanceFailure
			if !errors.As(err, &f) || f.Check != checkPreference(domain) {
				t.Fatalf("without its rule: %v, want a failure of %q\n%s", err, checkPreference(domain), out)
			}
			for _, other := range deniedPreferenceDomains {
				if other != domain && !strings.Contains(out, "ok "+checkPreference(other)+"\n") {
					t.Errorf("%s did not pass with its own rule in place:\n%s", other, out)
				}
			}
		})
	}
	everyDomain := append(slices.Clone(rules), "\n(deny user-preference-write)\n"...)
	out, err := probe(t, "every-domain", everyDomain)
	var f *ConformanceFailure
	if !errors.As(err, &f) || f.Check != checkPreference(deniedPreferenceDomains[0]) {
		t.Fatalf("denying every domain: %v, want a failure of %q\n%s", err, checkPreference(deniedPreferenceDomains[0]), out)
	}
	for _, domain := range deniedPreferenceDomains {
		if !strings.Contains(out, "FAIL "+checkPreference(domain)+": the sandbox denies writing "+unrelatedPreferenceDomain) {
			t.Errorf("%s passed though every domain is denied:\n%s", domain, out)
		}
	}
}
