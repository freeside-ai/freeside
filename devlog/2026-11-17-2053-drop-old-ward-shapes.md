# Ward No Longer Reads Old Handoff Journal and Review Shapes

Ward kept readers for five stored shapes it stopped writing between July and
September 2026, so a record from an older build could still resume or be
torn down. This note records why they go now and what that leaves. It covers
family 8 of #3266, split out as #3326.

## Decisions

- **Remove the readers rather than keep them for old records.** The owner
  decided on 2026-11-16 that prod history is disposable and prod starts on a
  fresh database (#3265), so nothing needs to read these shapes. No database
  a current build opens holds one (Evidence). The five shapes:
  - **Older spec digests.** The pre-size digest (`legacySpecDigest`, the
    unsized spec) and the pre-failure-transcript digest. A journal record
    whose digest matches only an older encoding is now refused with
    `ErrInvalidJournalRecord` at recovery.
  - **The `claude_explicit_bundle_v2` instruction composition.** The journal
    accepts only v3.
  - **The `codex_review_read_only_v2` topology and unversioned bindings.**
    A binding with the v2 topology, or with no instruction composition
    version, no longer authenticates, for teardown or anything else. The
    teardown-only `validateForTeardown` path is gone.
  - **The two six-resource review intent name sets.** The long pre-#587
    observer names and the #587..#590 short names without the #591 snapshot
    volume. `validatedResourceNames` accepts only the current topology per
    provider, and every authenticated intent now carries a snapshot volume,
    so the empty-volume branches in leasing, recovery, and cleanup are gone.
  - **The `workspace-observer` and `agents-observer` rig names.** The rig
    namespace no longer admits them.
- **An old shape now fails loudly, never reads as the current one.**
  - **Journal records.** A pre-size or pre-failure-transcript record no
    longer matches a re-supplied spec's digest, and recovery refuses it
    (`TestRecoverRefusesARecordJournaledBeforeSizes`). A v2 composition is
    refused at decode
    (`TestHandoffJournalInstructionsAcceptOnlyTheCurrentCompositionVersion`).
  - **Review bindings.** A v2 or unversioned binding is refused
    (`TestCodexReviewBindingRejectsTopologyEntryMismatch`, and the
    `codex v2 topology` case in `review_evidence_test.go`).
  - **Review intents.** A six-resource intent is refused under either name
    set (`TestCodexReviewIntentRefusesSixResourceGenerations`).
  - **Rig names.** The rig refuses to bind either long observer name
    (`TestRigBindClosesCodexReviewRuntimeNamespace`).
- **What recovery and rig cleanup no longer match.**
  - **One old intent stops review recovery.** `CodexReviewRecovery.Reconcile`
    and rig recovery read every row in `codex_review_intents`, closed rows
    included. A six-resource row now fails validation: `Reconcile` returns an
    error on every pass, and rig recovery stops at the first such row. An
    instance that holds one needs a fresh database, or the owner removes the
    row.
  - **Old resources are not torn down.** A container named with a long
    observer name, or a resource of an old-topology binding, is no longer a
    ward-owned teardown target. Rig cleanup leaves it in place.
- **A same-version backup taken before the fresh database restores, then
  fails on read.** Restore copies rows, not schema. The old rows then fail
  the checks above when ward reads them. No such backup is known; the old
  shapes found are all in databases below the migration baseline (120),
  which no current build opens.
- **Tests that only borrowed an old shape get current-shape fixtures.**
  Planning named 13; implementation found one more:
  `TestRecoverAdoptsARecordJournaledBeforeSizes` adopted a pre-size record,
  so it now pins the refusal instead. The restart test that ran on a legacy
  intent keeps its sequence under a current intent, since no other test
  covers a source-level `Reconcile` followed by a fresh `Inspect` relaunch.
  The removed test that alone checked recovery's rig authorization is
  ported to a current intent. The commit bodies record each removed test as
  superseded or accepted.

## Kept

These are outside #3326, and nothing here changes them: `LegacyEvaluatedSHA`
and the "request legacy" cases (#3324), the pre-#1585 lease path in
`recover.go`, the `omitzero` and `omitempty` tags, the `snapshot_volume`
field itself, the `TestSpecDigest` pre-registry pin, and the
`workspace-observer` role label in
`testdata/codex-review-helper-role-sequence.golden`.

## Evidence

- **Last writer.** No current writer produces any removed shape. Each one's
  replacement and its commit:
  - **The v3 composition.** 1cdc9043c (2026-08-27).
  - **The v3 topology.** 37ae6b217 (2026-08-08).
  - **The binding's instruction composition version.** b8ff69d00
    (2026-08-04).
  - **The #591 snapshot volume.** 5fc8f2f48 (2026-08-07).
  - **The #587 short names.** bb8d24050 (2026-08-06).
  - **The sized spec digest.** 39b7e700c (2026-09-29), which kept the
    unsized retry. The failure-transcript retry came from 752a80d31
    (2026-09-10), and the pre-size reader from 86d990528 (2026-07-28).
  `instruction.go` writes only v3, `buildReviewAgentSpec` writes the
  provider's current topology and the request's composition version, and
  neither the seed fixture nor `freeside-signet-dev` writes ward state.
- **Instance counts (2026-11-17, counts only).** Each database was counted
  from a SQLite backup-API copy, deleted after use. For each copy: intents
  without a snapshot volume, by state; bindings with an old topology or no
  composition version; journal rows carrying `claude_explicit_bundle_v2`;
  and open journal rows, with how many opened before 2026-10-01.
  - **Dev instance (schema 137):** 0 of each. The branch build's
    `check-database` passes (schema 137 to 146, 138 kinds of stored row
    decoded). Its first run failed making its copy (SQLite error 14) while
    the ward suite loaded the machine; `main`'s build and a branch rerun
    then passed.
  - **Kept roots at schema 120 or later (9):** 0 of each. One holds one open
    journal row, opened after 2026-10-01, a current shape. Two pass the
    branch's `check-database`. Seven fail it on kinds this unit doesn't
    touch (check proof, schedule occurrence, artifact, attention item),
    identically under `main`'s build (4481f907a).
  - **Kept roots below the migration baseline (27, schema 22 to 104):** no
    current build opens them. Seven hold old shapes: 2 closed intents
    without a snapshot volume, 4 old bindings, and 43 v2 journal rows, with
    no open row from before October. The PR asks the owner to treat these
    as discarded, as #3302 did.
  - **Rig containers:** 12 under Apple `container`, none with a long
    observer name; none under Docker.
- **Prod.** Not read by an agent for this unit. #3265 records prod started
  on a fresh database.

## Refute-First Record

- **Current-shape intents are judged the same as before (disproved by a
  check).** The base and branch versions of `validatedResourceNames`, the
  lease volume set, and the runtime name set ran over 200,000 generated
  intents built from all four name sets and mutated. The branch accepted
  exactly the base's 32,168 current-shape acceptances, with identical
  names, lease sets, and runtime names, and refused all 32,590 of the
  base's six-resource acceptances.
- **Recovery's rig authorization lost its only test (confirmed, fixed).**
  A fresh-context reviewer found that the removed legacy-topology test was
  the only check that `recoverCodexReviewIntent` authorizes the derived
  name set before any runtime call. It is ported to a current intent as
  `TestRecoverCodexReviewAuthorizesExactTopologyBeforeRuntime`, which fails
  when that call is removed.
- **One old row stops a whole recovery pass (allowed by the owner's
  decision).** Rig recovery returns at the first intent that fails
  validation, closed rows included, and review evidence errors for a round
  bound under the v2 topology. Both fail loudly, and no database a current
  build opens holds such a row.
- **Recovery rebuilds a spec whose digest differs from launch's (disproved
  by reading).** The reviewer traced the recovery spec through
  `exec/claude/spec.go`: it sets the failure transcript and sizes as launch
  does, so a current record still matches the one digest left.
- **Another package still calls a removed function (disproved).** The
  reviewer built and vetted the whole daemon at the branch tip and vetted
  `ward`, `wardname`, `daemonlock`, and `signet` at each commit.

Revisit when a database below the migration baseline needs to resume: it
needs a migration path first, and then these readers.
