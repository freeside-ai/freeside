package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestCallLaunchGolden(t *testing.T) {
	launch, err := domain.NewCallLaunch()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(launch, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "call_launch", append(body, '\n'))
}

// A call launch has no knobs: each clause the plan fixes is refused when it
// is stated any other way, even under a digest that matches the altered body.
func TestCallLaunchRefusesEveryOtherLaunch(t *testing.T) {
	for name, alter := range map[string]func(*domain.CallLaunch){
		"tools":            func(l *domain.CallLaunch) { l.Tools = true },
		"workspace":        func(l *domain.CallLaunch) { l.Workspace = true },
		"second turn":      func(l *domain.CallLaunch) { l.Turns = 2 },
		"free-form output": func(l *domain.CallLaunch) { l.StructuredOutput = false },
		"no severance":     func(l *domain.CallLaunch) { l.Severance = false },
		"saved session":    func(l *domain.CallLaunch) { l.SavedSession = true },
	} {
		t.Run(name, func(t *testing.T) {
			launch, err := domain.NewCallLaunch()
			if err != nil {
				t.Fatal(err)
			}
			alter(&launch)
			if launch.Digest, err = launch.ComputeDigest(); err != nil {
				t.Fatal(err)
			}
			if err := launch.Validate(); !errors.Is(err, domain.ErrInvalidCallLaunch) {
				t.Fatalf("Validate() = %v, want ErrInvalidCallLaunch", err)
			}
		})
	}
	launch, err := domain.NewCallLaunch()
	if err != nil {
		t.Fatal(err)
	}
	launch.Digest = "sha256:"
	if err := launch.Validate(); !errors.Is(err, domain.ErrInvalidDigest) {
		t.Fatalf("Validate() with a malformed digest = %v, want ErrInvalidDigest", err)
	}
	launch.Digest = launchSpec(t).Digest
	if err := launch.Validate(); !errors.Is(err, domain.ErrAgentDigestMismatch) {
		t.Fatalf("Validate() under another launch's digest = %v, want ErrAgentDigestMismatch", err)
	}
}

// A call's treatment digest is computed like a run's with the call launch in
// the launch position (plan §5.4), so the same agent's ward and call
// treatments differ by that position alone.
func TestCallTreatmentDiffersFromWardTreatmentByLaunchOnly(t *testing.T) {
	route, adapter, offer := routeFragment(t), adapterFragment(t), offerFragment(t)
	call, err := domain.NewCallLaunch()
	if err != nil {
		t.Fatal(err)
	}
	ward := launchSpec(t)
	digest := func(launch domain.Digest) domain.Digest {
		t.Helper()
		got, err := domain.ComputeTreatmentDigest(
			route, adapter.Digest, launch, offer, domain.EffortHarnessDefault, string(domain.EffortHarnessDefault))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if digest(call.Digest) == digest(ward.Digest) {
		t.Fatal("call and ward treatments share a digest")
	}
}

func TestCompareLineage(t *testing.T) {
	for _, tc := range []struct {
		judging, writing string
		want             domain.LineageRelation
	}{
		{"anthropic", "anthropic", domain.LineageMatched},
		{"openai", "anthropic", domain.LineageDiffered},
		{"", "anthropic", domain.LineageUnknown},
		{"anthropic", "", domain.LineageUnknown},
		{"", "", domain.LineageUnknown},
	} {
		if got := domain.CompareLineage(tc.judging, tc.writing); got != tc.want {
			t.Errorf("CompareLineage(%q, %q) = %q, want %q", tc.judging, tc.writing, got, tc.want)
		}
	}
	if err := domain.LineageRelation("").Validate(); !errors.Is(err, domain.ErrInvalidLineageRelation) {
		t.Fatalf("zero relation validated: %v", err)
	}
}

// The judging and writing roles are the plan's (§7): the reviewer, the
// finding adjudicator, and the drift auditor judge the implementer's and the
// remediator's work.
func TestJudgingAndWritingRoles(t *testing.T) {
	var judging []domain.RoleName
	for _, role := range domain.AllRoleNames {
		if role.JudgesWrittenWork() {
			judging = append(judging, role)
		}
	}
	want := []domain.RoleName{domain.RoleReviewer, domain.RoleFindingAdjudicator, domain.RoleDriftAuditor}
	if len(judging) != len(want) {
		t.Fatalf("judging roles = %v, want %v", judging, want)
	}
	for _, role := range want {
		if !role.JudgesWrittenWork() {
			t.Errorf("%s is not a judging role", role)
		}
	}
}
