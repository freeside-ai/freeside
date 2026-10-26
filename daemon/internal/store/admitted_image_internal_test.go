package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func testAdmittedImagePointer(image domain.ProjectImage) domain.AdmittedImagePointer {
	return domain.AdmittedImagePointer{
		RepositoryID: image.RepositoryID, Repository: image.Repository,
		Harness: domain.HarnessClientClaudeCode, ImageID: image.ID,
		ImageRef: image.ImageRef, AdvancedAt: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	}
}

func readAdmittedImagePointer(
	t *testing.T, s *Store, repositoryID int64,
) (domain.AdmittedImagePointer, domain.ProjectImage, bool, error) {
	t.Helper()
	var (
		pointer domain.AdmittedImagePointer
		image   domain.ProjectImage
		found   bool
	)
	err := s.Read(context.Background(), func(tx *ReadTx) error {
		var err error
		pointer, image, found, err = tx.GetAdmittedImagePointer(
			context.Background(), repositoryID, domain.HarnessClientClaudeCode)
		return err
	})
	return pointer, image, found, err
}

func TestAdmittedImagePointerAdvancesToRecordedImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openProjectImageStore(t)
	image := testProjectImage(t)
	if _, _, found, err := readAdmittedImagePointer(t, s, image.RepositoryID); err != nil || found {
		t.Fatalf("empty store: found=%v err=%v, want absent", found, err)
	}
	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pointer := testAdmittedImagePointer(image)
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordProjectImage(ctx, image); err != nil {
			return err
		}
		return tx.AdvanceAdmittedImagePointer(ctx, pointer)
	}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("revision %d -> %d, want daemon-internal write", before.Revision, after.Revision)
	}
	got, gotImage, found, err := readAdmittedImagePointer(t, s, image.RepositoryID)
	if err != nil || !found || got != pointer || gotImage.ID != image.ID {
		t.Fatalf("read = %+v image %s found=%v err=%v, want %+v", got, gotImage.ID, found, err, pointer)
	}

	// A second advance replaces the pointer for the same key.
	next, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: image.Repository, RepositoryID: image.RepositoryID,
		CommitSHA: strings.Repeat("e", 40), RecipeDigest: image.RecipeDigest,
		PreparationCommand: image.PreparationCommand, BaseImageRef: image.BaseImageRef,
		ImageRef: domain.ImageRef("example.test/project@sha256:" + strings.Repeat("d", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	advanced := testAdmittedImagePointer(next)
	advanced.AdvancedAt = pointer.AdvancedAt.Add(time.Hour)
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordProjectImage(ctx, next); err != nil {
			return err
		}
		return tx.AdvanceAdmittedImagePointer(ctx, advanced)
	}); err != nil {
		t.Fatalf("advance again: %v", err)
	}
	if got, _, _, err := readAdmittedImagePointer(t, s, image.RepositoryID); err != nil || got != advanced {
		t.Fatalf("after second advance = %+v err=%v, want %+v", got, err, advanced)
	}
}

func TestAdmittedImagePointerRefusesUnrecordedImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openProjectImageStore(t)
	image := testProjectImage(t)
	foreign, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeside-ai/other", RepositoryID: 42,
		CommitSHA: image.CommitSHA, RecipeDigest: image.RecipeDigest,
		PreparationCommand: image.PreparationCommand, BaseImageRef: image.BaseImageRef,
		ImageRef: domain.ImageRef("example.test/other@sha256:" + strings.Repeat("f", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordProjectImage(ctx, foreign)
	}); err != nil {
		t.Fatal(err)
	}
	foreignForOurs := testAdmittedImagePointer(foreign)
	foreignForOurs.RepositoryID, foreignForOurs.Repository = image.RepositoryID, image.Repository
	for name, pointer := range map[string]domain.AdmittedImagePointer{
		// The builder never recorded this image at all.
		"unrecorded": testAdmittedImagePointer(image),
		// Recorded, but for another repository.
		"foreign repository": foreignForOurs,
	} {
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.AdvanceAdmittedImagePointer(ctx, pointer)
		})
		if !errors.Is(err, ErrAdmittedImageUnrecorded) {
			t.Fatalf("%s: advance err = %v, want ErrAdmittedImageUnrecorded", name, err)
		}
	}
	if _, _, found, err := readAdmittedImagePointer(t, s, image.RepositoryID); err != nil || found {
		t.Fatalf("after refusals: found=%v err=%v, want absent", found, err)
	}
}

func TestAdmittedImagePointerReadFailsClosed(t *testing.T) {
	t.Parallel()
	for name, tamper := range map[string]string{
		"image row deleted": `DELETE FROM project_images`,
		"image_id edited": `UPDATE admitted_image_pointers SET image_id = 'sha256:` +
			strings.Repeat("0", 64) + `'`,
		"body names another image": `UPDATE admitted_image_pointers
SET body = replace(body, image_id, 'sha256:` + strings.Repeat("0", 64) + `'),
    image_id = 'sha256:` + strings.Repeat("0", 64) + `'`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openProjectImageStore(t)
			image := testProjectImage(t)
			if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				if err := tx.RecordProjectImage(ctx, image); err != nil {
					return err
				}
				return tx.AdvanceAdmittedImagePointer(ctx, testAdmittedImagePointer(image))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, tamper); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			_, _, found, err := readAdmittedImagePointer(t, s, image.RepositoryID)
			if !errors.Is(err, ErrRowInconsistent) || found {
				t.Fatalf("read after tamper: found=%v err=%v, want ErrRowInconsistent", found, err)
			}
		})
	}
}
