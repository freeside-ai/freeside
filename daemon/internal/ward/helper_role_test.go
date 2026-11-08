package ward

import (
	"context"
	"errors"
	"slices"
	"testing"
)

const helperRoleTestName = "helper-role-test"

// newHelperRoleTest returns a runner over a fresh fake runtime and a
// complete state-observer role whose optional steps record themselves in the
// fake's call list.
func newHelperRoleTest(t *testing.T) (*handoffFixture, runtimeOps, helperRole) {
	t.Helper()
	fx := newHandoffFixture(t)
	owner := testOwnershipLabel()
	spec := buildStateObserverSpec(
		fx.cfg, "helper-role-run", testHandoffSpec().Size,
		helperRoleTestName, "helper-role-volume", stateManifestEmpty, owner,
	)
	mark := func(step string) {
		fx.rt.mu.Lock()
		defer fx.rt.mu.Unlock()
		fx.rt.record("%s", step)
	}
	role := helperRole{
		spec: spec, label: "test role", claim: &objectClaim{}, owner: owner,
		check: CheckControlPlaneIsolation, class: LaunchWriter, timeout: fx.cfg.SeedTimeout,
		verify: func(rep InspectReport) error {
			return verifySeedRoleAllowlist(
				rep, spec, "helper-role-volume", stateObserverVolumeTarget, CheckControlPlaneIsolation,
			)
		},
		afterStart: func(context.Context) error { mark("after-start"); return nil },
		readProof:  func(context.Context) ([]byte, error) { mark("read-proof"); return []byte("proof"), nil },
		checkProof: func([]byte) error { mark("check-proof"); return nil },
	}
	return fx, newRuntimeOps(fx.rt, fx.cfg), role
}

func TestHelperRoleRunsTheWholeSequenceInOrder(t *testing.T) {
	t.Parallel()
	fx, ops, role := newHelperRoleTest(t)
	if err := ops.runHelperRole(t.Context(), role); err != nil {
		t.Fatalf("runHelperRole = %v, want success", err)
	}
	n := helperRoleTestName
	want := []string{
		"create-container " + n, "inspect " + n, "start-container " + n, "after-start",
		"inspect " + n, "inspect " + n, "read-proof", "check-proof",
		"delete-container " + n, "list-containers",
	}
	if !slices.Equal(fx.rt.calls, want) {
		t.Errorf("calls = %q, want %q", fx.rt.calls, want)
	}
	if *role.claim != (objectClaim{}) {
		t.Errorf("claim = %+v after success, want cleared", *role.claim)
	}
	fx.assertReaped(t)
}

