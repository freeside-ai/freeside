# Follow-Up Filing Proposals

Issue #1632 makes the engine propose a `follow_up_filing` effect, and open
its `effect_proposal` card, when a finding adjudication concludes that a
finding is separate work. It consumes the #1625 contract
(`2026-10-05-0921-follow-up-filing-kind.md`) and changes no shared package.
Nothing here decides, approves, or files: a person decides the card, and
#1626 and #1634 act on the decision.

## When a Conclusion Becomes a Proposal

**A deferred disposition proposes in the transaction that writes its row.**
`reconcileFindingAdjudicationWithDissent` returns at its top once a round is
fully dispositioned, so a proposal written in a later transaction would be
lost by a crash in between. The rule is "the proposal follows the row": a
P0 or P1 defer normally takes the dispute path and writes no row, and where
an accepted decision does write one, the proposal follows it.

**A separate-work verdict proposes once the operator accepts the route, in
a transaction of its own.** Rejected: proposing when the verdict is
recorded, which would put two cards in front of the operator for one
finding, and a discuss reply would then leave the proposal bound to a
superseded revision. Rejected: the plan's first choice, proposing inside
the disposition write. An accepted `park_separate_work` route writes no
disposition, and several paths return before that write on every pass (a
convergence stop, a dispute route elsewhere in the batch, a dispatched or
undeliverable remediation, a drift park). The parked run re-enters route
execution on each reconcile, so a separate transaction still converges.

**A diminishing-returns finish proposes nothing,** as the issue requires.
Its deferred rows come from the operator ending the round, not from an
adjudicated defer taking effect.

## Replay Reads the Batch Before It Composes

**An entry whose instance already exists is skipped before its text is
composed again.** The admission key is the adjudication artifact's digest
and the entry's 1-based position, so `AllocateProposalInstance` would also
find the row. It compares content, though. A renamed repository (the gate
matches the repository by ID and lets a proposal keep the old name) or a
later daemon that composes the text differently would turn the routine
replay of every parked run into `ErrImmutableConflict`, which stops the
publication lane. Rejected: relying on allocation's insert-or-return alone.
The card is opened only for an instance this pass inserted, so a decided
card is never reopened.

**A separate-work pass with nothing left to write stays a read.** Every
committed store write advances the revision clients sync against, and a
parked run re-enters route execution on each reconcile. The pass reads the
plan first and opens its write transaction only when an entry is pending or
a refusal has no notice yet. The independent review found the first draft
writing on every pass.

## Text and the Fixed Fallback

**The engine composes the title and body; no model writes them.** The title
is the finding message's first non-blank line. The body is the message, the
location, and the adjudication rationale. Each is cut to its limit on a rune
boundary and screened under `github-issue/1`. Rejected: a new adjudicator
field or an issue-author inference site, because either is a contract unit.

**A field the screen refuses takes fixed daemon text, alone.** The ruleset
refuses any `@name` token and any line that opens with a `/` command, so a
finding that quotes `@MainActor` or begins a line with an absolute path
fails. Without the fallback those findings would lose their follow-up
silently. A filing cannot be approved with changes (#1625), so the operator
approves or declines the card as written and reads the finding in the
review record.

## Refusals That Would Repeat

**A run that can carry no filing raises one advisory notice and its
dispositions still commit.** An error from this path propagates to the
publication lane and stops it, and these refusals repeat on every retry: a
malformed `follow_up_filing.*` policy value, a missing project record, and
a run submitted without a work-unit declaration (the declaration is
recorded only when the task carries one, and the proposal gate resolves
policy and project through it). Only errors from reading the plan are
classified; an error once allocation has begun propagates, so a notice
never commits beside a partial batch. The notice is a `system_health` advisory
offering acknowledge, with a deterministic per-run identity. Rejected:
`execution_failure`, which offering only acknowledge cannot close (#1342).
`ErrFollowUpFilingSourceStale` at admission is skipped without a notice:
the source stopped backing a filing before the pass proposed it. Every
other error is a store fault or a broken invariant and propagates.

## The Opener Takes an Instance Identity

**`signet.OpenFollowUpFilingItem` takes an instance ID and reads the row
through the proposal gate.** Rejected: the plan's sketch, which passed the
instance struct; a boundary that accepts an exported struct re-runs the
gate instead of trusting it (AGENTS.md, Daemon Coding Conventions). The
item identity is derived from the instance, so a second open returns the
item it finds, whatever its status, and writes nothing.

**The card's subject carries no run.** The sync reconstruction rejects an
item whose subject names a run but is not a run subject, so the card's task
display name falls back to the batch ID, as the closure card's does today.

## Refute-First Record

An independent reviewer tried to break the change before it was committed.

- **Confirmed and fixed:** the separate-work pass committed a write on every
  reconcile (above). A refusal class for an inconsistent target could not be
  reached and was removed.
- **Allowed by the contract, for the owner to weigh:** a finding that recurs
  and is deferred again in a later round gets a second proposal, because the
  contract is one proposal per deferred disposition. The first card's source
  is then stale and its approval is refused; closing it is the separate unit
  below. If the first card was approved before the later round, both carry
  an approval, so the filing ledger (#1626) is where one issue per finding
  would have to be enforced.
- **Disproved by a check:** 26 adversarial texts (control bytes, invalid
  UTF-8, entity-encoded mentions, U+2028, line-terminator tricks, a secret,
  cuts at rune boundaries) all produced text that passes parameter
  validation, both screens, and a strict JSON round trip. The fixed texts
  pass the screen. No agent or repository text reaches the item reason, the
  notice, or the target. The admission key cannot collide with a closure's.
  A stop, an undecided item, and a finish-now all return before either
  producer call.
- **Declined:** a stricter screen shipped later under the same ruleset name
  would fail the batch read. The ruleset is versioned to prevent exactly
  that, and such a change would already break every attention-item read.

## Not Covered

- No test drives the `startPublicationContinuation` call site; the tree has
  no fixture that reaches that write. It calls the same producer the
  adjudication write does.
- No end-to-end test chooses an alternative route or runs a batch with
  several findings; entry selection and ordinals are covered as a pure
  function over every route.
- #1625's note says this unit "should close a filing item when its source
  goes stale". The #1632 contract made that a non-goal; the implementation
  plan proposes it as its own unit.

Revisit when fixed-text cards turn out to be common for code-heavy
findings (the fix is a contract change, an editable filing or a narrower
mention rule, not a looser screen here), when an adjudication can carry
findings from an external review (#524), or when a filing batch's subject
can resolve its task for display.
