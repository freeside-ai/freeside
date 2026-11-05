# The Alternate-Agent Retry Card and Parked Move

#869 lets an operator retry a failed implementation attempt under another
agent, from the failure's card or by moving a task parked on an exhausted
quota window. Choosing an agent chooses which credential runs, so the work
is Tier A (`internal/engine/agent_admission.go`). This note records the
design choices that reject a reasonable option, where the implementation
departs from the plan comment, and the refute-first result.

## Decision

- **Chose one content-addressed record for both paths** over a card-only
  record plus an ordinary line change for the move. An
  `AlternateAgentRetry` names the task, role, failed invocation, the card
  (empty for a move), the command, the agent, its pool, and the rule that
  qualified it. A card's attempt cites it, and a moved line's `set_by` is
  its id. Admission can then hold either path to one stored choice and the
  command that made it, instead of trusting a line or an attempt field.
- **Chose to re-derive the rule at acceptance and at admission** over
  trusting the offer on the card or hold. An offer is a snapshot.
  Acceptance re-runs the offer derivation for the chosen agent, and
  admission of a card's attempt (`regateCardAgent`) checks it isn't the
  failed agent, draws on the chosen pool, and still escapes the failed
  invocation's recorded cause under the record's rule. A card that fails
  refuses the attempt and never falls back to the line or lineup.
- **Chose to anchor a moved line, not re-gate it.** A move re-pins the
  task as the router would, so its lines carry router-shaped provenance
  and hold on the new pool's windows (`requireRoutedPoolOpen`). Admission
  accepts one only when its record is a move of its own task, the
  command's receipt records that move, and the line was set at the
  record's instant on the record's pool. The moved lines serve every
  later attempt of every role they pin, not one retry of the failed
  invocation, so the window hold every routed line gets is the check that
  applies to them; the escape rule was re-derived when the move was
  accepted.
- **Chose a card retry that changes one attempt** over switching the
  task's line or the lineup. The card answers one failure; the task's
  lines stay, so later attempts run as before. A move is the operator's
  durable choice and changes the lines.
- **Rebuilt `task_lines` in migration 0125** to widen the provenance
  CHECK to the `alternate_agent_move` source. SQLite can't alter a CHECK
  in place. The rebuild copies every row and keeps every other
  constraint; `TestAlternateAgentRetryMigrationKeepsTaskLines` covers it.
  A table rebuild makes the migration Tier A by the AGENTS.md rule.
- **Chose 400 over 409 for a stale offer or a task no longer parked.** A
  409 carries current state for the client to reapply against, as
  `change_task_line` does with its task snapshot. A stale offer has no
  state to reapply: the daemon recorded nothing, and the client refreshes
  to read the current offers. Both errors join the determinate no-effect
  rejections in `isCommandAuthorityRejection`.
- **Offered no `wait` action.** A parked task already waits by default;
  the move is the only new choice there.
- **Left triage-sourced lines to #2511.** A move skips every role whose
  newest line an operator set, triage included, and #2511 edits the
  triage anchor in `agent_admission.go` after this unit.
- **Drew parked offers from the routing policy only.** A card also
  considers the lineup's implementer, but a move re-pins lines as the
  router would and needs a policy to pin the other roles against, so a
  task with no policy in effect is offered nothing.
- **Named the agent a move left as `passed_over` only when the policy
  orders it.** The issue asks for the agent left as the passed-over agent,
  and the task timeline reads `passed_over` to show the move as a switch.
  A line's anchor holds its passed-over agent to the policy it cites
  (`anchorLinePolicy`), and a parked task's failed agent can be a lineup
  agent the policy doesn't order, so in that case the field stays null
  rather than failing the anchor.
- **Kept a role the policy can't pin on the new pool on its line.** The
  move leaves it as it was rather than clearing it to the lineup, which
  would move work the operator didn't ask to move.

## Departures From the Plan

- **Implementation stage only.** The card and the move offer the
  implementer alone. The review-side (`review_dispute`,
  `review_configuration`) and specification-side offers are the plan's
  split seam and move to #2628, which starts after this unit.
  The domain item validation already admits offers on the two review
  types; the follow-up adds the action to their signet action sets, their
  raise sites, the spec's `alternate_agents` description, and the app
  fixtures.

## App

The client sends a move as a saved command, as it does a routing policy:
the command persists before it's sent, an unanswered send is retried only
by an explicit Retry that resends it unchanged, and the move reads as
recorded only once sync reaches the receipt's revision. A relaunch keeps
the saved command and never resends it on its own.

## Refute-First Pass

A fresh-context reviewer, told to refute rather than confirm, found no
blocker: nothing it traced lets a command, a replay, or a stale offer run an
agent the operator didn't choose. Its six findings:

- **Fixed: the move's success path had no end-to-end test.**
  `TestParkedTaskMovesToTheChosenAgent` parks a real quota refusal, moves
  the task through the operator service, and checks the line, its
  `passed_over`, the released hold, and that the parked attempt starts
  under the chosen agent's enrollment, selected by the moved line.
- **Fixed: a credential failure could offer the failed credential.** An
  expiry or revocation qualified any agent whose generation was valid,
  including another agent on the same enrollment and generation. The rule
  now needs another enrollment or a later generation, at the offer and at
  admission. No classifier emits these causes yet, so it was latent.
- **Fixed: a move was accepted on a stopped task.** A Stop committed before
  its workflow closes left the attempt parked, so the move succeeded but
  nothing would launch. It's now refused as not parked, as the park itself
  treats a stopped task (`TestParkedTaskMoveRefusesAStoppedTask`).
- **Declined: the moved-line anchor binds a non-moved role's line only to
  its record, instant, and pool, not to the receipt's line list.** A line
  that passes it can only be written to the daemon's database, which the
  threat model trusts.
- **Declined: a refused card attempt's park releases when the task's line
  names another pool.** The next attempt runs on its line under the
  ordinary rules, and a router line still holds on its own pool's windows,
  so the cost is at most one refused attempt.
- **Declined: an offer error can fail the terminal write.** The errors that
  reach it are database read errors; an agent that can't be resolved is
  skipped, not failed.

It disproved migration drift in the `task_lines` rebuild, a card record
cited across campaigns or items, two records from one command, a moved line
accepted without its receipt, card fallback to the line or lineup, and
partial writes on a refusal.

Revisit when the review-side follow-up lands, since a review attempt has
no production attempt to cite a record.
