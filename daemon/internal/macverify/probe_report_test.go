package macverify

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func reportSpec() probeSpec {
	return sandboxProbe(sandboxTargets{
		loopbackPort: "4242", resolverHost: "github.com", tcp: "192.0.2.1:443", udp: "192.0.2.1:53",
		ports: slotPortSet{47000, 47001, 47002, 47003}, foreignPort: 47004,
		outsideDirs: []string{"/private/tmp", "/Users/operator"},
	})
}

func TestCheckProbeReportAcceptsAnExactPassingReport(t *testing.T) {
	t.Parallel()
	spec := reportSpec()
	res := verify.StepResult{Output: []byte(passingReport(spec))}
	if err := checkProbeReport("slot1", spec, res); err != nil {
		t.Fatalf("checkProbeReport: %v", err)
	}
}

// TestCheckProbeReportFailsClosed is the tamper suite for the report parser:
// every way a report can fall short of proving each check exactly once, with
// exit 0, fails as a ConformanceFailure naming a confinement and the slot.
func TestCheckProbeReportFailsClosed(t *testing.T) {
	t.Parallel()
	spec := reportSpec()
	passing := passingReport(spec)
	for name, tc := range map[string]struct {
		output    string
		exit      int
		truncated bool
		want      Confinement
	}{
		"missing check": {
			output: strings.Replace(passing, "ok write-denied /Users/operator\n", "", 1),
			want:   ConfinementWriteConfinement,
		},
		"duplicated check": {
			output: passing + "ok " + checkTCPRefused + "\n",
			want:   ConfinementSandboxNetwork,
		},
		"unknown ok line": {
			output: passing + "ok write-denied /\n",
			want:   ConfinementSandboxNetwork,
		},
		"unknown free text": {
			output: passing + "everything is fine\n",
			want:   ConfinementSandboxNetwork,
		},
		"unindented detail": {
			output: "(refused as expected)\n" + passing,
			want:   ConfinementSandboxNetwork,
		},
		"unknown FAIL line": {
			output: passing + "FAIL something-else: no\n",
			want:   ConfinementSandboxNetwork,
		},
		"FAIL line with exit 0": {
			output: failingReport(spec, "write-denied /private/tmp"),
			want:   ConfinementWriteConfinement,
		},
		"FAIL line naming its own confinement": {
			output: failingReport(spec, checkUDPRefused),
			exit:   1,
			want:   ConfinementSandboxNetwork,
		},
		"ok report with a nonzero exit": {
			output: passing,
			exit:   1,
			want:   ConfinementSandboxNetwork,
		},
		"ok report killed by a signal": {
			output: passing,
			exit:   -1,
			want:   ConfinementSandboxNetwork,
		},
		"truncated report": {
			output:    passing,
			truncated: true,
			want:      ConfinementSandboxNetwork,
		},
		"empty report": {
			want: ConfinementSandboxNetwork,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := verify.StepResult{Output: []byte(tc.output), ExitCode: tc.exit, Truncated: tc.truncated}
			err := checkProbeReport("slot1", spec, res)
			var failure *ConformanceFailure
			if !errors.As(err, &failure) || !errors.Is(err, ErrConformance) {
				t.Fatalf("checkProbeReport = %v, want a ConformanceFailure", err)
			}
			if failure.Confinement != tc.want || failure.Slot != "slot1" {
				t.Fatalf("failure = %+v, want confinement %s on slot1", failure, tc.want)
			}
			if !strings.Contains(err.Error(), string(tc.want)) || !strings.Contains(err.Error(), "slot1") {
				t.Fatalf("error %q does not name the confinement and slot", err)
			}
		})
	}
}

func TestProbeSpecsNameEachConfinement(t *testing.T) {
	t.Parallel()
	covered := map[Confinement]bool{}
	for _, spec := range []probeSpec{reportSpec(), packetFilterProbe("a", "b"), homeMarkProbe(), homeCleanProbe()} {
		for _, c := range spec.checks {
			if !c.confinement.valid() {
				t.Fatalf("check %s has invalid confinement %q", c.name, c.confinement)
			}
			covered[c.confinement] = true
		}
	}
	for _, c := range AllConfinements {
		if !covered[c] {
			t.Errorf("no probe check proves %s", c)
		}
	}
	if Confinement("").valid() {
		t.Error("the zero Confinement is valid")
	}
}
