package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestPreRenameVocabularyIsRefused pins that a value spelled in the
// vocabulary from before the specification rename (#986) is refused rather
// than read as its current counterpart. The decode aliases that mapped it
// were removed once no database still held it (#3266); without them the old
// spelling reaches validation as an unknown member.
func TestPreRenameVocabularyIsRefused(t *testing.T) {
	t.Parallel()
	var stage StageName
	if err := json.Unmarshal([]byte(`"elaboration"`), &stage); err != nil {
		t.Fatal(err)
	}
	if stage.valid() {
		t.Fatalf("stage name %q decoded as a valid member", stage)
	}
	if _, err := CanonicalStageRole("elaboration"); !errors.Is(err, ErrUnknownStageRole) {
		t.Fatalf("CanonicalStageRole(elaboration) = %v, want ErrUnknownStageRole", err)
	}

	for _, tc := range []struct {
		name string
		body string
		want error
	}{
		{
			"legacy kind", `{"kind":"spec_artifact","spec_artifact_id":"artifact-1","issue_subject":null}`,
			ErrInvalidSpecificationSourceKind,
		},
		{
			"legacy arm field", `{"kind":"work_item_artifact","spec_artifact_id":"artifact-1","issue_subject":null}`,
			ErrEmptyID,
		},
	} {
		var source SpecificationSource
		if err := json.Unmarshal([]byte(tc.body), &source); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := source.Validate(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: Validate() = %v, want %v", tc.name, err, tc.want)
		}
	}

	if runID, ok := SpecificationRunIDFromInvocationID("inv-elaborate-run-elaboration-abc-1"); ok {
		t.Fatalf("legacy invocation identity parsed as run %q", runID)
	}
	if commandID, ok := SpecificationDiscussionCommandID("elaboration-discussion-command-1"); ok {
		t.Fatalf("legacy discussion identity parsed as command %q", commandID)
	}
}
