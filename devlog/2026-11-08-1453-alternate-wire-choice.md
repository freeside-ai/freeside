# Carry the Alternate-Agent Choice on the Wire

Issue #2808; follows `2026-11-08-1223-alternate-retry-selection.md` (#2735).
Tier A: the decision message, the move command, the receipt the app trusts,
and the offers the operator chooses from change shape.

Chose offers that name an enrollment and a choice of an enrollment with an
optional model and effort, over keeping tree agent names on the wire behind
signet's translation, because since #2735 the daemon records and admits a
selection. A name had to be reduced through the tree, which can rename or
drop the agent between the card and the decision, and an offer no agent
mapped to could not be shown at all. Signet's tree lookup is gone.

## Decisions

- **A choice is enrollment plus nullable model and effort.** Null means the
  lineup's value, the same rule the engine's `chosenSelection` applies.
  Rejected: sending a full `TaskLineSelection`, which would make every
  client restate the lineup's model and effort and turn a lineup change
  between render and decision into a refusal. A present empty string is
  refused, naming the field, so one choice has one wire form and one stored
  message.
- **The app computes the stored message's bytes itself.**
  `CommandResultTrust.alternateAgentChoiceMessage` reproduces Go's
  `json.Marshal` of `domain.AlternateAgentChoice`, reusing the escaper the
  finding-routes message already pins. Before this change the app expected
  the agent name as the message while the daemon had stored the choice JSON
  since #2735, so a receipt for a real retry could not match.
- **Offers stored before #2735 are dropped at render, with the action.**
  This revises the #2735 note's "a v1 offer is refused as not offered",
  where such a card still showed its agent offers and refused the choice.
  The wire offer now requires `enrollment`, which a v1 offer lacks, so it
  cannot be rendered. The card renders without `retry_with_alternate_agent`
  and the stored item is unchanged; the operator retries or stops.
  Rejected: translating a v1 agent to an enrollment, which would accept an
  offer derived under rules #2735 replaced.
- **Stored move requests keep all three shapes.** A request from this
  change holds `choice`; one from #2735 holds `agent` and `selection`; an
  older one holds `agent`. Each still decodes and replays under `Same`, so a
  command id already used keeps its answer. Mixing `choice` with either
  older field is refused. Only a receipt that holds a choice and a retry
  selection renders on the wire; an older receipt has no wire form, and a
  replay of its command must send the stored shape, which no current client
  can.
- **A move to an unoffered enrollment is "not qualified".** The issue asked
  for "not offered", but a move's offers are derived live from the park
  rather than read from a card, so the engine's existing
  `ErrAlternateAgentNotQualified` is the accurate refusal. Both are a 400
  that echoes no input.
- **The app cache moves to format 11 with translation 9.** The strict
  decode would discard a format 10 cache, ledger included, on one cached
  offer naming an agent. Translation 9 nulls such offer lists and drops only
  the pending decision, submission, or parked move that names an agent,
  whose verbatim retry the daemon now refuses. Formats 2 to 10 also keep the
  pending usage command, parked move, and line change, which format 10
  stored as current state and the bump would otherwise drop.

## Refute-First Findings

A fresh-context review tried to break the change.

- **Confirmed, fixed: the app's Go string escaper worked per Character.**
  Swift's `replacingOccurrences` skipped a `<`, `>`, or `&` that a combining
  mark follows, which `ValidLaunchOptionValue` allows and Go escapes, so a
  real receipt's message could fail the trust check. The escaper now works
  per Unicode scalar, pinned to bytes captured from Go's `json.Marshal`. The
  finding-routes message shared the defect and is fixed with it.
- **Confirmed, fixed: missing negative tests.** Added the move's malformed
  choice refusal (naming the field, never the text), the move's replay and
  conflict under one command id, the receipt arm's refusal of a receipt
  without a choice and a retry selection, `Same` against an agent-only
  request, and the app gate's refusal of a forged choice, retry selection,
  or implementer line.
- **Declined: Swift `String ==` compares by canonical equivalence.** A
  receipt whose value is another normalization of what was sent would pass
  the app's gate. Reaching it needs the daemon, which the threat model
  trusts with its own records, to answer with a renormalized string; the
  daemon stores and compares bytes, so nothing it acts on changes.
- **Declined: the generated client omits a nil `model_id` or `effort`.**
  The schema marks them required and nullable; Go decodes a missing key as
  null, and other required-nullable fields share the generator's behavior.
- **Disproved by check: offers, catalog, replay, and leftovers.** A choice
  outside the offers or the catalog is refused on both commands; `Same`
  separates the three stored shapes; no signet code reads the tree.
