package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// RecordRequiredCheckFailure stores the daemon's record of a published
// head's failed required check under its key and returns the digest a
// check_failure authority names (issue #3598). Replaying the same record is
// a no-op; another record under the same key is refused, because one
// failure of one head is recorded once.
func (tx *WriteTx) RecordRequiredCheckFailure(
	ctx context.Context, failure domain.RequiredCheckFailure,
) (domain.Digest, error) {
	body, digest, err := failure.Encode()
	if err != nil {
		return "", err
	}
	entry, _, err := tx.EnqueueOutbox(ctx, failure.Key(), domain.RequiredCheckFailureKind, body)
	if err != nil {
		return "", err
	}
	if entry.Kind != domain.RequiredCheckFailureKind || !bytes.Equal(entry.Payload, body) {
		return "", fmt.Errorf("required check failure %s disagrees with its stored record: %w",
			failure.Key(), domain.ErrImmutableTransition)
	}
	if err := tx.MarkOutboxDispatched(ctx, failure.Key()); err != nil {
		return "", err
	}
	return digest, nil
}

// GetRequiredCheckFailure returns the run's failure record whose content
// address is digest. The record is decoded strictly and checked against its
// own key and run, so a row stored under another key does not answer.
func (tx *ReadTx) GetRequiredCheckFailure(
	ctx context.Context, run domain.RunID, digest domain.Digest,
) (domain.RequiredCheckFailure, error) {
	prefix := domain.RequiredCheckFailureKeyPrefix(run)
	entries, err := tx.listOutboxQuery(ctx, `SELECT id, idempotency_key, kind, payload,
		payload_version, payload_digest, status, created_at FROM outbox
		WHERE kind = ? AND status = ? AND payload_digest = ? AND substr(idempotency_key, 1, length(?)) = ? ORDER BY id`,
		domain.RequiredCheckFailureKind, outboxStatusDispatched,
		domain.RequiredCheckFailureKind, outboxStatusDispatched, string(digest), prefix, prefix)
	if err != nil {
		return domain.RequiredCheckFailure{}, err
	}
	if len(entries) != 1 {
		return domain.RequiredCheckFailure{}, fmt.Errorf("required check failure %s of run %s: %w",
			digest, run, errors.Join(ErrNotFound, domain.ErrParentKeyMismatch))
	}
	failure, err := domain.DecodeRequiredCheckFailure(entries[0].Payload)
	if err != nil || failure.RunID != run || failure.Key() != entries[0].IdempotencyKey {
		return domain.RequiredCheckFailure{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return failure, nil
}
