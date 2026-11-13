package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	PublicationSuccessorKind       = "publication_successor_authority"
	PublicationSuccessorVersion    = "freeside.publication-successor/v1"
	PublicationContinuationVersion = "freeside.publication-successor/v2"
	PublicationReentryVersion      = "freeside.publication-successor/v3"
	// PublicationExternalReviewVersion is the v3 shape plus the finding and
	// trust profile an external_review re-entry rests on.
	PublicationExternalReviewVersion = "freeside.publication-successor/v4"
	// PublicationUnpublishedReentryVersion is the v3 shape for a run whose base
	// advanced before its first publication (issue #2058).
	PublicationUnpublishedReentryVersion = "freeside.publication-successor/v5"
	// PublicationSuccessorReentryVersion is the v5 shape for a successor whose
	// unpublished head was refused for a base advance after the run's pull
	// request already existed (issue #2111).
	PublicationSuccessorReentryVersion = "freeside.publication-successor/v6"
	// PublicationHandoffConflictVersion is the v3 shape plus a cause and a
	// first-publication flag, for a remediation at the tip a handoff check
	// sealed (issue #2646).
	PublicationHandoffConflictVersion = "freeside.publication-successor/v7"
	// PublicationHandBackVersion is the v4 shape for a hand_back re-entry: the
	// hand-back comment and the trust profile that admitted its author
	// (issue #3160).
	PublicationHandBackVersion = "freeside.publication-successor/v8"
)

type PublicationSuccessorOrigin string

const (
	PublicationSuccessorFeedback    PublicationSuccessorOrigin = "feedback"
	PublicationSuccessorRemediation PublicationSuccessorOrigin = "remediation_continuation"
	// PublicationSuccessorReadinessInvalidation re-enters verification and
	// review for a ready item the daemon invalidated (issue #1622). No operator
	// command starts it, so its identities key on the superseded item.
	PublicationSuccessorReadinessInvalidation PublicationSuccessorOrigin = "readiness_invalidation"
	// PublicationSuccessorExternalReview re-enters review for a ready item
	// whose published head drew a finding from an admitted external reviewer
	// (issue #1623). Like a readiness invalidation it has no operator command
	// and pushes nothing of its own, so it shares that origin's identities.
	PublicationSuccessorExternalReview PublicationSuccessorOrigin = "external_review"
	// PublicationSuccessorUnpublishedBaseAdvance re-enters a run whose
	// publication-time audit found the target base advanced before any
	// publication intent committed (issue #2058). Nothing is on a pull request
	// yet: the cycle keeps the candidate head, reviews its prospective merge into
	// the audited base, and makes the run's first publication against that base.
	// No operator command starts it, so its identities key on its predecessor.
	PublicationSuccessorUnpublishedBaseAdvance PublicationSuccessorOrigin = "unpublished_base_advance"
	// PublicationSuccessorBaseAdvance re-enters a successor cycle (a feedback
	// return, a remediation continuation, or an earlier such re-entry) whose
	// head was verified and reviewed but refused because the target base
	// advanced before its intent committed (issue #2111). Unlike
	// unpublished_base_advance the run's pull request exists: the cycle keeps
	// the head, reviews its prospective merge into the audited base, and then
	// updates that pull request. No operator command starts it, so its
	// identities key on its predecessor.
	PublicationSuccessorBaseAdvance PublicationSuccessorOrigin = "successor_base_advance"
	// PublicationSuccessorHandoffConflict remediates a head whose handoff
	// check found a conflict at the target's tip (plan §7, revision 110;
	// issue #2646): a textual conflict, or a merge that failed verification.
	// The cycle's remediations are admitted at the tip the check sealed, and
	// their new head is verified, reviewed, and handed off again. It makes
	// the run's first publication when nothing published before it and
	// otherwise updates the run's pull request. No operator command starts
	// it, so its identities key on its predecessor.
	PublicationSuccessorHandoffConflict PublicationSuccessorOrigin = "handoff_conflict"
	// PublicationSuccessorHandBack reworks a pull request the lander handed
	// back (plan §7; issue #3160). It adopts the pull request's current head,
	// which no Freeside invocation produced, verifies and reviews it in full,
	// and then hands it off again on the target's current tip. The hand-back
	// label and an admitted account's comment on that head are its trigger;
	// the comment and the profile that admitted its author are named on the
	// record. No operator command starts it, so its identities key on its
	// predecessor.
	PublicationSuccessorHandBack PublicationSuccessorOrigin = "hand_back"
)

