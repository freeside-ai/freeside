package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
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

type supersessionReviewSource struct {
	exec.ReviewSource
	err error
}

func (s supersessionReviewSource) VerifyReviewRequestSupersession(
	context.Context, domain.InvocationID, exec.ReviewRequest,
) error {
	return s.err
}

// A finished review whose request was persisted before reviewers received
// attachments stands under the authority it was persisted with, so the clean
// pass neither wedges on the re-gate nor reruns the round. Every other answer
// keeps the current authority, and a source failure keeps the gate pending.
func TestPersistedReviewAuthorityLetsAPreInputsReviewStand(t *testing.T) {
	t.Parallel()
	req := testReviewRequestWithInputs(t)
	current, err := req.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	withoutInputs := req
	withoutInputs.Inputs = nil
	persisted, err := withoutInputs.AuthorityDigest()
	if err != nil || persisted == current {
		t.Fatalf("inputs do not change the authority: %v", err)
	}
	other := errors.New("journal unavailable")
	superseded := &exec.ReviewSourceFailure{
		Class: domain.ReviewFailureTransient, Err: exec.ErrSupersededReviewRequest,
	}
	for _, tc := range []struct {
		name   string
		source exec.ReviewSource
		req    exec.ReviewRequest
		want   domain.Digest
		err    error
	}{
		{name: "no_verifier", source: &transientCleanupReviewSource{}, req: req, want: current},
		{name: "current", source: supersessionReviewSource{}, req: req, want: current},
		{name: "unknown", source: supersessionReviewSource{err: exec.ErrUnknownInvocation}, req: req, want: current},
		{name: "pre_inputs", source: supersessionReviewSource{err: superseded}, req: req, want: persisted},
		{name: "no_inputs_never_asks", source: supersessionReviewSource{err: other}, req: withoutInputs, want: current},
		{name: "failure", source: supersessionReviewSource{err: other}, req: req, err: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &productionPublicationWorkflow{reviewSource: tc.source}
			got, err := w.persistedReviewAuthority(t.Context(), "review-1", tc.req, current)
			if !errors.Is(err, tc.err) || (tc.err == nil && (err != nil || got != tc.want)) {
				t.Fatalf("authority = %q, %v; want %q, %v", got, err, tc.want, tc.err)
			}
		})
	}
}

func testReviewRequestWithInputs(t *testing.T) exec.ReviewRequest {
	t.Helper()
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 1, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Instructions: instructions,
		Admission:    *execfake.RecordAdmission(domain.RoleReviewer, "run-1", "review-1"),
		Verification: exec.ReviewVerificationEvidence{
			Outcome: domain.VerificationPassed, RecipeDigest: adjudicationDigest("a"),
			EvidenceSnapshotDigest: adjudicationDigest("b"),
		},
		Inputs: []domain.StageInputAttachment{{
			Name: "task/shot.png", Digest: adjudicationDigest("c"),
			MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: 2048,
		}},
		RequestedAt: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC),
	}
}
