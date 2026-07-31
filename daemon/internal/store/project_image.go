package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordProjectImageSQL = `
INSERT INTO project_images
    (id, repository, repository_id, commit_sha, recipe_digest, base_image_ref, image_ref, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (image_ref) DO NOTHING`
	getProjectImageSQL = `
SELECT id, repository, repository_id, commit_sha, recipe_digest, base_image_ref, image_ref, body
FROM project_images WHERE id = ?`
	getProjectImageBodySQL     = `SELECT body FROM project_images WHERE image_ref = ?`
	projectImageRefRecordedSQL = `SELECT EXISTS(
SELECT 1 FROM project_images WHERE image_ref = ?)`
	listProjectImagesSQL = `
SELECT id, repository, repository_id, commit_sha, recipe_digest, base_image_ref, image_ref, body
FROM project_images WHERE repository_id = ? ORDER BY rowid`
)

// RecordProjectImage writes one immutable build result. Project images are
// daemon-internal runtime artifacts rather than synchronized client state, so
// this belongs on InternalTx and does not bump the server revision.
func (tx *InternalTx) RecordProjectImage(ctx context.Context, image domain.ProjectImage) error {
	body, err := encode(image)
	if err != nil {
		return fmt.Errorf("record project image %q: %w", image.ID, err)
	}
	if err := tx.putImmutable(ctx, recordProjectImageSQL, []any{
		image.ID, image.Repository, image.RepositoryID, image.CommitSHA,
		image.RecipeDigest, image.BaseImageRef, image.ImageRef, body,
	}, getProjectImageBodySQL, []any{image.ImageRef}, body); err != nil {
		return fmt.Errorf("record project image %q: %w", image.ID, err)
	}
	return nil
}

// ProjectImageRefRecorded reports whether any repository has durably claimed
// an image reference. Failure cleanup uses this global ownership check before
// deleting content-addressed image state shared across repository builds.
func (tx *ReadTx) ProjectImageRefRecorded(
	ctx context.Context, imageRef domain.ImageRef,
) (bool, error) {
	if imageRef == "" {
		return false, fmt.Errorf("lookup project image ref: empty image_ref")
	}
	var recorded bool
	if err := tx.tx.QueryRowContext(ctx, projectImageRefRecordedSQL, imageRef).
		Scan(&recorded); err != nil {
		return false, fmt.Errorf("lookup project image ref %q: %w", imageRef, err)
	}
	return recorded, nil
}

// GetProjectImage reconstructs one immutable project-image result.
func (tx *ReadTx) GetProjectImage(ctx context.Context, id domain.Digest) (domain.ProjectImage, error) {
	image, err := scanProjectImage(tx.tx.QueryRowContext(ctx, getProjectImageSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProjectImage{}, fmt.Errorf("get project image %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("get project image %q: %w", id, err)
	}
	if image.ID != id {
		return domain.ProjectImage{}, fmt.Errorf("get project image %q: %w", id, errRowInconsistent)
	}
	return image, nil
}

// ListProjectImages returns one repository's build results in insertion order.
func (tx *ReadTx) ListProjectImages(ctx context.Context, repositoryID int64) ([]domain.ProjectImage, error) {
	rows, err := tx.tx.QueryContext(ctx, listProjectImagesSQL, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("list project images for repository %d: %w", repositoryID, err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err reports deferred-close failures
	var images []domain.ProjectImage
	for rows.Next() {
		image, err := scanProjectImage(rows)
		if err != nil {
			return nil, fmt.Errorf("list project images for repository %d: %w", repositoryID, err)
		}
		if image.RepositoryID != repositoryID {
			return nil, fmt.Errorf("list project images for repository %d: %w", repositoryID, errRowInconsistent)
		}
		images = append(images, image)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list project images for repository %d: %w", repositoryID, err)
	}
	return images, nil
}

func scanProjectImage(row scanner) (domain.ProjectImage, error) {
	var (
		id, repository, commitSHA, recipeDigest string
		baseImageRef, imageRef                  string
		repositoryID                            int64
		body                                    []byte
	)
	if err := row.Scan(&id, &repository, &repositoryID, &commitSHA,
		&recipeDigest, &baseImageRef, &imageRef, &body); err != nil {
		return domain.ProjectImage{}, err
	}
	image, err := decode[domain.ProjectImage](body)
	if err != nil {
		return domain.ProjectImage{}, err
	}
	if string(image.ID) != id || image.Repository != repository ||
		image.RepositoryID != repositoryID || image.CommitSHA != commitSHA ||
		string(image.RecipeDigest) != recipeDigest ||
		string(image.BaseImageRef) != baseImageRef || string(image.ImageRef) != imageRef {
		return domain.ProjectImage{}, errRowInconsistent
	}
	return image, nil
}