var AllPublicationSuccessorOrigins = []PublicationSuccessorOrigin{
	PublicationSuccessorFeedback, PublicationSuccessorRemediation, PublicationSuccessorReadinessInvalidation,
	PublicationSuccessorExternalReview, PublicationSuccessorUnpublishedBaseAdvance, PublicationSuccessorBaseAdvance,
	PublicationSuccessorHandoffConflict, PublicationSuccessorHandBack,
}

func (o PublicationSuccessorOrigin) valid() bool {
	switch o {
	case PublicationSuccessorFeedback, PublicationSuccessorRemediation, PublicationSuccessorReadinessInvalidation,
		PublicationSuccessorExternalReview, PublicationSuccessorUnpublishedBaseAdvance, PublicationSuccessorBaseAdvance,
		PublicationSuccessorHandoffConflict, PublicationSuccessorHandBack:
		return true
	default:
		return false
	}
}

// HandoffConflictCause is what a handoff check found at the target's tip.
type HandoffConflictCause string

const (
	// HandoffConflictTextual: the head does not merge cleanly into the tip.
	HandoffConflictTextual HandoffConflictCause = "textual_conflict"
	// HandoffConflictMergeVerificationFailed: the merge of the head into the
	// tip failed verification.
	HandoffConflictMergeVerificationFailed HandoffConflictCause = "merge_verification_failed"
)

var AllHandoffConflictCauses = []HandoffConflictCause{
	HandoffConflictTextual, HandoffConflictMergeVerificationFailed,
}

func (c HandoffConflictCause) valid() bool {
	switch c {
	case HandoffConflictTextual, HandoffConflictMergeVerificationFailed:
		return true
	default:
		return false
	}
}

// PublicationSuccessorReentry names what a superseded ready item re-enters
// for: the base and head already on its pull request. A re-entered cycle
// builds no candidate and pushes nothing; it re-earns readiness in place
// (devlog/2026-10-02-0222-readiness-reentry-authority.md). Reason is the
// readiness invalidation behind a readiness_invalidation re-entry and is
// empty for an external_review one, whose item was never invalidated: the
// pull request did not move, a reviewer spoke.
type PublicationSuccessorReentry struct {
	Reason  ReadinessInvalidationReason `json:"reason,omitempty"`
	BaseSHA string                      `json:"base_sha"`
	HeadSHA string                      `json:"head_sha"`
}

// A retarget records no base SHA to re-check and an identity change offers no
// pull request that is provably this one, so neither re-enters.
func (r PublicationSuccessorReentry) valid() bool {
	switch r.Reason {
	case ReadinessInvalidationBaseAdvanced, ReadinessInvalidationHeadChanged:
		return r.BaseSHA != "" && r.HeadSHA != ""
	case ReadinessInvalidationRetargeted, ReadinessInvalidationIdentityChanged:
		return false
	}
	return false
}

