package ward

import (
	"context"
	"errors"
	"fmt"
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

func TestHelperRoleJournalsTheFingerprintBeforeStart(t *testing.T) {
	t.Parallel()
	fx, ops, role := newHelperRoleTest(t)
	var journaled string
	role.journal = func(fingerprint string) error {
		journaled = fingerprint
		fx.rt.mu.Lock()
		defer fx.rt.mu.Unlock()
		fx.rt.record("journal")
		return nil
	}
	if err := ops.runHelperRole(t.Context(), role); err != nil {
		t.Fatalf("runHelperRole = %v, want success", err)
	}
	n := helperRoleTestName
	if journalAt, startAt := fx.rt.callIndex("journal"), fx.rt.callIndex("start-container "+n); journalAt < 0 ||
		journalAt != fx.rt.callIndex("inspect "+n)+1 || startAt != journalAt+1 {
		t.Errorf("journal did not run between the pre-start inspect and the start: %q", fx.rt.calls)
	}
	if journaled == "" {
		t.Error("journal received no fingerprint")
	}
	if *role.claim != (objectClaim{}) {
		t.Errorf("claim = %+v after success, want cleared", *role.claim)
	}
}

func TestHelperRoleJournalFailureStopsBeforeStart(t *testing.T) {
	t.Parallel()
	fx, ops, role := newHelperRoleTest(t)
	injected := errors.New("injected")
	var journaled string
	role.journal = func(fingerprint string) error { journaled = fingerprint; return injected }
	err := ops.runHelperRole(t.Context(), role)
	var cf *ConformanceFailure
	if !errors.Is(err, injected) || errors.As(err, &cf) {
		t.Errorf("error = %v, want the journal's own error unwrapped", err)
	}
	if !role.claim.attempted || !role.claim.owned || role.claim.fingerprint == "" ||
		role.claim.fingerprint != journaled {
		t.Errorf("claim = %+v, want attempted, owned, and the journaled fingerprint %q", *role.claim, journaled)
	}
	if fx.rt.callIndex("start-container "+helperRoleTestName) >= 0 {
		t.Errorf("a failed journal still started the role: %q", fx.rt.calls)
	}
}

// TestHelperRoleRechecksTheStoppedContainer checks that the re-check sees a
// fresh inspection taken after helperStopped and before the proof read,
// and that its refusal stops the role before the proof read and delete.
func TestHelperRoleRechecksTheStoppedContainer(t *testing.T) {
	t.Parallel()
	n := helperRoleTestName
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		fx, ops, role := newHelperRoleTest(t)
		role.recheckStopped = func(rep InspectReport) error {
			if rep.ID != n || rep.State != StateStopped {
				t.Errorf("recheck saw %+v, want the stopped role", rep)
			}
			fx.rt.mu.Lock()
			defer fx.rt.mu.Unlock()
			fx.rt.record("recheck")
			return nil
		}
		if err := ops.runHelperRole(t.Context(), role); err != nil {
			t.Fatalf("runHelperRole = %v, want success", err)
		}
		want := []string{
			"create-container " + n, "inspect " + n, "start-container " + n, "after-start",
			"inspect " + n, "inspect " + n, "inspect " + n, "recheck", "read-proof", "check-proof",
			"delete-container " + n, "list-containers",
		}
		if !slices.Equal(fx.rt.calls, want) {
			t.Errorf("calls = %q, want %q", fx.rt.calls, want)
		}
	})
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		fx, ops, role := newHelperRoleTest(t)
		injected := errors.New("injected")
		role.recheckStopped = func(InspectReport) error { return injected }
		err := ops.runHelperRole(t.Context(), role)
		var cf *ConformanceFailure
		if !errors.Is(err, injected) || errors.As(err, &cf) {
			t.Errorf("error = %v, want the re-check's own error unwrapped", err)
		}
		if fx.rt.callIndex("read-proof") >= 0 || fx.rt.callIndex("delete-container "+n) >= 0 {
			t.Errorf("a refused re-check still read the proof or deleted: %q", fx.rt.calls)
		}
		if !role.claim.attempted || !role.claim.owned || role.claim.fingerprint == "" {
			t.Errorf("claim = %+v, want it left for teardown", *role.claim)
		}
	})
}

