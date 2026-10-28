# A Maintenance Task Runs on the Image It Fixes

Mandatory note: returned-object trust boundary (#2336). When project-image
upkeep records a repair-class refusal, the daemon submits one maintenance
task. That task's runs get relaxed admission and verification checks, and
stored records decide when they apply: the refusal, the task's source
artifact, and the task submission. The plan is the implementation-plan
comment on #2336; this note records why the exception has this shape.

## Decisions

- **Chose the refusal's v1 preimage as the task's source over a prose
  description.** The source digest is then the refusal ID, which the
  store's daemon-origin gate (`gateDaemonSubmissionSource`) re-checks on
  every write and decode, and the blob store accepts the bytes only
  because they hash to that ID. A prose source would need a second link
  from task to refusal that nothing re-derives.
- **Chose a per-task base, derived from the refusal, over a base carried
  on the run.** `admitAttempt` sets the base to the refusal's `TipSHA`
  after the ordinary derivation. The refusal record is immutable, so
  every invocation of the task, including implementation and
  remediation, derives the same base without a new run field. Every
  other task keeps the daemon's `-base-sha`.
- **Kept no hydration for the task's rooms (owner decision,
  2026-10-27).** The preparation helper exits 42 when the tip's
  manifests differ from the image's baked copy, which is the normal state
  at a refused tip. The writer can edit manifests and code but has no
  installed dependencies; verification runs in a rebuilt image instead.
- **Required all four of the exception's conditions, each re-derived from
  stored state at use.** `repairTaskRefusal` qualifies a run only when its
  task's source artifact digest names a recorded repair-class refusal on
  the run's project's repository, and the submission recorded under that
  refusal's daemon identity is daemon-origin and created this run's task.
  Verification additionally requires the base to be the refusal's tip and
  the admitted image to be the one the refusal was measured from. A device
  task submitting the same bytes fails the task check: device commands
  are keyed under `client:`, and the engine refuses a daemon origin
  outside `daemon:`.
- **Chose to bypass image selection for the task, and to hold it as
  unserved once the pointer moves, over letting it take another image.**
  Other unserved writers hold on this task, so if it took the ordinary
  selection nothing would run. It gets the pointed-to image only while
  that image is the refusal's; after that it holds, and cancelling it
  releases the other writers to the ordinary hold.
- **Chose upkeep's gate for verification over the serves-base check.**
  The refusal stands in for the image serving the base.
  `projectimage.EvaluateUpkeep`, measured from the image's build commit,
  always applies, so the candidate is verified only in a rebuilt image,
  never the admitted one (`rebuiltImageKeepsAdmitted` excludes it). An
  image already rebuilt for the head's manifests is reused, as the lane
  already does for any rebuild, rather than building again at every
  commit. A re-entered cycle keeps the exception only while its base is
  still the refusal's tip.
- **Chose the first configured manual-submission project in `ProjectID`
  order.** Every configured project shares the daemon's one repository,
  and the store gate refuses a project on another one. With none
  configured, the submit returns `store.ErrNotFound` to the upkeep job.
- **Composed the submit only beside per-attempt image selection.** Only
  the unattended composition has selection. Without it, the task would be
  admitted at `-base-sha` with hydration and could not run at its tip.
- **Planning added `starts-after` edges on #2333, #2385, and #2405
  (owner-confirmed).** The upkeep job records the refusals, the writer
  selection and upkeep hold are what this task is exempt from, and the
  hydration fact and exported preimage are its admission inputs.

## Review Findings

A fresh-context refute-first review found no blocker. It confirmed that
only the daemon's own submission qualifies a run, and that a qualifying
run never runs commands in the admitted image. Mutation checks confirmed
that the integration tests fail when either verification branch is
disabled.

- **One broken image can open several maintenance tasks.** Confirmed and
  allowed: the plan accepted it as a risk. Each refusal record gets its
  own task, so a tip advance while the repair is open opens another. The
  review also found a plausible restart path, where volatile build output
  in a finding's detail mints a new refusal ID. Follow-up: #2410.
- **Changing the configured projects wedges the submit for an existing
  refusal.** Declined: the request digest binds the project on purpose,
  so a replay never retargets a recorded task. The case needs an operator
  reconfiguration, every pass logs the conflict, and the next refusal
  submits to the new project.
- **Attended mode submitted a task that could not run.** Confirmed and
  fixed by composing the submit only beside image selection.

Revisit when one task per broken image lands (#2410), or when the
exception should cover a base other than the refusal's tip.
