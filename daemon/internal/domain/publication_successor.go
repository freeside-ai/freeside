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
)

type PublicationSuccessorOrigin string

const (
	PublicationSuccessorFeedback    PublicationSuccessorOrigin = "feedback"
	PublicationSuccessorRemediation PublicationSuccessorOrigin = "remediation_continuation"
	// PublicationSuccessorReadinessInvalidation re-enters verification and
	// review for a ready item the daemon invalidated (issue #1622). No operator
	// command starts it, so its identities key on the superseded item.
	PublicationSuccessorReadinessInvalidation PublicationSuccessorOrigin = "readiness_invalidation"
)

var AllPublicationSuccessorOrigins = []PublicationSuccessorOrigin{
	PublicationSuccessorFeedback, PublicationSuccessorRemediation, PublicationSuccessorReadinessInvalidation,
}

func (o PublicationSuccessorOrigin) valid() bool {
	switch o {
	case PublicationSuccessorFeedback, PublicationSuccessorRemediation, PublicationSuccessorReadinessInvalidation:
		return true
	default:
		return false
	}
}

// PublicationSuccessorReentry names what an invalidated ready item re-enters
// for: the base and head already on its pull request. A re-entered cycle
// builds no candidate and pushes nothing; it re-earns readiness in place
// (devlog/2026-10-02-0222-readiness-reentry-authority.md).
type PublicationSuccessorReentry struct {
	Reason  ReadinessInvalidationReason `json:"reason"`
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
// approval, or a readiness invalidation. Remediation may produce a later
// candidate under this authority, but cannot change its predecessor or review
// floor.
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
	case PublicationSuccessorReadinessInvalidation:
		return ErrParentKeyMismatch
	}
	return nil
}

// AllowsRemediation binds the first continuation producer to the recheck's
// findings round. Later producers obey the cycle's ordinary review floor. A
// re-entered cycle admits remediation only after a base advance: a remediation
// candidate is rebuilt from the admitted base, so on a head Freeside did not
// produce it would replace the commits someone else pushed. The authority is
// for one base, so every request must be for findings reviewed against it, and
// the cycle's first review must be of the head it re-entered for.
func (s PublicationSuccessor) AllowsRemediation(r RemediationInvocationIntent) bool {
	if r.RunID != s.RunID || r.SuccessorPublicationID != s.PublicationID() {
		return false
	}
	if s.Reentry != nil {
		if s.Reentry.Reason != ReadinessInvalidationBaseAdvanced || r.BaseSHA != s.Reentry.BaseSHA ||
			(r.Round == s.ReviewRound && r.HeadSHA != s.Reentry.HeadSHA) {
			return false
		}
		return r.Round >= s.ReviewRound
	}
	return r.Round >= s.ReviewRound || (s.EffectiveOrigin() == PublicationSuccessorRemediation &&
		r.Round == s.ReviewRound-1 && r.ReviewInvocationID == s.PriorReviewInvocationID)
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
