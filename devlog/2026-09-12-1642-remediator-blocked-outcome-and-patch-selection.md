# Remediator Prompt: Blocked-Outcome Interrupt And Single-Patch Selection

Two decisions for `prompts/phase-1a/remediator.md`, both settled after Codex
review corrected an earlier assumption.

## Blocked outcome: keep it, for both round types

Chose to carry the implementer's Blocked Outcome section in the remediator,
scoped to a whole-round stop, for both remediation and operator-feedback
rounds. An earlier draft of this note omitted it, on the belief that a
`blocked.json` export in these rounds was a degenerate, untested generic
question. That belief was wrong.

`daemon/internal/integration/operator_feedback_retry_test.go:338-405`
(`answerFeedbackQuestion`) exercises the modeled path: an invocation writes a
`freeside.blocked-outcome/v1` owner decision, the daemon surfaces an
`agent_question` AttentionItem, and the operator resumes it through
`ActionAnswerAndRetry`, which re-invokes the agent with a
`freeside.operator-feedback-input/v1` input. The blocked outcome is the
agent's only modeled way to raise an owner decision, a specification
contradiction, or an unavailable capability mid-round; without it a compliant
agent must invent the decision or fail the stage. That is a real gap, not
hardening.

The remediator-pushback claim (`freeside.remediator-pushback/v1`) is a
different channel: it declines a specific *finding* in a remediation round and
routes to a review dispute. It is not documented in the prompt, and it does
not cover an owner decision. The two are complementary: pushback for a finding
you decline to fix; blocked outcome for a decision only a human can make.

Rejected: omit the blocked outcome and route everything through the summary or
pushback. It leaves an owner decision arising mid-round with no channel.

## Patch selection: apply the most recent patch-bearing artifact

Chose to instruct the agent to apply exactly one `candidate_patch_base64`, the
one on the most recent prior artifact that carries it, rather than "the
round's operator-feedback input."

`operatorFeedbackInputIDs` (`daemon/internal/engine/operator_feedback.go:171`)
appends the new feedback artifact to the source invocation's full `InputIDs`,
and `renderPromptParts` (`daemon/internal/exec/claude/spec.go:358-360`) renders
every prior artifact in full, so a `return_to_agent` round after a remediation
round presents two artifacts, each with a full base-to-candidate patch;
applying both conflicts. And an `answer_and_retry` feedback input carries no
patch at all (`operator_feedback.go:585-591`: only `ActionReturnToAgent` sets
one), so "apply the feedback input's patch" would leave the workspace at the
bare base. "Most recent artifact that carries a patch" is correct in every
case: one full patch reconstructs the candidate, and the workspace stays at
the exact base only when no artifact in the chain carries a patch (a question
answered before any candidate existed). A patchless newest input whose chain
still retains an earlier patch reconstructs that candidate; reading it as
"leaves the base" would reintroduce the reversion this decision records.

## Revisit when

A remediation round gains a structured owner-decision route distinct from the
generic `agent_question`, or the operator-feedback input schema changes which
artifact carries the authoritative patch.
