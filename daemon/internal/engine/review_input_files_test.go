package engine

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// A reviewer receives the task's attachments, in submitted order and under
// task/, and nothing from the discussion: a user's discussion attachment is
// writer conversation input. A run whose task has no attachments yields nil,
// so its review request keeps the shape and authority digest it had before
// review inputs existed.
func TestReviewInputFilesAreTheTaskAttachments(t *testing.T) {
	t.Parallel()
	f := newInputFilesFixture(t)
	w := &productionPublicationWorkflow{store: f.store}
	runID := f.binding.run.ID
	none, err := w.reviewInputFiles(t.Context(), runID)
	if err != nil || none != nil {
		t.Fatalf("review inputs without attachments = %#v, %v; want nil", none, err)
	}
	task := []domain.TaskAttachment{
		taskAttachment("screens/home.png", domain.TaskAttachmentMediaPNG, []byte("\x89PNG\r\n\x1a\nhome")),
		taskAttachment("A.png", domain.TaskAttachmentMediaPNG, []byte("\x89PNG\r\n\x1a\nupper")),
		taskAttachment("a.png", domain.TaskAttachmentMediaPNG, []byte("\x89PNG\r\n\x1a\nlower")),
	}
	f.putTaskAttachments(t, task)
	f.bindConversation(t, 1, inputMessage(t, 1, domain.AuthorUser, f.putBlob(t, []byte("\x89PNG\r\n\x1a\nsaid"))))
	got, err := w.reviewInputFiles(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]domain.StageInputAttachment, len(task))
	for i, a := range task {
		want[i] = domain.StageInputAttachment{
			Digest: a.Digest, Name: "task/" + a.Name, MediaType: a.MediaType, SizeBytes: a.SizeBytes,
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("review inputs = %#v, want %#v", got, want)
	}
	again, err := w.reviewInputFiles(t.Context(), runID)
	if err != nil || !slices.Equal(again, got) {
		t.Fatalf("rebuilt review inputs = %#v, %v; want the same files", again, err)
	}
}
