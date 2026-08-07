package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordReviewRecoveryTransitionSQL = `
INSERT INTO review_recovery_transitions
    (run_id, invocation_id, round, base_sha, head_sha, failure_digest,
     command_id, reason, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	latestReviewRecoveryTransitionSQL = `
SELECT run_id, invocation_id, round, base_sha, head_sha, failure_digest,
       command_id, reason, occurred_at
FROM review_recovery_transitions WHERE run_id = ? ORDER BY id DESC LIMIT 1`
)

// ReviewFailureBodyDigest returns the exact persisted body digest after the
// failure row has reconstructed successfully. Returning the stored digest,
// rather than re-encoding the domain value, preserves byte-level binding: even
// a semantically equivalent rewrite needs a distinct operator decision.
func (tx *ReadTx) ReviewFailureBodyDigest(
	ctx context.Context, id domain.InvocationID,
) (domain.Digest, error) {
	if _, err := tx.GetReviewFailure(ctx, id); err != nil {
		return "", err
	}
	var digest string
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT body_digest FROM review_failures WHERE invocation_id = ?`, id).Scan(&digest); err != nil {
		return "", fmt.Errorf("review failure body digest %q: %w", id, err)
	}
	if digest == "" {
		return "", fmt.Errorf("review failure body digest %q: %w", id, domain.ErrEmptyField)
	}
	return domain.Digest(digest), nil
}

// RecordReviewRecoveryTransition appends one command-backed authorization for
// one contradiction. Rows are never updated. The unique binding prevents a
// second command from recording another effective recovery for the same
// failure, while command replay is handled before this method by signet.
func (tx *InternalTx) RecordReviewRecoveryTransition(
	ctx context.Context, transition domain.ReviewRecoveryTransition,
) error {
	if err := transition.Validate(); err != nil {
		return fmt.Errorf("record review recovery transition: %w", err)
	}
	if err := tx.requireReviewRecoveryCommand(ctx, transition); err != nil {
		return fmt.Errorf("record review recovery transition: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordReviewRecoveryTransitionSQL,
		transition.RunID, transition.InvocationID, transition.Round,
		transition.BaseSHA, transition.HeadSHA, transition.FailureDigest,
		*transition.CommandID, transition.Reason, formatTime(transition.OccurredAt)); err != nil {
		return fmt.Errorf("record review recovery transition: %w", err)
	}
	return nil
}

// requireReviewRecoveryCommand re-derives the transition's complete authority:
// the accepted command action, the immutable binding on its carrier item, and
// the contradiction row plus canonical body digest must all agree.
func (tx *ReadTx) requireReviewRecoveryCommand(
	ctx context.Context, transition domain.ReviewRecoveryTransition,
) error {
	command, _, err := tx.GetCommandSnapshot(ctx, *transition.CommandID)
	if err != nil {
		return fmt.Errorf("review recovery command %q: %w", *transition.CommandID, err)
	}
	if command.Action != transition.AuthorizingAction() {
		return fmt.Errorf("review recovery backed by command %q with action %q: %w",
			command.CommandID, command.Action, domain.ErrTransitionCommandMismatch)
	}
	item, _, err := tx.GetAttentionItemSnapshot(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("review recovery command %q item %q: %w",
			command.CommandID, command.ItemID, err)
	}
	if item.ReviewRecoveryBinding == nil || *item.ReviewRecoveryBinding != transition.Binding() {
		return fmt.Errorf("review recovery command %q item %q binding: %w",
			command.CommandID, item.ID, domain.ErrReviewRecoveryBindingMismatch)
	}
	failure, err := tx.GetReviewFailure(ctx, transition.InvocationID)
	if err != nil {
		return fmt.Errorf("review recovery failure %q: %w", transition.InvocationID, err)
	}
	digest, err := tx.ReviewFailureBodyDigest(ctx, transition.InvocationID)
	if err != nil {
		return err
	}
	if failure.Class != domain.ReviewFailureContradiction ||
		!transition.Binding().Matches(failure, digest) {
		return fmt.Errorf("review recovery failure %q binding: %w",
			transition.InvocationID, domain.ErrReviewRecoveryBindingMismatch)
	}
	return nil
}

// LatestReviewRecoveryTransition reconstructs the newest recovery for a run.
// Presence is separate because no decision is the normal unrecovered state.
// Both structural validation and the command/failure re-gate run on read, so
// a tampered or unbacked row cannot authorize advancing the review round.
func (tx *ReadTx) LatestReviewRecoveryTransition(
	ctx context.Context, runID domain.RunID,
) (domain.ReviewRecoveryTransition, bool, error) {
	var (
		storedRunID, invocationID, baseSHA, headSHA, failureDigest string
		commandID                                                  sql.NullString
		round                                                      int
		reason, occurredAt                                         string
	)
	err := tx.tx.QueryRowContext(ctx, latestReviewRecoveryTransitionSQL, runID).Scan(
		&storedRunID, &invocationID, &round, &baseSHA, &headSHA, &failureDigest,
		&commandID, &reason, &occurredAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.ReviewRecoveryTransition{}, false, nil
	case err != nil:
		return domain.ReviewRecoveryTransition{}, false,
			fmt.Errorf("latest review recovery transition %q: %w", runID, err)
	}
	at, err := parseTime(occurredAt)
	if err != nil {
		return domain.ReviewRecoveryTransition{}, false,
			fmt.Errorf("latest review recovery transition %q occurred_at %q: %w",
				runID, occurredAt, err)
	}
	transition := domain.ReviewRecoveryTransition{
		RunID: domain.RunID(storedRunID), InvocationID: domain.InvocationID(invocationID),
		Round: round, BaseSHA: baseSHA, HeadSHA: headSHA,
		FailureDigest: domain.Digest(failureDigest), Reason: reason, OccurredAt: at,
	}
	if commandID.Valid {
		transition.CommandID = &commandID.String
	}
	if transition.RunID != runID {
		return domain.ReviewRecoveryTransition{}, false,
			fmt.Errorf("latest review recovery transition %q: %w", runID, errRowInconsistent)
	}
	if err := transition.Validate(); err != nil {
		return domain.ReviewRecoveryTransition{}, false,
			fmt.Errorf("latest review recovery transition %q: %w", runID, err)
	}
	if err := tx.requireReviewRecoveryCommand(ctx, transition); err != nil {
		return domain.ReviewRecoveryTransition{}, false,
			fmt.Errorf("latest review recovery transition %q: %w", runID, err)
	}
	return transition, true, nil
}
