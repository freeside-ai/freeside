# State The Reason And Claim-Marker Rules In Plan §9

Plan revision 82 changes §9 so the refined-interfaces sweeps (#1798 to #1803)
have plan text for two of the handoff's rules: R0, which retires the labeled
Context section, and R7, which prints one claim marker and one explanation
control per card. The owner accepted the handoff and decided R7 on 2026-10-06
([owner-decision note](2026-10-06-1434-refined-interfaces-rules.md)). That
note records what the owner approved. This note records how the plan text was
written and what was left alone. The agent chose the wording under #1797's
owner-approved contract.

## The Reason Is Placed By A Test, Not By A Type List

Chose one test that reads the item over a list of the types whose reason may
fold. The test is what the rest of the card already says ahead of its
actions. The reason stays there by default. It may move to *Recorded
Context*, or to the card's details alone, only when the lead, a typed fact,
or a revalidated recommendation already says everything it says.

- **A per-type list was rejected.** The handoff's rule-to-code ledger names
  two types that take the disclosure. Its frames draw *Recorded Context* on
  more: diminishing returns, execution failure, system health, finding
  adjudication, the question, and final review. A list copied from either
  source would contradict the other.
- **Reasons differ item by item within a type.** An effect proposal's reason
  is usually a fixed sentence the card's facts restate. One variant says the
  source issue could not be closed automatically and that the notice does not
  hold the pull request, which nothing else on the card says. An execution
  failure's reason often ends with the driver's terminal summary. A type-level
  fold would put both below the actions.
- **Leaving §9 silent was rejected.** Each sweep would then decide for itself
  when a daemon sentence may sit below the actions.

The four client fallbacks follow from the test without being named: an item
that lacks the lead, the typed fact, the claim, or the revalidated
recommendation that would say what the reason says keeps the reason ahead of
the actions. An undecidable case takes the default too.

- **A labeled claim stands in only for reason text the agent wrote.** Without
  that limit, an unverified claim could carry a daemon sentence's content
  ahead of the actions while the daemon's own statement sat below them. The
  same limit applies to the never-fold exception: only the lead, a typed
  fact, or a revalidated recommendation can state such a notice in the
  reason's place. A revalidated recommendation qualifies because the daemon
  re-derives its action and reason from authenticated provenance.
- **The disclosure and the details share one condition.** A separate test
  between them was rejected. Both are away from the actions and the details
  carry the full reason either way, so whether a card draws *Recorded
  Context* is a presentation choice. The frames use both: they omit the
  disclosure where the reason is the agent's summary, rendered as a claim,
  and on the blocked card, whose lead and facts restate a one-line daemon
  sentence.
- **A binding payload is not a statement to the reader.** The
  diminishing-returns reason ends in a `Binding:` JSON line the daemon embeds
  so reconstruction can re-prove the policy and adjudication. It carries
  identifiers, digests, and policy thresholds that no typed field carries, so
  "the typed facts already state it" would have been false, and the default
  would have put raw JSON under the ask. The text exempts the payload by its
  purpose instead. The sentence before it, the stop cause, is still held to
  the test.
- **The never-fold list is revision 78's, reused.** Stale or base-advanced,
  degraded or waived, missing capability, an action's consequence, and a
  commit-plan notice. It applies to the reason as it does to typed facts.
- **Agent-written reason text keeps its label.** Two fallbacks draw agent
  text as the reason: a specification approval without a summary claim, and
  a question without typed decisions. Summary Provenance already requires a
  label on agent prose a card leads with, so the new paragraph points there
  and adds no rule.

## Typed Facts Stay Where Revision 78 Put Them

Chose to leave the routine-coordinate split on its four types over extending
it. Each refined frame for a type outside the four was compared with the
as-built card, and none folds a typed layer 1 fact. What moves on those
frames is the reason and the claim's source identifiers.

The claim's own digest is the one identifier revision 78 did not name. The
built cards already draw it in the claim's source details on the four types. The
specification, execution-failure, and task-proposal frames move it into
source details with the invocation ID. Chose to name it among a claim's
source details in Summary Provenance over extending the split to those three
types, because the split would also permit folding stage, round, and
publication coordinates that no frame folds.

## The Explanation Sentence Was Never Plan Text

§9 required the producer or Unverified label and never said where the
sentence that explains it prints. Rule R7 therefore contradicted no plan
sentence. Chose to state the on-demand form anyway, with its two limits (the
label stays visible beside each claim, and the explanation stays reachable by
pointer, keyboard, touch, and VoiceOver), because the client test that pinned
the sentence to ten types read as a plan requirement and the next reader
would have had no text to settle it.

## Rules Checked And Left Compatible

- **R20, the recommendation head.** §9 still says a recommendation leads with
  its reason and a `project_policy` recommendation cites its policy key and
  digest. The key and digest already sit in a disclosure inside the
  recommendation block, which satisfies "cites". The refined finding frame
  draws one sentence naming actor, action, and confidence, and no separate
  reason line. That is a frame detail, not a rule: the survey's own pattern
  reads label, reason, action. The reason stays required.
- **R21 and R25, compact marks.** "The producer or Unverified label stays
  next to the prose" already permits a producer label alone. Each claim
  keeps one.
- **R24, template sentences.** §9 requires content, not sentences, and R24's
  own test keeps any sentence that carries a value the card does not
  otherwise show. One sentence R24 cannot remove: the statement that a
  decision cannot be taken on this client and that the item stays open.
- **R27, the type eyebrow.** It adds a type keyword above the lead. The lead
  still renders as a labeled claim, and the "Leads with" column is unchanged.

## Cards That The Plan Text Does Not Follow

Two cards put part of an action's consequence below the actions. The plan
stays as it is on both, because moving a consequence is a rule change for the
owner, not a wording change.

- **The diminishing-returns frame** draws the sentence that says Continue
  Under Policy undoes the listed fixes beneath the action row. The built card
  draws it above. The sweep that owns the card (#1801) keeps it above unless
  the owner decides otherwise.
- **The built finding-adjudication card** folds its reason whenever the
  recommendation revalidates. The recommendation states each finding's
  outcome. On a remediating batch the reason also says that the remediator
  may change any of the run's allowed paths and that the updated PR is
  reviewed again, which nothing else ahead of the actions says. Revision 78
  already listed an action's consequence as never folding; revision 82 makes
  the reason subject to that list in words. The card complies once that
  sentence renders ahead of the actions.

Revisit when the daemon types the content that today exists only in a reason
(the effect proposal's fallback notice, the driver summary on an execution
failure), since the test then decides more items toward the fold; when a
refined frame folds a typed fact on a type outside the four (extend revision
78's list by name); or when the owner rules on a consequence sentence below
the actions.
