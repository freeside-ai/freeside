package domain

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const OperatorFeedbackInvocationIntentVersion = "freeside.operator-feedback-request/v1"

const OperatorFeedbackRetryIntentVersion = "freeside.operator-feedback-request/v2"

// OperatorFeedbackRetry binds a fresh invocation to the command that retried
// one failed invocation. The enclosing intent retains the original feedback
// command and artifact coordinates; a retry never invents another input.
type OperatorFeedbackRetry struct {
	CommandID          string       `json:"command_id"`
	ItemID             ItemID       `json:"item_id"`
	FailedInvocationID InvocationID `json:"failed_invocation_id"`
}

// OperatorFeedbackInvocationIntent is shared by the producer and readers of
// retained feedback history. Its wire shape also predates these readers.
type OperatorFeedbackInvocationIntent struct {
	Version             string                 `json:"version"`
	InvocationID        InvocationID           `json:"invocation_id"`
	RunID               RunID                  `json:"run_id"`
	StageID             StageID                `json:"stage_id"`
	CommandID           string                 `json:"command_id"`
	ItemID              ItemID                 `json:"item_id"`
	SourceInvocationID  InvocationID           `json:"source_invocation_id"`
	InputArtifactID     ArtifactID             `json:"input_artifact_id"`
	InputArtifactDigest Digest                 `json:"input_artifact_digest"`
	BaseSHA             string                 `json:"base_sha,omitempty"`
	HeadSHA             string                 `json:"head_sha,omitempty"`
	Retry               *OperatorFeedbackRetry `json:"retry,omitempty"`
}

func (r OperatorFeedbackInvocationIntent) Validate() error {
	invocationCommand := r.CommandID
	if r.Retry == nil {
		if r.Version != OperatorFeedbackInvocationIntentVersion {
			return ErrParentKeyMismatch
		}
	} else {
		if r.Version != OperatorFeedbackRetryIntentVersion || r.Retry.CommandID == "" ||
			r.Retry.ItemID != ItemID("execution-failure-"+string(r.Retry.FailedInvocationID)) ||
			!strings.HasPrefix(string(r.Retry.FailedInvocationID), "inv-operator-feedback-") ||
			r.Retry.FailedInvocationID == r.InvocationID {
			return ErrParentKeyMismatch
		}
		invocationCommand = r.Retry.CommandID
	}
	if r.RunID == "" || r.CommandID == "" ||
		r.ItemID == "" || r.SourceInvocationID == "" ||
		r.InvocationID != InvocationID("inv-operator-feedback-"+invocationCommand) ||
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
