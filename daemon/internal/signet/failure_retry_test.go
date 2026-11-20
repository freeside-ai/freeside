package signet

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestFailureRetryGate: Submit accepts a retry on a failure card only when
// its reconciler can act on it (#3440). The command id must be trimmed for
// every retry action, and a plain Retry needs facts that still offer it.
func TestFailureRetryGate(t *testing.T) {
	t.Parallel()
	role := domain.RoleImplementer
	offering := domain.ExecutionFailureFacts{
		Outcome: domain.ExecutionOutcomeLost, Stage: domain.StageNameImplementation,
		InvocationID: "inv-implement", RetryOutlook: domain.RetryOutlookMaySucceed,
		Admitted: &domain.InvocationAdmissionFacts{
			InvocationID: "inv-implement", Kind: domain.AdmissionKindExecution, Role: &role,
		},
	}
	// A card written before #3440 lists Retry with no outlook.
	old := offering
	old.RetryOutlook = ""
	repeats := offering
	repeats.RetryOutlook = domain.RetryOutlookWouldRepeat
	card := func(facts domain.ExecutionFailureFacts) domain.AttentionItem {
		return domain.AttentionItem{ID: "item-execution_failure-inv-implement", ExecutionFailure: &facts}
	}
	cases := []struct {
		name    string
		command domain.Command
		item    domain.AttentionItem
		refused bool
	}{
		{"offered retry", domain.Command{CommandID: "c1", Action: domain.ActionRetry}, card(offering), false},
		{"untrimmed retry id", domain.Command{CommandID: "c1 ", Action: domain.ActionRetry}, card(offering), true},
		{
			"untrimmed capability retry id",
			domain.Command{CommandID: " c1", Action: domain.ActionRetryWithCapability},
			card(offering), true,
		},
		{
			"untrimmed alternate agent retry id",
			domain.Command{CommandID: "c1\n", Action: domain.ActionRetryWithAlternateAgent},
			card(offering), true,
		},
		{
			"retry with a message",
			domain.Command{CommandID: "c1", Action: domain.ActionRetry, Message: "again"},
			card(offering), true,
		},
		{"card without an outlook", domain.Command{CommandID: "c1", Action: domain.ActionRetry}, card(old), true},
		{"would-repeat card", domain.Command{CommandID: "c1", Action: domain.ActionRetry}, card(repeats), true},
		{"another action", domain.Command{CommandID: "c1 ", Action: domain.ActionStop}, card(old), false},
	}
	for _, tc := range cases {
		err := validateFailureRetry(tc.command, tc.item)
		if tc.refused != errors.Is(err, domain.ErrParentKeyMismatch) || (!tc.refused && err != nil) {
			t.Errorf("%s: validateFailureRetry = %v, refused want %v", tc.name, err, tc.refused)
		}
	}
}
