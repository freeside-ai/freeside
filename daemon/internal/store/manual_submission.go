package store

import (
	"context"
	"errors"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func (tx *ReadTx) GetManualSubmission(ctx context.Context, identity string) (domain.ManualSubmission, error) {
	s := domain.ManualSubmission{Identity: identity}
	err := tx.tx.QueryRowContext(ctx, `SELECT project_id, source_artifact_id, source_digest,
		request_digest, implementation_run_id FROM manual_submissions WHERE identity = ?`, identity).
		Scan(&s.ProjectID, &s.SourceArtifactID, &s.SourceDigest, &s.RequestDigest, &s.ImplementationRunID)
	if err != nil {
		return s, notFoundOr(err)
	}
	if err := s.Validate(); err != nil {
		return s, errRowInconsistent
	}
	a, err := tx.GetArtifact(ctx, s.SourceArtifactID)
	if err != nil {
		return s, err
	}
	if a.Type != domain.ArtifactKindSpecification || a.Digest != s.SourceDigest {
		return s, errRowInconsistent
	}
	return s, nil
}

// AcceptManualSubmission atomically binds the request and its task. Callers
// create the specification run and command result in this same transaction.
func (tx *WriteTx) AcceptManualSubmission(ctx context.Context, s domain.ManualSubmission) (domain.Task, error) {
	if err := s.Validate(); err != nil {
		return domain.Task{}, err
	}
	if existing, err := tx.GetManualSubmission(ctx, s.Identity); err == nil {
		if existing != s {
			return domain.Task{}, ErrImmutableConflict
		}
	} else if !errors.Is(err, ErrNotFound) {
		return domain.Task{}, err
	} else if _, err := tx.tx.ExecContext(ctx, `INSERT INTO manual_submissions
		(identity, project_id, source_artifact_id, source_digest, request_digest, implementation_run_id)
		VALUES (?, ?, ?, ?, ?, ?)`, s.Identity, s.ProjectID, s.SourceArtifactID, s.SourceDigest,
		s.RequestDigest, s.ImplementationRunID); err != nil {
		return domain.Task{}, err
	}
	source := domain.SpecificationSource{Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: s.SourceArtifactID}
	return tx.getOrCreateTask(ctx, s.ProjectID, "submission:"+s.Identity, &source, time.Now().UTC())
}
