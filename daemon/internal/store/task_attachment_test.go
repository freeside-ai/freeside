package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func sampleTaskAttachments() []domain.TaskAttachment {
	return []domain.TaskAttachment{
		{
			Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			Name:   "screenshot.png", MediaType: domain.TaskAttachmentMediaPNG,
			ClaimedMediaType: "image/png", SizeBytes: 4096,
		},
		{
			Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			Name:   "design_handoff/tokens/colors.json", MediaType: domain.TaskAttachmentMediaOctetStream,
			ClaimedMediaType: "application/json", SizeBytes: 0,
		},
	}
}

func putTaskAttachments(ctx context.Context, s *store.Store, taskID domain.TaskID, list []domain.TaskAttachment) error {
	return s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutTaskAttachments(ctx, taskID, list) })
}

func readTaskAttachments(ctx context.Context, s *store.Store, taskID domain.TaskID) ([]domain.TaskAttachment, error) {
	var list []domain.TaskAttachment
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		list, err = tx.TaskAttachments(ctx, taskID)
		return err
	})
	return list, err
}

// A task's attachments round-trip in submitted order, not sorted by name or
// digest, and are written once: a second list is refused and the first stays.
func TestTaskAttachmentsRoundTripInOrderAndWriteOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	if got, err := readTaskAttachments(ctx, s, taskID); err != nil || len(got) != 0 {
		t.Fatalf("attachments before any = %v, %v", got, err)
	}
	if err := putTaskAttachments(ctx, s, taskID, nil); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	want := sampleTaskAttachments()
	if err := putTaskAttachments(ctx, s, taskID, want); err != nil {
		t.Fatal(err)
	}
	if got, err := readTaskAttachments(ctx, s, taskID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("attachments = %+v, %v; want %+v", got, err, want)
	}
	if err := putTaskAttachments(ctx, s, taskID, want[:1]); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("second list: err = %v, want %v", err, store.ErrImmutableConflict)
	}
	if got, err := readTaskAttachments(ctx, s, taskID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("attachments after refused rewrite = %+v, %v", got, err)
	}
}

// A list that breaks a rule writes no row at all.
func TestTaskAttachmentsRefuseAnInvalidListWhole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	list := sampleTaskAttachments()
	list = append(list, list[0])
	if err := putTaskAttachments(ctx, s, taskID, list); !errors.Is(err, domain.ErrTaskAttachmentNameConflict) {
		t.Fatalf("duplicate name: err = %v", err)
	}
	if got, err := readTaskAttachments(ctx, s, taskID); err != nil || len(got) != 0 {
		t.Fatalf("attachments after refusal = %v, %v", got, err)
	}
}

// A row edited or removed in place fails the read loudly (Tier B).
func TestTaskAttachmentsFailLoudlyOnABadRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tamper := range map[string]string{
		"traversal name":   `UPDATE task_attachments SET name = '../escape' WHERE position = 0`,
		"oversized image":  `UPDATE task_attachments SET size_bytes = 10485761 WHERE position = 0`,
		"uppercase digest": `UPDATE task_attachments SET digest = upper(digest) WHERE position = 1`,
		"claim parameters": `UPDATE task_attachments SET claimed_media_type = 'text/plain; charset=x' WHERE position = 1`,
		"removed first":    `DELETE FROM task_attachments WHERE position = 0`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "task-attachments.db")
			s := openStoreAt(t, path, store.Options{})
			taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
			if err := putTaskAttachments(ctx, s, taskID, sampleTaskAttachments()); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.ExecContext(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			if _, err := readTaskAttachments(ctx, s, taskID); !errors.Is(err, store.ErrRowInconsistent) {
				t.Fatalf("read after tampering: err = %v, want %v", err, store.ErrRowInconsistent)
			}
		})
	}
}
