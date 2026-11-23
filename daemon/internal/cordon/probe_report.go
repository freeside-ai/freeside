package cordon

import (
	"strconv"
	"strings"
)

// checkProbeReport passes only a report that proves every check of spec:
// the probe exited 0, printed "ok <check>" exactly once for each expected
// check, printed exactly one "detached <pid> <start>" line when the spec
// asked for one, and printed nothing else but indented "  (…)" detail
// lines. Anything else fails closed as a ConformanceFailure naming the
// failed check, or "probe report" for a defect no check owns. It returns
// the detached process, for the suite to look for after the launch ends.
func checkProbeReport(spec probeSpec, output []byte, exitCode int) (*probeStandIn, error) {
	const report = "probe report"
	expected := spec.checks()
	if len(expected) == 0 && !spec.Detach {
		return nil, failure(report, "the spec has no checks")
	}
	known := make(map[string]bool, len(expected))
	for _, c := range expected {
		known[c] = true
	}
	seen := make(map[string]int, len(expected))
	var detached *probeStandIn
	for line := range strings.Lines(string(output)) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "  (") && strings.HasSuffix(line, ")") {
			continue
		}
		if name, ok := strings.CutPrefix(line, "ok "); ok && known[name] {
			seen[name]++
			continue
		}
		if rest, ok := strings.CutPrefix(line, "FAIL "); ok {
			if name, detail, _ := strings.Cut(rest, ": "); known[name] {
				return nil, failure(name, "%s", detail)
			}
		}
		if rest, ok := strings.CutPrefix(line, "detached "); ok && spec.Detach && detached == nil {
			if d, ok := parseDetached(rest); ok {
				detached = &d
				continue
			}
		}
		return nil, failure(report, "unexpected line %q", line)
	}
	for _, c := range expected {
		if seen[c] != 1 {
			return nil, failure(c, "the report proves it %d times, want once", seen[c])
		}
	}
	if spec.Detach && detached == nil {
		return nil, failure(checkDetached, "the probe named no detached process")
	}
	if exitCode != 0 {
		return nil, failure(report, "the probe exited %d with every check ok", exitCode)
	}
	return detached, nil
}

// checkDetached is the run-end proof's check: the probe's detached process
// is gone once the launch ends.
const checkDetached = "detached process ended"

func parseDetached(rest string) (probeStandIn, bool) {
	pidText, startText, ok := strings.Cut(rest, " ")
	if !ok {
		return probeStandIn{}, false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 1 || strconv.Itoa(pid) != pidText {
		return probeStandIn{}, false
	}
	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || start <= 0 || strconv.FormatInt(start, 10) != startText {
		return probeStandIn{}, false
	}
	return probeStandIn{Pid: pid, Start: start}, true
}
