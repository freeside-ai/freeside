package domain

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const (
	stageInputEncodingVersion            = "freeside.stage.inputs/v1"
	vendorStageInputEncodingVersion      = "freeside.stage.inputs/v2"
	boundVendorStageInputEncodingVersion = "freeside.stage.inputs/v3"
	// MaxVendorInstructionBytes is the admission and ward ceiling for one
	// vendor-native host instruction file.
	MaxVendorInstructionBytes int64 = 1 << 20
)

// VendorInstructionSnapshot binds one vendor-native instruction input and its
// conformed delivery mechanism. A nil Digest is explicit absence: admission
// looked at the configured host path and found no file. That differs from a nil
// *VendorInstructionSnapshot, which is a historical pre-v2 stage-input record
// that made no vendor-instruction claim. A zero Delivery is accepted only when
// reconstructing the implicit append-file binding of a historical Claude v2
// record; new admissions always write the explicit v3 binding.
type VendorInstructionSnapshot struct {
	Vendor   AgentVendor               `json:"vendor"`
	Delivery VendorInstructionDelivery `json:"delivery,omitempty"`
	Digest   *Digest                   `json:"digest"`
}

func (s VendorInstructionSnapshot) validate() error {
	// v2 records predate an explicit delivery binding. Claude was their only
	// valid vendor and append-file delivery was the contract they implemented,
	// so only that exact historical shape remains reconstructable.
	if s.Delivery == "" {
		if s.Vendor != AgentVendorClaude {
			return fmt.Errorf("vendor instruction snapshot binding (%q, %q): %w",
				s.Vendor, s.Delivery,
				errors.Join(ErrStageInputsNotCanonical, ErrUnsupportedVendorInstructionBinding))
		}
	} else if err := ValidateVendorInstructionBinding(s.Vendor, s.Delivery); err != nil {
		return fmt.Errorf("vendor instruction snapshot: %w",
			errors.Join(ErrStageInputsNotCanonical, err))
	}
	if s.Digest != nil && !contentaddr.Valid(string(*s.Digest)) {
		return fmt.Errorf("vendor instruction snapshot digest %q: %w",
			*s.Digest, ErrStageInputsNotCanonical)
	}
	return nil
}

// ValidateVendorInstructionBinding rejects any vendor/delivery pair that has
// not been conformed. Both current vendors preserve append authority through a
// delivered file; no replace-authority configuration silently falls back to
// that binding.
func ValidateVendorInstructionBinding(
	vendor AgentVendor, delivery VendorInstructionDelivery,
) error {
	switch vendor {
	case AgentVendorClaude:
		if delivery == VendorInstructionDeliveryAppendFile {
			return nil
		}
	case AgentVendorCodex:
		if delivery == VendorInstructionDeliveryAppendFile {
			return nil
		}
	}
	return fmt.Errorf("vendor %q with delivery %q: %w",
		vendor, delivery, ErrUnsupportedVendorInstructionBinding)
}

func (s VendorInstructionSnapshot) clone() VendorInstructionSnapshot {
	s.Digest = clonePtr(s.Digest)
	return s
}

// StageInputSnapshot is the immutable content identity of one stage's inputs.
// The roles stay distinct in the digest so the same bytes used as a prompt and
// as a prior artifact do not become interchangeable. Collection order is
// preserved because it is presentation order to the agent, not a set.
//
// ID is computed by NewStageInputSnapshot and rechecked by Validate. The
// content bytes live in the artifact store; this record is the admitted map
// from roles to content addresses that restart recovery replays.
type StageInputSnapshot struct {
	ID                   Digest                     `json:"id"`
	InputDigest          Digest                     `json:"input_digest"`
	SpecificationDigest  Digest                     `json:"specification_digest"`
	PromptPackageDigest  Digest                     `json:"prompt_package_digest"`
	PolicyDigest         Digest                     `json:"policy_digest"`
	VendorInstructions   *VendorInstructionSnapshot `json:"vendor_instructions,omitempty"`
	ConversationDigest   *Digest                    `json:"conversation_digest,omitempty"`
	PriorArtifactDigests []Digest                   `json:"prior_artifact_digests"`
	ImageInputDigests    []Digest                   `json:"image_input_digests"`
}

