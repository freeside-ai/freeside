package signet

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// EvidenceBlobWriter is the content-addressed write half proposal evidence
// needs. *BlobStore and the engine's artifact store both satisfy it.
type EvidenceBlobWriter interface {
	Put(domain.Digest, io.Reader) (bool, error)
}

// PutProposalEvidence persists one proposal's evidence in the caller's
// transaction: the canonical body in the blob store first, then its artifact
// row. The order means a blob failure aborts the row, so a checkpoint's
// artifact closure never names a proposal body that was never written. A nil
// writer fails closed with ErrAttachmentsUnavailable. The writer verifies the
// bytes against the digest, so a body can only land under the address it
// hashes to; a body written in a transaction that later rolls back is a
// harmless immutable orphan, as with PutStageInput.
func PutProposalEvidence(
	ctx context.Context,
	tx *store.WriteTx,
	blobs EvidenceBlobWriter,
	instance domain.ProposalInstance,
) (domain.Artifact, error) {
	if blobs == nil {
		return domain.Artifact{}, ErrAttachmentsUnavailable
	}
	artifact, body, err := instance.EvidenceArtifactBody()
	if err != nil {
		return domain.Artifact{}, err
	}
	if _, err := blobs.Put(artifact.Digest, bytes.NewReader(body)); err != nil {
		return domain.Artifact{}, fmt.Errorf("proposal evidence %q body: %w", instance.ID, err)
	}
	if err := tx.PutArtifact(ctx, artifact); err != nil {
		return domain.Artifact{}, err
	}
	return artifact, nil
}

// evidenceBlobs adapts the service's optional blob store, keeping an absent
// store a nil interface so PutProposalEvidence fails closed instead of
// dereferencing a nil *BlobStore.
func (s *Service) evidenceBlobs() EvidenceBlobWriter {
	if s.blobs == nil {
		return nil
	}
	return s.blobs
}
