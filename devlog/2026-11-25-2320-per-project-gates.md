# The Gates Read the Task's Project Settings

Unit #3209 makes the path, repository, recipe, review, and hand-back gates
read their inputs from the task's own project, through one per-project
lookup that reads the project's current settings record inside the gate's
transaction. An edit to a project's allowlist, repository, recipe, or
hand-back label now reaches each task's next gate with no restart. The
owner handed the unit over whole ("Don't split"), so the planned Part B
(the recipe) lands in the same PR as its own commits. Tier A: it changes
the admission, import, and publish gates and the store's recipe set.

## The Lookup

- **One source, two shapes.** The installed daemon reads each project's
  stored record and re-checks it as a start does: the record folds with
  every section, every section passes the settings gate, and the
  repository matches the project's binding. File and flag compositions
  (the deputy, tests) get a fixed source that answers every project with
  the composed values, so they behave as before.
- **Chose picking the source by `SettingsRevisions != nil` over a
  `ClaudeDriverConfig` field.** The config is compared by value in tests
  and settings checks, and a function field would break that. A
  configuration composed from the store always carries revisions.
- **A refused project holds; it never fails.** A missing record, a record
  the settings gate refuses, or a binding mismatch comes back joined with
  `ErrProjectSettingsRefused`. Admission classifies it as an existing
  admission-policy hold, and the publication lane treats it as retryable,
  so a corrected record releases the work. No hold reason or API value is
  added. A read or decode error still fails loudly (Tier B rows).
- **A refused project names no hand-back label.** Its pull requests keep
  their labels and its hand-backs start nothing. The gates that hold its
  tasks say why, so the label lookup adds no second report.

## Admission

- **One read per admission.** The derivation, the repository check, the
  writer class, and the recorded project revision all come from the same
  read, so the revision an admission records names the values that
  admitted it. The instance revision stays the composed one, since no
  instance gate reads live.
- **The derivation's other-branch tip is a mismatch, not unavailable.**
  When the stored tip names another repository or branch than the
  project's, the derivation returns `ErrRepositoryIdentityMismatch`. Chose
  this over the earlier "input unavailable", which would hold forever
  without saying a repository edit caused it. Until #3210, a repository
  edit stops new admissions this way until the next start, because the
  tip watch still follows the composed repository.
- **Review approval needed no new code.** Admission already reads the
  trust profile of the run's base repository; a two-project test now pins
  it.

## The Publish Gate

The gate compares the run's recorded base with the project's current
repository ID and refuses with the existing
`ErrRepositoryIdentityMismatch` before the publisher runs. It is retryable,
so the lane holds the task instead of stopping, and undoing the edit
publishes.

## The Recipe

- **Chose narrowing per task in the engine and keeping the store's set a
  union.** The publication lane's set for a task is the operator's recipes,
  plus the task's project's current recipe, plus that repository's adopted
  recipes. The store's set stays the open-time set plus every stored
  project's current recipe, read per transaction and memoized. Narrowing
  the store per project would make evidence unreadable from any reader
  that has no task, such as listings, backups, and checkpoints. The store
  set grants nothing: the lane's gate is the authority.
- **A replaced recipe leaves the store's set at the next start, as
  before.** The store reads each project's current recipe, not its
  history, so evidence under a recipe that was set and replaced within one
  process run stops reading at once. Keeping every recipe a project ever
  approved was rejected: it would turn a replacement into a permanent
  approval, which the re-gate on read exists to prevent.
- **A stopped installed root reads its projects' recipes itself.** Since
  every read approves each stored project's current recipe, a direct
  `freesided inspect task` on the installed root no longer needs
  `-approved-recipe` for them, which
  `2026-11-25-0234-installed-daemon-runs-from-settings.md` still required.
  It needs the flag only for a recipe a project has since replaced; a
  deputy root still needs it, since its recipe lives in a settings file.
- **The `operatorRecipes` sites keep the operator's set.** Fourteen
  attention-item and artifact constructions validate items that carry no
  recipe-bound evidence, so they need no project and keep the operator's
  set, which no longer carries any project's recipe.
- **Readiness and the writer-image check still read the start's recipe.**
  Readiness stays keyed to the composed project and settings (#3212), and
  admission does not gate on the recipe, so until the next start new work
  still starts on an image of the old recipe and the recipe gate blocks it
  at publication, a definitive block as at a restart. Gating admission on
  the recipe was rejected here: it is the writer-image check readiness
  owns. The command and the README say so and tell the operator to
  restart after a recipe edit.

## Command Wording

`settings set` and `settings import` say per section when an edit applies.
The import's `takes_effect` changed from one value to one per section.

## Revisiting Three Notes

- **`2026-11-16-0945-settings-revisions-on-admission.md`** asked for a
  reload point inside a process once settings reload without a restart.
  The reload point is each admission's project read; the instance revision
  keeps the composed value because no instance section is read live.
- **`2026-11-16-0950-project-readiness-record.md`** expected the record's
  key to change inside a process. It does not yet: readiness still keys to
  the start's revisions. A live edit therefore leaves the readiness record
  describing the start's settings until the next start; #3212 owns moving
  it.
- **`2026-11-25-0234-installed-daemon-runs-from-settings.md`** expected a
  settings change to need no restart. That now holds for the gate sections;
  `agent_image`, `enabled`, the per-repository loops (#3210), and readiness
  still need one, so the start order's switch reads stay where they are.

## Refute-First Findings

Mutation checks and one fresh-context review of the branch, before push:

- **Disproved by a check:** a gate could pass on another project's values.
  Pointing the publish gate's lookup, then the recipe set's, at a fixed
  other project made the two-project tests fail, so they catch it.
- **Confirmed, fixed:** a live recipe edit lets new writer runs start on
  the old recipe's image, which then block at publication. The wording now
  says so (above); no test covers the pre-verification block for a live
  edit, which takes the existing revoked-recipe path.
- **Allowed:** a project refusal, like the existing repository-mismatch,
  path-boundary, and trust-profile rows, ends the dispatch pass, so one
  project's refused intent would starve another project's later ones.
  While the daemon serves one enabled project (#3213) all live work is
  that project's, and the row must stay a mutable-policy verdict for the
  stage quarantine. Splitting it per invocation goes with serving two
  projects (#3658).
- **Allowed:** the fixed source answers every project with the composed
  values, as the issue specifies for file and flag compositions, which
  bind every submission to their one project.

Revisit when #3210 moves the per-repository loops or #3212 moves readiness
per project: the repository and recipe wording and the readiness key then
change.
