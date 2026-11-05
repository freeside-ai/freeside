package stage

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func testAttachments() []domain.StageInputAttachment {
	return []domain.StageInputAttachment{
		{Digest: digestOf([]byte("png")), Name: "task/shot.png", MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: 3},
		{Digest: digestOf([]byte("notes")), Name: "discussion/2/1.bin", MediaType: domain.TaskAttachmentMediaOctetStream, SizeBytes: 5},
	}
}

func withAttachments(in intent, list []domain.StageInputAttachment) intent {
	snapshot := *in.Spec.StageInputs
	snapshot.Attachments = list
	in.Spec.StageInputs = &snapshot
	return in
}

// The prompt's attachment list comes from the admitted spec, so a stored
// intent re-renders the same prompt at regate, and the provider cannot
// mutate the intent through it.
func TestPromptInputsCarryAdmittedAttachments(t *testing.T) {
	t.Parallel()
	list := testAttachments()
	spec := exec.StartSpec{StageInputs: &domain.StageInputSnapshot{Attachments: list}}
	got := providerPromptInputsFrom(durableInputs{}, spec)
	if !slices.Equal(got.Attachments, list) {
		t.Fatalf("prompt attachments = %+v, want %+v", got.Attachments, list)
	}
	got.Attachments[0].Name = "task/other.png"
	if spec.StageInputs.Attachments[0].Name != "task/shot.png" {
		t.Fatal("provider prompt inputs alias the admitted attachments")
	}
	if want := []ward.InputFile{
		{Name: "task/shot.png", Digest: list[0].Digest, SizeBytes: 3},
		{Name: "discussion/2/1.bin", Digest: list[1].Digest, SizeBytes: 5},
	}; !slices.Equal(InputFilesFor(spec), want) {
		t.Fatalf("input files = %+v, want %+v", InputFilesFor(spec), want)
	}
	if InputFilesFor(exec.StartSpec{}) != nil || InputFilesFor(exec.StartSpec{StageInputs: &domain.StageInputSnapshot{}}) != nil {
		t.Fatal("a start without attachments named input files")
	}
}

func TestProviderCannotChangeAdmittedInputFiles(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"none", "missing", "extra", "reordered", "renamed", "resized", "redigested", "argument_delivery", "without_attachments"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			d := newTestDriver(t, &stubGate{}, newStubExports())
			in := withAttachments(testHandoffIntent(t, d), testAttachments())
			in.PromptDelivery = PromptFileV6
			if change == "argument_delivery" {
				in.PromptDelivery = PromptArgument
			}
			if change == "without_attachments" {
				in = withAttachments(in, nil)
			}
			admitted := InputFilesFor(in.Spec)
			d.provider = testProvider{handoffMutate: func(hs *ward.HandoffSpec) {
				if in.PromptDelivery != PromptArgument {
					hs.Agent.PromptFile = ward.NewPromptFile([]byte(in.Prompt))
				}
				files := slices.Clone(InputFilesFor(in.Spec))
				switch change {
				case "missing":
					files = files[:1]
				case "extra":
					files = append(files, ward.InputFile{Name: "task/extra", Digest: files[0].Digest, SizeBytes: 3})
				case "reordered":
					files[0], files[1] = files[1], files[0]
				case "renamed":
					files[0].Name = "task/Shot.png"
				case "resized":
					files[0].SizeBytes++
				case "redigested":
					files[0].Digest = files[1].Digest
				case "without_attachments":
					files = []ward.InputFile{{Name: "task/smuggled", Digest: digestOf([]byte("x")), SizeBytes: 1}}
				}
				hs.Agent.Inputs = files
			}}
			hs, err := d.handoffSpec(t.Context(), in)
			if change == "none" {
				if err != nil || !slices.Equal(hs.Agent.Inputs, admitted) {
					t.Fatalf("admitted input files refused or changed: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("provider input change accepted: %v", err)
			}
		})
	}
}
