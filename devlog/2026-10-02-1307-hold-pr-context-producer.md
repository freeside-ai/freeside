# Publication Hold Pull Request Context: Producer and Reconciler

Work unit #531. It fills in the contract shapes from #1643
([contract note](2026-10-02-1039-hold-pr-reference-contract.md)): the engine
now writes the pull request onto a hold raised after publication, and the
active-resource reconciler watches that pull request while the run is held.

## Which Holds Carry the Pull Request

Chose "the store's own publication records prove a pull request" over "the
cause of the hold says publication happened". A hold carries the pull request
when the run's publication intent is dispatched and its outcome row exists,
and either the cause is not `HoldExternalConflict` or the item already carries
a reference.

- **Records over cause.** The same cause can be raised before and after the
  pull request opens (a recipe revoked before publication, or after it). Only
  the intent and outcome rows say which side of publication the run is on.
- **A first external-conflict hold carries nothing.** That cause means the
  records disagree with each other or with the task, so the lookup that would
  name the pull request is the thing in doubt.
- **A hold that already carries a reference keeps it under any cause.** The
  store refuses to drop or change a reference. If the lookup can no longer
  prove the same pull request, the engine returns an error and writes
  nothing, instead of restating the hold without it.

Rejected: carrying the pull request only on the causes known to fire after
publication. The list would have to track every new re-gate, and a cause
shared by both sides would be wrong on one of them.

## How the Pull Request Number Is Checked Before It Is Shown

The number is read from the inbox outcome row, never from the task or a
publisher call. Before it reaches an item it is tied back to the run:

- The outbox intent for the task's publication must be the publication kind
  and dispatched, and must name the task's invocation, head, producing
  invocation, and run.
- The outcome row is read at the key derived from that intent, and must agree
  with it on identity, repository, base ref, head, branch, and successor.
- The producing invocation's admission must belong to the run and agree with
  the intent on repository and base ref.

A missing intent or outcome means "not published yet" and yields no
reference. Any disagreement is an error, not an absence. The store then runs
its own proof when the binding is recorded and on every read, so this lookup
is a first check, not the only one.

Chose a store-only lookup over `publish.LoadOutcome`. That function serves
the publisher's replay path and its preconditions are the publisher's; the
hold needs only the two rows and the admission.

## A Held Run Stays Held After Its Pull Request Merges or Closes

Chose "record what happened, leave the hold open" over concluding the hold
from the reconciler. The reconciler records the pull request fact and, for a
declared work unit, the issue fact and completion. It does not resolve or
supersede the hold, settle schedules, or touch the publication task.

- **A hold is the engine's claim, not a readiness claim.** A ready item says
  "this pull request is reviewable"; a merge or close ends that claim, so the
  reconciler concludes it. A hold says "publication could not be verified";
  a merge does not answer that, and the engine re-evaluates the hold on its
  own schedule.
- **Completion without a ready milestone is already a supported state.** The
  completion mirror milestone is written only over a standing
  `publication_ready`, so a held run gets the completion row and stays at its
  publication outcome in listings.

A run that also has a ready item is watched through the ready binding alone,
so one pull request is polled once. An undeclared held run is polled until a
merge is on record; a closed pull request can reopen, so a close alone does
not stop the polling.

Revisit when holds gain an operator action that ends them: a merged pull
request behind an open hold may then want its own conclusion.

## Refute-First Findings

A fresh-context reviewer, given the diff and the intended outcome only, tried
to break the change before it was committed.

Confirmed and fixed:

- **A reconciler test did not reach the code it named.** A held binding the
  store cannot prove fails `ListAttentionItems` itself, so the per-hold read
  failure branch never ran and the assertion accepted either outcome. The
  test now asserts the pass fails at the listing and polls nothing.
- **No test covered a declared run recording its work-unit binding from a
  hold.** The behaviour was right but unasserted; a test now pins it.
- **A reconciler comment overstated what a held successor gets.** It is
  watched through its predecessor's ready binding, as before this change, and
  its own held binding is not read. The comment now says that.

Allowed by decision:

- **An unprovable held binding fails the whole reconciler pass.** The store
  gates every item read on its binding, ready or held, so this is the
  existing fail-closed contract and is reachable only through corruption. The
  per-hold failure branch stays for errors that are not corruption.
- **`putTerminalItem` still matches a hold by `[inspect_trust_failure]`
  alone.** Every caller that advances a hold to a definitive block runs
  before the intent is dispatched, so a hold that carries a reference never
  reaches it, and the store would refuse the transition for dropping the
  reference.

Disproved by a check:

- **A forged outcome row shows a different pull request.** The engine pass
  fails before any write and the item is unchanged.
- **A hold loses or changes its reference.** Refused by the engine before
  any write and again by the store's transition rule.
- **The reconciler changes a hold.** The held path is completion-only; the
  commit returns before any item or schedule write.
- **Signet reads a definitive block, or any other list, as a hold.** The
  predicate accepts the two exact lists only, and `open_pr` only with a
  reference.

Revisit when a held successor needs its new head watched before it reaches
readiness: the reconciler would then read the held binding for a run that
also has a ready item.
