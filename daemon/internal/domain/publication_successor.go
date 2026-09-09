package domain

import (
	"bytes"
	"encoding/json"
	"net/url"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	PublicationSuccessorKind       = "publication_successor_authority"
	PublicationSuccessorVersion    = "freeside.publication-successor/v1"
	PublicationContinuationVersion = "freeside.publication-successor/v2"
)

type PublicationSuccessorOrigin string

const (
	PublicationSuccessorFeedback    PublicationSuccessorOrigin = "feedback"
	PublicationSuccessorRemediation PublicationSuccessorOrigin = "remediation_continuation"
)

var AllPublicationSuccessorOrigins = []PublicationSuccessorOrigin{PublicationSuccessorFeedback, PublicationSuccessorRemediation}

func (o PublicationSuccessorOrigin) valid() bool {
	switch o {
	case PublicationSuccessorFeedback, PublicationSuccessorRemediation:
		return true
	default:
		return false
	}
}

// PublicationSuccessor binds a cycle to an accepted feedback return or recheck
// approval. Remediation may produce a later candidate under this authority,
// but cannot change its predecessor or review floor.
type PublicationSuccessor struct {
	Version                 string                     `json:"version"`
	RunID                   RunID                      `json:"run_id"`
	CommandID               string                     `json:"command_id"`
	FeedbackInvocationID    InvocationID               `json:"feedback_invocation_id"`
	PredecessorItemID       ItemID                     `json:"predecessor_item_id"`
	PriorReviewInvocationID InvocationID               `json:"prior_review_invocation_id"`
	ReviewRound             int                        `json:"review_round"`
	Origin                  PublicationSuccessorOrigin `json:"origin,omitempty"`
	ReevaluationCommandID   string                     `json:"reevaluation_command_id,omitempty"`
}

// EffectiveOrigin preserves the byte-identical v1 feedback representation.
func (s PublicationSuccessor) EffectiveOrigin() PublicationSuccessorOrigin {
	if s.Version == PublicationSuccessorVersion || s.Origin == "" {
		return PublicationSuccessorFeedback
	}
	return s.Origin
}

func (s PublicationSuccessor) PublicationID() InvocationID {
	if s.EffectiveOrigin() == PublicationSuccessorRemediation {
		return InvocationID("publish-continuation-" + s.CommandID)
	}
	return InvocationID("publish-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) Key() string {
	return "publication-successor/" + url.PathEscape(string(s.RunID)) + "/" + string(s.PublicationID())
}

func (s PublicationSuccessor) TaskKey() string {
	if s.EffectiveOrigin() == PublicationSuccessorRemediation {
		return "production-publication-continuation/" + url.PathEscape(string(s.RunID)) + "/" + s.CommandID
	}
	return "production-publication-successor/" + url.PathEscape(string(s.RunID)) + "/" + s.CommandID
}

func (s PublicationSuccessor) ReadyItemID() ItemID {
	if s.EffectiveOrigin() == PublicationSuccessorRemediation {
		return ItemID("production-ready-continuation-" + s.CommandID)
	}
	return ItemID("production-ready-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) BlockedItemID() ItemID {
	if s.EffectiveOrigin() == PublicationSuccessorRemediation {
		return ItemID("production-blocked-continuation-" + s.CommandID)
	}
	return ItemID("production-blocked-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) Validate() error {
	if s.RunID == "" || s.CommandID == "" || s.PredecessorItemID == "" ||
		s.PriorReviewInvocationID == "" || s.ReviewRound < 2 {
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
	}
	return nil
}

// AllowsRemediation binds the first continuation producer to the recheck's
// findings round. Later producers obey the cycle's ordinary review floor.
func (s PublicationSuccessor) AllowsRemediation(r RemediationInvocationIntent) bool {
	if r.RunID != s.RunID || r.SuccessorPublicationID != s.PublicationID() {
		return false
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
