package domain

import "testing"

// The expected IDs are written out rather than built from the prefix
// constants, so a changed prefix fails here instead of silently minting
// different stored IDs.
func TestSpecificationArtifactAndApprovalIDs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, got, want string
	}{
		{"feedback artifact", string(SpecificationFeedbackArtifactID("c")), "spec-feedback-c"},
		{"discussion artifact", string(SpecificationDiscussionArtifactID("c")), "spec-discussion-c"},
		{"approval item", string(SpecificationApprovalItemID("run-1", 2)), "spec-approval-run-1-2"},
		{"approval item prefix", SpecificationApprovalItemIDPrefix("run-1"), "spec-approval-run-1-"},
		// The spec revision gate refuses this empty-run ID by comparing with it.
		{"approval item, empty run", string(SpecificationApprovalItemID("", 2)), "spec-approval--2"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestSpecificationArtifactAndApprovalIDMatching(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id            string
		feedback      bool
		discussion    bool
		commandID     string
		commandOK     bool
		approvalRest  string
		approvalCutOK bool
	}{
		{id: "spec-feedback-c", feedback: true, commandID: "c", commandOK: true},
		// The predicates accept the bare prefix; the cut helpers reject it.
		{id: "spec-feedback-", feedback: true},
		{id: "spec-discussion-c", discussion: true},
		{id: "spec-discussion-", discussion: true},
		{id: "spec-approval-run-1-2", approvalRest: "run-1-2", approvalCutOK: true},
		{id: "spec-approval-"},
		{id: "spec-c"},
	} {
		if got := HasSpecificationFeedbackArtifactPrefix(ArtifactID(tc.id)); got != tc.feedback {
			t.Errorf("HasSpecificationFeedbackArtifactPrefix(%q) = %v, want %v", tc.id, got, tc.feedback)
		}
		if got := HasSpecificationDiscussionArtifactPrefix(ArtifactID(tc.id)); got != tc.discussion {
			t.Errorf("HasSpecificationDiscussionArtifactPrefix(%q) = %v, want %v", tc.id, got, tc.discussion)
		}
		if got, ok := SpecificationFeedbackCommandID(ArtifactID(tc.id)); ok != tc.commandOK || (ok && got != tc.commandID) {
			t.Errorf("SpecificationFeedbackCommandID(%q) = %q, %v; want %q, %v", tc.id, got, ok, tc.commandID, tc.commandOK)
		}
		if got, ok := CutSpecificationApprovalPrefix(tc.id); ok != tc.approvalCutOK || (ok && got != tc.approvalRest) {
			t.Errorf("CutSpecificationApprovalPrefix(%q) = %q, %v; want %q, %v", tc.id, got, ok, tc.approvalRest, tc.approvalCutOK)
		}
	}
}
