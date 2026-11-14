# One Supervised Daemon Runs Every Project

Plan revision 125 commits Freeside to one launchd-supervised `prod` daemon
that runs real work for every onboarded project, and returns
`freesided deputy` to an attended run of an unreleased build. No plan
passage or decision note ever made the deputy the operating path. It became
the only real-work launch path when #1938 folded the run script into it;
the step to the installed daemon (#2082, #2083) was re-deferred on
2026-10-27 with "no wave 9 consumer"; and #3132 then set daily development
"through the deputy" as a goal. The owner approved the correction on
2026-11-13 ([proposal](https://github.com/freeside-ai/freeside/issues/3201#issuecomment-6871849151),
tracker #3201). The proposal holds the evidence, the seam table, and the
list of process-wide waits; the unit issues on #3201 carry the mechanism.

## Owner Decisions (2026-11-13)

- **Chose to keep the deputy for attended runs of an unreleased build over
  retiring it.** Exit runs use it, and it is the owner's 2026-10-10
  decision. Rejected: retiring it and running exit runs by installing the
  tip build as `prod`. The cost accepted: every deputy run pauses work on
  every project while it lasts.
- **Chose to leave old deputy roots as read-only history over importing
  them.** In-flight work on a root finishes or is abandoned before `prod`
  takes that project.
- **Chose to retarget #3132 now over letting the deputy path deepen
  first.** Its goal and its two exit lines that named the deputy now name
  the installed daemon. That retargeting was done on 2026-11-13.
- **Chose to keep #2382 an attended deputy run and #2873 unchanged,** which
  reverses the proposal's line that moved that run to `prod`, so #2873
  doesn't wait on Stage 1.

## Records This Revises

Each older note stays frozen; this note carries the correction.

- **`devlog/2026-10-20-1013-prod-runs-driverless.md`, "Two sides, no
  locks."** It accepted the gap between the two start-time checks because
  the installed LaunchAgent passes no driver, so only a hand start reaches
  it. The assumption that changed: an executing `prod` under `KeepAlive`
  reaches that gap on every restart. Chose an authority that never has two
  holders, enforced by the daemon, with the two checks kept, over the two
  checks alone.
- **`devlog/2026-09-28-1800-capacity-scheduling.md`, one call per site.**
  It fixed each judgment-call site at one call at a time, with a second
  call getting the site's fallback. The assumption that changed: that
  collision was rare only because the publication lane and the engine's
  pass serialized the callers, and #3214 and #3215 remove that. Chose a
  busy site that makes the call wait, under a per-pool limit on calls per
  site starting at one, over keeping the fallback. This is not the reserved
  call slot per pool that note rejected: calls still hold no execution slot.
- **`devlog/2026-09-08-0900-explicit-demo-modes.md`, execution disabled by
  default.** Its revisit condition, "when guided onboarding can configure a
  real execution path", is met by #2082 and #2083. Chose `prod` that
  executes when its stored settings say so and its checks pass.
- **#1598's exemption from the memory budget** for automatic image builds,
  proof passes, and model discovery, which the capacity note states as "the
  operator runs them attended, when they choose". The assumption that
  changed: they run on their own, in the deputy today and in `prod` after
  Stage 1. Chose that every container the daemon starts on its own reserves,
  and only a command the operator runs by hand stays outside, over the
  exemption.

## An Earlier Rejection, Narrowed

Revision 71 rejected "running real work through `prod`'s own store" because
the harness isolates a run's database and state from `prod` on purpose
(`devlog/2026-09-28-1715-real-work-app-authority.md`). That reason is about
testing an unreleased build. It still holds for the deputy and doesn't apply
to daily work, so this revision runs daily work in `prod`'s store.

## Alternatives Rejected

- **A persistent deputy that serves many projects.** It duplicates `prod`:
  two daemons, two databases, a lease held forever, and the App authority on
  permanent loan, against Sections 5.2 and 10.
- **Several deputies, one per repository.** The App authority allows one
  writer, and the plan declines a coordinator across instances on purpose,
  so the memory and pool limits would be split by hand.
- **Many projects before `prod` executes.** The supervised launch is the
  smaller step and the one the owner asked for first.
- **The two start-time checks without a lock.** The gap between them
  becomes routine once `prod` executes.
- **Importing the deputy roots' task history into `prod`.** It is a Tier A
  data import for display value only; old roots stay readable with
  `freesided inspect`.

## Assumptions the Owner Can Still Reject

From the proposal:

- **#3214 and #3215 belong on #3201.** Commitment 3 is false without them.
  They don't depend on the daemon move, so they can be scheduled before
  Stage 1, or struck with commitment 3 narrowed to match.
- **Automatic image builds, proof passes, and model discovery count against
  the memory budget** (#3206). The other choice accepts that they can push
  the host past the budget while tasks run.
- **A busy judgment site waits instead of falling back** (#3215). The other
  choice keeps the fallback and keys it by project, which still interrupts
  the operator when two tasks of one project collide.
- **Operator commands come before a client editor for the new settings**
  (#3218), which keeps `api/` off the critical path.
- **The operator runs `execution pause` and `execution resume` by hand.**
  The exit-run skill names the step and checks the result; no agent or
  script runs them.
- **A merged daemon change reaches `prod` only through an install,** as the
  plan already says, so Freeside developing Freeside runs the update drain
  once per adopted daemon change.
- **Interim rule.** Until Stage 1 lands, no feature is added only to make
  the deputy a daily path, and new settings go into the settings types the
  deputy and the installed daemon share.
- **#3062 and #1958 change only after a check** of what one daemon leaves of
  them (#3217).

From planning this revision:

- **Commitment 4 lives in Section 10, Environments,** beside Resolution,
  over Section 5.2 or 5.12.
- **The stages get their own Section 11 subsection** over a paragraph under
  "The First Repository Is Deliberately Boring".
- **The plan stays silent on whether the stages gate a phase exit.** The
  owner hasn't decided it.

## Revisit When

A stage's unit finds that a commitment can't hold as worded, such as a
process-wide wait that no resource limit explains and that can't be
removed, or the App authority needing a second holder.
