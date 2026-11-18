# The Daemon No Longer Reads the Fake Publication Lane's Old Rows

#2790 retired the attended fake publication lane and kept four pieces that
stored data still needed (`devlog/2026-11-07-2010-fake-lane-remnants.md`).
This unit, #3320 (family 2 of #3266), removes three of them and keeps the
fourth. Mandatory note: Tier A work (`internal/store/entities.go` and the
deleted `internal/engine/legacy_fake_publication.go`) and an owner choice.

## Decisions

- **Remove the readers rather than keep them for old databases.** The owner
  decided on 2026-11-16 that prod history so far is disposable and prod
  starts on a fresh database (#3265), so nothing needs to read these
  formats. The removed readers are the converge that gave a
  pre-2026-08-02 fake-lane run a resolved policy row (on every Reconcile
  and at startup), the two store functions it called, the backup
  extractors for `engine.fake_publication` and
  `engine.fake_publication_invocation_owner`, and the `fakepublication`
  task codec, terminal digests, and validator mirror.
- **No store migration replaces the converge.** The #2790 note planned to
  move the converge into a store migration (#2844). Chose deleting it over
  that move, because no database a current build opens holds a run that
  needs converging (Evidence). A migration would rewrite nothing and stay
  as Tier A code to maintain.
- **Production's item IDs keep the fake lane's hash.** Production's ready
  and blocked items have always been `fake-publication-ready-…` and
  `fake-publication-blocked-…`, and each ID hashes the string
  `engine.fake_publication`. So `fakepublication` keeps `ReadyItemID`,
  `BlockedItemID`, and that string as an unexported constant. Chose
  shrinking the package over moving the functions beside their engine
  caller, because the shrink touches neither `internal/signet`'s tests nor
  `.golangci.yml`. A test pins both prefixes and one full ID from each
  function, copied from the code before it changed.
- **An old shape now fails loudly, never reads as the current one.**
  - An outbox row of either kind is an unregistered kind. The daemon still
    starts, seals no new checkpoint (`ErrBackupClosureIncomplete`), and
    backup health reports the artifact closure unhealthy, which holds
    unattended work. `TestRunStartsWithADurableRowItCannotReconstruct`
    pins this.
  - A fake-lane run that was never converged stays as stored: its
    `policy_digest` holds a trust-profile digest and it has no
    `resolved_policies` row. `GetResolvedPolicy` returns `ErrNotFound`, so
    every reader that needs the policy for authority, such as the
    scheduler's fire check and publication-watch arming, fails on it.
    Display readers (the task record and the observation database) show
    it as a run with no policy, the shape a walking-skeleton run already
    has. Nothing rewrites it: run and schedule policy bindings are now
    immutable without exception.
- **A same-version backup taken before the fresh database no longer
  restores to a working state if it holds these rows.** Restore copies
  rows, not schema, and the rows then fail as above. No such backup is
  known to exist.

## Evidence

- **Last writers.** `5ba320d36` (2026-11-07, "Delete the attended fake
  publication lane from the engine") removed every enqueue of both outbox
  kinds and the `PutRun` of the fake-lane run. `10d52417b` (2026-08-02,
  "Bind scheduler fires to resolved policy") is where new runs stopped
  carrying a trust-profile digest. No non-test code outside the deleted
  files names either kind or the `fake_candidate_publication` stage, and
  `internal/seedfixture/` and `cmd/freeside-signet-dev/` don't import the
  package.
- **Instance counts (2026-11-17, counts only).** Counted in a deleted copy
  of each database: outbox rows of the two kinds, fake-lane runs with no
  resolved policy row, and schedules bound to those runs.
  - **Dev instance:** 0, 0, 0. The branch's `check-database` passes.
  - **Kept roots at schema 120 or later (9):** 0, 0, 0. Two pass the
    branch's `check-database`. Seven fail it on kinds this unit doesn't
    touch (check proof, schedule occurrence, artifact, attention item),
    identically under `main`'s build (4481f907a).
  - **Kept roots below the migration baseline (27, schema 22 to 104):**
    one, at schema 22, holds 3 outbox rows and 1 unconverged run (it has no
    schedules table). It is the database the #2790 note kept the converge
    for. The other 26 hold none. No current build opens a database below
    the baseline, `main` included, so removing these readers strands
    nothing a build could otherwise read.
- **Prod.** Not read by an agent for this unit. #3265 records prod started
  on a fresh database on 2026-11-16.

## Refute-First Record

- **The item IDs changed (disproved by a check).** The pin test passed
  before and after the package shrink, and its expected hashes match an
  independent SHA-256 of the hash input.
- **A current writer still produces a removed shape (disproved).** See
  Last writers.
- **Reconcile changed for current data (disproved).** The converge listed
  only outbox rows of the fake kind, so on a database without them it did
  nothing. `TestStoredCorpusResumes` passes with its corpus unchanged.
- **An unconverged run reads as current (disproved by reading).** Its
  policy lookup returns `ErrNotFound`, and even a policy row would fail
  the readers' digest comparison against the run's trust-profile digest.

Revisit when a database below the migration baseline needs to resume: it
needs a migration path first, and then a converge for its fake-lane runs.
