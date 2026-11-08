package ward

import (
	"context"
	"fmt"
	"time"
)

// helperRole is one short-lived helper container: a seeder or observer the
// gate creates, verifies, runs to a stop, optionally reads a proof from,
// and proves deleted. runHelperRole runs that sequence once, so every role
// it carries checks the same things in the same order.
type helperRole struct {
	spec ContainerSpec
	// label names the role in failure reasons.
	label string
	// claim is the role's slot in runState; teardown and recovery read it,
	// so the runner leaves it exactly as each step left it on failure.
	claim *objectClaim
	owner Label
	// check is the contract check every runtime-step failure reports.
	check Check
	// class is the launch class a memory-limit kill reports; the size is
	// spec.Size.
	class   LaunchClass
	timeout time.Duration
	// verify is the role's allowlist check on the pre-start inspection.
	// It runs before the ownership fingerprint: a report that fails the
	// allowlist is never bound as this run's object.
	verify func(InspectReport) error
	// afterStart, when set, runs once the container has started, before the
	// stopped wait; seeders copy their staged input here.
	afterStart func(context.Context) error
	// readProof, when set, reads the stopped container's proof; checkProof,
	// when set, judges it. Both run before the delete, so a refused proof
	// leaves the container for teardown.
	readProof  func(context.Context) ([]byte, error)
	checkProof func([]byte) error
}

// runHelperRole runs one helper role: mark attempted, create, mark owned,
// inspect, verify, fingerprint, start, afterStart, wait for stop,
// helperStopped, readProof, checkProof, delete, verify absent, clear the
// claim.
//
// Every failure of a runtime step is a ConformanceFailure for the role's
// check, built with failw so the cause stays reachable. Steps the role
// supplies (verify, afterStart, readProof, checkProof) and the absence check
// return their own errors, and helperStopped's *MemoryLimitError passes
// through unwrapped because callers detect it. The runner never marks
// content evidence; only a role's proof check may.
func (o runtimeOps) runHelperRole(ctx context.Context, r helperRole) error {
	if r.verify == nil || r.claim == nil || r.timeout <= 0 {
		return fmt.Errorf("helper role %s is incomplete: it needs an allowlist check, a claim, and a timeout", r.label)
	}
	name := r.spec.Name
	r.claim.attempted = true
	if err := o.rt.CreateContainer(ctx, cloneContainerSpec(r.spec)); err != nil {
		return failw(r.check, err, "create %s: %v", r.label, err)
	}
	r.claim.owned = true
	rep, err := o.rt.Inspect(ctx, name)
	if err != nil {
		return failw(r.check, err, "inspect %s before execution: %v", r.label, err)
	}
	if err := r.verify(rep); err != nil {
		return err
	}
	r.claim.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, r.owner)
	if err != nil {
		return failw(r.check, err, "%s ownership: %v", r.label, err)
	}
	if err := o.rt.StartContainer(ctx, name); err != nil {
		return failw(r.check, err, "start %s: %v", r.label, err)
	}
	if r.afterStart != nil {
		if err := r.afterStart(ctx); err != nil {
			return err
		}
	}
	if err := o.waitStopped(ctx, name, *r.claim, r.owner, r.timeout); err != nil {
		return failw(r.check, err, "%s: %v", r.label, err)
	}
	if err := o.helperStopped(ctx, r.class, r.spec.Size, name); err != nil {
		return err
	}
	if r.readProof != nil {
		proof, err := r.readProof(ctx)
		if err != nil {
			return err
		}
		if r.checkProof != nil {
			if err := r.checkProof(proof); err != nil {
				return err
			}
		}
	}
	if err := o.rt.DeleteContainer(ctx, name); err != nil {
		return failw(r.check, err, "delete stopped %s: %v", r.label, err)
	}
	if err := o.verifyContainerAbsent(ctx, name, *r.claim, r.owner, r.check); err != nil {
		return err
	}
	*r.claim = objectClaim{}
	return nil
}
