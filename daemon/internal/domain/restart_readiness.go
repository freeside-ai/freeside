package domain

import (
	"fmt"
	"slices"
	"time"
)

// RestartBlockerKind names a kind of running work a daemon restart would cut
// short (issue #3103). The readiness read counts each kind; it never names the
// run or task behind a count. The zero value is invalid.
type RestartBlockerKind string

const (
	// RestartBlockerExecutionLaunch: an execution admission still holds its
	// slot.
	RestartBlockerExecutionLaunch RestartBlockerKind = "execution_launch"
	// RestartBlockerReviewLaunch: a review admission still holds its slot.
	RestartBlockerReviewLaunch RestartBlockerKind = "review_launch"
	// RestartBlockerVerificationJob: a verification job is running.
	RestartBlockerVerificationJob RestartBlockerKind = "verification_job"
	// RestartBlockerImageBuild: a project image build is running.
	RestartBlockerImageBuild RestartBlockerKind = "image_build"
	// RestartBlockerProbe: a model-discovery or credential probe is running.
	RestartBlockerProbe RestartBlockerKind = "probe"
	// RestartBlockerPublication: a publication is in progress.
	RestartBlockerPublication RestartBlockerKind = "publication"
	// RestartBlockerCredentialLease: an auth-store mutation lease is held,
	// such as an enrollment or a token refresh in progress.
	RestartBlockerCredentialLease RestartBlockerKind = "credential_lease" //nolint:gosec // G101 false positive: a blocker kind, not a credential.
)

// AllRestartBlockerKinds is the single registration point for restart
// blocker kinds, and the order a readiness value lists its blockers in.
var AllRestartBlockerKinds = []RestartBlockerKind{
	RestartBlockerExecutionLaunch,
	RestartBlockerReviewLaunch,
	RestartBlockerVerificationJob,
	RestartBlockerImageBuild,
	RestartBlockerProbe,
	RestartBlockerPublication,
	RestartBlockerCredentialLease,
}

func (k RestartBlockerKind) valid() bool {
	switch k {
	case RestartBlockerExecutionLaunch, RestartBlockerReviewLaunch, RestartBlockerVerificationJob,
		RestartBlockerImageBuild, RestartBlockerProbe, RestartBlockerPublication,
		RestartBlockerCredentialLease:
		return true
	default:
		return false
	}
}

// RestartBlocker is how many of one kind of running work a restart would cut
// short. A kind with nothing running is left out, so Count is at least 1.
type RestartBlocker struct {
	Kind  RestartBlockerKind `json:"kind"`
	Count int                `json:"count"`
}

// RestartReadiness answers whether it is safe to restart the daemon now, and
// what a restart is waiting on. Ready needs a drain in force: with no drain,
// a launch can start the moment after the read, so an empty blocker list
// alone proves nothing.
type RestartReadiness struct {
	Ready bool `json:"ready"`
	// Drain is the update drain in force, nil when there is none.
	Drain *UpdateDrainTransition `json:"drain"`
	// Blockers lists each kind with running work, in AllRestartBlockerKinds
	// order.
	Blockers []RestartBlocker `json:"blockers"`
	AsOf     time.Time        `json:"as_of"`
}

// NewRestartReadiness derives a readiness value from the drain in force and
// per-kind counts, so Ready and the blocker list can't disagree. Kinds with a
// zero count are left out.
func NewRestartReadiness(
	drain *UpdateDrainTransition, counts map[RestartBlockerKind]int, asOf time.Time,
) (RestartReadiness, error) {
	blockers := []RestartBlocker{}
	for _, kind := range AllRestartBlockerKinds {
		if counts[kind] > 0 {
			blockers = append(blockers, RestartBlocker{Kind: kind, Count: counts[kind]})
		}
	}
	readiness := RestartReadiness{
		Ready:    drain != nil && len(blockers) == 0,
		Drain:    drain,
		Blockers: blockers,
		AsOf:     asOf,
	}
	if err := readiness.Validate(); err != nil {
		return RestartReadiness{}, err
	}
	return readiness, nil
}

// Validate reports whether the readiness value is consistent: a draining
// drain row when one is present, known kinds with a positive count listed
// once each in canonical order, and Ready exactly when a drain is in force and
// nothing blocks.
func (r RestartReadiness) Validate() error {
	if r.Drain != nil {
		if err := r.Drain.Validate(); err != nil {
			return fmt.Errorf("restart readiness drain: %w", err)
		}
		if r.Drain.State != UpdateDrainDraining {
			return fmt.Errorf("restart readiness drain state %q: %w", r.Drain.State, ErrRestartReadinessInconsistent)
		}
	}
	if r.Blockers == nil {
		return fmt.Errorf("restart readiness blockers are nil: %w", ErrRestartReadinessInconsistent)
	}
	last := -1
	for _, blocker := range r.Blockers {
		if !blocker.Kind.valid() {
			return fmt.Errorf("restart blocker kind %q: %w", blocker.Kind, ErrInvalidRestartBlockerKind)
		}
		if blocker.Count < 1 {
			return fmt.Errorf("restart blocker %q count %d: %w", blocker.Kind, blocker.Count, ErrRestartReadinessInconsistent)
		}
		position := slices.Index(AllRestartBlockerKinds, blocker.Kind)
		if position <= last {
			return fmt.Errorf("restart blocker %q out of order: %w", blocker.Kind, ErrRestartReadinessInconsistent)
		}
		last = position
	}
	if r.Ready != (r.Drain != nil && len(r.Blockers) == 0) {
		return fmt.Errorf("restart readiness ready %t: %w", r.Ready, ErrRestartReadinessInconsistent)
	}
	if r.AsOf.IsZero() {
		return fmt.Errorf("restart readiness as_of: %w", ErrMissingTimestamp)
	}
	if r.AsOf.Location() != time.UTC {
		return fmt.Errorf("restart readiness as_of: %w", ErrTimestampNotUTC)
	}
	return nil
}
