# Unit Size Budget

Adopted a soft ~1,000-changed-line budget for the pull request a work
unit is expected to produce, enforced as decomposition guidance at
three checkpoints (wave decomposition, the planning stage, and
mid-implementation deferral), after review-history data showed
convergence cost is flat below that size and multiplies above it.

## Decision

- The budget, the repository's size-amplifier checklist, and the
  standard split seams live in docs/coordination.md (Unit Sizing);
  the plan-wave skill's decomposition step and the planning stage's
  plan-comment contract both apply them.
- Soft, not a gate: a unit that genuinely cannot split records its
  reason on its issue and proceeds. No AGENTS.md coordination gate was
  added.
- The planning stage surfaces an over-budget estimate and proposes the
  split in the plan comment; executing a split (new issues, rescoping)
  remains a spine or owner action, so planning's allowed mutations are
  unchanged. Once a plan proposes a split, the scheduling door defers
  pickup until the spine applies it or records the deliberately-larger
  reason on the issue (fiat remains independent).

## Evidence

Codex findings-reviews (reviews posted by `chatgpt-codex-connector[bot]`,
a proxy for review rounds) across the 58 merged PRs #704..#827,
bucketed by lines added:

| Lines added   | PRs | Mean findings-reviews |
| ------------- | --- | --------------------- |
| under 500     | 30  | 1.9                   |
| 500 to 1,000  | 14  | 1.6                   |
| 1,000 to 2,000| 6   | 6.5                   |
| over 2,000    | 8   | 11.9                  |

The cost compounds because each fold push re-reviews the whole diff.
Natural experiment inside one feature area: label intake landed partly
as mega-units (#735 at 3,881 lines / 16 findings-reviews, #746 at
3,441 / 6) and partly persistence-first (#741 migration plus store at
567 lines / 0, then #742 and #743 consuming it at 3 and 1).

Size is not the only driver: small outliers (#738 at 10, #814 at 8,
#827 at 6, #719 at 15) are the validation and contract-prose classes
that draw rounds regardless of size; the budget does not address
those.

## Rejected Alternatives

- A hard cap: the data shows the trade is roughly neutral below 1,000
  lines and some units are genuinely atomic; a recorded-reason
  override keeps judgment with the spine and owner.
- Placing the rule in AGENTS.md's coordination gates: this is
  decomposition heuristics, not an authorization door; a gate would
  make every large-but-atomic unit a policy exception.

Revisit when the reviewer, review trigger, or fold workflow changes
materially (the bucket means are measured against whole-diff
re-review per push), or when post-adoption waves show the budget
splitting units whose parts routinely fail to stand alone.
