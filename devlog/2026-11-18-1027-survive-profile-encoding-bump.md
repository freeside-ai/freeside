# Survive a Trust-Profile Encoding Bump One Run at a Time

#1435's contracted v7 profile encoding bump failed `TestStoredCorpusResumes`
on both `publication-pending` fixtures: the publication task's admitted
profile row no longer reconstructed, the load error left the publication
lane, and the daemon shell recorded a durable stop for the whole daemon.
Re-approving the profile could not clear it, because the task stays bound
to the old digest. #3171 makes the next bump safe without making one. The
work is Tier A by path (`internal/store/trust.go`,
`internal/engine/production_publication.go`,
`internal/engine/production_reentry.go`).

## Decisions

- **An unreadable admitted profile ends that run in the definitive trust
  block, not a retried hold.** The block is
  `PublicationBlockTrust` with `rerun_trust_evaluation`,
  `inspect_trust_failure`, and `stop`, and the drift fact
  (`TrustRuleTrustProfileDrift`). A hold was rejected: the row cannot
  become readable again under this build, so a retry only repeats the
  failure, and the owner's existing way out of drift is a rerun that
  rebinds to the current profile. The stale row authorizes nothing: no
  intent, forge call, or ready item is recorded under its digest, and the
  rerun's authorization names the new one.
- **The failure is deferred until the binding's other checks pass.**
  `loadBinding` and `loadReentryBinding` keep loading after the profile
  read fails, skip only the profile comparisons, and return the
  unreadable-profile error after the records' consistency check. For a
  forward task, `loadBinding` also checks the verification image and the
  replay policy's path boundary first, since neither reads the profile. A
  task whose records disagree for another reason still fails loudly as a
  lane error. The checks that need the profile (protected paths, the
  commit message) can't run, and a re-entered cycle's image check runs
  later in its reconcile, so the stop skips it; both outcomes publish
  nothing, and the rerun's binding runs every check.
- **A committed intent is held, pending or finalized.** The issue named a
  pending intent. A finalized intent gets the same per-run
  `HoldTrustBlocked`, because a definitive block would contradict a pull
  request the forge already holds. Neither case offers a rerun.
- **The store marks the unreadable row with a sentinel on both reads.**
  `GetTrustProfile` and `LatestTrustProfile` join
  `store.ErrTrustProfileUnreadable` with the reconstruction error;
  not-found and query errors keep their own. The plan left
  `LatestTrustProfile` alone, but after a real bump the active profile is
  stale too, and the scenario daemon's seed read it and refused to start.
  The seed now treats an unreadable latest profile as already seeded, so
  the owner's re-approval, not the seed, replaces it.
- **The stored-state corpus covers a bumped profile.** The planned answer
  held. `TestStoredCorpusResumesAfterProfileEncodingBump` runs over every
  `publication-pending` fixture: it stales the copied row (which is both
  the admitted and the active profile), starts the scenario daemon,
  asserts the per-run block and no durable stop, records a changed
  profile, decides `rerun_trust_evaluation`, and waits for the published
  PR. Its resume helper, `resumeAfterProfileEncodingBump`, is what the
  plain `publication-pending` resume should call once a real bump lands.
  The re-approved profile has to differ in content, because this build
  would give the admitted content its old digest.
- **The engine tests live in `internal/integration`.** The plan placed
  them in `internal/engine`, but the publication lane's harness (forge
  fake, room, runner) lives in the integration package. No harness runs
  two runs in one lane, so run isolation is shown by the lane returning no
  error and the corpus test's no-durable-stop check, not by a second run
  publishing beside the blocked one.

## Review Findings

A fresh-context refute pass found no path that publishes, authorizes, or
marks a run ready under the stale digest. Its findings:

- **Confirmed and fixed: the deferral skipped two profile-free checks.**
  The verification image and the replay policy's path boundary ran after
  the deferred error, so a task with a bad rebuild record or no explicit
  paths would have ended in a quiet block. `loadBinding` now runs both
  first.
- **Confirmed, fixed in tests: the finalized-intent hold had no test.**
  The committed-intent test now covers a pending and a finalized intent.
- **Declined: a test of an inconsistent binding plus an unreadable
  profile.** The deferral moves no consistency check, each keeps its own
  existing test, and building a task that is wrong in both ways needs
  forged, digest-bound rows.
- **Accepted, deferred: re-entries from a hand-back or external review
  still stop the daemon.** The successor store reads the admitting profile
  before the re-entry binding, which the issue's non-goals left out.
  Only invalidation re-entries (head or base change) stop per cycle.
- **Accepted: a task interrupted after its ready item gets a lasting
  hold.** A re-entered cycle in that state is stopped and its ready item
  superseded; a forward task gets a hold beside its ready item that can't
  clear under this build. Both fail closed, and both replaced a durable
  stop; the follow-up issue tracks them.

## Deferred Readers

Other readers of an admitted or active profile still fail on a stale row:
the export record, import authority, the observe snapshot, the publish
gate's superseded-profile lookup, the successor and external-finding
stores, a rerun intent whose own profile goes stale before it runs, the
review gate's superseding-profile read, and `LatestTrustProfile` readers
that run while the active profile is stale. Follow-up: #3379.

Revisit when a profile encoding bump lands: switch the plain
`publication-pending` resume to `resumeAfterProfileEncodingBump`, and
check that the deferred readers were resolved first.
