package domain

import (
	"fmt"
	"time"
)

// ReviewRecoveryBinding identifies exactly one persisted contradiction row.
// It is rendered on the AttentionItem so the operator decision binds the
// failure that was displayed, then revalidated against that immutable failure
// before a recovery transition is accepted.
type ReviewRecoveryBinding struct {
	RunID         RunID        `json:"run_id"`
	InvocationID  InvocationID `json:"invocation_id"`
	Round         int          `json:"round"`
	BaseSHA       string       `json:"base_sha"`
	HeadSHA       string       `json:"head_sha"`
	FailureDigest Digest       `json:"failure_digest"`
}

// Validate reports whether the binding names every coordinate of one failed
// review invocation.
func (b ReviewRecoveryBinding) Validate() error {
	if b.RunID == "" || b.InvocationID == "" {
		return fmt.Errorf("review recovery identity: %w", ErrEmptyID)
	}
	if b.Round < 1 {
		return fmt.Errorf("review recovery round %d: %w", b.Round, ErrNonPositive)
	}
	if b.BaseSHA == "" || b.HeadSHA == "" || b.FailureDigest == "" {
		return fmt.Errorf("review recovery binding: %w", ErrEmptyField)
	}
	return nil
}

// Matches reports whether the binding identifies failure and its canonical
// persisted body digest exactly.
func (b ReviewRecoveryBinding) Matches(failure ReviewFailure, digest Digest) bool {
	return b.RunID == failure.RunID &&
		b.InvocationID == failure.InvocationID &&
		b.Round == failure.Round &&
		b.BaseSHA == failure.BaseSHA &&
		b.HeadSHA == failure.HeadSHA &&
		b.FailureDigest == digest
}

// ReviewRecoveryTransition is one appended operator authorization to advance
// past one persisted contradiction. The original failure remains immutable;
// digest binding makes the transition single-use for that exact row. CommandID
// is mandatory because reconstruction re-derives authority from the accepted
// signet command rather than trusting the decoded transition.
type ReviewRecoveryTransition struct {
	RunID         RunID        `json:"run_id"`
	InvocationID  InvocationID `json:"invocation_id"`
	Round         int          `json:"round"`
	BaseSHA       string       `json:"base_sha"`
	HeadSHA       string       `json:"head_sha"`
	FailureDigest Digest       `json:"failure_digest"`
	CommandID     *string      `json:"command_id"`
	Reason        string       `json:"reason"`
	OccurredAt    time.Time    `json:"occurred_at"`
}

// Binding returns the exact contradiction coordinate this transition
// authorizes recovering.
func (t ReviewRecoveryTransition) Binding() ReviewRecoveryBinding {
	return ReviewRecoveryBinding{
		RunID: t.RunID, InvocationID: t.InvocationID, Round: t.Round,
		BaseSHA: t.BaseSHA, HeadSHA: t.HeadSHA, FailureDigest: t.FailureDigest,
	}
}

// AuthorizingAction returns the sole action that can authorize this recovery.
func (ReviewRecoveryTransition) AuthorizingAction() Action { return ActionRecoverReview }

// Validate reports whether the transition is structurally sound.
func (t ReviewRecoveryTransition) Validate() error {
	if err := t.Binding().Validate(); err != nil {
		return err
	}
	if t.CommandID == nil || *t.CommandID == "" {
		return fmt.Errorf("review recovery transition: %w", ErrTransitionUnbacked)
	}
	if t.Reason == "" {
		return fmt.Errorf("review recovery reason: %w", ErrEmptyField)
	}
	if t.OccurredAt.IsZero() {
		return fmt.Errorf("review recovery occurred_at: %w", ErrMissingTimestamp)
	}
	if t.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("review recovery occurred_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
