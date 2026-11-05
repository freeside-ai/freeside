package domain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func stageDigest(fill string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(fill, 64))
}

func stageInputSnapshotInput() domain.StageInputSnapshotInput {
	vendorDigest := stageDigest("8")
	return domain.StageInputSnapshotInput{
		InputDigest:         stageDigest("1"),
		SpecificationDigest: stageDigest("2"),
		PromptPackageDigest: stageDigest("3"),
		PolicyDigest:        stageDigest("4"),
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor:   domain.AgentVendorCodex,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			Digest:   &vendorDigest,
		},
		PriorArtifactDigests: []domain.Digest{stageDigest("5"), stageDigest("6")},
		ImageInputDigests:    []domain.Digest{stageDigest("7")},
	}
}

func mustStageInputSnapshot(t *testing.T) domain.StageInputSnapshot {
	t.Helper()
	snapshot, err := domain.NewStageInputSnapshot(stageInputSnapshotInput())
	if err != nil {
		t.Fatalf("NewStageInputSnapshot: %v", err)
	}
	return snapshot
}

func TestNewStageInputSnapshotCanonicalAndDetached(t *testing.T) {
	in := stageInputSnapshotInput()
	snapshot, err := domain.NewStageInputSnapshot(in)
	if err != nil {
		t.Fatal(err)
	}
	in.PriorArtifactDigests[0] = "sha256:changed"
	in.ImageInputDigests[0] = "sha256:changed"
	*in.VendorInstructions.Digest = "sha256:changed"
	if snapshot.PriorArtifactDigests[0] != stageDigest("5") ||
		snapshot.ImageInputDigests[0] != stageDigest("7") ||
		*snapshot.VendorInstructions.Digest != stageDigest("8") {
		t.Fatal("snapshot followed caller-owned input slices")
	}

	empty, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         stageDigest("1"),
		SpecificationDigest: stageDigest("2"),
		PromptPackageDigest: stageDigest("3"),
		PolicyDigest:        stageDigest("4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "" ||
		string(body) == `{"prior_artifact_digests":null,"image_input_digests":null}` {
		t.Fatalf("empty snapshot did not serialize canonical arrays: %s", body)
	}
	if empty.PriorArtifactDigests == nil || empty.ImageInputDigests == nil {
		t.Fatal("empty input collections must be non-nil")
	}
	if empty.VendorInstructions != nil {
		t.Fatal("legacy constructor input unexpectedly claimed vendor instructions")
	}

	absentInput := stageInputSnapshotInput()
	absentInput.VendorInstructions.Digest = nil
	absent, err := domain.NewStageInputSnapshot(absentInput)
	if err != nil {
		t.Fatal(err)
	}
	absentBody, err := json.Marshal(absent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(absentBody), `"vendor_instructions":{"vendor":"codex","delivery":"append_file","digest":null}`) {
		t.Fatalf("explicit absence is not serialized: %s", absentBody)
	}
	if absent.ID == empty.ID {
		t.Fatal("explicit vendor-instruction absence collapsed onto a legacy snapshot")
	}

	repeated := stageInputSnapshotInput()
	repeated.PriorArtifactDigests = append(
		repeated.PriorArtifactDigests, repeated.PriorArtifactDigests[0])
	if _, err := domain.NewStageInputSnapshot(repeated); err != nil {
		t.Fatalf("repeated content in distinct ordered inputs must remain representable: %v", err)
	}
}

func TestStageInputSnapshotValidate(t *testing.T) {
	valid := mustStageInputSnapshot(t)
	cases := []struct {
		name    string
		edit    func(*domain.StageInputSnapshot)
		wantErr error
	}{
		{"valid", func(*domain.StageInputSnapshot) {}, nil},
		{"missing prompt", func(s *domain.StageInputSnapshot) {
			s.PromptPackageDigest = ""
		}, domain.ErrEmptyField},
		{"unknown vendor", func(s *domain.StageInputSnapshot) {
			s.VendorInstructions.Vendor = "unknown"
		}, domain.ErrUnsupportedVendorInstructionBinding},
		{"missing binding", func(s *domain.StageInputSnapshot) {
			s.VendorInstructions.Delivery = ""
		}, domain.ErrUnsupportedVendorInstructionBinding},
		{"replace-authority binding", func(s *domain.StageInputSnapshot) {
			s.VendorInstructions.Delivery = "instructions_key"
		}, domain.ErrUnsupportedVendorInstructionBinding},
		{"malformed vendor digest", func(s *domain.StageInputSnapshot) {
			digest := domain.Digest("sha256:not-hex")
			s.VendorInstructions.Digest = &digest
		}, domain.ErrStageInputsNotCanonical},
		{"nil prior artifacts", func(s *domain.StageInputSnapshot) {
			s.PriorArtifactDigests = nil
		}, domain.ErrStageInputsNotCanonical},
		{"empty image", func(s *domain.StageInputSnapshot) {
			s.ImageInputDigests[0] = ""
		}, domain.ErrEmptyField},
		{"retargeted artifact", func(s *domain.StageInputSnapshot) {
			s.PriorArtifactDigests[0] = stageDigest("f")
		}, domain.ErrStageInputDigestMismatch},
		{"malformed artifact", func(s *domain.StageInputSnapshot) {
			s.PriorArtifactDigests[0] = "sha256:not-hex"
		}, domain.ErrStageInputsNotCanonical},
		{"cleared id", func(s *domain.StageInputSnapshot) {
			s.ID = ""
		}, domain.ErrEmptyID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := valid
			snapshot.PriorArtifactDigests = append([]domain.Digest{}, valid.PriorArtifactDigests...)
			snapshot.ImageInputDigests = append([]domain.Digest{}, valid.ImageInputDigests...)
			vendor := *valid.VendorInstructions
			digest := *valid.VendorInstructions.Digest
			vendor.Digest = &digest
			snapshot.VendorInstructions = &vendor
			tc.edit(&snapshot)
			if err := snapshot.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestVendorInstructionBindingConformance(t *testing.T) {
	for _, vendor := range []domain.AgentVendor{
		domain.AgentVendorClaude,
		domain.AgentVendorCodex,
	} {
		if err := domain.ValidateVendorInstructionBinding(
			vendor, domain.VendorInstructionDeliveryAppendFile,
		); err != nil {
			t.Errorf("append-file binding for %q = %v, want nil", vendor, err)
		}
	}

	for _, tc := range []struct {
		name     string
		vendor   domain.AgentVendor
		delivery domain.VendorInstructionDelivery
	}{
		{"unknown vendor", "unknown", domain.VendorInstructionDeliveryAppendFile},
		{"missing delivery", domain.AgentVendorCodex, ""},
		{"replace authority", domain.AgentVendorCodex, "instructions_key"},
		{"cross-vendor CLI flag", domain.AgentVendorCodex, "append_system_prompt_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.ValidateVendorInstructionBinding(tc.vendor, tc.delivery)
			if !errors.Is(err, domain.ErrUnsupportedVendorInstructionBinding) {
				t.Fatalf("ValidateVendorInstructionBinding() = %v, want typed refusal", err)
			}
		})
	}
}

func TestLegacyClaudeVendorInstructionSnapshotRemainsValid(t *testing.T) {
	in := stageInputSnapshotInput()
	in.VendorInstructions.Vendor = domain.AgentVendorClaude
	in.VendorInstructions.Delivery = ""
	if _, err := domain.NewStageInputSnapshot(in); err != nil {
		t.Fatalf("legacy Claude v2 snapshot = %v, want valid reconstruction", err)
	}
}

// stageInputAttachments is a valid list: an image at the top level and a file
// in a nested folder, in presentation order.
func stageInputAttachments() []domain.StageInputAttachment {
	return []domain.StageInputAttachment{
		{
			Digest: stageDigest("a"), Name: "screenshot.png",
			MediaType: domain.TaskAttachmentMediaPNG, SizeBytes: 2048,
		},
		{
			Digest: stageDigest("b"), Name: "design_handoff/tokens/colors.json",
			MediaType: domain.TaskAttachmentMediaOctetStream, SizeBytes: 512,
		},
	}
}

func TestStageInputSnapshotAttachmentsOmittedWhenEmpty(t *testing.T) {
	base := mustStageInputSnapshot(t)
	baseBody, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(baseBody), `"attachments"`) {
		t.Fatalf("snapshot without attachments serialized the key: %s", baseBody)
	}

	emptyInput := stageInputSnapshotInput()
	emptyInput.Attachments = []domain.StageInputAttachment{}
	empty, err := domain.NewStageInputSnapshot(emptyInput)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Attachments != nil {
		t.Fatal("an empty attachment list must normalize to nil")
	}
	emptyBody, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if empty.ID != base.ID || string(emptyBody) != string(baseBody) {
		t.Fatalf("empty list changed the snapshot: %s vs %s", emptyBody, baseBody)
	}

	withInput := stageInputSnapshotInput()
	withInput.Attachments = stageInputAttachments()
	with, err := domain.NewStageInputSnapshot(withInput)
	if err != nil {
		t.Fatal(err)
	}
	if with.ID == base.ID {
		t.Fatal("attachments did not enter the snapshot identity")
	}
	withInput.Attachments[0].Name = "changed.png"
	if with.Attachments[0].Name != "screenshot.png" {
		t.Fatal("snapshot followed the caller-owned attachment slice")
	}

	// A stored record may spell absence as an empty array or null; either
	// decodes to the same snapshot under the same ID.
	for _, spelling := range []string{"[]", "null"} {
		stored := strings.TrimSuffix(string(baseBody), "}") + `,"attachments":` + spelling + "}"
		var decoded domain.StageInputSnapshot
		if err := json.Unmarshal([]byte(stored), &decoded); err != nil {
			t.Fatal(err)
		}
		if err := decoded.Validate(); err != nil {
			t.Fatalf("stored attachments %s: %v", spelling, err)
		}
		if id, err := decoded.ComputeID(); err != nil || id != base.ID {
			t.Fatalf("stored attachments %s resolve to %s, want %s (%v)", spelling, id, base.ID, err)
		}
	}
}

func TestStageInputSnapshotAttachmentsRejected(t *testing.T) {
	entry := func(name domain.TaskAttachmentName, media domain.TaskAttachmentMediaType, size int64) domain.StageInputAttachment {
		return domain.StageInputAttachment{Digest: stageDigest("c"), Name: name, MediaType: media, SizeBytes: size}
	}
	tooMany := make([]domain.StageInputAttachment, domain.MaxTaskAttachments+1)
	for i := range tooMany {
		tooMany[i] = entry(domain.TaskAttachmentName(fmt.Sprintf("f%d", i)), domain.TaskAttachmentMediaOctetStream, 0)
	}
	overBudget := make([]domain.StageInputAttachment, 4)
	for i := range overBudget {
		overBudget[i] = entry(domain.TaskAttachmentName(fmt.Sprintf("big%d.bin", i)),
			domain.TaskAttachmentMediaOctetStream, domain.MaxTaskAttachmentFileBytes)
	}
	octet := domain.TaskAttachmentMediaOctetStream
	cases := map[string]struct {
		list []domain.StageInputAttachment
		want error
	}{
		"duplicate name":  {[]domain.StageInputAttachment{entry("a.txt", octet, 1), entry("a.txt", octet, 1)}, domain.ErrTaskAttachmentNameConflict},
		"directory name":  {[]domain.StageInputAttachment{entry("docs/a.md", octet, 1), entry("docs", octet, 1)}, domain.ErrTaskAttachmentNameConflict},
		"invalid name":    {[]domain.StageInputAttachment{entry("../escape", octet, 1)}, domain.ErrInvalidTaskAttachmentName},
		"unknown media":   {[]domain.StageInputAttachment{entry("a.svg", "image/svg+xml", 1)}, domain.ErrInvalidTaskAttachmentMediaType},
		"oversize image":  {[]domain.StageInputAttachment{entry("a.png", domain.TaskAttachmentMediaPNG, domain.MaxTaskAttachmentImageBytes+1)}, domain.ErrTaskAttachmentTooLarge},
		"oversize file":   {[]domain.StageInputAttachment{entry("a.bin", octet, domain.MaxTaskAttachmentFileBytes+1)}, domain.ErrTaskAttachmentTooLarge},
		"negative size":   {[]domain.StageInputAttachment{entry("a.bin", octet, -1)}, domain.ErrTaskAttachmentTooLarge},
		"invalid digest":  {[]domain.StageInputAttachment{{Digest: "sha256:not-hex", Name: "a.bin", MediaType: octet}}, domain.ErrInvalidTaskAttachmentDigest},
		"too many":        {tooMany, domain.ErrTooManyTaskAttachments},
		"over the budget": {overBudget, domain.ErrTaskAttachmentsTooLarge},
	}
	valid := func() domain.StageInputSnapshot {
		in := stageInputSnapshotInput()
		in.Attachments = stageInputAttachments()
		snapshot, err := domain.NewStageInputSnapshot(in)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := stageInputSnapshotInput()
			in.Attachments = tc.list
			if _, err := domain.NewStageInputSnapshot(in); !errors.Is(err, tc.want) {
				t.Fatalf("NewStageInputSnapshot() = %v, want %v", err, tc.want)
			}
			stored := valid
			stored.Attachments = tc.list
			if err := stored.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestStageInputSnapshotAttachmentEditFailsClosed(t *testing.T) {
	in := stageInputSnapshotInput()
	in.Attachments = stageInputAttachments()
	valid, err := domain.NewStageInputSnapshot(in)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]domain.StageInputAttachment){
		"reorder": func(l []domain.StageInputAttachment) { l[0], l[1] = l[1], l[0] },
		"rename":  func(l []domain.StageInputAttachment) { l[0].Name = "other.png" },
		"retype":  func(l []domain.StageInputAttachment) { l[0].MediaType = domain.TaskAttachmentMediaJPEG },
		"resize":  func(l []domain.StageInputAttachment) { l[1].SizeBytes++ },
		"retarget": func(l []domain.StageInputAttachment) {
			l[1].Digest = stageDigest("f")
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			stored := valid
			stored.Attachments = slices.Clone(valid.Attachments)
			edit(stored.Attachments)
			if err := stored.Validate(); !errors.Is(err, domain.ErrStageInputDigestMismatch) {
				t.Fatalf("Validate() = %v, want %v", err, domain.ErrStageInputDigestMismatch)
			}
		})
	}
}

// A stored admission is decoded and then validated; its snapshot's attachment
// list is re-checked there, so a name edited in the stored row is refused.
func TestDecodedAdmissionRevalidatesAttachments(t *testing.T) {
	in := admissionInput()
	snapshotInput := stageInputSnapshotInput()
	snapshotInput.VendorInstructions = nil
	snapshotInput.PolicyDigest = in.PolicyDigest
	snapshotInput.Attachments = stageInputAttachments()
	snapshot, err := domain.NewStageInputSnapshot(snapshotInput)
	if err != nil {
		t.Fatal(err)
	}
	in.StageInputs = &snapshot
	admission := mustAdmission(t, in)
	snapshot.Attachments[0].Name = "changed.png"
	if admission.StageInputs.Attachments[0].Name != "screenshot.png" {
		t.Fatal("admission shared the caller's attachment slice")
	}
	body, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.ExecutionAdmission
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded admission: %v", err)
	}
	if len(decoded.StageInputs.Attachments) != 2 {
		t.Fatalf("decoded attachments = %v", decoded.StageInputs.Attachments)
	}

	tampered := strings.Replace(string(body), `"design_handoff/tokens/colors.json"`, `"../colors.json"`, 1)
	if tampered == string(body) {
		t.Fatal("fixture did not carry the nested attachment name")
	}
	var forged domain.ExecutionAdmission
	if err := json.Unmarshal([]byte(tampered), &forged); err != nil {
		t.Fatal(err)
	}
	if err := forged.Validate(); !errors.Is(err, domain.ErrInvalidTaskAttachmentName) {
		t.Fatalf("Validate() = %v, want %v", err, domain.ErrInvalidTaskAttachmentName)
	}
}
