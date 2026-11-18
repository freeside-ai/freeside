# The Daemon No Longer Reads Old Stage Intent Shapes

The Claude stage driver keeps one intent file per invocation under
`<state-dir>/claude-driver/`. Each time the driver changed what it writes
there, the readers for the old shape stayed, so an intent written before the
change would still recover. This note records why those readers go now (#3327,
family 9 of #3266) and what that leaves.

## Decisions

- **Remove the readers rather than keep them for old state directories.** The
  owner decided on 2026-11-16 that prod history is disposable and prod starts
  on a fresh database (#3265), so nothing needs these formats, and handed
  #3327 to implementation. No current writer produces any of them (Evidence).
  Keeping them would keep seven launch protocols, 22 command goldens, and a
  second prompt path that production never takes.
- **`argument` delivery goes too.** #3266 left it open because the stage test
  suite's own provider returned it. The suite moves to `file_v8` in the first
  commit with production unchanged, so on the current shape it runs the
  prompt-file check production runs. `PromptFileV8` is now the only
  `PromptDelivery` member; the rule that a changed launch command is a new
  member stays.
- **An old shape fails loudly, never reads as the current one.** A missing or
  removed delivery and an export with no `recorded_at` fail `decodeIntent`.
  An intent without `prior_artifacts_persisted`, or with the head-only refusal
  summary, fails the reconstruction gate. A `provider_registry` intent with
  no recorded registry set is refused when its handoff is rebuilt. That last
  check sits in the handoff, not in intent validation, because a start
  refused for its registry set commits with no set and must still decode.
- **The operator sees one attention item per refused file.** Restart recovery
  quarantines an intent it cannot decode as unreadable, and one that fails
  the gate as unauthenticated, and startup keeps one attention item per
  quarantined intent. The run is not resumed.
- **Restore brings back no intent files.** Restore copies database rows,
  and these files live in the state directory, so this unit changes nothing
  a restored backup holds. An old state directory left beside a restored
  database holds files the daemon now quarantines.
- **The pre-extraction fixture goes.** Its decode test is replaced by refusal
  tests that start from a current intent and change one field, so each
  refusal has one cause. A fixture in an old shape would fail for whichever
  field the decoder checks first.

## Evidence

- **Last change to each writer**, by `git log -S` over `daemon/`:
  `launchDelivery = stage.PromptFileV8` in 4cc7eb0c2 (2026-11-10); the
  export stamp in 9308531a5 (2026-09-08); `PriorArtifactsPersisted: true` in
  986d0fa02 (2026-08-11); the tail-keeping refusal summary in 9b6a9060a
  (2026-11-04); the recorded registry set in f55f20050 (2026-10-28).
  `saveIntent` is the only intent writer, `claudeProvider` the only
  production provider, and neither `seedfixture` nor `freeside-signet-dev`
  names a delivery. A declared registry set is never empty
  (`RegistrySet.Validate`), so a launched registry intent always records one.
- **Instance counts (2026-11-17, branch build, counts only).** Files counted
  under each root's `claude-driver/` by prompt delivery, plus the other four
  shapes in every file at schema 120 or later.
  - **Dev instance:** `check-database` passes; 0 files.
  - **Kept roots at schema 120 or later (7):** 22 files. 15 are `file_v8`
    and carry every current shape. **7 are `file_v7`** (6 committed, 1
    `import_pending`), in the two #2603 exit-run roots at schema 132; they
    carry the export stamp and the prior marker and no registry set. These
    are the nonzero count for the owner. `check-database` fails on all 7
    roots, on row kinds this unit does not touch (check proof, schedule
    occurrence, artifact, attention item), identically under `main`'s build
    (4481f907a).
  - **Kept roots below the migration baseline (26, schema 22 to 104):** no
    current build opens them. 25 hold old-shape files (159 in all).
- **Prod.** Not read by an agent for this unit. #3265 records prod started on
  a fresh database on 2026-11-16 and runs no driver.

## Refute-First Record

- **The `file_v8` command is unchanged (disproved by a check).** The four
  `handoff-command-file-v8*.golden` files are byte-identical to `main`, and
  `TestHandoffSpecCommandGolden` passes against them.
- **The export stamp read cannot meet a nil export (disproved).** Both reads
  of `in.Export.RecordedAt` run only on an exported intent: the live path
  sets the export in `advance` first, and recovery reaches them through
  `recoverExported`, which reads `in.Export` before anything else and is
  entered only in the phases whose validation requires an export. A
  fresh-context reviewer corrected the first reason given here.
- **The empty registry set is unreachable for a launched intent
  (disproved).** `startRegistryHosts` requires a valid declared set, and a
  valid set holds at least one host; the only intent with none under
  `provider_registry` is a refused start, which commits before any handoff.
- **A test passed vacuously (confirmed, fixed).** `TestOversizedPromptIsRejected`
  set no delivery, so the delivery check refused it before the size check.
  It now runs on `file_v8` at `ward.MaxPromptFileBytes`, and the render tests
  were checked for the same trap.

Revisit when a state directory from before 2026-11-10 needs to resume: it
needs the launch protocol it started under, from `git show 4481f907a`.
