package engine

import (
	"errors"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// loadVerificationReport reads the immutable verifier bytes, never a reconstructed
// copy of the execution result. A read failure can retry; contradictory evidence
// is a durable state failure even if wrapped by the production retry classifier.
func loadVerificationReport(artifacts ArtifactStore, records []domain.Artifact, invocation domain.InvocationID) ([]byte, error) {
	var report *domain.Artifact
	for i := range records {
		if records[i].Type != domain.ArtifactKindVerificationReport {
			continue
		}
		if report != nil || records[i].Provenance.ProducerInvocationID != invocation {
			return nil, fmt.Errorf("verification report count or invocation mismatch: %w", domain.ErrParentKeyMismatch)
		}
		report = &records[i]
	}
	if report == nil {
		return nil, fmt.Errorf("verification report missing: %w", domain.ErrParentKeyMismatch)
	}
	reader, err := artifacts.Open(report.Digest)
	if err != nil {
		return nil, fmt.Errorf("open verification report: %w", err)
	}
	const maxReportBytes = 1 << 20
	raw, readErr := io.ReadAll(io.LimitReader(reader, maxReportBytes+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, fmt.Errorf("read verification report: %w", err)
	}
	if len(raw) > maxReportBytes || domain.Digest(contentaddr.Sum(raw)) != report.Digest {
		return nil, fmt.Errorf("verification report size or digest mismatch: %w", domain.ErrParentKeyMismatch)
	}
	return raw, nil
}
