package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	sqlite "modernc.org/sqlite"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordReviewConfigRecoveryTransitionSQL = `
INSERT INTO review_configuration_recovery_transitions
    (run_id, invocation_id, round, base_sha, head_sha, failure_digest,
     repo, repository_id, superseded_profile_digest, superseding_profile_digest,
     command_id, reason, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	latestReviewConfigRecoveryTransitionSQL = `
SELECT run_id, invocation_id, round, base_sha, head_sha, failure_digest,
       repo, repository_id, superseded_profile_digest, superseding_profile_digest,
       command_id, reason, occurred_at
FROM review_configuration_recovery_transitions WHERE run_id = ? ORDER BY id DESC LIMIT 1`
)

// RecordReviewConfigurationRecoveryTransition appends one command-backed
// authorization for one parked configuration failure. Rows are never updated.
// The unique binding prevents a second command from recording another
// effective recovery for the same failure, while command replay is handled
// before this method by signet.
func (tx *InternalTx) RecordReviewConfigurationRecoveryTransition(
	ctx context.Context, transition domain.ReviewConfigurationRecoveryTransition,
) error {
	if err := transition.Validate(); err != nil {
		return fmt.Errorf("record review configuration recovery transition: %w", err)
	}
	if err := tx.requireReviewConfigurationRecoveryCommand(ctx, transition); err != nil {
		return fmt.Errorf("record review configuration recovery transition: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordReviewConfigRecoveryTransitionSQL,
		transition.RunID, transition.InvocationID, transition.Round,
		transition.BaseSHA, transition.HeadSHA, transition.FailureDigest,
		transition.Repo, transition.RepositoryID,
		transition.SupersededProfileDigest, transition.SupersedingProfileDigest,
		*transition.CommandID, transition.Reason, formatTime(transition.OccurredAt)); err != nil {
		return fmt.Errorf("record review configuration recovery transition: %w", err)
	}
	return nil
}

// requireReviewConfigurationRecoveryCommand re-derives the transition's
// complete authority: the accepted command action, the immutable binding on
// its carrier item, the parked configuration row plus canonical body digest,
// and the explicit profile supersession must all agree. The supersession is
// re-gated from the stored profiles, never from the decoded transition: the
// superseding revision must be the repository's latest and differ from the
// superseded revision only in its review configuration digest. The daemon's
// currently effective configuration digest is engine knowledge, so that final
// equality is enforced by the engine on every read of this transition, not
// here.
func (tx *ReadTx) requireReviewConfigurationRecoveryCommand(
	ctx context.Context, transition domain.ReviewConfigurationRecoveryTransition,
) error {
	command, _, err := tx.GetCommandSnapshot(ctx, *transition.CommandID)
	if err != nil {
		return fmt.Errorf("review configuration recovery command %q: %w", *transition.CommandID, err)
	}
	if command.Action != transition.AuthorizingAction() {
		return fmt.Errorf("review configuration recovery backed by command %q with action %q: %w",
			command.CommandID, command.Action, domain.ErrTransitionCommandMismatch)
	}
	item, _, err := tx.GetAttentionItemSnapshot(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("review configuration recovery command %q item %q: %w",
			command.CommandID, command.ItemID, err)
	}
	if item.ReviewConfigurationRecovery == nil ||
		*item.ReviewConfigurationRecovery != transition.Binding() {
		return fmt.Errorf("review configuration recovery command %q item %q binding: %w",
			command.CommandID, item.ID, domain.ErrReviewConfigRecoveryBindingMismatch)
	}
	failure, err := tx.GetReviewFailure(ctx, transition.InvocationID)
	if err != nil {
		return fmt.Errorf("review configuration recovery failure %q: %w", transition.InvocationID, err)
	}
	digest, err := tx.ReviewFailureBodyDigest(ctx, transition.InvocationID)
	if err != nil {
		return err
	}
	if failure.Class != domain.ReviewFailureConfiguration ||
		!transition.Binding().Matches(failure, digest) {
		return fmt.Errorf("review configuration recovery failure %q binding: %w",
			transition.InvocationID, domain.ErrReviewConfigRecoveryBindingMismatch)
	}
	superseded, err := tx.GetTrustProfile(ctx, transition.SupersededProfileDigest)
	if err != nil {
		return fmt.Errorf("review configuration recovery superseded profile %q: %w",
			transition.SupersededProfileDigest, err)
	}
	superseding, err := tx.GetTrustProfile(ctx, transition.SupersedingProfileDigest)
	if err != nil {
		return fmt.Errorf("review configuration recovery superseding profile %q: %w",
			transition.SupersedingProfileDigest, err)
	}
	latest, err := tx.LatestTrustProfile(ctx, transition.Repo)
	if err != nil {
		return fmt.Errorf("review configuration recovery latest profile for %q: %w",
			transition.Repo, err)
	}
	if latest.ProfileDigest != superseding.ProfileDigest ||
		superseding.Repo != transition.Repo || superseding.RepositoryID != transition.RepositoryID ||
		superseded.Repo != transition.Repo || superseded.RepositoryID != transition.RepositoryID {
		return fmt.Errorf("review configuration recovery profile %q is not %q's latest: %w",
			transition.SupersedingProfileDigest, transition.Repo,
			domain.ErrReviewConfigRecoveryBindingMismatch)
	}
	reviewOnly, err := domain.ReviewConfigurationOnlySupersession(superseded, superseding)
	if err != nil {
		return fmt.Errorf("review configuration recovery supersession: %w", err)
	}
	if !reviewOnly {
		return fmt.Errorf("review configuration recovery %q -> %q: %w",
			transition.SupersededProfileDigest, transition.SupersedingProfileDigest,
			domain.ErrReviewConfigSupersessionInvalid)
	}
	return nil
}

// reviewConfigRecoveryEnvironmental reports whether a read re-gate failure
// is an operational error (a driver failure, a cancelled context, a dead
// connection or transaction) that must propagate for retry. Everything else
// the decode, validation, and re-gate of persisted state can produce is a
// determinate integrity or policy rejection, so classification defaults to
// ineffective rather than enumerating rejection sentinels: a misclassified
// operational error costs one parked tick and is re-read on the next
// reconcile pass, while the inverse misclassification error-loops the lane
// on a tampered row.
func reviewConfigRecoveryEnvironmental(err error) bool {
	var driverErr *sqlite.Error
	return errors.As(err, &driverErr) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, sql.ErrTxDone)
}

func ineffectiveReviewConfigRecovery(runID domain.RunID, err error) error {
	return fmt.Errorf("latest review configuration recovery transition %q: %w: %w",
		runID, domain.ErrReviewConfigRecoveryIneffective, err)
}

// LatestReviewConfigurationRecoveryTransition reconstructs the newest
// configuration recovery for a run. Presence is separate because no decision
// is the normal unrecovered state. Both structural validation and the
// command/failure/profile re-gate run on read, so a tampered or unbacked row,
// or one whose adopted profile is no longer the repository's latest, cannot
// authorize resuming the parked review; every such determinate rejection is
// additionally wrapped with domain.ErrReviewConfigRecoveryIneffective, while
// environmental read failures propagate unwrapped for retry.
func (tx *ReadTx) LatestReviewConfigurationRecoveryTransition(
	ctx context.Context, runID domain.RunID,
) (domain.ReviewConfigurationRecoveryTransition, bool, error) {
	var (
		storedRunID, invocationID, baseSHA, headSHA, failureDigest string
		repo, supersededDigest, supersedingDigest                  string
		repositoryID                                               int64
		commandID                                                  sql.NullString
		round                                                      int
		reason, occurredAt                                         string
	)
	err := tx.tx.QueryRowContext(ctx, latestReviewConfigRecoveryTransitionSQL, runID).Scan(
		&storedRunID, &invocationID, &round, &baseSHA, &headSHA, &failureDigest,
		&repo, &repositoryID, &supersededDigest, &supersedingDigest,
		&commandID, &reason, &occurredAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.ReviewConfigurationRecoveryTransition{}, false, nil
	case err != nil:
		return domain.ReviewConfigurationRecoveryTransition{}, false,
			fmt.Errorf("latest review configuration recovery transition %q: %w", runID, err)
	}
	at, err := parseTime(occurredAt)
	if err != nil {
		return domain.ReviewConfigurationRecoveryTransition{}, false,
			ineffectiveReviewConfigRecovery(runID, fmt.Errorf("occurred_at %q: %w", occurredAt, err))
	}
	transition := domain.ReviewConfigurationRecoveryTransition{
		RunID: domain.RunID(storedRunID), InvocationID: domain.InvocationID(invocationID),
		Round: round, BaseSHA: baseSHA, HeadSHA: headSHA,
		FailureDigest: domain.Digest(failureDigest),
		Repo:          repo, RepositoryID: repositoryID,
		SupersededProfileDigest:  domain.Digest(supersededDigest),
		SupersedingProfileDigest: domain.Digest(supersedingDigest),
		Reason:                   reason, OccurredAt: at,
	}
	if commandID.Valid {
		transition.CommandID = &commandID.String
	}
	if transition.RunID != runID {
		return domain.ReviewConfigurationRecoveryTransition{}, false,
			ineffectiveReviewConfigRecovery(runID, errRowInconsistent)
	}
	if err := transition.Validate(); err != nil {
		return domain.ReviewConfigurationRecoveryTransition{}, false,
			ineffectiveReviewConfigRecovery(runID, err)
	}
	if err := tx.requireReviewConfigurationRecoveryCommand(ctx, transition); err != nil {
		if reviewConfigRecoveryEnvironmental(err) {
			return domain.ReviewConfigurationRecoveryTransition{}, false,
				fmt.Errorf("latest review configuration recovery transition %q: %w", runID, err)
		}
		return domain.ReviewConfigurationRecoveryTransition{}, false,
			ineffectiveReviewConfigRecovery(runID, err)
	}
	return transition, true, nil
}

// HasReviewConfigurationRecoveryTransition reports whether any adoption row
// exists for exactly this failure coordinate. It is deliberately
// authority-free: presence only distinguishes an operator's adopt conclusion
// from a decline on an already-concluded item, so the engine can keep an
// adopted run parked instead of terminalizing it while the adoption is not
// (or not yet) effective. Every consumer that advances a run still goes
// through LatestReviewConfigurationRecoveryTransition's full re-gate.
func (tx *ReadTx) HasReviewConfigurationRecoveryTransition(
	ctx context.Context, runID domain.RunID, invocationID domain.InvocationID, failureDigest domain.Digest,
) (bool, error) {
	var one int
	err := tx.tx.QueryRowContext(ctx, `
SELECT 1 FROM review_configuration_recovery_transitions
WHERE run_id = ? AND invocation_id = ? AND failure_digest = ? LIMIT 1`,
		runID, invocationID, failureDigest).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("review configuration recovery presence %q: %w", runID, err)
	}
	return true, nil
}
