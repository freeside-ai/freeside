package domain

import (
	"fmt"
	"strings"
)

// An operator feedback invocation and the card that reports a failed
// invocation carry fixed ID prefixes. Stored rows, outbox idempotency keys,
// and attention item IDs embed these bytes, so every site that mints or
// matches one goes through these helpers instead of spelling a prefix.
const (
	operatorFeedbackInvocationIDPrefix = "inv-operator-feedback-"
	executionFailureItemIDPrefix       = "execution-failure-"
	specificationRevisionFailurePrefix = executionFailureItemIDPrefix + "spec-revision-"
)

// OperatorFeedbackInvocationID names the invocation that answers one operator
// feedback command.
func OperatorFeedbackInvocationID(commandID string) InvocationID {
	return InvocationID(operatorFeedbackInvocationIDPrefix + commandID)
}

// HasOperatorFeedbackInvocationPrefix reports whether id starts with the
// operator feedback prefix, including the bare prefix. The prefix alone
// doesn't prove a feedback invocation: a client-chosen specification
// discussion ID can carry it too, so a caller that needs proof also checks
// the invocation's stage.
func HasOperatorFeedbackInvocationPrefix(id InvocationID) bool {
	return strings.HasPrefix(string(id), operatorFeedbackInvocationIDPrefix)
}

// OperatorFeedbackCommandID recovers the command from an operator feedback
// invocation ID or idempotency key. It rejects the bare prefix, which names
// no command.
func OperatorFeedbackCommandID(id InvocationID) (string, bool) {
	commandID, ok := strings.CutPrefix(string(id), operatorFeedbackInvocationIDPrefix)
	return commandID, ok && commandID != ""
}

// ExecutionFailureItemID names the recovery card for one failed invocation.
func ExecutionFailureItemID(invocationID InvocationID) ItemID {
	return ItemID(executionFailureItemIDPrefix + string(invocationID))
}

// HasExecutionFailureItemPrefix reports whether id starts with the prefix
// every execution-failure recovery card shares: the cards ExecutionFailureItemID
// and the two specification revision helpers mint. A quarantine notice is an
// execution_failure item too, but its ID carries a "<kind>-marker-quarantined-"
// prefix instead, so this check separates a recovery card from a marker notice
// without reading the card's optional ExecutionFailure facts (a
// delivery-refusal card can carry none).
func HasExecutionFailureItemPrefix(id ItemID) bool {
	return strings.HasPrefix(string(id), executionFailureItemIDPrefix)
}

// IsOperatorFeedbackExecutionFailureItemID reports whether id has the prefix
// of a card minted by ExecutionFailureItemID for an operator feedback
// invocation, including the bare combined prefix.
func IsOperatorFeedbackExecutionFailureItemID(id ItemID) bool {
	return strings.HasPrefix(string(id), executionFailureItemIDPrefix+operatorFeedbackInvocationIDPrefix)
}

// SpecificationRevisionFailureItemID names the card for one failed
// specification revision iteration of an implementation run.
func SpecificationRevisionFailureItemID(implementationRunID RunID, iteration int) ItemID {
	return ItemID(fmt.Sprintf("%s%s-%d", specificationRevisionFailurePrefix, implementationRunID, iteration))
}

// SpecificationIterationsExhaustedItemID names the one card per
// implementation run that reports its specification revisions ran out of
// iterations. It differs from every SpecificationRevisionFailureItemID of the
// run, which carries an iteration suffix.
func SpecificationIterationsExhaustedItemID(implementationRunID RunID) ItemID {
	return ItemID(specificationRevisionFailurePrefix + string(implementationRunID))
}
