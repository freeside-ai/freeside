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

## Classifier

The class comes from the job facts GitHub reports, and infrastructure is
checked first. In #3591's first attempt the macOS test job reported
`conclusion: cancelled`, `runner_id: 0`, and no steps, while two jobs that
only waited on it, `daemon / macos (build, test)` and `required`, reported
`failure` with steps that ran. A failed job with steps is therefore no proof
of a code failure. A runner that dies mid-job has steps and reads as other or
code; that costs a wasted cycle or a card a person reads, and never grants
anything.

Revisit when Freeside gains a landing step, which owns the interval this unit
leaves open: later tips, and the check after the card closes.