// PublicationSuccessor binds a cycle to an accepted feedback return, a recheck
// approval, a readiness invalidation, an admitted external review, or a base
// advance found before the run's first publication. Remediation may produce a
// later candidate under any but the last of these, but cannot change its
// predecessor or review floor.
type PublicationSuccessor struct {
	Version                 string                       `json:"version"`
	RunID                   RunID                        `json:"run_id"`
	CommandID               string                       `json:"command_id"`
	FeedbackInvocationID    InvocationID                 `json:"feedback_invocation_id"`
	PredecessorItemID       ItemID                       `json:"predecessor_item_id"`
	PriorReviewInvocationID InvocationID                 `json:"prior_review_invocation_id"`
	ReviewRound             int                          `json:"review_round"`
	Origin                  PublicationSuccessorOrigin   `json:"origin,omitempty"`
	ReevaluationCommandID   string                       `json:"reevaluation_command_id,omitempty"`
	Reentry                 *PublicationSuccessorReentry `json:"reentry,omitempty"`
	// ExternalFindingID and AdmittingProfileDigest are set together, on an
	// external_review or hand_back record only: the finding that triggered
	// the cycle (for hand_back, the hand-back comment) and the trust profile
	// revision whose allowlist admitted its reviewer. The
	// profile is named, not looked up, so removing the reviewer later stops
	// new cycles without making this one unreadable.
	ExternalFindingID      FindingID `json:"external_finding_id,omitempty"`
	AdmittingProfileDigest Digest    `json:"admitting_profile_digest,omitempty"`
	// HandoffConflictCause and FirstPublication are set on a handoff_conflict
	// record only. Reentry.BaseSHA is the tip the handoff check sealed and
	// Reentry.HeadSHA the head that conflicted with it. FirstPublication says
	// the cycle makes the run's first publication; the store proves it from
	// the run's ready bindings on every read.
	HandoffConflictCause HandoffConflictCause `json:"handoff_conflict_cause,omitempty"`
	FirstPublication     bool                 `json:"first_publication,omitempty"`
}

// EffectiveOrigin preserves the byte-identical v1 feedback representation.
func (s PublicationSuccessor) EffectiveOrigin() PublicationSuccessorOrigin {
	if s.Version == PublicationSuccessorVersion || s.Origin == "" {
		return PublicationSuccessorFeedback
	}
	return s.Origin
}

// commandless reports whether the cycle's identities key on its predecessor
// item. The derivations branch on this, not on the origin's name, so a later
// commandless origin joins without changing them.
func (s PublicationSuccessor) commandless() bool {
	return s.CommandID == ""
}

// predecessorKey depends only on the superseded item, never on the reason or
// coordinates, so one item admits one commandless successor. Item IDs are free
// text and the publication ID enters an outbox key unescaped; the digest keeps
// it path-safe.
func (s PublicationSuccessor) predecessorKey() string {
	sum := sha256.Sum256([]byte(s.PredecessorItemID))
	return hex.EncodeToString(sum[:])
}

