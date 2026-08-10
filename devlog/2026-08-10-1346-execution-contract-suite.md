# Bind Execution Fakes With Shared Contract Runners

Chose one assertion-owning contract runner per execution interface over
duplicated implementation tests or one shared progression script because the
permanent fake, provider-neutral stage driver, and Codex review source advance
through different mechanisms. Each implementation supplies only scenario
realization, synchronization, and restart construction; the runner owns API
calls, sentinel identities, lifecycle expectations, and redelivery checks.

Chose issue-linked, exact-failure divergence allowances over weakening the
common expectation or fixing existing semantics in this unit. An allowed case
still runs and skips only after observing the cited mismatch; a passing allowed
case fails until the stale allowance is removed, and an unrelated failure in
the same scenario remains a failure. This keeps the contract explicit and the
tree green without making known debt invisible.

The first cross-implementation run confirmed four mismatches:

- the stage driver collapses the fake's pending phase into running (#661);
- the stage driver reports a committed result's terminal status after restart
  where the fake reports a lost session with a collectable result (#662);
- Codex review turns restart loss before result commit into a failed outcome
  rather than gone plus no result (#663);
- Codex review reports completed after a post-result restart where the fake
  reports a lost session with a pollable result (#664).
- the stage driver returns an empty stream instead of the admitted transcript
  on every read (#666).

Rejected fixing those mismatches here because #568 explicitly limits this unit
to building the harness and filing what it surfaces. Rejected unconditional
skips because they would not prove the mismatch still exists or force cleanup
when a follow-up repairs it.

The review-authority contract treats `ReviewRequestAuthorityVerifier` as
mandatory, then verifies both a mismatched canonical digest and harness-proven
teardown before it accepts the contradiction. That reflects the production
engine's configuration requirement and keeps a rejected request from becoming
an unobserved credential-bearing topology. The permanent fake removes its
scripted invocation on rejection; the production harness proves its durable
abort outcome and empty runtime topology.

The shared review suite also distinguishes a failed terminal review from a
lost session after restart, and verifies both stale head and stale base
revisions. This exposed and corrected the permanent fake's recovery model:
a durably failed review remains failed after reconstruction rather than being
rewritten as lost.

Revisit when the execution crash taxonomy changes, or when another
StageDriver or ReviewSource implementation lands and cannot realize one of the
current scenarios through its production seam.
