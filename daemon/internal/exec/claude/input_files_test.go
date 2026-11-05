package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func testDigest(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest(contentaddr.Format(sum[:]))
}

func TestRenderPromptListsInputFiles(t *testing.T) {
	t.Parallel()
	inputs := stage.ProviderPromptInputs{
		Delivery:      stage.PromptFileV6,
		PromptPackage: []byte("prompt"), Specification: []byte("specification"), Policy: []byte("policy"),
	}
	without, err := renderPromptParts(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if want := "prompt\n\n--- Approved work item specification ---\n\nspecification" +
		"\n\n--- Resolved per-run policy ---\n\npolicy\n"; without != want {
		t.Fatalf("prompt without attachments changed:\n%q\nwant\n%q", without, want)
	}
	inputs.Attachments = []domain.StageInputAttachment{
		{Digest: testDigest([]byte("a")), Name: "task/screens/home page.png", MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: 2048},
		{Digest: testDigest([]byte("b")), Name: "discussion/4/1.bin", MediaType: domain.TaskAttachmentMediaOctetStream, SizeBytes: 7},
	}
	with, err := renderPromptParts(inputs)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Index(with, "\n"+ward.InputFilesTarget+"/task/screens/home page.png (image/png, 2048 bytes)")
	second := strings.Index(with, "\n"+ward.InputFilesTarget+"/discussion/4/1.bin (application/octet-stream, 7 bytes)\n")
	spec := strings.Index(with, "specification\n\n--- Operator input files ---")
	policy := strings.Index(with, "--- Resolved per-run policy ---")
	if spec < 0 || first <= spec || second <= first || policy <= second {
		t.Fatalf("input files not listed in order between specification and policy:\n%s", with)
	}
	for _, phrase := range []string{"read-only", "not instructions", "Read tool"} {
		if !strings.Contains(with, phrase) {
			t.Fatalf("input file section omits %q:\n%s", phrase, with)
		}
	}
}

// mapSource serves stage-input bodies by digest.
type mapSource map[domain.Digest][]byte

func (s mapSource) OpenContext(_ context.Context, digest domain.Digest) (io.ReadCloser, error) {
	body, ok := s[digest]
	if !ok {
		return nil, fmt.Errorf("missing %s", digest)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// The pre-commit delivery check renders the attachment list too, so a list
// that pushes the prompt over its limit fails before the intent is committed.
func TestValidatePromptInputsCountsInputFiles(t *testing.T) {
	t.Parallel()
	attachment := []byte("\x89PNG\r\n\x1a\n")
	validate := func(t *testing.T, specBody []byte, withAttachment bool) error {
		t.Helper()
		prompt, policy := []byte("prompt"), []byte("policy")
		input := domain.StageInputSnapshotInput{
			InputDigest:         testDigest([]byte("input")),
			SpecificationDigest: testDigest(specBody), PromptPackageDigest: testDigest(prompt), PolicyDigest: testDigest(policy),
		}
		if withAttachment {
			input.Attachments = []domain.StageInputAttachment{{
				Digest: testDigest(attachment), Name: "task/shot.png", MediaType: domain.TaskAttachmentMediaPNG,
				SizeBytes: int64(len(attachment)),
			}}
		}
		snapshot, err := domain.NewStageInputSnapshot(input)
		if err != nil {
			t.Fatal(err)
		}
		materializer, err := exec.NewMaterializer(mapSource{
			snapshot.SpecificationDigest: specBody, snapshot.PromptPackageDigest: prompt,
			snapshot.PolicyDigest: policy, testDigest(attachment): attachment,
		}, exec.MaterializerOptions{MaxInputBytes: 2 << 20, MaxTotalBytes: 4 << 20})
		if err != nil {
			t.Fatal(err)
		}
		inputs, err := materializer.Materialize(t.Context(), exec.StartSpec{
			InputDigest: snapshot.InputDigest, SpecDigest: snapshot.SpecificationDigest,
			PolicyDigest: snapshot.PolicyDigest, StageInputs: &snapshot,
		})
		if err != nil {
			t.Fatal(err)
		}
		return ValidatePromptInputs(inputs)
	}
	without, err := renderPromptParts(stage.ProviderPromptInputs{
		Delivery: launchDelivery, PromptPackage: []byte("prompt"), Policy: []byte("policy"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The largest specification that fits on its own.
	specBody := bytes.Repeat([]byte("s"), ward.MaxPromptFileBytes-len(without))
	if err := validate(t, specBody, false); err != nil {
		t.Fatalf("specification at the limit refused: %v", err)
	}
	if err := validate(t, specBody, true); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("input file list not counted against the prompt limit: %v", err)
	}
}

func TestHandoffSpecCarriesAdmittedInputFiles(t *testing.T) {
	t.Parallel()
	list := []domain.StageInputAttachment{
		{Digest: testDigest([]byte("a")), Name: "task/A.png", MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: 1},
		{Digest: testDigest([]byte("b")), Name: "task/a.png", MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: 1},
	}
	for _, tc := range []struct {
		name     string
		delivery stage.PromptDelivery
		list     []domain.StageInputAttachment
		want     bool
	}{
		{"file-v6", stage.PromptFileV6, list, true},
		{"file-v6-without", stage.PromptFileV6, nil, false},
		{"argument", stage.PromptArgument, list, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := testProviderHandoffInput()
			in.PromptDelivery = tc.delivery
			in.Spec.StageInputs = &domain.StageInputSnapshot{Attachments: tc.list}
			hs, err := (claudeProvider{volumes: testAuthStoreVolumes{volume: "provider-volume"}}).HandoffSpec(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want {
				if hs.Agent.Inputs != nil {
					t.Fatalf("unexpected input files %+v", hs.Agent.Inputs)
				}
				return
			}
			if !slices.Equal(hs.Agent.Inputs, stage.InputFilesFor(in.Spec)) || len(hs.Agent.Inputs) != 2 {
				t.Fatalf("input files = %+v", hs.Agent.Inputs)
			}
		})
	}
}