func (s PublicationSuccessor) PublicationID() InvocationID {
	if s.commandless() {
		return InvocationID("publish-reentry-" + s.predecessorKey())
	}
	if s.EffectiveOrigin() == PublicationSuccessorRemediation {
		return InvocationID("publish-continuation-" + s.CommandID)
	}
	return InvocationID("publish-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) Key() string {
	return "publication-successor/" + url.PathEscape(string(s.RunID)) + "/" + string(s.PublicationID())
}

func (s PublicationSuccessor) TaskKey() string {
	if s.commandless() {
		return "production-publication-reentry/" + url.PathEscape(string(s.RunID)) + "/" + s.predecessorKey()
	}
	if s.EffectiveOrigin() == PublicationSuccessorRemediation {
		return "production-publication-continuation/" + url.PathEscape(string(s.RunID)) + "/" + s.CommandID
	}
	return "production-publication-successor/" + url.PathEscape(string(s.RunID)) + "/" + s.CommandID
}

func (s PublicationSuccessor) ReadyItemID() ItemID {
	if s.commandless() {
		return ItemID("production-ready-reentry-" + s.predecessorKey())
	}
	if s.EffectiveOrigin() == PublicationSuccessorRemediation {
		return ItemID("production-ready-continuation-" + s.CommandID)
	}
	return ItemID("production-ready-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) BlockedItemID() ItemID {
	if s.commandless() {
		return ItemID("production-blocked-reentry-" + s.predecessorKey())
	}
	if s.EffectiveOrigin() == PublicationSuccessorRemediation {
		return ItemID("production-blocked-continuation-" + s.CommandID)
	}
	return ItemID("production-blocked-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) Validate() error {
	if s.RunID == "" || s.PredecessorItemID == "" ||
		s.PriorReviewInvocationID == "" || s.ReviewRound < 2 {
		return ErrParentKeyMismatch
	}
	if s.Version == PublicationHandoffConflictVersion {
		if s.Origin != PublicationSuccessorHandoffConflict || s.CommandID != "" ||
			s.FeedbackInvocationID != "" || s.ReevaluationCommandID != "" ||
			s.ExternalFindingID != "" || s.AdmittingProfileDigest != "" ||
			s.Reentry == nil || s.Reentry.Reason != "" || s.Reentry.BaseSHA == "" || s.Reentry.HeadSHA == "" ||
			s.Reentry.BaseSHA == s.Reentry.HeadSHA || !s.HandoffConflictCause.valid() {
			return ErrParentKeyMismatch
		}
		return nil
	}
	if s.HandoffConflictCause != "" || s.FirstPublication {
		return ErrParentKeyMismatch
	}
	if s.Version == PublicationExternalReviewVersion || s.Version == PublicationHandBackVersion {
		origin := PublicationSuccessorExternalReview
		if s.Version == PublicationHandBackVersion {
			origin = PublicationSuccessorHandBack
		}
		if s.Origin != origin || s.CommandID != "" ||
			s.FeedbackInvocationID != "" || s.ReevaluationCommandID != "" ||
			s.Reentry == nil || s.Reentry.Reason != "" || s.Reentry.BaseSHA == "" || s.Reentry.HeadSHA == "" ||
			s.ExternalFindingID == "" || s.AdmittingProfileDigest == "" {
			return ErrParentKeyMismatch
		}
		return nil
	}
	if s.ExternalFindingID != "" || s.AdmittingProfileDigest != "" {
		return ErrParentKeyMismatch
	}
	if s.Version == PublicationUnpublishedReentryVersion || s.Version == PublicationSuccessorReentryVersion {
		origin := PublicationSuccessorUnpublishedBaseAdvance
		if s.Version == PublicationSuccessorReentryVersion {
			origin = PublicationSuccessorBaseAdvance
		}
		if s.Origin != origin || s.CommandID != "" ||
			s.FeedbackInvocationID != "" || s.ReevaluationCommandID != "" ||
			s.Reentry == nil || s.Reentry.Reason != ReadinessInvalidationBaseAdvanced || !s.Reentry.valid() {
			return ErrParentKeyMismatch
		}
		return nil
	}
	if s.Version == PublicationReentryVersion {
		if s.Origin != PublicationSuccessorReadinessInvalidation || s.CommandID != "" ||
			s.FeedbackInvocationID != "" || s.ReevaluationCommandID != "" ||
			s.Reentry == nil || !s.Reentry.valid() {
			return ErrParentKeyMismatch
		}
		return nil
	}
	if s.CommandID == "" || s.Reentry != nil {
		return ErrParentKeyMismatch
	}
	if s.Version == PublicationSuccessorVersion {
		if s.Origin != "" || s.ReevaluationCommandID != "" || s.FeedbackInvocationID == "" {
			return ErrParentKeyMismatch
		}
		return nil
	}
	if s.Version != PublicationContinuationVersion || !s.Origin.valid() {
		return ErrParentKeyMismatch
	}
	switch s.Origin {
	case PublicationSuccessorFeedback:
		if s.FeedbackInvocationID == "" || s.ReevaluationCommandID != "" {
			return ErrParentKeyMismatch
		}
	case PublicationSuccessorRemediation:
		if s.FeedbackInvocationID != "" || s.ReevaluationCommandID == "" {
			return ErrParentKeyMismatch
		}
	case PublicationSuccessorReadinessInvalidation, PublicationSuccessorExternalReview,
		PublicationSuccessorUnpublishedBaseAdvance, PublicationSuccessorBaseAdvance,
		PublicationSuccessorHandoffConflict, PublicationSuccessorHandBack:
		return ErrParentKeyMismatch
	}
	return nil
}

// RepublishesUnpublished reports whether the authority re-enters a run that
// never published, so its cycle makes the run's first publication rather than
// updating a pull request.
func (s PublicationSuccessor) RepublishesUnpublished() bool {
	return s.Version == PublicationUnpublishedReentryVersion &&
		s.Origin == PublicationSuccessorUnpublishedBaseAdvance
}

// MakesFirstPublication reports whether the cycle publishes the run's first
// pull request rather than updating one: an unpublished re-entry (v5), or a
// handoff conflict remediation (v7) of a run that never published.
func (s PublicationSuccessor) MakesFirstPublication() bool {
	return s.RepublishesUnpublished() ||
		(s.RemediatesHandoffConflict() && s.FirstPublication)
}

// CarriesForeignHead reports whether the authority's head is one no Freeside
// invocation produced: a head_changed readiness re-entry adopts the head
// someone pushed to the branch. A handoff conflict on such a head gets no
// remediation (issue #2646), since rewriting it would overwrite their work.
//
// A hand_back head is foreign too, but the authority itself is the
// permission to adopt it (issue #3160), so it is not refused as one.
func (s PublicationSuccessor) CarriesForeignHead() bool {
	return s.Reentry != nil && s.Reentry.Reason == ReadinessInvalidationHeadChanged
}

// HandsBack reports whether the authority reworks a pull request the lander
// handed back (v8).
func (s PublicationSuccessor) HandsBack() bool {
	return s.EffectiveOrigin() == PublicationSuccessorHandBack
}

// AnswersExternalFindings reports whether the cycle answers admitted
// external findings in its first round: an external_review re-entry (v4), or
// a hand_back one (v8), whose findings are the hand-back comment and any other
// admitted comment on the adopted head. The two share the first round's
// adjudication, dispositions, and producer gates.
func (s PublicationSuccessor) AnswersExternalFindings() bool {
	origin := s.EffectiveOrigin()
	return origin == PublicationSuccessorExternalReview || origin == PublicationSuccessorHandBack
}

// InPlaceHandoffID names the handoff check a hand_back cycle runs on its
// adopted head before it re-earns readiness. It is not the cycle's
// publication ID: a fix the cycle starts publishes under that ID and hands
// its own head off there, and a publication has one handoff check.
func (s PublicationSuccessor) InPlaceHandoffID() InvocationID {
	return InvocationID("handoff-" + string(s.PublicationID()))
}

// RemediatesHandoffConflict reports whether the authority remediates a head
// whose handoff check conflicted with the target's tip (v7).
func (s PublicationSuccessor) RemediatesHandoffConflict() bool {
	return s.Version == PublicationHandoffConflictVersion &&
		s.Origin == PublicationSuccessorHandoffConflict
}

// ReentersUnpublishedHead reports whether the authority re-enters, at a newer
// base, a head that was verified and reviewed but never published: v5 for the
// run's first publication, v6 for a successor of a published pull request.
// Both keep the head and review its prospective merge, and neither admits
// remediation; only v5 makes a first publication.
func (s PublicationSuccessor) ReentersUnpublishedHead() bool {
	return s.RepublishesUnpublished() || (s.Version == PublicationSuccessorReentryVersion &&
		s.Origin == PublicationSuccessorBaseAdvance)
}

// AdmitsSuccessorReentry reports whether a base-advance refusal of this
// cycle's unpublished head may seal a successor re-entry (v6) after it. The
// cycle must have built or kept a head of its own and update an existing pull
// request: a feedback return, a remediation continuation, or an earlier
// successor re-entry. An in-place re-entry pushes nothing of its own, and an
// unpublished re-entry, which opens the pull request, re-enters as v5.
func (s PublicationSuccessor) AdmitsSuccessorReentry() bool {
	switch s.EffectiveOrigin() {
	case PublicationSuccessorFeedback, PublicationSuccessorRemediation, PublicationSuccessorBaseAdvance:
		return true
	case PublicationSuccessorReadinessInvalidation, PublicationSuccessorExternalReview,
		PublicationSuccessorUnpublishedBaseAdvance, PublicationSuccessorHandoffConflict,
		PublicationSuccessorHandBack:
		return false
	}
	return false
}

// AllowsRemediation binds the first continuation producer to the recheck's
// findings round. Later producers obey the cycle's ordinary review floor. A
// re-entered cycle admits remediation after a base advance or an external
// review and never after a head change: a remediation candidate is rebuilt
// from the admitted base, so on a head Freeside did not produce it would
// replace the commits someone else pushed. The store refuses an
// external_review authority on such a head for the same reason. The authority
// is for one base, so every request must be for findings reviewed against it,
// and the cycle's first review must be of the head it re-entered for. A
// re-entry before publication admits none: a remediation is admitted at the
// base the composition derives, the run's admitted one, and a fix built there
// would be a candidate on the base the re-entry left behind. A successor's
// re-entry (v6) admits none for the same reason.
//
// A hand_back re-entry (v8) adopts a head no Freeside invocation produced,
// and admits remediation on it all the same: its authority is the permission
// to replace that head, and its cycle verified and reviewed it first.
//
// A handoff conflict (v7) is the one authority whose remediations are
// admitted at a base other than the run's: every one is at the tip it
// sealed. Its first answers the conflict on the prior review's round, on the
// head that conflicted; the rest answer its own review rounds.
func (s PublicationSuccessor) AllowsRemediation(r RemediationInvocationIntent) bool {
	if r.RunID != s.RunID || r.SuccessorPublicationID != s.PublicationID() || s.ReentersUnpublishedHead() {
		return false
	}
	if s.RemediatesHandoffConflict() {
		if r.BaseSHA != s.Reentry.BaseSHA {
			return false
		}
		if r.Round == s.ReviewRound-1 {
			return s.AdmitsHandoffConflictRemediation(r)
		}
		return r.Round >= s.ReviewRound
	}
	if s.Reentry != nil {
		ownHead := s.Reentry.Reason == ReadinessInvalidationBaseAdvanced ||
			s.AnswersExternalFindings()
		if !ownHead || r.BaseSHA != s.Reentry.BaseSHA ||
			(r.Round == s.ReviewRound && r.HeadSHA != s.Reentry.HeadSHA) {
			return false
		}
		return r.Round >= s.ReviewRound
	}
	return r.Round >= s.ReviewRound || (s.EffectiveOrigin() == PublicationSuccessorRemediation &&
		r.Round == s.ReviewRound-1 && r.ReviewInvocationID == s.PriorReviewInvocationID)
}

// AdmitsHandoffConflictRemediation reports whether r is the cycle's first
// remediation: the one that answers the conflict itself rather than a
// review. It names no adjudication, because no round adjudicated it, and it
// is on the prior review's round and invocation and the head that
// conflicted, at the sealed tip.
func (s PublicationSuccessor) AdmitsHandoffConflictRemediation(r RemediationInvocationIntent) bool {
	return s.RemediatesHandoffConflict() && r.RunID == s.RunID &&
		r.SuccessorPublicationID == s.PublicationID() &&
		r.Round == s.ReviewRound-1 && r.ReviewInvocationID == s.PriorReviewInvocationID &&
		r.BaseSHA == s.Reentry.BaseSHA && r.HeadSHA == s.Reentry.HeadSHA &&
		r.AdjudicationDigest == "" && len(r.FindingIDs) == 0
}

func DecodePublicationSuccessor(body []byte) (PublicationSuccessor, error) {
	var s PublicationSuccessor
	if err := strictjson.Decode(body, &s, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20)); err != nil {
		return s, err
	}
	if err := s.Validate(); err != nil {
		return s, err
	}
	canonical, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	if !bytes.Equal(canonical, body) {
		return s, ErrParentKeyMismatch
	}
	return s, nil
}
