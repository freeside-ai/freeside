package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ErrAdmittedImagePointerInvalid marks a malformed admitted-image pointer.
var ErrAdmittedImagePointerInvalid = errors.New("admitted image pointer is invalid")

// AdmittedImagePointer names the project image new work on one repository and
// agent base starts from (plan §5.7, Project-image upkeep). It is keyed by the
// forge's stable repository identity and the harness, so one pointer survives
// a base-image digest bump: the upkeep job advances it to the rebuilt image
// rather than leaving a pointer per base digest behind.
//
// The pointer selects an image; it never vouches for one. It carries no
// admissibility verdict, because every AdmissibleAt boundary re-derives
// admissibility from the image record and the exact base tree, and a verdict
// stored here would go stale the moment the default branch advanced. ImageID
// and ImageRef name the write-once project_images row the store requires
// before it advances the pointer and re-reads whenever it decodes one.
type AdmittedImagePointer struct {
	RepositoryID int64             `json:"repository_id"`
	Repository   string            `json:"repository"`
	Harness      HarnessClientKind `json:"harness"`
	ImageID      Digest            `json:"image_id"`
	ImageRef     ImageRef          `json:"image_ref"`
	AdvancedAt   time.Time         `json:"advanced_at"`
}

// Validate checks the pointer's own shape. Whether the image row exists and
// agrees is the store's check, since only it can read the row in the same
// transaction.
func (p AdmittedImagePointer) Validate() error {
	if p.RepositoryID <= 0 {
		return fmt.Errorf("admitted image pointer repository_id %d: %w", p.RepositoryID, ErrNonPositive)
	}
	if !projectRepositoryPattern.MatchString(p.Repository) ||
		strings.Contains(p.Repository, "..") {
		return fmt.Errorf("admitted image pointer repository %q: %w",
			p.Repository, ErrAdmittedImagePointerInvalid)
	}
	if !p.Harness.valid() {
		return fmt.Errorf("admitted image pointer harness %q: %w", p.Harness, ErrInvalidHarnessClientKind)
	}
	if !contentaddr.Valid(string(p.ImageID)) {
		return fmt.Errorf("admitted image pointer image_id %q: %w",
			p.ImageID, ErrAdmittedImagePointerInvalid)
	}
	if err := p.ImageRef.Validate(); err != nil {
		return fmt.Errorf("admitted image pointer image_ref: %w", err)
	}
	if p.AdvancedAt.IsZero() {
		return fmt.Errorf("admitted image pointer advanced_at: %w", ErrMissingTimestamp)
	}
	if p.AdvancedAt.Location() != time.UTC {
		return fmt.Errorf("admitted image pointer advanced_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// Names reports whether the pointer names exactly this image record: the same
// ID, repository, and produced image. The store uses it on both the advance
// and the read path.
func (p AdmittedImagePointer) Names(image ProjectImage) bool {
	return p.ImageID == image.ID && p.ImageRef == image.ImageRef &&
		p.RepositoryID == image.RepositoryID && p.Repository == image.Repository
}
