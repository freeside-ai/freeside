# External Finding Dispositions

Issue #1749 gives an admitted external finding a durable outcome: declined,
deferred, or fixed. It is a contract unit over `daemon/migrations`,
`daemon/internal/domain`, and `daemon/internal/store`. Nothing writes the
record yet. #1767 adjudicates and remediates admitted findings in the engine,
and #1636 replies on the forge.

An external finding is one an allowlisted reviewer outside Freeside left on a
published pull request (plan §5.19). It is quarantined: it never joins a
review record, and `PutFinding` refuses it. An `external_review` authority
admits it into a review cycle, and plan §7 has that cycle consume the same
adjudication as any other round.

## A Separate Table

Chose a new `external_finding_dispositions` table and a new
`ExternalFindingDisposition` type over widening `finding_dispositions` to
accept external findings.

Widening was rejected because every reader of `finding_dispositions` takes a
row as proof that its finding came from the round's review record. Three of
them fail closed when it did not: the signet review facts, the publication
disposition history, and the review yield history. Each would need its own
exception for an external row, and a missed one would turn an external
disposition into a publication blocker or a convergence input. A separate
table leaves all of them untouched: review completeness, convergence, the
drift audit, and the publication history never read the new rows, and
`PutFindingDisposition` still refuses an external finding.

The two records carry the same fields under the same rules and differ in
which findings they may name. `ExternalFindingDisposition.Validate` builds a
`ReviewDispositionRecord` from its fields by position and reuses its rules,
so a field added to that record stops this one compiling until someone
decides what it means here.

## An Absent Binding Renders Null

Chose nullable `adjudication_digest` and `remediation_invocation_id` that
render an explicit null over the omitted keys `ReviewDispositionRecord` uses.
The implementation plan asked for omitted keys, to match that record. The
daemon convention is that an optional contract member renders null, and each
earlier exception rested on stored bytes a null would change (#1048's yield
history is the example). This table is new and holds no rows, so nothing here
earns the exception. `ReviewDispositionRecord` keeps its shape, because
changing it would change the bytes of rows already stored.

The cost is that the two types no longer convert to each other, which is why
`Validate` copies by position. `Validate` also refuses a binding that is set
but empty, so null is the only way to say a binding is absent.

## What a Row Proves

A disposition is bound to the first round of the external review cycle that
answers its finding. The store re-runs the whole binding on every write and
every read:

- **The round is an external cycle's first round.** One dispatched
  `external_review` authority of the run names the round as its
  `ReviewRound`, and passes its own gate.
- **The cycle admits the finding.** The finding is an external finding of the
  run, on the authority's head, and the profile the authority names lists its
  reviewer. The named profile is used, not the active one, so a row reads the
  same after the owner edits the allowlist.
- **Declined and deferred carry an adjudication.** It belongs to the same run
  and round, and its entry for the finding admits that outcome under
  `AuthorizesFinalDisposition`, the rule review dispositions use.
- **Fixed names a later review record.** It is a record of the same run, in a
  later round, on the same base and another head.

`fixed` claims no more than that. It does not say a fingerprint proof ran or
that the finding is proven absent on the remediated head: #524's decision 7
has Freeside make no such claim for an external finding. Keeping the record
in its own type keeps a reader from taking its `fixed` for a review
disposition's. #1767 decides what evidence the engine requires before it
writes one.

## The Adjudication Rule Is One-Way

Chose an additive rule for the adjudication gate over a second artifact for
external findings. An adjudication must still carry an entry for every
finding of its round's review record. It may now also carry entries for
findings outside the record, and only when the round is an external cycle's
first round and that cycle admits each of them. An ordinary round's
adjudication never reads a successor, so it behaves as before.

The store does not require an entry for every admitted external finding. The
admitted set is whatever the cycle's read returns at the time, and a rule
that demanded all of them would make a stored adjudication unreadable when a
later finding is admitted. The engine owns that completeness: #1767 must
adjudicate every admitted finding before it dispositions any.

