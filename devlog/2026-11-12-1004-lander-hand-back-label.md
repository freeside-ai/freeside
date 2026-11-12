# The Lander Hands Back With `needs-rework`

Issue #3126, from tracker #3132's assessment of what Freeside needs before
its own development moves onto it. It changes the Landing Record, a
contract the `land-approved-prs` skill reads, so a mandatory note.

Chose a separate `needs-rework` label for the lander's pull-request
hand-back over keeping `needs-human` for both meanings, because
`needs-human` is fiat-only and never agent-selected (AGENTS.md,
Coordination Gates). Once Freeside reworks handed-back pull requests
(#3127), a fiat-only label on them would tell Freeside to stay away from
the pull requests it should fix.

## Decision

- **`needs-rework` is the Landing Record's Hand-back.** It marks a pull
  request that could not land and carries no fiat-only rule. On a
  hand-authored pull request it means what the hand-back meant before: the
  owner sends a session back by fiat, or the authoring session refreshes it.
- **`needs-human` keeps only its strict meaning,** which is about issues:
  maintainer-only, unmilestoned, fiat-only. Its forge description and the
  issue rules in `docs/coordination.md` stay as they were.
- **The landing measures count both labels** rather than switching at a
  date, so a window that spans the change keeps the hand-backs from before
  it. Neither label reads as a hand-back's reason.

## Revises

`2026-10-19-1500-landing-record.md` recorded that `needs-human` "already
existed" and reused it as the hand-back. That reuse held while only a
human acted on a handed-back pull request; it stops holding once Freeside
does. The note stays as it was.

Revisit when the lander stops labelling hand-backs, or when `needs-human`
starts being put on pull requests, which the measures would then count as
hand-backs.
