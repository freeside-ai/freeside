package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrAdmittedImageUnrecorded is returned (wrapped) when a pointer advance
// names an image the trusted builder did not record for that repository.
var ErrAdmittedImageUnrecorded = errors.New("admitted image is not a recorded project image for the repository")

const (
	upsertAdmittedImagePointerSQL = `
INSERT INTO admitted_image_pointers
    (repository_id, harness, image_id, image_ref, advanced_at, body)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (repository_id, harness) DO UPDATE SET
    image_id = excluded.image_id,
    image_ref = excluded.image_ref,
    advanced_at = excluded.advanced_at,
    body = excluded.body`
	getAdmittedImagePointerSQL = `
SELECT repository_id, harness, image_id, image_ref, advanced_at, body
FROM admitted_image_pointers WHERE repository_id = ? AND harness = ?`
)

// AdvanceAdmittedImagePointer points one repository's agent base at a project
// image, creating or replacing the pointer. Only the trusted builder's output
// may be pointed at: the image's write-once project_images row must exist in
// this transaction and name the pointer's repository and image reference, or
// the advance fails with ErrAdmittedImageUnrecorded. Like project images, the
// pointer is daemon-internal, so this belongs on InternalTx and does not bump
// the server revision.
func (tx *InternalTx) AdvanceAdmittedImagePointer(
	ctx context.Context, pointer domain.AdmittedImagePointer,
) error {
	body, err := encode(pointer)
	if err != nil {
		return fmt.Errorf("advance admitted image pointer: %w", err)
	}
	image, err := tx.GetProjectImage(ctx, pointer.ImageID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("advance admitted image pointer to %q: %w", pointer.ImageID, ErrAdmittedImageUnrecorded)
	}
	if err != nil {
		return fmt.Errorf("advance admitted image pointer: %w", err)
	}
	if !pointer.Names(image) {
		return fmt.Errorf("advance admitted image pointer to %q for repository %d: %w",
			pointer.ImageID, pointer.RepositoryID, ErrAdmittedImageUnrecorded)
	}
	if _, err := tx.tx.ExecContext(ctx, upsertAdmittedImagePointerSQL,
		pointer.RepositoryID, pointer.Harness, pointer.ImageID, pointer.ImageRef,
		formatTime(pointer.AdvancedAt), body,
	); err != nil {
		return fmt.Errorf("advance admitted image pointer: %w", err)
	}
	return nil
}

// GetAdmittedImagePointer reconstructs one repository's pointer for an agent
// base together with the image record it names, reporting absence rather
// than erroring. A decoded pointer is never trusted from its row alone: the
// image row is re-read and must exist and agree, or the read fails closed
// with errRowInconsistent. The caller still decides admissibility from the
// returned record.
func (tx *ReadTx) GetAdmittedImagePointer(
	ctx context.Context, repositoryID int64, harness domain.HarnessClientKind,
) (domain.AdmittedImagePointer, domain.ProjectImage, bool, error) {
	var (
		rowRepositoryID                         int64
		rowHarness, imageID, imageRef, advanced string
		body                                    []byte
	)
	err := tx.tx.QueryRowContext(ctx, getAdmittedImagePointerSQL, repositoryID, harness).
		Scan(&rowRepositoryID, &rowHarness, &imageID, &imageRef, &advanced, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AdmittedImagePointer{}, domain.ProjectImage{}, false, nil
	}
	if err != nil {
		return domain.AdmittedImagePointer{}, domain.ProjectImage{}, false,
			fmt.Errorf("get admitted image pointer %d/%s: %w", repositoryID, harness, err)
	}
	pointer, err := decode[domain.AdmittedImagePointer](body)
	if err != nil {
		return domain.AdmittedImagePointer{}, domain.ProjectImage{}, false,
			fmt.Errorf("get admitted image pointer %d/%s: %w", repositoryID, harness, err)
	}
	if pointer.RepositoryID != repositoryID || pointer.Harness != harness ||
		rowRepositoryID != repositoryID || string(pointer.Harness) != rowHarness ||
		string(pointer.ImageID) != imageID || string(pointer.ImageRef) != imageRef ||
		formatTime(pointer.AdvancedAt) != advanced {
		return domain.AdmittedImagePointer{}, domain.ProjectImage{}, false,
			fmt.Errorf("get admitted image pointer %d/%s: %w", repositoryID, harness, errRowInconsistent)
	}
	image, err := tx.GetProjectImage(ctx, pointer.ImageID)
	if errors.Is(err, ErrNotFound) {
		return domain.AdmittedImagePointer{}, domain.ProjectImage{}, false,
			fmt.Errorf("get admitted image pointer %d/%s: image row missing: %w",
				repositoryID, harness, errRowInconsistent)
	}
	if err != nil {
		return domain.AdmittedImagePointer{}, domain.ProjectImage{}, false,
			fmt.Errorf("get admitted image pointer %d/%s: %w", repositoryID, harness, err)
	}
	if !pointer.Names(image) {
		return domain.AdmittedImagePointer{}, domain.ProjectImage{}, false,
			fmt.Errorf("get admitted image pointer %d/%s: image row disagrees: %w",
				repositoryID, harness, errRowInconsistent)
	}
	return pointer, image, true, nil
}
