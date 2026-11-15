# A Run Starts At The Tip Of Its First Admission

Issue #3203. A `kind:contract` unit (it adds `Run.Base` to the domain
run record) and Tier A (it changes `internal/engine/admission.go` and the
Claude driver's composition, which choose where a writer workspace is
seeded and which image it runs in), so a mandatory note.

Chose to record each run's base on the run at its first admission, from
the tip the tip watch stores, over the daemon-wide `-base-sha` every run
shared. A configured base made every run start at one commit until an
operator restarted the daemon with another, so a long-running daemon
that serves every project (#3220) would keep starting new work at an
ever older commit. Re-reading the tip on every attempt was rejected too:
a retry, a remediation round, or an operator-feedback turn of a run
already admitted builds on work done at the run's first base, and moving
it would discard or misapply that work.

## Decision

- **The base is fixed at the first admission, not at submission.** A run
  held before its first admission (no image serves the tip yet, a quota
  park, a drain) has no work to keep, so it follows the tip and starts at
  the newest one when it finally runs. The base becomes fixed only once
  an attempt exists that later attempts continue.
- **It is recorded in the write that appends the attempt.** `recordAttempt`
  already reads the run, appends the attempt, and writes the run in one
  transaction, so the base and the first attempt commit together. A first
  admission that lost a race to another pass (the run gained a different
  base after this pass read it) holds as `input_unavailable` and the next
  pass admits it at the recorded base. `ValidateRunTransition` refuses
  any change or clear of a set base, so nothing overwrites it.
- **The recorded base is the one before the per-attempt exceptions.** A
  maintenance task's refusal tip and the two handoff-conflict tips still
  replace an attempt's base after the run's base is resolved; they are
  facts about that attempt, not where the run started.
- **Runs stored before this change take their earliest admission's base.**
  A run with attempts and no base reads its admission records, oldest
  first, and uses the first one's base. That read happens only for such
  runs, so ordinary admissions never scan the table. A maintenance task
  stored before the change adopts its refusal tip this way, which is the
  base its exception would give every attempt anyway.
- **A recorded base for another repository is refused.** A run whose
  base names another repository, repository ID, or ref than this
  composition's fails closed with `ErrRepositoryIdentityMismatch` rather
  than seeding and publishing through the wrong repository.
- **No column and no migration.** The base lives in the run's stored
  body, which `PutRun` already validates on every write. Nothing queries
  runs by base, and a column would be a cross-check of the body, which
  Tier B state no longer carries. A stored body without `base` decodes as
  null.
- **Admission reads the stored tip, never the network.** The tip watch
  already reads the tip with conditional requests on every
  active-resource pass. Admission is on the dispatch path, so it reads
  that row; an attempt with no stored tip holds as `input_unavailable`.
  Every attempt derives the tip, including one whose run has a recorded
  base, so a missing row also holds those; the row is never deleted once
  stored, so only a fresh root before its first read can meet that.
- **Start and proofs use the stored tip.** The start selects ward's first
  image there, and each conformance and adapter proof seeds at the tip
  stored when its pass starts. Every start reads the tip and stores it
  through the tip watch's own write, which keeps the tip image upkeep
  last handled, before the engine loop can admit anything. The contract
  asked for a read only when none is stored; chose a read on every start
  because a stored tip can be stale (the branch moved, or an exit-root
  rerun reset it, while the daemon was down), and a first admission
  before the watch's first pass would record that stale commit as the
  run's base for good. A start that cannot read the tip starts at a
  stored one, which the watch then corrects. A root that starts offline
  with no stored tip now fails where it used to start; the daemon has no
  other base to start at.
- **The daemon's `-base-sha` is removed, preflight's is kept.** A flag
  the daemon ignores would mislead the operator, so `freesided` rejects
  it. `freesided preflight` still checks the writer image at its own
  `-base-sha`, which the deputy fills from `repository.base_sha`, and
  `freesided onboard` still builds at that commit. Nothing sets that to
  the tip: the two agree with the daemon only when the operator's
  `base_sha` is the tip, as at the start of a fixture pass, which resets
  `main` to `fixture-baseline`. A real-repository exit run whose
  `base_sha` is behind the tip onboards and checks one commit while its
  runs start at another; #3205 moves the check into the daemon.
- **Image inputs are memoized per base, bounded.** Runs on different
  bases select at their own bases, so the base-inputs memo is keyed by
  commit and layout and keeps the eight most recently first-read commits.
  A commit's tree never changes, so a kept entry never goes stale.

## Review Findings

The refute-first pass was one fresh-context reviewer given the diff and
the contract.

- **Confirmed, fixed:** a start trusted a stored tip, so a run admitted
  before the tip watch's first pass could record a commit the branch had
  since left (above).
- **Confirmed, fixed:** the docs said the deputy sets `base_sha` to the
  tip; nothing does (above).
- **Allowed by decision:** the earliest-admission read for older runs
  lists every admission in the table and filters by run. The full scan
  is deliberate: a row whose run column was rewritten fails it instead of
  vanishing from an indexed read
  (`TestCostCompletenessEnumerationsRejectCopiedRunIDTamper`). It runs
  only for a run with attempts and no base, and that run's next
  admission records one, so the scan repeats only while such a run is
  held.
- **Confirmed, fixed:** a "retry" test case replayed the first invocation
  instead of admitting a new one; a real retry is tested separately.
- **Confirmed, deferred to #3222:** a run whose recorded base later
  leaves the branch (a force-push) holds as `input_unavailable` forever,
  refetching that commit on every pass. Before this change a configured
  base that left the branch held every run the same way, so the change
  narrows the failure to the runs recorded there rather than adding it.
- **Allowed by decision:** a maintenance run stored before the change
  adopts its refusal tip, not the pre-exception base a new one records.
  The repair exception replaces the base on every attempt of such a run,
  so the two never differ in effect.
- **Allowed by decision:** the base-inputs memo forgets the oldest
  first-read commit, not the least recently used one. Past eight live
  bases an old run's base is fetched again; that costs one fetch.
- **Disproved by reading:** no admission bypasses `recordAttempt`; the
  set-once check runs in the write transaction after a fresh read; a lost
  race holds rather than looping, because the next pass reads the
  recorded base; and `ORDER BY rowid` lists admissions oldest first.

## Revisit When

- Ward can hold a proof for more than one image (#3211). Until then two
  runs on bases with different image inputs make ward repoint and rerun
  its suite on each switch.
- An exit run or operator needs a run to start behind the tip on
  purpose. The deputy can no longer ask the daemon for that.
