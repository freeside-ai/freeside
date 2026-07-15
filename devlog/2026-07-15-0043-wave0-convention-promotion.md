---
run: manual
stage: wave0-exit-convention-review
date: 2026-07-15
branch: docs/wave0-convention-promotion
---

# Wave 0 exit review: domain-package conventions promoted (#27)

Spine-role review gate from `2026-07-13-1528-domain-package.md`
(`## To promote`), scheduled as #27. Decider: maintainer.

## Decisions

- **Promoted all three conventions (enum, switch, golden) to a binding
  AGENTS.md "Daemon coding conventions" section**, over the source
  entry's own disposition (decline; point-of-use docs suffice). The
  changed condition is real recurrence, which that disposition
  predates: `daemon/internal/exec` follows all three and cites the
  domain conventions by name (`exec/status.go`, `exec/capability.go`,
  `exec/golden_test.go`); `daemon/internal/store` follows the golden
  convention via the shared `daemon/internal/golden` helper; and Wave 1
  fans out four more daemon lanes (signet, gauntlet, publish, ward)
  that will each mint enums, dispatch switches, and goldens, so
  convergence should not depend on each lane happening to read
  `domain/doc.go`. The switch convention is also load-bearing for the
  repo's lint config (`exhaustive` with
  `default-signifies-exhaustive: true` in `daemon/.golangci.yml`): the
  omit-`default` rule is what makes that linter catch unhandled new
  members. The AGENTS.md section is pointer-style; the detail stays at
  point-of-use (`domain/doc.go`, `daemon/README.md`,
  `daemon/internal/golden`) so the two cannot drift far.
- **Closed #58 (revision-gated write-path invariant) as not needed**,
  the close-as-not-needed option the #27 scheduling comment authorized
  this review to adjudicate. Its promotion condition (the
  write-path-capability pattern recurring beyond
  `daemon/internal/store`) is unmet; the invariant is structurally
  enforced in code and documented in `doc.go`/`InternalTx`. Under the
  current devlog protocol a not-yet-actionable observation is a
  "Revisit when ..." condition, not open work, so the dormant issue
  duplicated what the code docs already carry. Revisit when the
  write-path-capability pattern (mutation of synchronized state gated
  on a revision-bumping handle) recurs outside
  `daemon/internal/store`; promotion is then its own docs-gated change.
- **#27's acceptance criterion 2 (write `->` markers into the source
  entry) is superseded, not satisfied.** The 2026-07-14 protocol change
  (faf46a5) froze historical `## To promote` entries: never mutate
  them, take no queue action from them (`devlog/README.md`). The
  criterion predates that change; the issue's closure plus this note
  carry the drain state instead, and
  `2026-07-13-1528-domain-package.md` stays untouched.

- **The promotion binds new and changed code (a ratchet), not the
  existing tree.** Codex's review of the promotion PR found the
  pre-promotion deviations, all in the exec fake: `Outcome` lacks
  `valid()`/`AllOutcomes`, its dispatch switches lean on `default` or
  a pre-normalizer, and the review fake treats the zero `Outcome` as
  `OutcomeComplete` (`outcomeOrComplete`), against the zero-invalid
  rule (a repo-wide enum sweep confirmed no deviations outside the
  fake). Chose ratchet-plus-filed-issue (#89, covering all of it)
  over widening this docs unit to fix the fake (scope discipline: the
  unit declares `AGENTS.md, devlog/`; never fix in passing) and over
  narrowing the convention to exclude fakes (the fake should follow
  it; its fail-loud unknown-outcome error survives as the trailing
  fallback the switch convention already prescribes).

- **The golden convention's no-maps clause is scoped to the contract
  shapes goldens pin, not to any JSON that touches disk.** Codex read
  the promoted clause against the exec fake's map-backed,
  package-private persistence format (`fake/persist.go`). Chose
  narrowing the clause over filing a second deviation: that file is a
  fixture format with no consumer beyond the fake itself, never
  golden-pinned, so rewriting its maps into slices would be defensive
  churn the convention was never aimed at.

After the second same-class review finding (a promoted clause read
against the existing tree), one adversarial enumeration lens checked
every clause against the code. It raised one candidate, rejected by
verification (recorded so it is not re-raised): the `key_provenance`
golden fixture's type has no `Validate`, but the fixture is
`policyKey.Provenance`, the same value the `policy_key` case validates
via `PolicyKey.Validate` (provenance source and digest), so it cannot
drift invalid without failing that sibling case; the
validation-positive clause holds transitively. Everything else
confirmed.

Sibling #52 (store trust-boundary re-gate line, recurrence condition
met) is a separate unit serialized after this one on AGENTS.md; this
review deliberately does not pre-decide its wording.

Revisit when: a daemon package has a principled reason to deviate from
one of the promoted conventions; the deviation then argues against the
AGENTS.md section, not silently past it.
