# Freeside Reacts to a Published Pull Request's Required Check

A published pull request's required check is now part of what Freeside
watches while its ready card is open: the card shows the check's state, an
infrastructure failure is rerun, and a test or build failure sends the work
back through a rework cycle. Issue #3598, plan revision 136. The owner kept
the planning defaults below and chose to build the whole unit in one PR over
the plan's six-part split.

The gap came from #2382's routing rerun on 2026-11-24: #3591's macOS test job
was never acquired by a hosted runner, was cancelled after 15 minutes, and
turned `required` red. The pull request sat red under a ready card for about
eleven hours until a person reran the job, which then passed.

## Chose Showing the Check on the Card Over Holding the Card

The ready card is raised as before and shows `pending`, `passed`, or
`failed`. Holding it until the check settles would need a second way to
watch a published pull request, because the ready item is what the
reconciler observes the pull request through. A card that says `pending` is
honest, and a person can wait for it. `pr_checks_deadline` now records an
elapse only while the check is still pending.

## Chose Reading the Base Branch's Rules Over a Project Setting

Freeside reads which checks are required from the base branch, both classic
branch protection and rulesets, and each check's state from the GitHub
Actions jobs on the published head. The base branch already says which
checks it requires, so a setting naming the check could only drift from it.
The publication token already holds `actions: read` and
`administration: read`, so the reads need no new permission. A required
check that Actions doesn't produce stays `pending`, because reading a
third-party status needs `checks: read` or `statuses: read`, which the App
doesn't hold.

## Chose Asking for `actions: write` Over Not Rerunning

GitHub reruns a workflow only for a token with `actions: write`. The App
declared `actions: read`, so the registration now declares `write`, and the
rerun runs on a token narrowed to `actions: write` and `metadata: read`. An
installed App must have the new permission accepted on GitHub; until then the
mint is refused, the card stays `failed`, and Freeside reruns nothing. A
refused mint never fails the daemon.

## Chose a New `check_failure` Authority Over Reusing `hand_back`

A test or build failure starts a `check_failure` cycle that runs as a
hand-back cycle does, on Freeside's own published head. The `hand_back`
authority is the permission to adopt a head Freeside did not push, so a
failed check must never grant it. The new authority is sealed only on the head
Freeside published, from the failure record the daemon itself wrote, and
every read re-proves both.

The authority's base is the base the head's last review used, as an
`external_review` cycle's is, so sealing it needs no forge read. Its gate
refuses a head someone else pushed even when a later re-entry re-earned
readiness on it, because the cycle replaces that head. The one exception
is a head a `hand_back` cycle adopted: that authority already permits
replacing it, and its author was admitted by the trust profile. One failure record
starts one cycle, and a cycle that ended on a person blocks the next until
that person's item closes, as for `hand_back`.

## Chose Escalating the Cycle's Review Findings Over Remediating Them

The plan let the cycle's review route findings to the same remediation that
fixes the check. The implementation escalates them to a person, as every
other in-place re-entry does, so the check's remediation always follows a
clean review and names no finding. Routing both through one remediation
would need an adjudication shape that answers a failure and findings at
once; no case so far needs it, and a person seeing the findings first loses
nothing.

## Chose the Existing Endings Over a New Blocked Item

The plan named one blocked item for every way a cycle stops. The cycle
instead ends the way its parts already do: review findings raise the review
dispute item, a remediator that can't fix the head stops through its Blocked
outcome, and a round limit with no room or an input too large to deliver
raises the re-entry cycle's `publish_blocked` item, whose reason names the
failed jobs and asks a person to fix the head by hand. Each is a card a
person already knows how to answer.

## Chose a Handoff Conflict That Carries No Check Failure

When the cycle's in-place handoff conflicts with the tip, the existing
`handoff_conflict` remediation resolves the conflict and does not carry the
failed jobs. Its result is published and read again, so a check that still
fails starts a new cycle with a new record. Carrying both would need one
input that answers a conflict and a failure at once.

## Classifier

The class comes from the job facts GitHub reports, and infrastructure is
checked first. In #3591's first attempt the macOS test job reported
`conclusion: cancelled`, `runner_id: 0`, and no steps, while two jobs that
only waited on it, `daemon / macos (build, test)` and `required`, reported
`failure` with steps that ran. A failed job with steps is therefore no proof
of a code failure. A runner that dies mid-job has steps and reads as other or
code; that costs a wasted cycle or a card a person reads, and never grants
anything.

## Refute-First Review

One fresh-context reviewer tried to prove the change wrong before it was
committed.

- **Confirmed and fixed:** a log read that fails the same way every pass
  was retried forever. The probe now applies the project gate before any
  log is read, the read asks the storage host for the tail alone, and an
  empty or oversized log is recorded as such. A required job that ends
  `action_required` failed the check but named no job, so the card was
  refused on every pass; every conclusion that doesn't pass now names its
  job. Tests now cover the signed URL staying out of a failed download's
  error and the tail reader.
- **Confirmed, doc only:** this note said the gate refuses every head
  someone else pushed; a head a `hand_back` cycle adopted is allowed.
- **Disproved by reading and tests:** the token following the redirect,
  unscreened CI text reaching the store or the remediator, an authority
  sealed for another run, pull request, head, or record, one failure
  starting two cycles, the `handoff_conflict` refactor changing behavior,
  and an unbounded cycle.
- **Allowed by decision:**
  - **The log host is checked by name, not by resolved address.** Reaching
    an internal address needs GitHub's API to answer with a hostile
    redirect, and that API already decides what Freeside reviews and
    publishes. The download carries no credential and needs a valid
    certificate for the name.
  - **A rerun waits on the App permission without a card.** The README
    names the `check rerun sweep` warning for an App registered before
    `actions: write`.

Revisit when Freeside gains a landing step, which owns the interval this unit
leaves open: later tips, and the check after the card closes.
