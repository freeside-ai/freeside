package domain

import (
	"bytes"
	"encoding/json"
	"net/url"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	PublicationSuccessorKind    = "publication_successor_authority"
	PublicationSuccessorVersion = "freeside.publication-successor/v1"
)

// PublicationSuccessor preserves the accepted return's predecessor and the
// first completed feedback export. Remediation may produce a later candidate
// under this authority, but cannot change its predecessor or review floor.
type PublicationSuccessor struct {
	Version                 string       `json:"version"`
	RunID                   RunID        `json:"run_id"`
	CommandID               string       `json:"command_id"`
	FeedbackInvocationID    InvocationID `json:"feedback_invocation_id"`
	PredecessorItemID       ItemID       `json:"predecessor_item_id"`
	PriorReviewInvocationID InvocationID `json:"prior_review_invocation_id"`
	ReviewRound             int          `json:"review_round"`
}

func (s PublicationSuccessor) PublicationID() InvocationID {
	return InvocationID("publish-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) Key() string {
	return "publication-successor/" + url.PathEscape(string(s.RunID)) + "/" + string(s.PublicationID())
}

func (s PublicationSuccessor) TaskKey() string {
	return "production-publication-successor/" + url.PathEscape(string(s.RunID)) + "/" + s.CommandID
}

func (s PublicationSuccessor) ReadyItemID() ItemID {
	return ItemID("production-ready-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) BlockedItemID() ItemID {
	return ItemID("production-blocked-feedback-" + s.CommandID)
}

func (s PublicationSuccessor) Validate() error {
	if s.Version != PublicationSuccessorVersion || s.RunID == "" || s.CommandID == "" ||
		s.FeedbackInvocationID == "" || s.PredecessorItemID == "" ||
		s.PriorReviewInvocationID == "" || s.ReviewRound < 2 {
		return ErrParentKeyMismatch
	}
	return nil
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
