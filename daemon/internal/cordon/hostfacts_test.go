package cordon

import (
	"errors"
	"os"
	"strings"
	"testing"
	"testing/synctest"
)

// TestApprovalsRefuseLaunchesAndPasses shows the backend refuses a launch,
// and the suite's preconditions fail, while the launching program holds
// either approval or the approvals can't be read, before the launch claims
// anything; a program holding neither launches.
func TestApprovalsRefuseLaunchesAndPasses(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		facts  HostFacts
		err    error
		reason string
	}{
		"neither":          {facts: HostFacts{DesktopSession: true}},
		"screen recording": {facts: HostFacts{ScreenRecording: true}, reason: "holds the screen recording approval"},
		"accessibility":    {facts: HostFacts{Accessibility: true}, reason: "holds the accessibility approval"},
		"both": {
			facts:  HostFacts{ScreenRecording: true, Accessibility: true},
			reason: "holds the screen recording and accessibility approval",
		},
		"unreadable": {err: errors.New("tccd went away"), reason: "tccd went away"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			pinnedCLI(t, &cfg)
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{host: newFakeHost(), script: func(_ processSpec, p *fakeProcess) { p.exit(0) }}
				b := fakeBackend(t, cfg, l)
				b.facts = func() (HostFacts, error) { return tc.facts, tc.err }
				s := testSuite(t, b)
				_, launchErr := b.Launch(t.Context(), request("run-1"))
				passErr := s.preconditions()
				if tc.reason == "" {
					if launchErr != nil || passErr != nil {
						t.Fatalf("Launch = %v, preconditions = %v, want both to pass", launchErr, passErr)
					}
					return
				}
				for what, err := range map[string]error{"Launch": launchErr, "preconditions": passErr} {
					var f *ConformanceFailure
					if !errors.As(err, &f) || f.Check != checkApprovals || !strings.Contains(f.Reason, tc.reason) {
						t.Errorf("%s = %v, want the %q check naming %q", what, err, checkApprovals, tc.reason)
					}
				}
				if _, err := os.Lstat(layoutFor(cfg.RunsRoot, "run-1").dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a refused launch claimed its directory: %v", err)
				}
				if len(l.specs) != 0 {
					t.Fatalf("a refused launch started %d processes", len(l.specs))
				}
			})
		})
	}
}
