package stage

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// failedWriterResult consumes diagnostic evidence without ever importing the
// writer's source tree. Blobs and the invocation-bound claim precede terminal
// persistence; retries use the same immutable timestamp and claim identity.
// size is the writer's declared container size from the rebuilt handoff
// spec; a recorded memory-limit kill names it.
func (d *Driver) failedWriterResult(
	ctx context.Context, in intent, recovered *ward.RecoveryResult, size ward.ContainerSize,
) (exec.StageResult, error) {
	result := exec.StageResult{
		InvocationID: in.InvocationID, Status: exec.StatusFailed,
		Summary: fmt.Sprintf("%s writer exited with status %d.", d.displayName, recovered.FailureStatus),
	}
	if recovered.FailureStatus == d.provider.PrepareFailedStatus() {
		result.Summary = fmt.Sprintf("Workspace preparation failed before the agent started (status %d): the project-image hydration helper exited nonzero.", recovered.FailureStatus)
	}
	// The kill comes from ward's journal, not the live handoff error, so the
	// live path and a restart before the result commits name it alike.
	if recovered.MemoryLimitKill {
		result.Cause = domain.FailureCauseMemoryLimit
		result.Summary = truncateFailureSummary(fmt.Sprintf(
			"Writer container was killed at its memory limit (%s). %s", size, result.Summary))
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
	// Classified from the same verified bytes on the live and restart paths,
	// so both commit the same result. A memory-limit kill keeps its cause.
	if classifier, ok := d.provider.(QuotaRefusalClassifier); ok && result.Cause == "" {
		if refusal := classifier.ClassifyQuotaRefusal(evidence.Body); refusal != nil {
			result.Cause = domain.FailureCauseQuota
			result.QuotaRefusal = refusal
			result.Summary = truncateFailureSummary(quotaRefusalSummary(*refusal) + " " + result.Summary)
		}
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

// quotaRefusalSummary names the refused window and, when the provider gave
// one, its reset, ahead of the writer's exit status.
func quotaRefusalSummary(refusal domain.QuotaRefusal) string {
	if refusal.ResetsAt == nil {
		return fmt.Sprintf("Provider refused the turn on its %s quota window; it gave no reset.", refusal.Window)
	}
	return fmt.Sprintf("Provider refused the turn on its %s quota window until %s.",
		refusal.Window, refusal.ResetsAt.UTC().Format(time.RFC3339))
}
