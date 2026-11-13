package macverify

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// The probe's modes (RunProbe). Each is one job on a slot.
const (
	probeModeSandbox      = "sandbox"
	probeModePacketFilter = "packet-filter"
	probeModeHomeMark     = "home-mark"
	probeModeHomeClean    = "home-clean"
)

// The probe's check names. RunProbe prints them and checkProbeReport
// requires them, so the two cannot drift.
const (
	checkLoopbackRefused       = "loopback-refused"
	checkForeignBindRefused    = "foreign-bind-refused"
	checkForeignConnectRefused = "foreign-connect-refused"
	checkResolverEmpty         = "resolver-empty"
	checkTCPRefused            = "tcp-refused"
	checkUDPRefused            = "udp-refused"
	checkWriteJobDir           = "write-job-dir"
	checkTCPDropped            = "tcp-dropped"
	checkUDPDropped            = "udp-dropped"
	checkHomeMarked            = "home-marked"
	checkHomeReset             = "home-reset"
)

func checkWriteDenied(dir string) string { return "write-denied " + dir }

func checkReservedPort(port int) string { return "reserved-port " + strconv.Itoa(port) }

// probeExecutable is the probe's file name in the probe workspace, and
// probeCommand the arguments before the mode: the daemon's own hidden verb,
// which the live suite's test binary answers too (TestMain).
const probeExecutable = "probe"

var probeCommand = []string{"./" + probeExecutable, "macverify", "probe"}

// permissiveProfile turns the sandbox off, so the packet-filter mode sees the
// anchor alone.
var permissiveProfile = []byte("(version 1)\n(allow default)\n")

type probeCheck struct {
	name        string
	confinement Confinement
}

// probeSpec is one probe job: its mode and arguments, and the checks its
// report must prove.
type probeSpec struct {
	mode   string
	args   []string
	checks []probeCheck
}

func (s probeSpec) argv() []string {
	argv := append([]string{}, probeCommand...)
	argv = append(argv, s.mode)
	return append(argv, s.args...)
}

// sandboxTargets are what the sandbox mode tries to reach and write. Off-host
// targets are host:port literals the probe dials without resolving. ports are
// the slot's reserved ports, which the probe must be able to use, and
// foreignPort one outside them, which it must not.
type sandboxTargets struct {
	loopbackPort string
	resolverHost string
	tcp          string
	udp          string
	ports        slotPortSet
	foreignPort  int
	outsideDirs  []string
}

func sandboxProbe(t sandboxTargets) probeSpec {
	args := append([]string{
		t.loopbackPort, t.resolverHost, t.tcp, t.udp, t.ports.String(), strconv.Itoa(t.foreignPort),
	}, t.outsideDirs...)
	checks := []probeCheck{
		{checkLoopbackRefused, ConfinementSandboxNetwork},
	}
	for _, port := range t.ports {
		checks = append(checks, probeCheck{checkReservedPort(port), ConfinementSandboxNetwork})
	}
	checks = append(checks,
		probeCheck{checkForeignBindRefused, ConfinementSandboxNetwork},
		probeCheck{checkForeignConnectRefused, ConfinementSandboxNetwork},
		probeCheck{checkResolverEmpty, ConfinementSandboxNetwork},
		probeCheck{checkTCPRefused, ConfinementSandboxNetwork},
		probeCheck{checkUDPRefused, ConfinementSandboxNetwork},
	)
	for _, dir := range t.outsideDirs {
		checks = append(checks, probeCheck{checkWriteDenied(dir), ConfinementWriteConfinement})
	}
	checks = append(checks, probeCheck{checkWriteJobDir, ConfinementWriteConfinement})
	return probeSpec{mode: probeModeSandbox, args: args, checks: checks}
}

func packetFilterProbe(tcp, udp string) probeSpec {
	return probeSpec{mode: probeModePacketFilter, args: []string{tcp, udp}, checks: []probeCheck{
		{checkTCPDropped, ConfinementPacketFilter},
		{checkUDPDropped, ConfinementPacketFilter},
	}}
}

func homeMarkProbe() probeSpec {
	return probeSpec{mode: probeModeHomeMark, checks: []probeCheck{{checkHomeMarked, ConfinementHomeReset}}}
}

func homeCleanProbe() probeSpec {
	return probeSpec{mode: probeModeHomeClean, checks: []probeCheck{{checkHomeReset, ConfinementHomeReset}}}
}

// checkProbeReport passes only a report that proves every check of spec: the
// probe exited 0, printed "ok <name>" exactly once for each expected check,
// and printed nothing else but indented "  (…)" detail lines. Anything else
// fails closed as a ConformanceFailure: a FAIL line names its own check's
// confinement, and any other defect names the first confinement the report
// leaves unproven.
func checkProbeReport(slot string, spec probeSpec, res verify.StepResult) error {
	if len(spec.checks) == 0 {
		return fmt.Errorf("probe mode %s has no checks", spec.mode)
	}
	fail := func(c probeCheck, format string, args ...any) error {
		return &ConformanceFailure{
			Confinement: c.confinement, Slot: slot,
			Reason: fmt.Sprintf("%s probe: ", spec.mode) + fmt.Sprintf(format, args...),
		}
	}
	first := spec.checks[0]
	if res.Truncated {
		return fail(first, "report truncated")
	}
	byName := make(map[string]probeCheck, len(spec.checks))
	for _, c := range spec.checks {
		byName[c.name] = c
	}
	seen := make(map[string]int, len(spec.checks))
	for line := range strings.Lines(string(res.Output)) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "  (") && strings.HasSuffix(line, ")") {
			continue
		}
		if name, ok := strings.CutPrefix(line, "ok "); ok {
			if _, known := byName[name]; !known {
				return fail(first, "unknown report line %q", line)
			}
			seen[name]++
			continue
		}
		if rest, ok := strings.CutPrefix(line, "FAIL "); ok {
			name, detail, _ := strings.Cut(rest, ": ")
			c, known := byName[name]
			if !known {
				return fail(first, "unknown report line %q", line)
			}
			return fail(c, "%s: %s", name, detail)
		}
		return fail(first, "unknown report line %q", line)
	}
	for _, c := range spec.checks {
		if seen[c.name] != 1 {
			return fail(c, "report proves %s %d times, want once", c.name, seen[c.name])
		}
	}
	if res.ExitCode != 0 {
		return fail(first, "probe exited %d with every check ok", res.ExitCode)
	}
	return nil
}
