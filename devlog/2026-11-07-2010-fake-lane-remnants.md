# Keep Four Fake-Lane Remnants When the Lane Goes

Work unit: #2790, which carries out the owner's retirement decision in
`devlog/2026-11-07-1117-retire-fake-publication.md` (#2786). Mandatory
note: Tier A work that deletes a GitHub publish path and moves a
converge that rewrites stored runs.

## Decision: Delete the Lane, Keep What Stored Data Needs

The `-fake-publication` command, its 12 lane-only flags, and the
engine's copy of the publication workflow are deleted. Four pieces stay,
because stored data written by the lane, or by production, depends on
them rather than on the lane:

- **The legacy policy converge stays, moved into
  `internal/engine/legacy_fake_publication.go`.** Fake-lane runs written
  before 2026-08-02 have no resolved policy row, and scheduler fires and
  publication watches on such a run need one. Planning found a local
  database (`~/.freeside/state/freeside.db`, schema 22) that still holds
  an unconverged run, so deleting the converge would strand it. Chose
  keeping it in the engine over moving it into a store migration now,
  because migrations change only through `kind:contract` units; the move
  is a follow-up (below).
- **The two backup extractors stay.** Checkpoint closure records a gap
  for any outbox row whose kind has no extractor
  (`internal/store/local_backup.go`), so dropping
  `engine.fake_publication` or `engine.fake_publication_invocation_owner`
  would mark backups unhealthy on any database holding an old fake-lane
  row. The same local database holds three.
- **Production's item IDs keep the fake lane's namespace.** Production's
  ready and blocked items have always been `fake-publication-ready-…`
  and `fake-publication-blocked-…`. Renaming would orphan every stored
  item, so `readyItemID` and `blockedItemID` now call
  `fakepublication.ReadyItemID` and `BlockedItemID` directly, which the
  deleted wrappers already called.
- **The `fakepublication` package stays unchanged**, because store
  migrations decode old fake-lane rows with it.

The converge's file is listed as Tier A beside the data-rewriting
migrations, since it rewrites stored runs; the lane file's entry goes
with the lane.

## Smaller Choices

- **The publication test harness moved, not rewritten.** 22 integration
  test files use `publicationHarness` and its helpers, so they moved
  unchanged into their own file, keeping the `fakePublication*` constant
  names to keep the move a pure move.
- **Kept tests lost only lane-only setup.** The converge and decoder
  tests built their fixtures with lane helpers (the expected
  invocation owners, `OperatingModeAttendedDev`). They now build the
  same values directly; their existing assertions are unchanged, and the
  invocation-owner test gains a check that its valid owner decodes.
- **Four shared constants moved to `cmd/freesided/main.go`.** The lane's
  command file also defined the GitHub API and remote bases and the
  janitor interval and removal bound that the production driver,
  onboarding, preflight, and setup use.

## Refute-First Record

Each claim that the removal could break kept behavior, and its outcome:

- **The converge, codecs, or extractors changed in the move:** disproved.
  A script compared every moved declaration with its base text; all are
  byte-identical apart from the const block, which lost only the two
  lane-only constants.
- **The long-running daemon reached the lane:** disproved. Only
  `WithFakePublication` set `Engine.publication`, and only the deleted
  command passed it.
- **The deleted command file did something the daemon needs:**
  confirmed for four constants (moved to `main.go`), disproved for
  anything else: the build is clean, and the `.fake-stage-driver`
  default the daemon uses lives in `main.go`.
- **Deleting the lane's tests dropped coverage of kept code:** confirmed
  by the fresh-context review. The only negative tests of
  `fakepublication`'s task and commit-date validation, which every kept
  decode runs, lived in the lane's test file; they moved to the kept
  test file. The lane's terminal-binding test stays deleted: it covered
  only lane code.
- **The leased-`prod` refusal weakened:** disproved. It refuses every
  `-driver` mode but `disabled` with the same message, and its test
  still covers every mode and lease state.

## Revisit When

Revisit when the follow-up `kind:contract` unit moves the converge into
a store migration: then the converge file, its startup and Reconcile
calls, and `store.MigrateLegacyTrustProfileRunPolicy` can go, and the
Tier A entry with them. Follow-up: #2844.
