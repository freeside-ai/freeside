package domain

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	PublicationContinuationRequestedKind = "publication_continuation_requested"
	PublicationContinuationIntentVersion = "freeside.publication-continuation/v1"
	PublicationContinuationItemPrefix    = "successor-reevaluation-review-"
)

// PublicationReevaluationRequest is shared by Signet's acceptance boundary and
// the store's continuation authenticator; its existing wire shape is unchanged.
type PublicationReevaluationRequest struct {
	RunID              RunID  `json:"run_id"`
	ItemID             ItemID `json:"item_id"`
	ItemVersion        int    `json:"item_version"`
	CommandID          string `json:"command_id"`
	PRHeadSHA          string `json:"pr_head_sha"`
	TrustProfileDigest Digest `json:"trust_profile_digest"`
	ReviewRound        int    `json:"review_round"`
}

type PublicationReevaluationCompletion struct {
	RunID                RunID                          `json:"run_id"`
	CommandID            string                         `json:"command_id"`
	IntentKey            string                         `json:"intent_key"`
	Outcome              PublicationReevaluationOutcome `json:"outcome"`
	PRHeadSHA            string                         `json:"pr_head_sha"`
	EvidenceItemID       ItemID                         `json:"evidence_item_id"`
	EvidenceItemVersion  int                            `json:"evidence_item_version"`
	TerminalInvocationID InvocationID                   `json:"terminal_invocation_id"`
}

type PublicationReevaluationOutcome string

const (
	PublicationReevaluationPublished       PublicationReevaluationOutcome = "published"
	PublicationReevaluationBlocked         PublicationReevaluationOutcome = "blocked"
	PublicationReevaluationReviewEscalated PublicationReevaluationOutcome = "review_escalated"
)

var AllPublicationReevaluationOutcomes = []PublicationReevaluationOutcome{
	PublicationReevaluationPublished, PublicationReevaluationBlocked, PublicationReevaluationReviewEscalated,
}

func (o PublicationReevaluationOutcome) valid() bool {
	switch o {
	case PublicationReevaluationPublished, PublicationReevaluationBlocked, PublicationReevaluationReviewEscalated:
		return true
	default:
		return false
	}
}

func (c PublicationReevaluationCompletion) Validate() error {
	if c.RunID == "" || c.CommandID == "" || c.IntentKey == "" || !c.Outcome.valid() ||
		c.PRHeadSHA == "" || c.EvidenceItemID == "" || c.EvidenceItemVersion < 1 ||
		c.TerminalInvocationID != InvocationID("production-reevaluation-terminal/"+c.CommandID) {
		return ErrParentKeyMismatch
	}
	return nil
}

// PublicationContinuationIntent freezes the source task and findings answered
// by one accepted approve command. It never replaces the rechecked task.
type PublicationContinuationIntent struct {
	Version            string               `json:"version"`
	Successor          PublicationSuccessor `json:"successor"`
	SourceTaskKey      string               `json:"source_task_key"`
	SourceTaskDigest   Digest               `json:"source_task_digest"`
	AdjudicationDigest Digest               `json:"adjudication_digest"`
}

func (r PublicationContinuationIntent) Key() string {
	return "publication-continuation/" + url.PathEscape(string(r.Successor.RunID)) + "/" + r.Successor.CommandID
}

func PublicationContinuationCoordinates(key string) (RunID, string, bool) {
	rest, ok := strings.CutPrefix(key, "publication-continuation/")
	if !ok {
		return "", "", false
	}
	encoded, command, ok := strings.Cut(rest, "/")
	run, err := url.PathUnescape(encoded)
	return RunID(run), command, ok && err == nil && run != "" && command != "" && url.PathEscape(run) == encoded
}

func (r PublicationContinuationIntent) Validate() error {
	if r.Version != PublicationContinuationIntentVersion || r.Successor.Validate() != nil ||
		r.Successor.EffectiveOrigin() != PublicationSuccessorRemediation || r.SourceTaskKey == "" ||
		!contentaddr.Valid(string(r.SourceTaskDigest)) || !contentaddr.Valid(string(r.AdjudicationDigest)) {
		return ErrParentKeyMismatch
	}
	return nil
}

func DecodePublicationContinuationIntent(body []byte) (PublicationContinuationIntent, error) {
	var r PublicationContinuationIntent
	if err := strictjson.Decode(body, &r, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20)); err != nil {
		return r, err
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	canonical, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	if !bytes.Equal(canonical, body) {
		return r, ErrParentKeyMismatch
	}
	return r, nil
}
