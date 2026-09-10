package stage

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// failedWriterResult consumes diagnostic evidence without ever importing the
// writer's source tree. Blobs and the invocation-bound claim precede terminal
// persistence; retries use the same immutable timestamp and claim identity.
func (d *Driver) failedWriterResult(ctx context.Context, in intent, recovered *ward.RecoveryResult) (exec.StageResult, error) {
	result := exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusFailed,
		Summary: fmt.Sprintf("%s writer exited with status %d.", d.displayName, recovered.FailureStatus),
	}
	if recovered.FailureStatus == d.provider.PrepareFailedStatus() {
		result.Summary = fmt.Sprintf("Workspace preparation failed before the agent started (status %d): the project-image hydration helper exited nonzero.", recovered.FailureStatus)
	}
	if recovered.FailureStatus < 1 || recovered.FailureStatus > 255 || recovered.Outcome != ward.RecoveryFailed ||
		recovered.ExportDir != "" || len(recovered.Manifest.Entries) != 0 {
		return result, fmt.Errorf("%w: invalid failed writer result", ErrRecoveryRetryable)
	}
	evidence := recovered.FailureEvidence
	if evidence == nil {
		return result, nil
	}
	if evidence.Unavailable {
		if evidence.Digest != "" || len(evidence.Body) != 0 {
			return result, fmt.Errorf("%w: conflicting failure evidence", ErrRecoveryRetryable)
		}
		result.Summary += " Diagnostic transcript unavailable: capture was absent or refused."
		return result, nil
	}
	if len(evidence.Body) > ward.MaxFailureTranscriptBytes || !utf8.Valid(evidence.Body) ||
		contentaddr.Sum(evidence.Body) != evidence.Digest {
		return result, fmt.Errorf("%w: invalid failure transcript", ErrRecoveryRetryable)
	}
	digest := domain.Digest(evidence.Digest)
	claim := domain.AgentClaim{
		Label:    "agent-transcript",
		Artifact: domain.ArtifactID("artifact-failed-transcript-" + string(in.InvocationID)),
		Digest:   digest,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: in.InvocationID, HeadBinding: domain.HeadIndependent,
			SensitivityClass: domain.SensitivitySensitive,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextPlain,
			SizeBytes: int64(len(evidence.Body)), CreatedAt: in.RecordedAt,
			Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
	if err := claim.Validate(); err != nil {
		return result, fmt.Errorf("%w: invalid failure claim: %w", ErrRecoveryRetryable, err)
	}
	if err := d.artifacts.PutBlob(ctx, digest, evidence.Body); err != nil {
		return result, fmt.Errorf("%w: persist failure transcript: %w", ErrRecoveryRetryable, err)
	}
	if err := d.artifacts.RecordClaims(ctx, in.InvocationID, []domain.AgentClaim{claim}); err != nil {
		return result, fmt.Errorf("%w: persist failure claim: %w", ErrRecoveryRetryable, err)
	}
	result.Artifacts = []domain.Digest{digest}
	result.Summary += " Diagnostic transcript retained as sensitive evidence."
	return result, nil
}
