package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// RestartReadiness reports whether it is safe to restart the daemon at now:
// the update drain in force and a count of each kind of running work the
// store records (issue #3103). It writes nothing.
//
// The store records three of the blocker kinds. Verification jobs, image
// builds, probes, and publications exist only in the running process, so the
// daemon adds them where it holds each one (#3104). Until then no drain can
// be requested, so Ready can't be true while those kinds go unread.
//
//   - execution_launch: an execution admission holding its slot, by the
//     rule pool admission applies (executionSlotsHeld).
//   - review_launch: a review admission holding its slot (reviewSlotHeld).
//   - credential_lease: a live auth-store mutation lease whose holder is not
//     a launch already counted. A handoff lease names its invocation as
//     holder, so it isn't counted twice; a Codex auth refresh names its own
//     holder, so it is counted beside its launch. Counting too much only
//     delays readiness.
func (tx *ReadTx) RestartReadiness(ctx context.Context, now time.Time) (domain.RestartReadiness, error) {
	drain, err := tx.UpdateDrainInForce(ctx)
	if err != nil {
		return domain.RestartReadiness{}, fmt.Errorf("restart readiness: %w", err)
	}
	executions, err := tx.executionSlotsHeld(ctx, "")
	if err != nil {
		return domain.RestartReadiness{}, fmt.Errorf("restart readiness: %w", err)
	}
	reviews, err := tx.reviewSlotsHeld(ctx)
	if err != nil {
		return domain.RestartReadiness{}, fmt.Errorf("restart readiness: %w", err)
	}
	counted := make(map[domain.InvocationID]bool, len(executions)+len(reviews))
	for _, id := range executions {
		counted[id] = true
	}
	for _, id := range reviews {
		counted[id] = true
	}
	leases, err := tx.liveMutationLeases(ctx, now)
	if err != nil {
		return domain.RestartReadiness{}, fmt.Errorf("restart readiness: %w", err)
	}
	uncounted := 0
	for _, lease := range leases {
		if !counted[lease.Holder] {
			uncounted++
		}
	}
	readiness, err := domain.NewRestartReadiness(drain, map[domain.RestartBlockerKind]int{
		domain.RestartBlockerExecutionLaunch: len(executions),
		domain.RestartBlockerReviewLaunch:    len(reviews),
		domain.RestartBlockerCredentialLease: uncounted,
	}, now)
	if err != nil {
		return domain.RestartReadiness{}, fmt.Errorf("restart readiness: %w", err)
	}
	return readiness, nil
}

// reviewSlotsHeld returns every review admission holding a slot under
// reviewSlotHeld's rule, as reviewSlotHolders iterates them but without
// reading whose identity holds each.
func (tx *ReadTx) reviewSlotsHeld(ctx context.Context) ([]domain.InvocationID, error) {
	ids, err := queryIDs[domain.InvocationID](ctx, tx, `SELECT invocation_id FROM review_admissions`)
	if err != nil {
		return nil, fmt.Errorf("list review admissions: %w", err)
	}
	var held []domain.InvocationID
	for _, id := range ids {
		ok, err := tx.reviewSlotHeld(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			held = append(held, id)
		}
	}
	return held, nil
}

// liveMutationLeases returns the auth-store mutation lease of every identity
// that declares one, when it is held at now.
func (tx *ReadTx) liveMutationLeases(ctx context.Context, now time.Time) ([]domain.AuthStoreMutationLease, error) {
	identities, err := tx.ListAuthIdentities(ctx)
	if err != nil {
		return nil, err
	}
	var live []domain.AuthStoreMutationLease
	for _, identity := range identities {
		if !identity.AuthStoreMutationLease {
			continue
		}
		lease, err := tx.GetAuthStoreMutationLease(ctx, identity.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if lease.HeldAt(now) {
			live = append(live, lease)
		}
	}
	return live, nil
}
