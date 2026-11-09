package domain

import "testing"

// The expected IDs are literals, not built from the prefix constants, so a
// changed prefix fails here instead of silently re-minting stored IDs.
func TestOperatorFeedbackIdentityMints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"feedback invocation", string(OperatorFeedbackInvocationID("c")), "inv-operator-feedback-c"},
		{"execution failure card", string(ExecutionFailureItemID("inv-implement-run-1")), "execution-failure-inv-implement-run-1"},
		{
			"feedback execution failure card",
			string(ExecutionFailureItemID(OperatorFeedbackInvocationID("c"))),
			"execution-failure-inv-operator-feedback-c",
		},
		{"spec revision failure card", string(SpecificationRevisionFailureItemID("run-1", 2)), "execution-failure-spec-revision-run-1-2"},
		{"spec iterations exhausted card", string(SpecificationIterationsExhaustedItemID("run-1")), "execution-failure-spec-revision-run-1"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestOperatorFeedbackIdentityMatches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id   InvocationID
		has  bool
		cmd  string
		isOK bool
	}{
		{"inv-operator-feedback-c", true, "c", true},
		// The bare prefix still matches the prefix check, as the literal
		// HasPrefix sites did, but names no command.
		{"inv-operator-feedback-", true, "", false},
		{"inv-operator-feedback", false, "", false},
		{"inv-implement-run-1", false, "", false},
		{"", false, "", false},
	} {
		if got := HasOperatorFeedbackInvocationPrefix(tc.id); got != tc.has {
			t.Errorf("HasOperatorFeedbackInvocationPrefix(%q) = %v, want %v", tc.id, got, tc.has)
		}
		cmd, ok := OperatorFeedbackCommandID(tc.id)
		if ok != tc.isOK || (ok && cmd != tc.cmd) {
			t.Errorf("OperatorFeedbackCommandID(%q) = %q, %v, want %q, %v", tc.id, cmd, ok, tc.cmd, tc.isOK)
		}
	}

	for _, tc := range []struct {
		id               ItemID
		executionFailure bool
		feedbackFailure  bool
	}{
		{"execution-failure-inv-operator-feedback-c", true, true},
		{"execution-failure-inv-operator-feedback-", true, true},
		{"execution-failure-inv-implement-run-1", true, false},
		{"execution-failure-spec-revision-run-1", true, false},
		{"execution-failure-", true, false},
		{"operator-feedback-marker-quarantined-execution-failure-x", false, false},
		{"", false, false},
	} {
		if got := HasExecutionFailureItemPrefix(tc.id); got != tc.executionFailure {
			t.Errorf("HasExecutionFailureItemPrefix(%q) = %v, want %v", tc.id, got, tc.executionFailure)
		}
		if got := IsOperatorFeedbackExecutionFailureItemID(tc.id); got != tc.feedbackFailure {
			t.Errorf("IsOperatorFeedbackExecutionFailureItemID(%q) = %v, want %v", tc.id, got, tc.feedbackFailure)
		}
	}
}
