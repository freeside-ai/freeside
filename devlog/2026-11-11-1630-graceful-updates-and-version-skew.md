# Graceful Updates and Version Skew

Issue #3102; plan revision 116; tracker #3113, with runner hosts in #3112 on
tracker #2161. The owner decided on 2026-11-11, in chat, that Freeside
commits to four things it doesn't do today: updating a running daemon
without destroying work, recovering from a crash without a card for every
interruption, serving apps that lag the daemon, and reading what earlier
builds stored.

Chose to commit to all four now, before any of it is built, because
Freeside is about to develop Freeside (tracker #2873). The daemon and apps
that run the work will be updated many times a day while runs are in flight
and work is queued. The plan states the commitments; nothing in revision 116
exists yet, and each new sentence in the plan says so and names its tracker.

## Today's Behavior

- **An update restarts the daemon with no drain.** That holds until #3106
  merges.
- **Shutdown cancels every live agent session.** `Driver.Close` cancels each
  in-process session and waits for ward teardown
  (`daemon/internal/exec/stage/driver.go:771`).
- **A run lost that way gets a card and no new attempt.** When recovery
  proves an invocation lost (`ward.RecoveryLoss`,
  `daemon/internal/exec/stage/pipeline.go:2229`), it ends with no result,
  and the production lane records a gone terminal with an
  `execution_failure` item. The engine mints no fresh attempt.
- **Any contract difference can stop an app from syncing.** The contract is
  strict on both sides, so a difference can fail every sync read. The app
  then probes `/health` and reports a mismatch when the digests differ
  (`app/Sources/FreesideModel/SyncCoordinator.swift:1086`).
- **Nothing states what a new build must be able to read.** #854 is the
  first known gap.

## Decisions

Each is the owner's, of 2026-11-11, unless it says otherwise.

- **Chose a drain before an update over restarting into running work.**
  While draining, the daemon keeps accepting submissions, starts no new
  launch, and lets running launches finish.
- **Chose a drain that lifts by itself over one the operator resumes.** A
  drain exists only so an update can swap builds, so the start of a
  different build ends it, as the last step of startup and before any launch
  or publication. A restart of the same build keeps it, so a crash in the
  middle of a drain can't resume launches in the middle of a swap. This is
  the one exception to revision 20's rule that a restart never reopens
  unattended admission. The rule stands for the operator stop and for every
  system stop, and lifting a drain lifts neither.
- **Chose to offer a forced update over making every update wait.** The
  operator may cancel running launches instead of waiting for them. Canceled
  launches re-run.
- **Chose re-running a lost invocation over a card for every interruption.**
  An invocation lost to a restart or a forced update is attempted again
  without the operator, up to a durable limit counted across restarts. Past
  the limit the ordinary card appears. Forced-update cancellations don't
  count, because routine updates would otherwise exhaust the limit and raise
  cards for healthy work.
- **Chose one hold for repeated restarts over letting each invocation spend
  its own limit.** Repeated restarts hold every launch and raise one item.
  Otherwise a broken daemon would spend every invocation's limit separately
  and raise a card for each. The plan calls that hold a system stop, so no
  restart lifts it; #3105 has the operator resume it. The agent added that
  sentence after review, because the plan otherwise didn't say what lifts
  the hold.
- **Chose proof of loss before a re-run over speed.** A re-run starts only
  after the lost invocation is proven ended, so no second writer starts
  while the first may still be alive. Revision 94's rejection of automatic
  failover stays, restated as what it rejects: a second writer started while
  the first may still be alive.
- **Chose a provisional update over trusting a new build with the data.** A
  new build is checked against a copy of the database before the drain. If
  the updated daemon fails to start, the previous bundle and database come
  back. This undoes only an update that never reopened: once the new build
  has lifted the drain it may have launched work and published, so restoring
  the old database would discard real state. It is not a downgrade path.
  An update that is abandoned or undone ends its own drain, since the build
  that comes back is the one that drained and its restart would keep the
  drain; #3108 has the restore cancel it. The agent added that sentence to
  the plan after review.
- **Chose a compatibility floor over requiring the same build.** Responses
  tolerate additions, the daemon advertises the features it serves, and it
  states a floor that moves only when a change breaks older apps. An app
  below the floor refuses to work and prompts for an update. The floor
  counts breaking changes, not builds: `api/openapi.yaml` changed in 16
  commits in the six days before 2026-11-11, so a rule of "the last N
  versions" would expire an iPhone build within days (#3109).
- **Chose that the daemon never refuses a client on version.** Strict
  command decoding and digest-bound decisions stay as they are. They, not
  matching versions, keep a stale app from getting a decision through
  against a rendering the daemon didn't produce.
- **Chose that every build reads what earlier builds stored.** That covers
  resuming work and showing history. The reverse isn't promised. A format
  that must change is migrated, and the old reader isn't dropped. This
  answers #854's gating question: yes.
- **Chose the runner protocol version over the build as what hosts must
  share.** It replaces revision 94's rule that the first increment requires
  the same build on every host. Most daemon changes don't touch the runner
  side (Merge Figures, below), so most updates need nothing from the other
  hosts. A drain covers every host. A host on another version is awaiting
  update, which raises no alarm and holds no lease because the host has no
  launch; revision 94 treated it as unreachable. A runner host installs the
  control plane's build when idle and never picks a version for itself.
- **Chose to keep the two sanctioned lifecycle paths.** The installer drains
  through the daemon before it swaps. Scripts and agents still never stop,
  unload, or re-register `ai.freeside.daemon`.

## Settled at Planning

Planning on 2026-11-11 set four defaults the owner can veto. The plan
revision follows them.

- **New commitments say they aren't built.** Without that the plan would
  read as if an update drains today, when the installer restarts the daemon
  with no drain until #3106 merges.
- **A launch lost on a runner host keeps today's rule.** The re-run covers
  an invocation the control plane lost to its own restart or to a forced
  drain. A launch on an unreachable or restarted runner host is still held,
  recovered launch by launch when the runner returns, and offered for retry
  on the ordinary card. #3105 leaves that re-run to tracker #2161's work.
- **§5.14 gains the skew rule, and §5.2's `/health` bullet stays.** The
  digest still diagnoses an app and a daemon built from different specs.
  #1266 and #3109 change the bullet when they change the read.
- **The runner-host sentences land in three paragraphs of Execution Hosts:**
  Transport, Installation, and An Unreachable Host.

## A Correction Found While Writing

The issue says revision 116 overturns "the app's refusal on any digest
difference (#1265)". The app has no such refusal. It reads `/health` only
after a sync read has already failed, and reports the mismatch state when
the digests differ (`app/Sources/FreesideModel/SyncCoordinator.swift:1063`
to `1088`); #1266's planning found the same. What makes the same build
necessary in practice is the strict contract, under which a difference can
fail every read. Revision 116's §13 entry therefore says it replaces strict
response decoding and overturns revision 57's non-goals of version tolerance
and per-feature gating, and that revision 57's digest diagnosis stays. The
commitment is unchanged. The agent made this wording call; the owner can
veto it.

## Deferred

- **Automatic updates of the primary daemon.** No issue. The operator still
  starts every update of the control plane.
- **A separate process for the ward runtime and proxy on the daemon host,**
  so a control-plane restart doesn't end running agents. Anything that needs
  that process waits for it. This revisits
  `devlog/2026-10-22-1042-execution-hosts.md` item 3, which rejected a
  separate runner process on the daemon host for the first increment because
  a second process under one account separates nothing. The assumption that
  changed: that note weighed isolation only. It didn't weigh independent
  lifetimes, where a second process under the same account does buy
  something: the agents outlive a control-plane restart. The decision
  stands for now, with a second trigger added below.

## Rejected

- **Updating runners before the control plane, with two-version protocol
  tolerance.** A drain covers every host, so nothing is running when the
  versions change, and hosts and control plane can update in any order
  (#3112).
- **Salvaging a lost invocation's partial workspace, or resuming its agent
  session.** The re-run is a new attempt. Recovery still adopts only a
  writer-complete run and tears down everything earlier (plan §5.7).
- **Soaking builds on the `dev` install.** It never holds production
  publication credentials.

## Unchanged

- **No downgrade.** An older build isn't promised to read what a newer one
  wrote, and the provisional update is not a way back after a build has
  reopened.
- **No rebuild-from-source button in the app.**

## Written Down

When the installed daemon is broken, the fallback is the hand-run agent
workflow in AGENTS.md, which needs no running Freeside.

## Left to the Units

The re-run limit, and the count and window that make restarts "repeated"
(#3105).

## Merge Figures

How often a daemon update touches the runner side bears on two decisions:
matching hosts on the protocol version, and the deferred separate process,
which would spare running agents exactly the updates that leave the runner
side alone.

- **Range:** the 200 most recent first-parent merge commits on `main` with a
  committer date before 2026-11-11 00:00 -06:00, which run from #2621
  (2026-11-04) to #3057 (2026-11-10).
- **Changed files:** `git diff --name-only <merge>^1 <merge>`.
- **Non-test:** a path under `daemon/` or `api/` that isn't a `_test.go`
  file, isn't under a `testdata/` directory, and isn't Markdown.
- **Runner side:** `daemon/internal/ward/`, `wardstore/`, `wardname/`,
  `buildproxy/`, `exec/`, `verify/`, and `macverify/`.
- **Result:** 124 of the 200 changed non-test `daemon/` or `api/` files, and
  88 of those 124 (71%) changed no non-test file in a runner-side package.

The filing figures were 127 and 90, and planning's recount gave 116 to 142
and 80 to 113 depending on the method. Variants of this method give 66% to
74%: counting Markdown as code gives 138 and 102; ending the range at
`6e2728d71` gives 128 and 94; and widening the runner side to
`projectimage/`, `procbound/`, `export/`, `exportscan/`, and `importer/`
gives 124 and 82. Every variant leaves about seven in ten daemon and API
merges clear of the runner side. Those updates need nothing from the other
hosts, and a separate process would carry running agents through them.

## Revisit When

- The live proof (#3111) reports drain waits and re-runs per update:
  reconsider the separate process on the daemon host, weighing those costs
  against the supervision unit, listener, and recovery path the 2026-10-22
  note counted against it.
- The hardened tier's dedicated user lands, that note's existing trigger.
