# Trim Three Tier A Test Matrices and Delete the Device-Node Test

Owner decision of 2026-11-16, from the daemon test audit at `754d60844`:
remove slow tests of unlikely, low-harm failures and add them back only on
evidence. For three Tier A matrices the audit proposed a trim, not a removal;
#3255 carries it out. Each kept row set is chosen from per-row evidence
gathered before the cut, not from the audit's estimate.

## Decisions

- **Filer: one kill per durable state, over killing at every step.**
  `TestFollowUpFilerSurvivesAKillInEveryDurableState` (renamed from
  `...AtEveryStep`, which no longer describes it) stops a filing at four
  named points instead of all twelve. A kill can land only where the filer
  reaches outside the store, and a stop-by-stop probe showed the twelve
  points leave the filing in four states: an open intent with no
  pre-dispatch set (the nine repository, milestone, and listing steps), a
  recorded set before the create (the create's `token`), a dispatched create
  the forge never saw (`send POST .../issues`), and one it answered
  (`receive POST .../issues`). Every stop in a state recovers the same way.
  The audit counted 13; the thirteenth was the counter's uninterrupted pass.
  Each kept stop is matched by step name and store state, and fails if the
  pass never reaches it.
- **A pinned step list replaces "every step" discovery.** Killing at every
  step meant a new outside-the-store step was tested automatically. An
  uninterrupted pass now compares its steps with a pinned list, so a new
  step fails the test until someone decides whether it reaches a new
  durable state and adds a stop for it. Rejected: keeping the counter loop,
  which costs about five times as long and re-tests the same four states.
- **Signet replay matrix: 21 rows to 9.** Choice mode and attachment mode
  are independent fields. Per action, three replay rows span choice
  `absent`, `empty`, `present` and attachment `absent`, `empty`; one
  conflict row has a `present` attachment; choose adds a `different`-choice
  conflict row. Accept's conflict row is `empty/present`, not the
  planned `absent/present`: a retry whose choices don't rebuild a decision
  message keeps its structural command and relies on `PutCommand` for the
  conflict, and only `empty/present` puts a conflict on that path. The
  fresh-context review found this; the coverage checks below did not.
- **Engine acceptance matrix: 9 rows to 3,** `decline/park_unknown`,
  `defer/decline`, `remediate/defer`, so each accepted route and each
  waiting route appears once. All three waiting routes stay: `park_unknown`
  is demoted by `acceptedModelAdjudicationEntries`, while `decline` (goal
  contradictory) and `defer` (goal adjacent) are not and wait for other
  reasons, so each takes its own branches.
- **`TestWalkWorkspaceDeviceNode` deleted.** It skipped in every unprivileged
  run, including both CI platforms, so it guarded nothing. `classifyFile`
  records anything not a symlink or proven-regular file as `special` in one
  `default` branch; `TestWalkWorkspaceClassification` (a `fs.ModeDevice`
  entry, FIFO, socket), `TestExportHostileGolden` (the device entry's wire
  form), and `TestWalkWorkspaceRealFilesystem` (a real FIFO through
  `os.DirFS`) keep that branch covered. Rejected: a hand-written `fs.FS`
  exposing `/dev/null`, which would test a fake instead of the real
  `os.DirFS` path the test existed for.

## Evidence for the Signet and Engine Cuts

Every original row ran alone with `-coverpkg=./internal/...`. Two checks:

- **Union.** Every block a dropped row covers is covered by a kept row, for
  all 12 dropped signet rows and all 6 dropped engine rows.
- **Separability,** since block coverage alone can miss a combination. For
  each block, the rows covering it were tested against "factor one is in
  set X, or factor two is in set Y". In the engine matrix all 2,663 blocks
  fit, so no block depends on the pairing of accepted and waiting routes.
  In the signet matrix (factored as action and choice, by attachment) all
  but 3 of 1,587 fit, and those 3 are the successful-replay path, reached
  exactly when neither field conflicts; the six kept replay rows reach it.
- **Paths, not only blocks.** Separability still passes when two separable
  branches combine into one path: in signet, whether the retry rebuilds a
  canonical command depends only on action and choice, and whether it
  conflicts only on attachment, yet the fallback-then-conflict path is a
  pairing. The kept signet rows reach all four pairings of canonical or
  fallback with replay or conflict. In the engine matrix each finding's
  branches read only its own entry, so no such pairing exists there.

The outcome assertions in both tests read each finding's or command's own
result, so a row with the same branches records the same durable state.

Revisit when the filer gains an outside-the-store step (the pinned list
fails) or a durable write between two existing steps (the pinned list does
not change, so a state that a later write overwrites goes unkilled), when a finding's route handling starts reading another finding's
route, or when replay identity gains a field that interacts with choice or
attachment.
