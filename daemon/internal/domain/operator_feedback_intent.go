package domain

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const OperatorFeedbackInvocationIntentVersion = "freeside.operator-feedback-request/v1"

// OperatorFeedbackInvocationIntent is shared by the producer and readers of
// retained feedback history. Its wire shape also predates these readers.
type OperatorFeedbackInvocationIntent struct {
	Version             string       `json:"version"`
	InvocationID        InvocationID `json:"invocation_id"`
	RunID               RunID        `json:"run_id"`
	StageID             StageID      `json:"stage_id"`
	CommandID           string       `json:"command_id"`
	ItemID              ItemID       `json:"item_id"`
	SourceInvocationID  InvocationID `json:"source_invocation_id"`
	InputArtifactID     ArtifactID   `json:"input_artifact_id"`
	InputArtifactDigest Digest       `json:"input_artifact_digest"`
	BaseSHA             string       `json:"base_sha,omitempty"`
	HeadSHA             string       `json:"head_sha,omitempty"`
}

func (r OperatorFeedbackInvocationIntent) Validate() error {
	if r.Version != OperatorFeedbackInvocationIntentVersion || r.RunID == "" || r.CommandID == "" ||
		r.ItemID == "" || r.SourceInvocationID == "" ||
		r.InvocationID != InvocationID("inv-operator-feedback-"+r.CommandID) ||
		r.StageID != StageID("operator-feedback-"+string(r.InvocationID)) ||
		r.InputArtifactID != ArtifactID("operator-feedback-"+r.CommandID) ||
		!contentaddr.Valid(string(r.InputArtifactDigest)) {
		return ErrParentKeyMismatch
	}
	validSHA := func(s string) bool { return len(s) == 40 && strings.Trim(s, "0123456789abcdef") == "" }
	if (r.BaseSHA == "") != (r.HeadSHA == "") ||
		(r.BaseSHA != "" && (!validSHA(r.BaseSHA) || !validSHA(r.HeadSHA))) {
		return ErrParentKeyMismatch
	}
	return nil
}

func DecodeOperatorFeedbackInvocationIntent(payload []byte) (OperatorFeedbackInvocationIntent, error) {
	var request OperatorFeedbackInvocationIntent
	if err := strictjson.Decode(payload, &request, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20)); err != nil {
		return request, err
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return request, err
	}
	if !bytes.Equal(canonical, payload) {
		return request, ErrParentKeyMismatch
	}
	return request, nil
}
