package domain

import (
	"fmt"
	"time"
)

// ReviewConfigurationRecoveryBinding identifies exactly one persisted
// configuration-class review failure and the trust context it was parked
// under. It is rendered on the AttentionItem so the operator decision binds
// the failure that was displayed, then revalidated against that immutable
// failure before a recovery transition is accepted. It deliberately carries
// no superseding profile digest: the adoption target is resolved at decision
// time as the repository's latest recorded profile, because the approved
// revision can legitimately advance while the item is parked.
type ReviewConfigurationRecoveryBinding struct {
	RunID                   RunID        `json:"run_id"`
	InvocationID            InvocationID `json:"invocation_id"`
	Round                   int          `json:"round"`
	BaseSHA                 string       `json:"base_sha"`
	HeadSHA                 string       `json:"head_sha"`
	FailureDigest           Digest       `json:"failure_digest"`
	Repo                    string       `json:"repo"`
	RepositoryID            int64        `json:"repository_id"`
	SupersededProfileDigest Digest       `json:"superseded_profile_digest"`
}

// Validate reports whether the binding names every coordinate of one parked
// configuration failure and its admission-pinned profile.
func (b ReviewConfigurationRecoveryBinding) Validate() error {
	if b.RunID == "" || b.InvocationID == "" {
		return fmt.Errorf("review configuration recovery identity: %w", ErrEmptyID)
	}
	if b.Round < 1 {
		return fmt.Errorf("review configuration recovery round %d: %w", b.Round, ErrNonPositive)
	}
	if b.BaseSHA == "" || b.HeadSHA == "" || b.FailureDigest == "" ||
		b.Repo == "" || b.SupersededProfileDigest == "" {
		return fmt.Errorf("review configuration recovery binding: %w", ErrEmptyField)
	}
	if b.RepositoryID <= 0 {
		return fmt.Errorf("review configuration recovery repository_id %d: %w",
			b.RepositoryID, ErrNonPositive)
	}
	return nil
}

// Matches reports whether the binding identifies failure and its canonical
// persisted body digest exactly.
func (b ReviewConfigurationRecoveryBinding) Matches(failure ReviewFailure, digest Digest) bool {
	return b.RunID == failure.RunID &&
		b.InvocationID == failure.InvocationID &&
		b.Round == failure.Round &&
		b.BaseSHA == failure.BaseSHA &&
		b.HeadSHA == failure.HeadSHA &&
		b.FailureDigest == digest
}

// ReviewConfigurationRecoveryTransition is one appended operator
// authorization to resume a run parked on one configuration-class review
// failure by adopting one explicitly named profile revision. The original
// failure row and the superseded profile revision remain immutable; digest
// binding makes the transition single-use for that exact row. CommandID is
// mandatory because reconstruction re-derives authority from the accepted
// signet command rather than trusting the decoded transition. The
// SupersedingProfileDigest is recorded at decision time and re-gated on every
// read: it must remain the repository's latest profile, differ from the
// superseded revision only in its review configuration digest, and approve
// the daemon's currently effective configuration, so a decoded row can never
// smuggle a broader trust change through a review recovery.
type ReviewConfigurationRecoveryTransition struct {
	RunID                    RunID        `json:"run_id"`
	InvocationID             InvocationID `json:"invocation_id"`
	Round                    int          `json:"round"`
	BaseSHA                  string       `json:"base_sha"`
	HeadSHA                  string       `json:"head_sha"`
	FailureDigest            Digest       `json:"failure_digest"`
	Repo                     string       `json:"repo"`
	RepositoryID             int64        `json:"repository_id"`
	SupersededProfileDigest  Digest       `json:"superseded_profile_digest"`
	SupersedingProfileDigest Digest       `json:"superseding_profile_digest"`
	CommandID                *string      `json:"command_id"`
	Reason                   string       `json:"reason"`
	OccurredAt               time.Time    `json:"occurred_at"`
}

// Binding returns the exact parked-failure coordinate this transition
// authorizes recovering.
func (t ReviewConfigurationRecoveryTransition) Binding() ReviewConfigurationRecoveryBinding {
	return ReviewConfigurationRecoveryBinding{
		RunID: t.RunID, InvocationID: t.InvocationID, Round: t.Round,
		BaseSHA: t.BaseSHA, HeadSHA: t.HeadSHA, FailureDigest: t.FailureDigest,
		Repo: t.Repo, RepositoryID: t.RepositoryID,
		SupersededProfileDigest: t.SupersededProfileDigest,
	}
}

// AuthorizingAction returns the sole action that can authorize this recovery.
func (ReviewConfigurationRecoveryTransition) AuthorizingAction() Action {
	return ActionAdoptReviewConfiguration
}

// Validate reports whether the transition is structurally sound.
func (t ReviewConfigurationRecoveryTransition) Validate() error {
	if err := t.Binding().Validate(); err != nil {
		return err
	}
	if t.SupersedingProfileDigest == "" {
		return fmt.Errorf("review configuration recovery superseding profile: %w", ErrEmptyField)
	}
	if t.CommandID == nil || *t.CommandID == "" {
		return fmt.Errorf("review configuration recovery transition: %w", ErrTransitionUnbacked)
	}
	if t.Reason == "" {
		return fmt.Errorf("review configuration recovery reason: %w", ErrEmptyField)
	}
	if t.OccurredAt.IsZero() {
		return fmt.Errorf("review configuration recovery occurred_at: %w", ErrMissingTimestamp)
	}
	if t.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("review configuration recovery occurred_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// ReviewConfigurationOnlySupersession reports whether superseding differs
// from superseded in exactly its review configuration digest (or not at all:
// a restored configuration re-adopts the pinned revision itself). It decides
// by content address, not field comparison: overlaying the superseded
// revision's review configuration digest onto the superseding body must
// reproduce the superseded revision's own profile digest, so any other
// delta, however it was encoded, fails the equality. Both profiles are
// re-validated first so a tampered body cannot pass under a stale digest.
func ReviewConfigurationOnlySupersession(superseded, superseding AutomationTrustProfile) (bool, error) {
	if err := superseded.Validate(); err != nil {
		return false, fmt.Errorf("superseded trust profile: %w", err)
	}
	if err := superseding.Validate(); err != nil {
		return false, fmt.Errorf("superseding trust profile: %w", err)
	}
	overlay := superseding
	overlay.Review.ConfigDigest = superseded.Review.ConfigDigest
	digest, err := overlay.ComputeDigest()
	if err != nil {
		return false, err
	}
	return digest == superseded.ProfileDigest, nil
}
