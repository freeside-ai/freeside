package domain_test

import (
	"encoding/json"
	"errors"
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
			Vendor: domain.AgentVendorClaude,
			Digest: &vendorDigest,
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
	if !strings.Contains(string(absentBody), `"vendor_instructions":{"vendor":"claude","digest":null}`) {
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
		}, domain.ErrStageInputsNotCanonical},
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
