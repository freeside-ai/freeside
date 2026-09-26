package domain

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// RemediationInvocationIntent is the retained dispatch contract shared by the
// engine and successor publication authentication.
type RemediationInvocationIntent struct {
	Version                string       `json:"version"`
	InvocationID           InvocationID `json:"invocation_id"`
	RunID                  RunID        `json:"run_id"`
	StageID                StageID      `json:"stage_id"`
	Round                  int          `json:"round"`
	ReviewInvocationID     InvocationID `json:"review_invocation_id"`
	AdjudicationDigest     Digest       `json:"adjudication_digest"`
	InputArtifactID        ArtifactID   `json:"input_artifact_id"`
	InputArtifactDigest    Digest       `json:"input_artifact_digest"`
	BaseSHA                string       `json:"base_sha"`
	HeadSHA                string       `json:"head_sha"`
	FindingIDs             []FindingID  `json:"finding_ids"`
	SuccessorPublicationID InvocationID `json:"successor_publication_id,omitempty"`
}

// RemediationInvocationID and RemediationStageID are the workflow-owned
// identities of one round's remediator. The engine mints them and read
// boundaries re-derive them, so they live here rather than in either package.
func RemediationInvocationID(runID RunID, round int) InvocationID {
	return InvocationID(fmt.Sprintf("inv-remediate-%d-%s", round, runID))
}

func RemediationStageID(runID RunID, round int) StageID {
	return StageID(fmt.Sprintf("remediate-%d-%s", round, runID))
}

// DecodeRemediationInvocationIntent strictly decodes a remediation dispatch
// payload. It proves shape only; AuthenticateInvocationDispatchIntent binds it
// to an invocation, run and stage.
func DecodeRemediationInvocationIntent(payload []byte) (RemediationInvocationIntent, error) {
	var request RemediationInvocationIntent
	if err := strictjson.Decode(payload, &request, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		return RemediationInvocationIntent{}, err
	}
	return request, nil
}
