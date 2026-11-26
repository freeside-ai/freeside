package cordon

import (
	"errors"
	"strings"
	"testing"
)

func reportSpec() probeSpec {
	return probeSpec{
		Reads:       []probeRead{{Path: "/home/.ssh", Present: true}},
		Ports:       []int{47100},
		Denied:      []string{"job-creation"},
		Preferences: []string{"com.apple.security"},
		StandIn:     &probeStandIn{Pid: 4242, Start: 17},
		Detach:      true,
	}
}

const passingReport = "ok read /home/.ssh\n  (refused as expected: operation not permitted)\n" +
	"ok bind 47100\nok denied job-creation\nok preference-write com.apple.security\n" +
	"ok signal 4242\ndetached 5151 99\n"

func TestCheckProbeReportAcceptsAnExactPassingReport(t *testing.T) {
	t.Parallel()
	detached, err := checkProbeReport(reportSpec(), []byte(passingReport), 0)
	if err != nil {
		t.Fatalf("checkProbeReport: %v", err)
	}
	if detached == nil || *detached != (probeStandIn{Pid: 5151, Start: 99}) {
		t.Fatalf("detached = %+v", detached)
	}
}

func TestCheckProbeReportFailsClosed(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		report    string
		exit      int
		wantCheck string
	}{
		"a failed check": {
			report:    strings.Replace(passingReport, "ok bind 47100", "FAIL bind 47100: succeeded", 1),
			exit:      1,
			wantCheck: "bind 47100",
		},
		"a missing check": {
			report:    strings.Replace(passingReport, "ok denied job-creation\n", "", 1),
			wantCheck: "denied job-creation",
		},
		"a repeated check": {
			report:    passingReport + "ok bind 47100\n",
			wantCheck: "bind 47100",
		},
		"an unknown check":    {report: passingReport + "ok read /elsewhere\n", wantCheck: "probe report"},
		"an unknown FAIL":     {report: passingReport + "FAIL detach: no\n", wantCheck: "probe report"},
		"a stray line":        {report: "hello\n" + passingReport, wantCheck: "probe report"},
		"a second detach":     {report: passingReport + "detached 5152 99\n", wantCheck: "probe report"},
		"a malformed detach":  {report: strings.Replace(passingReport, "detached 5151 99", "detached 05151 99", 1), wantCheck: "probe report"},
		"no detached process": {report: strings.Replace(passingReport, "detached 5151 99\n", "", 1), wantCheck: checkDetached},
		"a nonzero exit":      {report: passingReport, exit: 1, wantCheck: "probe report"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := checkProbeReport(reportSpec(), []byte(c.report), c.exit)
			var f *ConformanceFailure
			if !errors.As(err, &f) || !errors.Is(err, ErrConformance) || f.Check != c.wantCheck {
				t.Fatalf("checkProbeReport = %v, want a failure of %q", err, c.wantCheck)
			}
		})
	}
}

func TestCheckProbeReportRefusesAnEmptySpec(t *testing.T) {
	t.Parallel()
	if _, err := checkProbeReport(probeSpec{}, nil, 0); !errors.Is(err, ErrConformance) {
		t.Fatalf("checkProbeReport = %v", err)
	}
}

func TestRunProbeRefusesBadUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"{}", "{}"},
		{"not json"},
		{`{"reads":[{"path":"relative"}]}`},
		{`{"unknown":1}`},
		{`{"commands":[{"name":"x","argv":["launchctl"]}]}`},
		{`{"preferences":[""]}`},
		{`{} {}`},
	} {
		var out strings.Builder
		if code := RunProbe(args, &out); code != 2 || !strings.HasPrefix(out.String(), "usage:") {
			t.Errorf("RunProbe(%q) = %d, %q", args, code, out.String())
		}
	}
}
