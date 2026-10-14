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

// RestoreProposalEvidence writes every effect-proposal evidence body missing
// from blobs, rebuilt from the durable instance and authenticated revision
// rows, and returns how many it wrote. It repairs stores written before
// proposal paths persisted their bodies; the daemon runs it at startup before
// its first backup pass. A present blob is left untouched even when corrupt:
// this is recovery of absent bodies, not repair, and Verify still reports a
// corrupt one. Any proposal row the store refuses to reconstruct or any blob
// error fails the pass; the read re-gates every instance and revision, not
// only those an artifact row names.
func RestoreProposalEvidence(ctx context.Context, st *store.Store, blobs *BlobStore) (int, error) {
	if blobs == nil {
		return 0, ErrAttachmentsUnavailable
	}
	var evidence []domain.ProposalInstance
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		evidence, err = tx.ListProposalEvidence(ctx)
		return err
	}); err != nil {
		return 0, fmt.Errorf("restore proposal evidence: %w", err)
	}
	written := 0
	for _, instance := range evidence {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		artifact, body, err := instance.EvidenceArtifactBody()
		if err != nil {
			return written, fmt.Errorf("restore proposal evidence %q: %w", instance.ID, err)
		}
		present, err := blobs.Has(artifact.Digest)
		if err != nil {
			return written, fmt.Errorf("restore proposal evidence %q: %w", instance.ID, err)
		}
		if present {
			continue
		}
		created, err := blobs.Put(artifact.Digest, bytes.NewReader(body))
		if err != nil {
			return written, fmt.Errorf("restore proposal evidence %q: %w", instance.ID, err)
		}
		if created {
			written++
		}
	}
	return written, nil
}
