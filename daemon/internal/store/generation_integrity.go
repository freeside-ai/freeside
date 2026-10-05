package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Credential-integrity marks (plan §5.4 admission rule 4, issue #1624).
// Daemon-internal like the generations they name: never synchronized, written
// on InternalTx, append-only.
//
// Trust posture: no record carries a marked or unmarked bit. A boundary that
// decides whether work may start asks RequireGenerationUnmarked in its own
// transaction, which reads these rows, so a decoded or caller-supplied
// "unmarked" claim has nowhere to live. A mark row is never read back on its
// own authority either: the generation it names must reconstruct, and every
// column is cross-checked against the validated body. A row that fails any of
// that is an error, never an absent mark.

const (
	insertGenerationIntegrityMarkSQL = `
INSERT INTO generation_integrity_marks
    (enrollment_id, ordinal, finding, observed_at, body)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (enrollment_id, ordinal, finding) DO NOTHING`
	listGenerationIntegrityMarksSQL = `
SELECT enrollment_id, ordinal, finding, observed_at, body
FROM generation_integrity_marks
WHERE enrollment_id = ? AND ordinal = ? ORDER BY finding`
)

// RecordGenerationIntegrityMark records that the credential-integrity probe
// found one generation's stored credential damaged, and returns the mark the
// store holds. The generation must exist and reconstruct (ErrNotFound when
// it does not).
//
// Recording is first-observation-wins, not write-once: a repeat of the same
// finding for the same generation keeps the first row and returns it, even
// though the repeat carries a later ObservedAt. The scheduled probe records
// on every pass, and putImmutable would report each later pass as
// ErrImmutableConflict.
func (tx *InternalTx) RecordGenerationIntegrityMark(
	ctx context.Context, mark domain.GenerationIntegrityMark,
) (domain.GenerationIntegrityMark, error) {
	fail := func(err error) (domain.GenerationIntegrityMark, error) {
		return domain.GenerationIntegrityMark{}, fmt.Errorf(
			"record generation integrity mark %q/%d: %w", mark.EnrollmentID, mark.Ordinal, err)
	}
	body, err := encode(mark)
	if err != nil {
		return fail(err)
	}
	if _, err := tx.GetEnrollmentGeneration(ctx, mark.EnrollmentID, mark.Ordinal); err != nil {
		return fail(err)
	}
	if _, err := tx.tx.ExecContext(ctx, insertGenerationIntegrityMarkSQL,
		mark.EnrollmentID, mark.Ordinal, mark.Finding, formatTime(mark.ObservedAt), body); err != nil {
		return fail(err)
	}
	marks, err := tx.GenerationIntegrityMarks(ctx, mark.EnrollmentID, mark.Ordinal)
	if err != nil {
		return fail(err)
	}
	for _, stored := range marks {
		if stored.Finding == mark.Finding {
			return stored, nil
		}
	}
	return fail(errRowInconsistent)
}

// GenerationIntegrityMarks reconstructs every mark on one generation, in
// finding order. The generation must itself reconstruct under its enrollment
// (ErrNotFound when it does not exist), so marks are never reported for a
// generation the store cannot read back. A generation with no marks returns
// an empty list.
func (tx *ReadTx) GenerationIntegrityMarks(
	ctx context.Context, id domain.ClientEnrollmentID, ordinal int,
) ([]domain.GenerationIntegrityMark, error) {
	fail := func(err error) ([]domain.GenerationIntegrityMark, error) {
		return nil, fmt.Errorf("generation integrity marks %q/%d: %w", id, ordinal, err)
	}
	generation, err := tx.GetEnrollmentGeneration(ctx, id, ordinal)
	if err != nil {
		return fail(err)
	}
	rows, err := tx.tx.QueryContext(ctx, listGenerationIntegrityMarksSQL, id, ordinal)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = rows.Close() }()
	var marks []domain.GenerationIntegrityMark
	for rows.Next() {
		mark, err := scanGenerationIntegrityMark(rows, generation)
		if err != nil {
			return fail(err)
		}
		marks = append(marks, mark)
	}
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	return marks, nil
}

// RequireGenerationUnmarked is the credential-integrity half of §5.4
// admission rule 4: it refuses with a *domain.GenerationIntegrityMarkedError
// when the generation carries a mark, naming the first in finding order.
// Unreadable mark state is its own error and never passes as unmarked.
//
// Like RequireBackendConformant it is a precondition on what starts next,
// not part of a recorded admission's meaning: role resolution and the
// admitting transaction call it, and reconstruction (scanExecutionAdmission)
// deliberately does not. A mark that lands after an admission was recorded
// must stop the next admission, not make recorded history unreadable.
func (tx *ReadTx) RequireGenerationUnmarked(
	ctx context.Context, id domain.ClientEnrollmentID, ordinal int,
) error {
	marks, err := tx.GenerationIntegrityMarks(ctx, id, ordinal)
	if err != nil {
		return err
	}
	if len(marks) > 0 {
		return &domain.GenerationIntegrityMarkedError{Mark: marks[0]}
	}
	return nil
}

// scanGenerationIntegrityMark is the single reconstruction path for mark
// rows: scan, decode and validate the body, cross-check every extracted
// column against it, and require the row to name the generation it was read
// for.
func scanGenerationIntegrityMark(
	row scanner, generation domain.EnrollmentGeneration,
) (domain.GenerationIntegrityMark, error) {
	var (
		enrollmentID string
		ordinal      int
		finding      string
		observedAt   string
		body         []byte
	)
	if err := row.Scan(&enrollmentID, &ordinal, &finding, &observedAt, &body); err != nil {
		return domain.GenerationIntegrityMark{}, err
	}
	mark, err := decode[domain.GenerationIntegrityMark](body)
	if err != nil {
		return domain.GenerationIntegrityMark{}, err
	}
	if string(mark.EnrollmentID) != enrollmentID ||
		mark.Ordinal != ordinal ||
		string(mark.Finding) != finding ||
		!timeColumnEqual(observedAt, mark.ObservedAt) ||
		mark.EnrollmentID != generation.EnrollmentID ||
		mark.Ordinal != generation.Ordinal {
		return domain.GenerationIntegrityMark{}, errRowInconsistent
	}
	return mark, nil
}
