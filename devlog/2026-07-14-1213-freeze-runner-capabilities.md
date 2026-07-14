---
run: manual
stage: freeze-runner-capabilities
date: 2026-07-14
branch: fix/freeze-runner-capabilities
---

# Freeze runner capability declarations at spawn (issue #39)

Spine-role session. #39 is the tail of Track B (`kind:fix`, `exec`/`exec/fake`)
of the Wave 0 exit-fixes batch (#4), on the chain `#34 → #35/#36 → #39`:
predecessors #35 (PR #44) and #36 (PR #46) merged, #39 the only remaining chain
unit with no active claim. Track B serializes on declared paths, not a contract
chain; the one open PR (#45, Track A `domain`/`store`/`api`) shares no path.
Declared paths: `daemon/internal/exec`, `daemon/internal/exec/fake`, `devlog/`. PR #47.

The finding: `CapabilitySet` is an exported map and `RunnerBackend.Capabilities()`
returned it by alias (the fake `return b.Caps`), so a holder could widen/narrow a
backend's declaration after the §5.7 gate read it and a later read would observe the
mutation — the non-waivable gate could admit against one set and audit against another.

## Decisions

- **Admitted snapshot lives in-memory in `exec`, not persisted (user choice).**
  Acceptance 2 ("bind the snapshot to the attempt/run record") has no home in Wave 0:
  no run/attempt record carries a capability field, and capabilities are deliberately
  non-persisted runtime facts (capability.go). Persisting one would add a `domain`/`store`
  type that collides with open Track A PR #45 and reshapes #39 into contract work.
  Chose returning an `Admission{Backend, Declared}` value from the gate over a schema
  change; actual persistence is deferred to ward's real-backend selection (see Deferred).
- **The gate is the single admission entry point.** `CheckCapabilities` now returns
  `(Admission, error)`, reading `backend.Capabilities()` once and cloning that same read
  into `Admission.Declared`. Chose this over a separate `Admit()` that re-reads
  `Capabilities()`: two reads reopen the observe-a-different-set race the snapshot exists
  to close. Only callers were tests, so the signature change is low-blast-radius and the
  `RunnerBackend` interface is untouched (not a contract-interface change).
- **Defensive copy over an opaque immutable type.** Acceptance 1 permits either;
  chose `CapabilitySet.Clone()` (`maps.Clone`, nil-preserving) + returning clones at the
  read boundary, mirroring #35's `fake/clone.go`, over hiding the map behind read-only
  methods — proportionate, and no real backend exists yet to share a redesign.
- **Documented the copy expectation on the interface**, so a future real backend that
  returns its live map is a build-time-obvious contract break, not a silent reopening
  (fix the class at the contract, not only the fake).

## Deferred

- **Persist the admitted capability snapshot into the real run/attempt record** when
  ward's real `RunnerBackend` selection lands (acceptance 2's durable half). Already
  tracked by #39's own Dependencies note ("coordinate with ward before real RunnerBackend
  selection"); no new issue — the dependency is the record.

## Verification

- Refute-first pass (returned-object-trust boundary + non-waivable gate): revert-checked
  both fixes — with the clones removed, `TestCheckCapabilitiesAdmissionFrozen` and
  `TestRunnerBackendCapabilitiesAreCopied` both fail (and the former even catches the
  held-snapshot mutation leaking back into the live backend), so the tests measure the
  fix, not co-exist with it. No other defect found: single-read gate cannot diverge from
  its snapshot; nil-map path clones to nil and `maps.Equal(nil, empty)` holds; refusal
  returns the zero `Admission`. No golden moved (capabilities are JSON-out-of-scope; the
  `golden` and `store` suites pass untouched).
- Queue: grepped `## To promote` / deferred / `needs-human`; the two standing open items
  (`approved-recipe-boundary` store invariant `-> open`, `domain-package` spine-review
  conventions) are outside this unit's `exec` scope — no drain, consistent with #35/#36.