// StageInputSnapshotInput carries the caller-supplied fields. It has no ID:
// the snapshot identity is derived from these bindings, never asserted.
type StageInputSnapshotInput struct {
	InputDigest          Digest
	SpecificationDigest  Digest
	PromptPackageDigest  Digest
	PolicyDigest         Digest
	VendorInstructions   *VendorInstructionSnapshot
	ConversationDigest   *Digest
	PriorArtifactDigests []Digest
	ImageInputDigests    []Digest
}

type canonicalStageInputSnapshot struct {
	Version              string   `json:"version"`
	InputDigest          Digest   `json:"input_digest"`
	SpecificationDigest  Digest   `json:"specification_digest"`
	PromptPackageDigest  Digest   `json:"prompt_package_digest"`
	PolicyDigest         Digest   `json:"policy_digest"`
	ConversationDigest   *Digest  `json:"conversation_digest,omitempty"`
	PriorArtifactDigests []Digest `json:"prior_artifact_digests"`
	ImageInputDigests    []Digest `json:"image_input_digests"`
}

type canonicalVendorStageInputSnapshot struct {
	Version              string                    `json:"version"`
	InputDigest          Digest                    `json:"input_digest"`
	SpecificationDigest  Digest                    `json:"specification_digest"`
	PromptPackageDigest  Digest                    `json:"prompt_package_digest"`
	PolicyDigest         Digest                    `json:"policy_digest"`
	VendorInstructions   VendorInstructionSnapshot `json:"vendor_instructions"`
	ConversationDigest   *Digest                   `json:"conversation_digest,omitempty"`
	PriorArtifactDigests []Digest                  `json:"prior_artifact_digests"`
	ImageInputDigests    []Digest                  `json:"image_input_digests"`
}

type canonicalBoundVendorStageInputSnapshot struct {
	Version              string                    `json:"version"`
	InputDigest          Digest                    `json:"input_digest"`
	SpecificationDigest  Digest                    `json:"specification_digest"`
	PromptPackageDigest  Digest                    `json:"prompt_package_digest"`
	PolicyDigest         Digest                    `json:"policy_digest"`
	VendorInstructions   VendorInstructionSnapshot `json:"vendor_instructions"`
	ConversationDigest   *Digest                   `json:"conversation_digest,omitempty"`
	PriorArtifactDigests []Digest                  `json:"prior_artifact_digests"`
	ImageInputDigests    []Digest                  `json:"image_input_digests"`
}

// NewStageInputSnapshot builds the canonical, detached snapshot admitted for a
// stage. Empty collections become non-nil arrays so one semantic input set has
// one serialized form and therefore one identity.
func NewStageInputSnapshot(in StageInputSnapshotInput) (StageInputSnapshot, error) {
	s := StageInputSnapshot{
		InputDigest:          in.InputDigest,
		SpecificationDigest:  in.SpecificationDigest,
		PromptPackageDigest:  in.PromptPackageDigest,
		PolicyDigest:         in.PolicyDigest,
		VendorInstructions:   cloneVendorInstructionSnapshot(in.VendorInstructions),
		ConversationDigest:   clonePtr(in.ConversationDigest),
		PriorArtifactDigests: append([]Digest{}, in.PriorArtifactDigests...),
		ImageInputDigests:    append([]Digest{}, in.ImageInputDigests...),
	}
	id, err := s.ComputeID()
	if err != nil {
		return StageInputSnapshot{}, err
	}
	s.ID = id
	if err := s.Validate(); err != nil {
		return StageInputSnapshot{}, err
	}
	return s, nil
}