An external entry is routed like any other, as plan §7 requires ("this same
adjudication and routing, not a second path"). Convergence iterates the
record's findings, so an external entry is not a convergence input. The
follow-up filing gate does accept one: an entry routed to separate work is
the whole link to its filing proposal, with no check that the finding is in
the record. That is the plan's routing for an admitted finding, and the
filing still needs its own approval. The proposal then carries a reviewer's
words, so #1767 owns how that text is framed, as it does for the adjudicator
and the remediator.

## The First Round Is the Authority's Round

The issue fixes "the cycle's first round" as the authority's `ReviewRound`,
and this unit implements that. It is narrower than what the engine does. A
terminal review failure consumes its round, so a cycle whose first review
attempt fails gets its first review record one round later. The store then
refuses every disposition and external entry for that cycle: the authority's
round has no review record, and the round that has one is not the
authority's.

Chose to ship the contract as written over widening it here. The refusal is
fail-closed, so nothing is admitted wrongly, and nothing writes these
records until #1767. A correct wider rule has to say which successor owns a
later round without relying on insertion order, which is a contract decision
of its own.

Follow-up: #1781

## One Disposition per Cycle

The key is the finding and the round, as it is for a review disposition. A
cycle admits every listed reviewer's finding on its head, so a second
external cycle on the same head admits the first cycle's findings too, and
the store accepts a disposition for one finding in each cycle's first round.
The two can disagree. Nothing in the store says which is current; the
first reader decides, and the later round is the natural choice. Whether a
second cycle answers a finding an earlier cycle already answered is the
engine's call in #1767.

## The Chain Is Walked, Except Inside a Successor's Own Read

The implementation plan said to find the cycle by walking
`PublicationSuccessorChain`. That alone fails in a store with a later
remediation successor. Its gate reads the adjudication of the cycle's round.
With a chain walk inside the adjudication gate, that read re-enters the
successor being authenticated, and the publication read guard refuses it as
a cycle.

Chose two paths, by whether the read is already reconstructing a publication
record:

- **A read that starts at an adjudication or a disposition walks the chain.**
  Every sealed row passes its own gate, and a branch or an orphan is refused.
  The round's cycle is the one `external_review` authority on that chain
  whose `ReviewRound` is the round. Two for one round are refused, because
  nothing says which of them names the findings the round answers.
- **A read inside a successor's or ready item's reconstruction links the
  sealed rows and gates the authority alone.** Linking applies the chain
  walk's structural rule without its gates: two successors of one item are
  refused, and so is a successor the root does not reach.

The walk is required because the authority's own gate is not enough. It
admits any superseded ready item, including one a feedback return already
superseded; only sealing checks that the item is the run's current one. A row
written beside a real successor passes its gate. Linking alone is not enough
either: a second rewritten row can follow the forged authority, so the rows
link cleanly, and only that row's own gate gives it away.

The nested path is weaker on purpose. It runs only when a remediation
successor's gate asks whether the adjudication it names still reads, and the
read that started that reconstruction decides what it trusts. A chain walk
gates every row. A read of one record never claimed more than that record's
gate.

Rejected: gating, at every depth, the rows before the first one under
reconstruction. Each remediated external cycle would then authenticate its
whole prefix again inside the next one's gate, and the cost of a read would
double with every such cycle.

## The Round Is Bound to the Authority's Base and Head

Nothing in the store ties a round's review record to the base and head its
authority names. The engine reviews a re-entry on those commits, but a
record on other commits can be written for the round.

That mattered for `fixed`. With the round's record on another head, a later
round on the finding's own head would pass as its remediation. Both the Go
gate and the trigger now require the round's record to be on the authority's
base and head, and an adjudication's external entries are refused with it.

## What the Trigger Proves

The trigger closes the direct-SQL gap around what a row can name. It proves
the finding is an external one of the row's run, that a dispatched
`external_review` authority of the run starts at the row's round on the base
and head the round reviewed, that this head is the finding's, and the `fixed`
remediation rule.

It does not prove the reviewer is admitted, that an adjudication authorizes
the outcome, or that the authority is on the run's successor chain. Those need
the profile and artifact gates and the chain link, which stay in Go and run on
every read, as adjudication authority does for `finding_dispositions`. A row
inserted by direct SQL past the trigger still fails every read.

## Refute-First Findings

Two passes tried to prove the change wrong before it was committed: a
mutation check that removed each gate clause in turn, and a reviewer in a
fresh session given the diff and the intended outcome.

Confirmed, and fixed in this unit:

- **A round reviewed off the authority's commits.** See the section above.
- **Three refusals no test reached.** A `fixed` remediation in an earlier
  round, a row naming a run other than its finding's, and a round started by
  a dispatched authority of another origin, kind, run, or status were each
  refused only because another clause happened to refuse the fixture too.
  Each now has a case that only its own clause refuses.
- **An authority off the successor chain.** Found in review of the pull
  request, not by either pass, in two steps: a row beside the real successor
  of a superseded ready item, then a second row rewritten to follow it. See
  "The Chain Is Walked, Except Inside a Successor's Own Read".

Confirmed, and deferred or left to another unit:

- **A cycle whose first review attempt fails.** #1781.
- **Two cycles on one head.** Allowed, as recorded above.
- **Follow-up filing from an external entry.** Allowed by plan §7, as
  recorded above.

Allowed by decision:

- **A `dispute`-routed entry admits `declined`.**
  `AuthorizesFinalDisposition` checks an entry's goal relationship and
  compatibility against the outcome, not its route. Review dispositions
  have the same rule, and changing it for one record would split the
  authority the two share.
- **A read of an adjudication with external entries depends on its
  authority.** Every row that read touches is write-once: the authority, the
  ready binding, the named profile, and the findings. Ordinary work cannot
  make it fail, and one unreadable row failing a whole list is the store's
  rule everywhere.

Disproved by a check:

- **Read recursion.** A store with two external cycles reads cleanly through
  the successor chain, the current successor, both adjudications, and the
  disposition list.
- **The refactors changed behavior.** `PublicationSuccessorChain` and
  `ExternalReviewCycleFindings` differ from their earlier forms only in
  which error surfaces when two rows are bad.
- **The trigger refuses what the Go door admits.** The cast of the BLOB
  payload, the guard on a payload that is not JSON, NULL handling, and the
  integer round comparison all hold; a direct insert of an admitted row
  succeeds and reads back.
- **A tampered finding joins the admitted set.** Every finding is validated
  as it is listed.

Two clauses have no test that fails without them, because another check
refuses the same state: the trigger's test that the finding is external (a
review finding has no head to match the round's), and the gate's refusal
when the round has no cycle (the empty authority is not an external one).
Both stay, because each states a rule the other clause only implies.

## Revisit When

- #1767 produces the first remediation successor after an external cycle
  whose adjudication carries external entries. Until then no store holds
  that state, and the nested lookup is tested under a simulated enclosing
  read. #1767's integration test is its first real run.
- A reader needs review and external dispositions in one list. Build the
  union at that reader; do not merge the tables.
- #1767 needs a disposition outside the cycle's first round, for example
  when remediation of an external finding takes several rounds.
- A second kind of quarantined finding appears. The admission step in the
  binding is then the part to generalize.