// TestHelperRoleWrapsOnlyRuntimeStepFailures fails each step of a role that
// sets every optional step and a wrapper, and checks the wrapper covers
// exactly the six runtime steps: never the fingerprint failure, a
// role-supplied step, or the absence check.
func TestHelperRoleWrapsOnlyRuntimeStepFailures(t *testing.T) {
	t.Parallel()
	injected := errors.New("injected")
	wrapped := errors.New("wrapped")
	for _, tc := range []struct {
		step    string
		wrapped bool
	}{
		{step: "create", wrapped: true},
		{step: "inspect", wrapped: true},
		{step: "verify"},
		{step: "fingerprint"},
		{step: "journal"},
		{step: "start", wrapped: true},
		{step: "after-start"},
		{step: "stopped", wrapped: true},
		{step: "stopped-inspect", wrapped: true},
		{step: "recheck"},
		{step: "read-proof"},
		{step: "check-proof"},
		{step: "delete", wrapped: true},
		{step: "absent"},
	} {
		t.Run(tc.step, func(t *testing.T) {
			t.Parallel()
			fx, ops, role := newHelperRoleTest(t)
			role.journal = func(string) error { return nil }
			role.recheckStopped = func(InspectReport) error { return nil }
			role.wrapRuntimeFailure = func(err error) error { return fmt.Errorf("%w: %w", wrapped, err) }
			inspects := 0
			switch tc.step {
			case "create":
				fx.rt.onCreateContainer = func(ContainerSpec) error { return injected }
			case "inspect", "stopped", "stopped-inspect":
				// The first inspect is the pre-start one, the second the
				// stopped wait's first poll, the third helperStopped's, and
				// the fourth the re-check's.
				fail := map[string]int{"inspect": 1, "stopped": 2, "stopped-inspect": 4}[tc.step]
				fx.rt.onInspect = func(_ string, rep InspectReport) (InspectReport, error) {
					inspects++
					if inspects == fail {
						return InspectReport{}, injected
					}
					return rep, nil
				}
			case "verify":
				role.verify = func(InspectReport) error { return injected }
			case "fingerprint":
				role.owner = Label{Key: ownershipLabelKey, Value: "another-invocation"}
			case "journal":
				role.journal = func(string) error { return injected }
			case "start":
				fx.rt.onStart = func(string) error { return injected }
			case "after-start":
				role.afterStart = func(context.Context) error { return injected }
			case "recheck":
				role.recheckStopped = func(InspectReport) error { return injected }
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
			if err == nil {
				t.Fatal("runHelperRole succeeded, want the injected failure")
			}
			if got := errors.Is(err, wrapped); got != tc.wrapped {
				t.Errorf("wrapped = %t, want %t: %v", got, tc.wrapped, err)
			}
			// Each row must fail at its own step: the injected error is the
			// cause, directly or as a runtime step's ConformanceFailure. The
			// fingerprint row injects no error, so it must stop with the
			// container owned but not fingerprinted.
			var cf *ConformanceFailure
			isFailure := errors.As(err, &cf)
			switch {
			case tc.step == "fingerprint":
				if !isFailure || cf.Check != CheckControlPlaneIsolation ||
					!role.claim.owned || role.claim.fingerprint != "" {
					t.Errorf("error = %v, claim = %+v, want an ownership failure before the fingerprint",
						err, *role.claim)
				}
			case !errors.Is(err, injected) && (!isFailure || !errors.Is(cf.Err, injected)):
				t.Errorf("error = %v, want one caused by the injected %s failure", err, tc.step)
			case tc.wrapped && (!isFailure || cf.Check != CheckControlPlaneIsolation || !errors.Is(cf.Err, injected)):
				t.Errorf("error = %v, want the wrapper around a %s ConformanceFailure caused by the injected error",
					err, CheckControlPlaneIsolation)
			}
		})
	}
}

func TestHelperRolePassesTheMemoryLimitErrorThroughTheWrapper(t *testing.T) {
	t.Parallel()
	fx, ops, role := newHelperRoleTest(t)
	wrapped := errors.New("wrapped")
	role.wrapRuntimeFailure = func(err error) error { return fmt.Errorf("%w: %w", wrapped, err) }
	fx.rt.bootLogs = map[string][]byte{helperRoleTestName: bootLogKilling(t, helperRoleTestName)}
	err := ops.runHelperRole(t.Context(), role)
	var limit *MemoryLimitError
	if !errors.As(err, &limit) || limit.Class != LaunchWriter || limit.Size != role.spec.Size {
		t.Fatalf("error = %v, want the helper's MemoryLimitError", err)
	}
	if errors.Is(err, wrapped) || errors.Is(err, ErrConformance) {
		t.Errorf("memory-limit error was wrapped: %v", err)
	}
}
