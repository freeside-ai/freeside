# One Definition for Ward's Rig Names

Issue #2759; Tier A (`internal/ward/`, and the new `internal/wardname/`
joins the Tier A list as a destructive-cleanup gate). The rig lease's
container, volume, and network patterns hand-copied every ward role, and
the missed `-claude` copy broke routed Claude review on held rigs until
#2718.

Chose a leaf package, `internal/wardname`, holding the prefixes, run-ID
shapes, and per-shape role sets, which ward builds names from and
daemonlock renders its patterns from, over leaving the copies and adding
only a cross-check test, because a shared table plus a two-direction ward
test makes a new role fail before it reaches `main` instead of after.

## Decisions

- **A leaf, not an import of ward.** daemonlock can't import ward: ward's
  package-internal tests (`lineup_probe_test.go`) import daemonlock, so the
  test build would cycle. A leaf both import avoids the cycle, so the
  test-only fallback (a ward test comparing ward's names against
  daemonlock's literals) wasn't needed.
- **Role sets per shape, not one flat list.** Each run-ID shape admits a
  different set: a review run's handoff prefix carries only the seeder,
  observer, and workspace; a conformance handoff carries seven containers.
  A flat list would admit, for example, `freeside-handoff-review-<id>-agent`,
  which ward never creates. Widening the rig is the failure this gate
  exists to prevent.
- **Roles are plain string constants, not a named type.** Ward's existing
  tests mix the suite's role constants with strings
  (`conformanceObjectPrefix + runID + "-"`, `[]string{"excl-ws", ...}`), so
  a named type would have forced edits to tests the issue requires to pass
  unchanged. The test that ties ward to the table, not the type, is what
  catches a missing role.
- **Semantically identical patterns, not byte-identical strings.** The
  renderer wraps a single role in parentheses and emits one handoff network
  alternative per run-ID shape where the old literal shared one. A pinned
  table, committed before the refactor and passing on both sides, is the
  proof of equivalence.
- **`RigShapes` returns a fresh table.** An exported slice could be widened
  by any importer for every other reader of the gate.

## Coverage and Its Limits

Ward's coverage test checks that every name its enumerators produce is
admitted, that every shape-and-role pair the table admits is produced by
some enumerator, and, by reflection, that every field of `handoffNames` and
`codexReviewResourceNames` reaches an enumerator. A name built ad hoc
outside those structs and enumerators would escape it; today that covers
only the conformance probes, preflight credential observer, and lineup
probe, which the test reaches through their enumerator calls.

Outside the definition on purpose: `freeside-enroll-credential-` (never
bound to the rig), the conformance audit-marker path, and host temporary
directories. The run-ID shapes record what `exec/claude.RunIDFor`, the
conformance driver, and the review gate mint but don't bind them; tying
them is follow-up #2788.

## Refute-First Results

- **Claim: the rebuilt patterns admit a name the old ones refused, or the
  reverse.** Disproved by check: the pinned table (every combination of a
  known prefix, ID shape, and role per class, plus ID-length, case,
  non-hex, and leading or trailing text mutations) passes before and
  after; and a one-off comparison of the old literals against the rendered
  patterns over 2,757,188 generated names (structured combinations over ID
  lengths 0 to 40 and random byte mutations) found no disagreement.
- **Claim: a persisted name changed, so teardown of an existing review
  intent or handoff would fail to authenticate.** Disproved by check: a
  dump of every ward enumerator and name struct at the base and the head,
  over production, review, conformance, and malformed run IDs, is
  byte-identical, and the `ward/testdata/` goldens are unchanged.
- **Claim: a role metacharacter could widen a pattern.** Disproved by
  check: the renderer quotes every prefix, lead, and role, and a leaf test
  shows a `.` in a role matches only itself.
- **Claim: the coverage test doesn't bite.** Disproved by check: dropping
  the Claude review role from `RigShapes` fails it with the exact
  `-claude` container named.
- **Claim (fresh-context review): a class with no roles would render
  `^()$` and admit the empty name.** Confirmed in principle, unreachable
  with today's table; a leaf test now fails if any rendered pattern admits
  `""`.
- **Claim (fresh-context review): a new enumerator left out of the ward
  test's source list escapes it.** Allowed: the rig refuses the unlisted
  name at bind time, so the miss fails closed (an availability failure,
  never a wider cleanup), and the field walk covers the two name structs.

Revisit when ward gains a runtime namespace the rig binds that isn't built
through `handoffNames`, `codexReviewResourceNames`, or a named enumerator,
or when a run-ID producer changes its format.
