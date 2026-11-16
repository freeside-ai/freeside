# Project Readiness: the Record, the Card, and the Gate on New Work

Unit #3205 moves the launch-time checks a deputy's preflight runs into the
daemon, so a daemon that runs for weeks notices when a project stops being
able to run unattended work, and takes no new work for it until it can.
The owner approved the plan these decisions follow and handed the unit
over whole. The issue body carries the acceptance criteria; this note
keeps the choices that rejected a reasonable option.

## The Record

- **Keyed by project and settings revisions.** A record answers whether
  this project is ready under the settings the daemon composed from. New
  revisions start with no record, which reads as not ready until the next
  evaluation, so a settings edit never inherits a pass it did not earn. A
  daemon composed from a settings file keys its record with no revisions.
- **Five checks, each `passed`, `failed`, or `unavailable`.** Chose a
  per-check result over one boolean so the card and the refusal can name
  what to fix. `trust_profile` and `review_configuration` read only the
  store; `repository_base`, `publication_authority`, and `writer_image`
  read GitHub or the base tree.
- **Shared rules, not copies.** The trust-profile rule is the one
  unattended admission applies, and the writer-image rule is the one the
  deputy's preflight applies, each moved to one function both call. A
  difference between preflight and the daemon would hold a deputy that
  launched cleanly. `publication_authority` reads the daemon's own App
  resolution, which already refuses zero or several Apps, so the
  preflight's `selectRepositoryAuthority` stayed where it is rather than
  being moved for an identical rule.
- **An unreachable GitHub keeps an earlier pass.** Chose keeping the last
  `passed` result for the same key, with its check time, over recording
  `unavailable` on every failed connection. The card holds the whole
  daemon, so a network blip would otherwise stop every project. With no
  earlier pass the check reads `unavailable` and the project is not ready.
  A dispatch pass re-observes a record whose GitHub-side checks did not
  all pass at most once a minute instead of waiting for the daily doctor
  pass, so a start that could not reach GitHub, or a repair made outside
  the daemon (an image rebuilt, an App reinstalled), clears the card
  quickly. Retrying only `unavailable` checks left a repaired `failed`
  check holding the daemon for up to a day.
- **No lock or transaction waits on GitHub.** The GitHub checks run first
  and hold nothing; the store-local checks, the write, and the card then
  run under the doctor's convergence lock
  (`2026-11-15-1440-no-locks-across-network.md`).

## The Card and the Hold

- **Held through the operating gate, not a new hold reason.** A not-ready
  record converges on one Blocking `system_health` item for the project
  (code `project_not_ready`). Every open Blocking `system_health` item
  already holds unattended admission (`blocking_system_health`). Rejected
  a new `RunHoldReason`, which would widen the unit to `api/` and `app/`
  for a distinction the item's text already makes.
- **Filed under the real project.** The doctor's other items file under
  `project-system`; this one names the project it is about, so a client
  shows it with that project.
- **A daemon that does not evaluate a project concludes its card.** Only
  the daemon composed for a project converges that project's card, so a
  settings change to another project, or a composition that keeps no
  record, would leave it holding admission forever. Each doctor pass
  concludes readiness cards for every project the daemon does not
  evaluate. This relies on one daemon serving one project; #3212, which
  narrows the hold to one project, revisits it.

## The Gate on New Work

- **The card alone is not enough.** It holds admitted invocations, but a
  submission, an intake admission, or a re-entry start still writes a task,
  a run, or a cycle's authority, which then waits in the queue under a
  hold the submitter never sees. Chose refusing new work at the point it
  is written, with a message naming the failed checks, over accepting it
  and holding it.
- **Re-gated on read.** The gate does not trust the stored record. It
  refuses a record keyed to another project or settings revision, and it
  reruns the two store-local checks in the same transaction, so a trust
  profile revoked since the last pass is refused at once. The GitHub
  checks are trusted from the record because rerunning them would put a
  network call inside a write transaction.
- **New work only.** A replay of a recorded submission, and every path
  that continues an existing task (specification approval, retries,
  operator feedback, remediation), are not gated; each ends in an admitted
  invocation, which the card holds. The engine's enumeration test lists
  each such path with its reason, so a new path that writes work fails
  the test until it calls the gate or is listed.
- **A refusal writes nothing and fails no loop.** Each gated path refuses
  before its first write, so its trigger stays in place and the next pass
  tries again: an intake occurrence stays unadmitted, and a due re-entry
  stays due. A refusal is reported as one resource's failure, never as a
  pass error, because a pass error ends the reconcile loop that returned
  it. A cycle whose head moved while it ran, which starts the next cycle
  as it ends, is retried under a `blocking_system_health` run hold. One
  consequence was accepted: a refused readiness re-entry also leaves its
  ready item open, since superseding it and starting the cycle are one
  transaction. That item's actions are opening the pull request, returning
  it to the agent (which admission holds), marking it seen, dismissing it,
  and stopping the run, so none of them starts new work.
- **Image repair waives `writer_image` alone.** The daemon's image-repair
  task fixes exactly that check, so refusing it while `writer_image` fails
  would deadlock the repair. Any other failed check refuses it too.

## Gaps Accepted

- **A flag-composed unattended daemon has no record and no gate.** It
  names no project, so there is nothing to key a record by. #2083 composes
  every daemon from stored settings, which closes this.
- **`freesided submit` with no running daemon is not gated.** No daemon
  has composed settings to evaluate readiness under. The next daemon's
  card holds the task at admission.
- **Intake logs a deferral for each labeled issue on each pass** while
  the project is not ready, at Info level, since the card already says
  why.

## Refute-First Results

An independent reviewer tried to prove Parts B and C wrong before they
were committed. Three findings were confirmed and fixed: a refused
head-change re-entry ended the active-resource pass for every resource,
and in the publication workflow it ended the reconcile loop; a card left
by another composition was never concluded; a repaired `failed` GitHub
check stayed held until the daily pass. Three were declined:

- **A file-composed record is keyed with no revisions, so an earlier pass
  can outlive a file edit.** The startup evaluation commits before any
  server or loop starts, and the deputy's launch preflight checks the same
  GitHub facts under the current file, so a kept pass was earned under it.
- **The enumeration test does not follow calls through exempt
  functions,** so the exported `SubmitProductionRun` and the artifact arm
  of `SubmitSpecificationRun` write runs ungated. Neither has a production
  caller.
- **The image-repair task picks the lowest manual project,** which the
  gate refuses when it differs from the evaluated project. One project
  runs per daemon until #3212.

These claims held: the subset rule and the revisions record are as Part
A's note states; the gate fails closed on a missing, unreadable, or
mis-keyed record and reruns the store-local checks; the waiver needs a
record; replays and refusals write nothing; every production composition
wires the gate and attended and fake ones do not; no lock or transaction
waits on GitHub; and no credential reaches a check's detail.

Revisit when the daemon reloads settings without a restart, since the
record's key then changes inside a process, or when a hold reason that
clients render separately is wanted.
