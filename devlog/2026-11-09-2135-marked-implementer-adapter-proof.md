# Skip the Adapter Proof When the Implementer Is Marked

The Claude adapter conformance pass proves the adapter on the lineup
implementer's subscription. When that implementer's generation carries a
credential-integrity mark, the pass now records nothing about the adapter
and returns nil, so the adapter's last record stands
([#2955](https://github.com/freeside-ai/freeside/issues/2955)). Every other
resolution or launch-selection failure still records `failed`.

Chose the mark-only skip because the adapter record is keyed by adapter
digest, not enrollment. A `failed` record written because the proving
subscription is marked refused every Claude writer on every subscription,
which breaks the owner decision on
[#1740](https://github.com/freeside-ai/freeside/issues/1740) that a mark
holds only its own generation's admissions
([2026-10-28 note](2026-10-28-1502-marked-generation-startup-posture.md)).
A mark says the proof could not run, not that the adapter failed it.
Startup's `checkWriterRoles` already makes the same exception.

## Rejected Options

- **Skip on every resolution failure.** Rejected: a missing lineup line, a
  disabled identity, or an unreadable tree already holds the whole lineup at
  startup, so a `failed` record there holds nothing wider, and a skip would
  keep a pass the daemon can no longer stand behind.
- **Prove on another subscription.** Rejected for this unit: it needs a rule
  for choosing the substitute, and the proof's credential binding
  (`adapterConformanceLeaser`) is tied to the implementer's resolved agent.
  The kept record is enough for a mark, which re-enrollment clears.

## Refute-First Findings

- **A marked implementer launches through this path: disproved.** The skip
  returns before `observe`; the regression test's `observe` fails the test
  if called. Interrupted-proof recovery binds a recorded agent only to
  bring an earlier proof to rest; it starts no new proof, records
  `failed`, and is unchanged.
- **The skip matches more than a mark: disproved.** Only
  `store.RequireGenerationUnmarked` produces
  `GenerationIntegrityMarkedError`, the one type whose `Is` matches
  `ErrGenerationIntegrityMarked`, and `resolveRole` returns it alone.
  `ResolveRole`'s `errors.Join` adds only `ErrAgentNotAdmissible`. An
  unreadable mark is a different error and records `failed`.
- **A kept pass outlives the image it was proved on: allowed, not new.**
  The adapter digest covers the pinned harness build
  (`agentbaseline.ClaudeCodeHarnessBuild`), and a test binds that pin to
  `images/agent-claude/Containerfile`. Nothing at run time checks that the
  operator-chosen base image carries that build, and the proof itself
  checks capabilities, not the build. So the record has never been scoped
  to an image; the same pass already stands across repoints when the
  implementer runs another adapter. A daemon upgrade that changes the pin
  changes the digest, which has no record and refuses. The fresh-context
  review confirmed the skip adds a second way to keep a pass across an
  image change, so the comment above the proof in `runClaudeConformance`
  now says the record is scoped to the harness build, not the image.
- **No earlier record: fails closed.** For a new adapter digest with a
  marked implementer, nothing is recorded and every Claude writer is
  refused with no conformance record until a pass resolves an unmarked
  implementer, which is no worse than the old `failed` record.

## Limits

Both cases stem from the proof running only on the implementer's
subscription. Follow-up: [#2971](https://github.com/freeside-ai/freeside/issues/2971).

- **Enrollment-specific failure.** An expired token or exhausted quota on
  the proving subscription still writes an adapter-wide `failed` or reduced
  record, and nothing arms a retry before the next scheduled pass.
- **Interrupted proof, then a mark.** Interrupted-proof recovery records
  `failed` before the next pass, which then skips for the mark, so the
  `failed` record stands until a pass after re-enrollment. Recovery keeps
  revoking because an interrupted proof may have been failing; not
  revoking would weaken the rule that a failed observation supersedes a
  pass.

Revisit when the proof can run on a chosen subscription, or when adapter
records gain an enrollment or image scope.
