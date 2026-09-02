package domain

import (
	"encoding/json"
	"strings"
)

// Pre-rename vocabulary (#986). This file is the only place in the package
// that may spell it; scripts/check-vocabulary.sh enforces that. Persisted rows
// written before the rename keep their bytes: identifiers because idempotency
// keys, subjects, and backup manifests embed them, JSON bodies because
// digest-addressed artifacts and canonical-body checks cover them. The code
// instead accepts both identifier families and canonicalizes the legacy enum
// values and JSON keys on decode, so a database written by the pre-rename
// daemon reconstructs through the renamed code.
const (
	legacySpecificationStageName          = "elaboration"
	legacySpecificationRunIDPrefix        = "run-elaboration-"
	legacySpecificationStageIDPrefix      = "elaborate-"
	legacySpecificationInvocationIDPrefix = "inv-elaborate-"
	legacySpecificationRunSeed            = "freeside.elaboration-run/v1"

	legacySpecificationDiscussionInvocationIDPrefix = "elaboration-discussion-"

	// Untyped on purpose: it is a spelling to canonicalize, never a member the
	// kind's switches or registration slice must carry.
	legacySpecificationSourceWorkItemArtifact = "spec_artifact"
)

// LegacySpecificationRun reports whether the run was minted before the rename
// and so carries the legacy identifier family.
func LegacySpecificationRun(runID RunID) bool {
	return strings.HasPrefix(string(runID), legacySpecificationRunIDPrefix)
}

// LegacySpecificationRunIDForImplementation derives the specification run
// identity the pre-rename daemon minted for an implementation run. Only
// reconstruction and replay of a pre-rename database need it; new runs derive
// through SpecificationRunIDForImplementation.
func LegacySpecificationRunIDForImplementation(implementationRunID RunID) RunID {
	return derivedSpecificationRunID(legacySpecificationRunSeed, legacySpecificationRunIDPrefix, implementationRunID)
}

// canonicalStageName maps the legacy specification stage spelling onto the
// canonical member and leaves every other name alone.
func canonicalStageName(name string) StageName {
	if name == legacySpecificationStageName {
		return StageNameSpecification
	}
	return StageName(name)
}

// UnmarshalJSON canonicalizes the legacy stage spelling wherever a StageName
// is decoded, so persisted card facts, launch specs, and admissions written
// before the rename validate and compare as the canonical member.
func (s *StageName) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = canonicalStageName(raw)
	return nil
}

// CanonicalizeStoredRow maps the legacy spelling of the specification stage's
// name onto the canonical one. Stage.Name is a plain string, so no decoder
// canonicalizes it on the way in; the engine matches stages by name and would
// otherwise never recognize a pre-rename specification run's stage.
func (r *Run) CanonicalizeStoredRow() {
	for i := range r.Stages {
		if r.Stages[i].Name == legacySpecificationStageName {
			r.Stages[i].Name = string(StageNameSpecification)
		}
	}
}

// UnmarshalJSON canonicalizes the legacy work-item source kind.
func (k *SpecificationSourceKind) UnmarshalJSON(data []byte) error {
	var raw SpecificationSourceKind
	if err := json.Unmarshal(data, (*string)(&raw)); err != nil {
		return err
	}
	if string(raw) == legacySpecificationSourceWorkItemArtifact {
		raw = SpecificationSourceWorkItemArtifact
	}
	*k = raw
	return nil
}

// UnmarshalJSON accepts the legacy work-item arm field name beside the
// canonical one. It stays as lenient about unknown fields as the store's
// row decoder, which is the only path that decodes this union from storage;
// the two spellings of the same arm are the tolerated pair, and a body
// carrying both with different values is inconsistent.
func (s *SpecificationSource) UnmarshalJSON(data []byte) error {
	type canonical SpecificationSource
	var wire struct {
		canonical
		LegacyWorkItemArtifactID ArtifactID `json:"spec_artifact_id,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.LegacyWorkItemArtifactID != "" {
		if wire.WorkItemArtifactID != "" && wire.WorkItemArtifactID != wire.LegacyWorkItemArtifactID {
			return ErrSpecificationSourceInconsistent
		}
		wire.WorkItemArtifactID = wire.LegacyWorkItemArtifactID
	}
	*s = SpecificationSource(wire.canonical)
	return nil
}