// ComputeID returns the sha256 content address of the versioned role-to-digest
// map. It does not hash the referenced bytes again; each referenced digest is
// verified against its bytes when the snapshot is materialized.
func (s StageInputSnapshot) ComputeID() (Digest, error) {
	var canonical any
	if s.VendorInstructions == nil {
		canonical = canonicalStageInputSnapshot{
			Version:              stageInputEncodingVersion,
			InputDigest:          s.InputDigest,
			SpecificationDigest:  s.SpecificationDigest,
			PromptPackageDigest:  s.PromptPackageDigest,
			PolicyDigest:         s.PolicyDigest,
			ConversationDigest:   clonePtr(s.ConversationDigest),
			PriorArtifactDigests: s.PriorArtifactDigests,
			ImageInputDigests:    s.ImageInputDigests,
		}
	} else if s.VendorInstructions.Delivery == "" {
		canonical = canonicalVendorStageInputSnapshot{
			Version:              vendorStageInputEncodingVersion,
			InputDigest:          s.InputDigest,
			SpecificationDigest:  s.SpecificationDigest,
			PromptPackageDigest:  s.PromptPackageDigest,
			PolicyDigest:         s.PolicyDigest,
			VendorInstructions:   s.VendorInstructions.clone(),
			ConversationDigest:   clonePtr(s.ConversationDigest),
			PriorArtifactDigests: s.PriorArtifactDigests,
			ImageInputDigests:    s.ImageInputDigests,
		}
	} else {
		canonical = canonicalBoundVendorStageInputSnapshot{
			Version:              boundVendorStageInputEncodingVersion,
			InputDigest:          s.InputDigest,
			SpecificationDigest:  s.SpecificationDigest,
			PromptPackageDigest:  s.PromptPackageDigest,
			PolicyDigest:         s.PolicyDigest,
			VendorInstructions:   s.VendorInstructions.clone(),
			ConversationDigest:   clonePtr(s.ConversationDigest),
			PriorArtifactDigests: s.PriorArtifactDigests,
			ImageInputDigests:    s.ImageInputDigests,
		}
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("stage input snapshot id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate is the reconstruction boundary for an admitted input snapshot.
func (s StageInputSnapshot) Validate() error {
	required := []struct {
		name   string
		digest Digest
	}{
		{"input_digest", s.InputDigest},
		{"specification_digest", s.SpecificationDigest},
		{"prompt_package_digest", s.PromptPackageDigest},
		{"policy_digest", s.PolicyDigest},
	}
	for _, field := range required {
		if field.digest == "" {
			return fmt.Errorf("stage input snapshot %s: %w", field.name, ErrEmptyField)
		}
		if !contentaddr.Valid(string(field.digest)) {
			return fmt.Errorf("stage input snapshot %s %q: %w",
				field.name, field.digest, ErrStageInputsNotCanonical)
		}
	}
	if s.ConversationDigest != nil &&
		!contentaddr.Valid(string(*s.ConversationDigest)) {
		return fmt.Errorf("stage input snapshot conversation_digest %q: %w",
			*s.ConversationDigest, ErrStageInputsNotCanonical)
	}
	if s.VendorInstructions != nil {
		if err := s.VendorInstructions.validate(); err != nil {
			return err
		}
	}
	if s.PriorArtifactDigests == nil || s.ImageInputDigests == nil {
		return fmt.Errorf("stage input snapshot collections: %w", ErrStageInputsNotCanonical)
	}
	if err := validateStageInputDigests("prior_artifact_digests", s.PriorArtifactDigests); err != nil {
		return err
	}
	if err := validateStageInputDigests("image_input_digests", s.ImageInputDigests); err != nil {
		return err
	}
	if s.ID == "" {
		return fmt.Errorf("stage input snapshot id: %w", ErrEmptyID)
	}
	computed, err := s.ComputeID()
	if err != nil {
		return err
	}
	if s.ID != computed {
		return fmt.Errorf("stage input snapshot %s, content resolves to %s: %w",
			s.ID, computed, ErrStageInputDigestMismatch)
	}
	return nil
}

func validateStageInputDigests(name string, digests []Digest) error {
	for i, digest := range digests {
		if digest == "" {
			return fmt.Errorf("stage input snapshot %s[%d]: %w", name, i, ErrEmptyField)
		}
		if !contentaddr.Valid(string(digest)) {
			return fmt.Errorf("stage input snapshot %s[%d] %q: %w",
				name, i, digest, ErrStageInputsNotCanonical)
		}
	}
	return nil
}

func (s StageInputSnapshot) clone() StageInputSnapshot {
	s.VendorInstructions = cloneVendorInstructionSnapshot(s.VendorInstructions)
	s.ConversationDigest = clonePtr(s.ConversationDigest)
	s.PriorArtifactDigests = append([]Digest{}, s.PriorArtifactDigests...)
	s.ImageInputDigests = append([]Digest{}, s.ImageInputDigests...)
	return s
}

func cloneVendorInstructionSnapshot(
	in *VendorInstructionSnapshot,
) *VendorInstructionSnapshot {
	if in == nil {
		return nil
	}
	cloned := in.clone()
	return &cloned
}
