package domain

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
