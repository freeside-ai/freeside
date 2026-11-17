# The Daemon No Longer Reads the Pre-Rename Specification Vocabulary

The daemon stopped writing the `elaboration` vocabulary when #986 renamed it
to `specification` (`devlog/2026-09-01-2256-specification-vocabulary.md`).
Its readers stayed so a database from before the rename would still resume.
This note records why they go now and what that leaves. It covers family 1 of
#3266 only.

## Decisions

- **Remove the readers rather than keep them for old databases.** The owner
  chose family 1 by handing #3266 to implementation before its split was
  applied, which the plan comment reads as that choice. No database the
  daemon can still open holds the old spelling (Evidence), and prod moved to a
  fresh database under #3265. Keeping the readers would keep the two-family
  identity code, the alias-aware SQL filters, and about 3,700 lines of
  readers, fixture, and tests for data nobody has.
- **An old shape now fails loudly, never reads as the current one.** A
  legacy queue kind no longer matches the kind filters, and backup reports it
  as an incomplete closure. A legacy JSON key leaves its current field empty,
  so validation refuses the record. A legacy payload version or policy key is
  refused by the existing checks (`ErrPolicyMissing` for a key).
  `TestPreRenameVocabularyIsRefused` pins the decode cases that a reader of
  the diff can't see: the old stage name, the `spec_artifact` source kind and
  field, and the old invocation and discussion identities.
- **A same-version backup of a pre-rename database no longer restores to a
  working state.** Restore copies the rows, but the first read of a
  pre-rename row fails to decode. No such backup is known to exist; every
  pre-rename database found is also below the migration baseline (120), so no
  current build opens it anyway.
- **One refusal narrows, unreachably.** `authorizeProductionSubmission`
  without a specification grant refused an implementation run reserved under
  either family's claim key; it now sees only the current key. The only
  ungranted entry is the exported `engine.SubmitProductionRun`, which has no
  production caller, and the approval path always carries a grant. The PR
  states this rather than adding a guard for a reservation no database holds.
- **Fixtures that only borrowed the frozen database get current-shape
  seeds.** The plan listed the tests that tested the old shape. Three more
  only used the frozen pre-rename SQL as a convenient approved campaign: the
  reattempt fixture in `cmd/freesided` (behind five reattempt, stop, and
  control-route tests) and two task-history tests in `internal/store`. They
  now start from a campaign seeded through the store's own write gates or
  from a submitted specification, and keep their assertions.

## Evidence

- **Last writer.** Every token's latest change is fb0f5607b (2026-09-01,
  "Rename the elaboration vocabulary to specification"), by `git log -S`
  over `daemon/`. The policy-key prefix lived only in the deleted reader.
  `scripts/check-vocabulary.sh` keeps the old spelling out of daemon code,
  and the seed fixture and `freeside-signet-dev` don't write it.
- **Instance counts (2026-11-16, branch build, counts only).** Rows counted
  in a deleted copy of each database, across every table, for `elaborat` or
  the `spec_artifact` source kind; files counted under `claude-driver/`.
  - **Dev instance:** `check-database` passes; 0 rows, 0 files.
  - **Kept roots at schema 120 or later (9):** 0 rows, 0 files. Two pass
    `check-database`. Seven fail it on kinds this unit doesn't touch (check
    proof, schedule occurrence, artifact, attention item), identically under
    `main`'s build (0352137a2).
  - **Kept roots below the migration baseline (18, schema 22 to 104):** no
    current build opens them. Six hold pre-rename rows (193 in all) and five
    hold pre-rename stage-intent files (38).
- **Prod.** Not read by an agent for this unit. #3265 records prod started
  on a fresh database on 2026-11-16, created by a build from after the
  rename.

## Refute-First Record

- **Current-family identities are unchanged (disproved by a check).** The
  base and branch identity functions ran over 20,000 generated inputs each
  (140,005 decisions). Every difference had a legacy-family input.
- **SQL placeholders match their arguments (disproved).** A fresh-context
  reviewer counted both outbox list queries and both production-attempt
  queries; no `kind IN (?, ?)` remains.
- **The Tier A files weaken nothing current code produces (disproved).**
  Lookups for a legacy specification run find no rows and fail closed; the
  ungranted submission case is the unreachable one above.
- **The invocation parser lost its refusal tests (confirmed, fixed).** The
  deleted legacy test also pinned leading zeros, an empty or overflowing
  iteration, and a foreign prefix, which host verification admission relies
  on. They now run on the current prefix.
- **The vocabulary check still exempts `legacy_vocabulary.go` (confirmed,
  deferred).** The exemption lives in `scripts/`, outside this unit.

Revisit when a database below the migration baseline needs to resume: it
needs a migration path first, and then these readers.