// TestHelperRoleFailureLeavesTheClaimForTeardown fails each step and checks
// the claim the runner leaves, which teardown reads to reap the container,
// and the error's class: a runtime step's failure is a ConformanceFailure
// carrying the cause, a role-supplied step's error passes through, and
// neither carries content evidence.
func TestHelperRoleFailureLeavesTheClaimForTeardown(t *testing.T) {
	t.Parallel()
	injected := errors.New("injected")
	for _, tc := range []struct {
		step        string
		owned       bool
		fingerprint bool
		runtime     bool // a runtime step, wrapped as a ConformanceFailure
		started     bool // start-container was called
	}{
		{step: "create", runtime: true},
		{step: "inspect", owned: true, runtime: true},
		{step: "verify", owned: true},
		{step: "fingerprint", owned: true, runtime: true},
		{step: "start", owned: true, fingerprint: true, runtime: true, started: true},
		{step: "after-start", owned: true, fingerprint: true, started: true},
		{step: "stopped", owned: true, fingerprint: true, runtime: true, started: true},
		{step: "read-proof", owned: true, fingerprint: true, started: true},
		{step: "check-proof", owned: true, fingerprint: true, started: true},
		{step: "delete", owned: true, fingerprint: true, runtime: true, started: true},
		{step: "absent", owned: true, fingerprint: true, runtime: true, started: true},
	} {
		t.Run(tc.step, func(t *testing.T) {
			t.Parallel()
			fx, ops, role := newHelperRoleTest(t)
			inspects := 0
			switch tc.step {
			case "create":
				fx.rt.onCreateContainer = func(ContainerSpec) error { return injected }
			case "inspect", "stopped":
				fx.rt.onInspect = func(_ string, rep InspectReport) (InspectReport, error) {
					// The first inspect is the pre-start one; the second is
					// the stopped wait's first poll.
					inspects++
					if (tc.step == "inspect" && inspects == 1) || (tc.step == "stopped" && inspects == 2) {
						return InspectReport{}, injected
					}
					return rep, nil
				}
			case "verify":
				role.verify = func(InspectReport) error { return injected }
			case "fingerprint":
				// The spec carries the fixture's ownership label, so the
				// inspected report cannot show this one.
				role.owner = Label{Key: ownershipLabelKey, Value: "another-invocation"}
			case "start":
				fx.rt.onStart = func(string) error { return injected }
			case "after-start":
				role.afterStart = func(context.Context) error { return injected }
			case "read-proof":
				role.readProof = func(context.Context) ([]byte, error) { return nil, injected }
			case "check-proof":
				role.checkProof = func([]byte) error { return injected }
			case "delete":
				fx.rt.onDeleteContainer = func(string) (bool, error) { return false, injected }
			case "absent":
				fx.rt.onListContainers = func([]ContainerSummary) ([]ContainerSummary, error) { return nil, injected }
			}
			err := ops.runHelperRole(t.Context(), role)
			if errors.Is(err, errContentEvidence) {
				t.Errorf("error %v carries content evidence", err)
			}
			var cf *ConformanceFailure
			switch {
			case tc.runtime:
				if !errors.As(err, &cf) || cf.Check != CheckControlPlaneIsolation ||
					(tc.step != "fingerprint" && !errors.Is(cf.Err, injected)) {
					t.Errorf("error = %v, want a %s ConformanceFailure caused by the injected error",
						err, CheckControlPlaneIsolation)
				}
			case !errors.Is(err, injected) || errors.As(err, &cf):
				t.Errorf("error = %v, want the role's own error unwrapped", err)
			}
			if !role.claim.attempted || role.claim.owned != tc.owned ||
				(role.claim.fingerprint != "") != tc.fingerprint {
				t.Errorf("claim = %+v, want attempted, owned=%t, fingerprint=%t",
					*role.claim, tc.owned, tc.fingerprint)
			}
			if started := fx.rt.callIndex("start-container "+helperRoleTestName) >= 0; started != tc.started {
				t.Errorf("start called = %t, want %t: %q", started, tc.started, fx.rt.calls)
			}
		})
	}
}

func TestHelperRoleRefusesAnIncompleteRoleBeforeAnyRuntimeCall(t *testing.T) {
	t.Parallel()
	for name, breakRole := range map[string]func(*helperRole){
		"no allowlist check": func(r *helperRole) { r.verify = nil },
		"no claim":           func(r *helperRole) { r.claim = nil },
		"no timeout":         func(r *helperRole) { r.timeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx, ops, role := newHelperRoleTest(t)
			breakRole(&role)
			if err := ops.runHelperRole(t.Context(), role); err == nil {
				t.Fatal("runHelperRole accepted an incomplete role")
			}
			if len(fx.rt.calls) != 0 {
				t.Errorf("incomplete role reached the runtime: %q", fx.rt.calls)
			}
			if role.claim != nil && role.claim.attempted {
				t.Error("incomplete role marked its claim attempted")
			}
		})
	}
}

func TestHelperRolePassesTheMemoryLimitErrorThrough(t *testing.T) {
	t.Parallel()
	fx, ops, role := newHelperRoleTest(t)
	fx.rt.bootLogs = map[string][]byte{helperRoleTestName: bootLogKilling(t, helperRoleTestName)}
	err := ops.runHelperRole(t.Context(), role)
	var limit *MemoryLimitError
	if !errors.As(err, &limit) || limit.Class != LaunchWriter || limit.Size != role.spec.Size {
		t.Fatalf("error = %v, want the helper's MemoryLimitError", err)
	}
	if errors.Is(err, ErrConformance) {
		t.Errorf("memory-limit error was wrapped as a conformance failure: %v", err)
	}
	if fx.rt.callIndex("read-proof") >= 0 || fx.rt.callIndex("delete-container "+helperRoleTestName) >= 0 {
		t.Errorf("a memory-limit kill still read the proof or deleted: %q", fx.rt.calls)
	}
}
