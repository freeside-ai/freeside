---
name: exit-run
description: Run a live exit run, the real-backlog production exercise that closes a wave tracker or an ad hoc tracker. Drive scripts/run-real-work.sh against a real issue in a real project from the default-branch tip, walk the operator through the Mac and iPhone checks, file an issue per finding, propose which findings must be fixed before exit, and record the run, the dispositions, and the implementation order as a tracking comment on the tracker. Use when the user asks to "run the exit run", "run the exit exercise", "do the exit run for #N", "close out wave N with a real run", "prove the close condition", "run gh-imgup#<n> through production for #1211", or names a tracker plus a target issue to run, even without the words "exit run". Also use to re-run a prior exit run or to file and disposition a finished run's findings. Not for the fresh-context adversarial review (review-wave), wave planning (plan-wave), implementing an exit-required unit (ordinary work-unit flow), or a hermetic test run.
---

# Exit Run (Spine)

You are the spine role (AGENTS.md, Coordination). An exit run is the
close-condition proof for a tracker: the production pipeline runs on a real
issue in a real project, from the current default-branch tip, and the run's
evidence and findings decide whether the tracker can close. This session
owns the whole run: preparation, driving the harness, collecting evidence,
filing findings, proposing exit dispositions, and recording everything on the
tracker. Three parties share it:

- **You** drive the harness and the forge, and write every record.
- **The operator** does what needs a human or a physical device: stops the
  supervised daemon, pairs the Mac and iPhone, approves the specification in
  a client, exercises cards, merges the PR. Hand each of these over as an
  explicit checkpoint and wait; never assume it happened.
- **The owner** decides which findings block exit. You propose; the owner
  adopts, amends, or rejects; only then do you record dispositions as
  decided.

Stop and report when a precondition fails, when the harness fails before the
first daemon launch, when a fix would need a production store mutation, or
when an exit disposition needs a call only the owner can make. A partial
record on the tracker is better than a guessed one.

## Inputs

`/exit-run <tracker> <target>`: the tracker issue number (a wave tracker
like #1001, or an ad hoc tracker like #1211 or #1445) and the target issue
URL in the managed repository (for example
`https://github.com/freeasinbird/gh-imgup/issues/82`). Missing either? Ask;
never pick a target by browsing the target repository's backlog.

The run **mode** is derived, not chosen: CLI mode (`freesided submit` seeds
the task) or client-target mode (`--client-target`, the task comes from the
client composer). Read it from the tracker's close condition and its latest
exit-gate comment, which may also pin daemon flags or target properties
(`references/prior-runs.md` lists the ones past trackers required). State
the derived mode, its reason, and the expected spend, then wait for the
user's go-ahead before preflight: a run costs real money, opens a real PR
on someone's repository, and takes the operator's afternoon, so a wrong
reading should cost one message, not a run.

## Derive the Exit Contract

Before preparing anything, write down what this run has to prove. Read, in
order:

1. The tracker body: the close condition, the exit gates, and the unit list
   with each unit's exit disposition (`Exit required`, `Deferred beyond
   exit`, and so on).
2. Every prior exit-run comment on the same tracker, newest first. They
   carry the gates still open, the evidence gaps a previous run left (an
   action nobody exercised on a real item, a check that ran as a no-op), and
   owner decisions that narrowed the remaining exit (for example "only a
   live Stop check remains").
3. For a wave tracker, the plan §11 row and the internal-exit subsection its
   phase parenthetical names: the exit evaluation is written against those
   elements, one row each.
4. `references/prior-runs.md`, the digest of previous exit runs, and the
   latest two or three exit-run comments on other trackers: the tracker
   issues among
   `gh search issues --repo <owner/repo> "exit run" --sort updated`. The
   same defects recur across runs; the digest names them so you look for
   them on purpose.

The written contract has five parts: the gates this run must satisfy; the
mode; the actions that must be exercised on real items, on which device; the
boundaries the record may rely on without proving (an accepted policy, an
inactive configuration, a clean-break decision); and what a clean review
would leave unproven. That last one matters: a run whose review returns zero
findings exercises adjudication, remediation, and re-review as no-ops. If a
gate needs a non-empty finding round, plan a controlled defect in a
dedicated challenge repository, never the real target (#1275 is the
precedent), or record the gate as not measurable, before the run rather
than after.

## Preflight (Stop on Failure)

Check each item and record the result. A failure here is reported on the
tracker as an "Exit Preparation" comment (template in
`references/tracker-comment.md`), stating what was verified, what was not
created, and what unblocks the run. Never work around a failed precondition
with a partial run.

- **Every unit in the exit set is resolved**, Required ones merged and
  Investigate-before-exit ones diagnosed with any resulting fix merged, and
  `main` is at the tip you will run from. Record that SHA; the run's
  evidence belongs to it. A unit still open means the run would prove a
  deployment the tracker doesn't describe.
- **The target issue is runnable:** open, its prerequisites closed, no open
  PR already implementing it, and inside the scope the tracker's close
  condition names. Verify with `gh issue view <n> --repo <owner/repo>
  --json state,closedByPullRequestsReferences` for the issue's linked PRs,
  then read the open PR list (`gh pr list --repo <owner/repo>`) for an
  implementation that never linked the issue; not from memory. The target
  usually lives in another repository (gh-imgup), and a bare `gh` call
  queries the current checkout's repository instead.
- **The environment is loaded** per the `scripts/run-real-work.sh` header:
  every required variable, prompt packages from the approved default-branch
  commit (a stale package reused from an earlier run cost run-70 its
  publication), the approved recipe digest, and for client-target mode the
  manual-submission file. Read the header in this session; it is the
  authority and it changes.
- **Credentials are live:** the Codex reviewer token and the provider auth
  identity. An expired token fails at composition preflight, after
  onboarding and image work are already spent (#1363).
- **The supervised daemon is stopped** through the Freeside menu or
  `launchctl`, before the harness acquires the rig, and the restore path is
  known (docs/production-walkthrough.md, Restore The Supervised Daemon).
- **Clients are rebuilt and installed from the tip** on both the Mac and the
  physical iPhone, and both are on the tailnet the daemon will listen on. A
  VPN on the Mac can block the tailnet range; a client built from an older
  commit proves the older commit. Pairing itself happens after launch,
  against the active campaign (the pairing checkpoint below); a fresh state
  root has no paired endpoint to check here.
- **The state root is usable:** a fresh root, or a retained one the current
  daemon can open. Clean-break migrations retire old roots at startup; don't
  spend a run discovering that.
- **Nothing else holds the rig** or an execution slot: no stranded
  dispatched invocation, no orphaned cleanup reservation (#1181, #1465).

## Run the Harness

Follow docs/production-walkthrough.md; this section only says where to stop
and what to write down. Keep the harness shell open the whole time. Say in a
line what you are about to start, because a run can take an hour and the
operator needs to know when their step is next.

1. Start `scripts/run-real-work.sh` in the derived mode. Record the session
   directory, the submission id, and the composition-preflight result.
2. **Checkpoint: pairing.** Once the daemon is up, pair both clients against
   the active campaign's endpoint (a fresh state root) or confirm both still
   reach the retained root's paired endpoint. A startup code that expires
   before the iPhone pairs is renewed from the session's retained binary
   (docs/production-walkthrough.md, the `pairing-code` command). Record a
   redacted receipt and which device paired, never the code.
3. **Checkpoint (client-target mode):** when the harness prints its
   awaiting-target line, hand the operator the exact source text to enter in
   the composer. Then run
   `real-work-session.sh select-target <session-directory> <task-id>` with
   the retained session directory and the task id they report, and record
   the acceptance.
4. **Checkpoint:** the specification-approval card. The operator approves it
   in a client; record which device and which actions the card offered.
   Leave the human gate on unless the contract says otherwise.
5. Follow implementation to publication. Record the specification and
   implementation run ids, the campaign and attempt, base and head SHAs, the
   review id, model, and outcome, the verifier result, and the spend.
6. If submit's output is lost or the submission looks stranded, recover it
   by its saved identity: each session mints a `submission-id`, and
   `freesided submit --db <database> --retry-submission-id <id>` reloads
   the saved inputs (docs/production-walkthrough.md). Don't mutate the
   store, and don't
   resubmit with edited inputs; that creates a second task next to the
   stranded one.

A defect that stops the run is a finding first: file it, then stop and
report. Running this skill authorizes no implementation.

- An in-run fix proceeds only when the owner assigns it in this session,
  and it still passes the Coordination Gates (claims, `exclusive-with`,
  contract serialization): a shared-package change is a contract unit even
  when it is one line.
- An assigned fix lands on its own branch and PR from the current tip and
  is verified before the run resumes. The run record marks the finding
  **Fixed in-run** only when the fix is merged and the exercised runtime
  carries it: the daemon rebuilt or restarted from the new `main` SHA
  (docs/production-walkthrough.md, Replace A Retained Runtime) and the
  affected evidence repeated on it. Otherwise the finding is Required with
  its PR named, and the run's evidence stays attributed to the SHA that
  ran. Both prior in-run fixes met this bar (`references/prior-runs.md`).
- Writing to the production store to unblock a run is the last resort and
  needs the owner's explicit yes. Back the store up first and record
  exactly what changed.
- A store edit makes the state root unclean evidence; the tracker comment
  carries that caveat (the run-78 `outbox` edit is the precedent and the
  warning).

## Walk Through on Both Clients

The run's PR is not the proof; the walkthrough is. Two consecutive exit runs
lost their ready-card evidence because the operator merged the PR as soon as
its link arrived, and the merge resolved the card before `open_pr`,
`mark_seen`, `dismiss`, and `return_to_agent` were exercised. So:

- Hand the operator the action list **before** the PR link, and say
  plainly that merging ends the walkthrough. Get their report of each
  action, on each device, and only then hand over the merge.
- Record, per action: the card, the device, whether it actually ran, and
  the resulting state. "Offered" is not "exercised". An action the run
  produced no real item for is recorded as covered by its engine and client
  tests, never left blank.
- Cover the contract's action list, plus anything the tracker's gates name
  (a live Stop, a duplicate submission, a retry). A Stop check follows
  docs/task-cancellation-check.md and records its runtime evidence
  separately.
- After the merge, wait for and record the completion fact (the
  `work_unit_completions` row or its current equivalent) and the resolved
  ready item. Then complete the session deliberately with its printed
  `complete` command. If the harness died first, use
  `real-work-session.sh recover` rather than a fresh session; a session
  the host killed is recoverable and a second session would not be this
  run's evidence.
- Verify the supervised daemon is restored, not just requested: restoration
  has timed out with `registered=false` more than once, and relaunching the
  Mac client can re-register the normal daemon beside a campaign daemon.
- Ask the operator for screenshots of anything visually wrong; they attach
  to the finding issue and are the only evidence a client defect leaves.

## File Findings

Every defect the run shows becomes an issue, filed as you go rather than
reconstructed at the end. Use the work-unit template, so the issue is a work
contract someone can pick up by fiat.

- **One issue per defect**, not per symptom and not per run. Two symptoms
  of one cause share an issue; one symptom with two causes gets two.
- **Search first.** A finding that reproduces an open issue gets a dated
  comment there, not a duplicate. A finding an open issue already covers is
  listed as "not filed, covered by #N" in the tracker comment so the reader
  can check the claim.
- **Labels:** `deferral` as the origin label, plus `lane:*` from the unit's
  declared paths via the canonical lane table (docs/coordination.md) and
  `kind:*` per type. A shared-package change is `kind:contract` and
  `lane:spine`. No milestone on an ad hoc tracker's findings; on a wave
  tracker the spine assigns it at scheduling, not here. A finding only the
  maintainer can act on (a credential, a repository setting, GitHub App
  administration) gets `needs-human` and no lane label instead
  (docs/coordination.md, Deferral escalation); it is never milestoned or
  listed as a schedulable unit, whatever its exit disposition, and the
  comment's table is where its disposition lives.
- **Dependencies:** record typed relationships in the issue's Dependencies
  field when the finding depends on another unit or shares its files. The
  tracker comment's chains are derived from these fields, so a relationship
  that exists only in the comment is a repair waiting to happen.
- **Severity and exit disposition are separate.** State severity (P1 to
  P3) from the defect's reach in the issue's Objective and in the tracker
  table; the template has no severity field and status labels are not
  used. Leave the disposition to the next section.
- **In-run fixes** are still findings: file the issue, link the PR, mark it
  fixed in the table.
- **Harness and exercise defects** (the script, the session tooling, the
  walkthrough procedure) are findings too, labeled by their lane. A
  procedure that lost evidence is a defect in the procedure, not operator
  error; say so and file it.
- **Keep private material out.** No pairing codes, credentials, device
  identifiers, tailnet addresses, or raw transcripts in issues or comments.
  Run and invocation ids, SHAs, and PR links are fine.

## Propose Exit Dispositions

Each finding gets one of: **Fixed in-run**, **Required** (merge before exit),
**Investigate before exit** (a diagnosis that may spawn a required fix), or
**Deferred beyond exit**. The test for Required is the tracker's stated
outcome, not its checklist: Wave 7's first run passed every checklist item
while the operator could not tell which card was live, and the owner adopted
a second closure core for that reason. Use these rules:

- A defect that contradicts the tracker's stated outcome or the plan
  premise it proves (unattended operation that silently bricks, a decision
  surface the operator can't read from the phone) is Required whatever its
  severity label.
- A known defect that stays reachable in the deployment under evaluation is
  not excused by "it didn't happen during this run".
- An accepted policy, an inactive configuration, or a recorded clean break
  is a legitimate boundary; name it in the record instead of proving
  through it.
- A finding whose fix is cheap and sits on the path the next run will take
  (a prompt rule, a harness flag) is worth folding in even when it isn't
  strictly Required; say which it is.
- A cosmetic or ergonomic defect with no functional loss is Deferred, with
  the trigger that would promote it.

Write the proposal, then ask the owner. Put the recommendation first, the
reason in one line each, and the conditionally blocking items (a check to
run before deciding) as their own group. Record the owner's answer on the
tracker with its date; until then the comment says "proposal".

## Record on the Tracker

The tracker comment is the durable record; chat is not. Write it with the
template in `references/tracker-comment.md` and the tracking-issue format in
docs/coordination.md (Tracking Issues): prose digest first, then the diagram
when the graph is more than one chain, fixed edge semantics with the legend
line, double borders on merged units, and the authority disclaimer.

- **Bottom line first:** can the tracker close, and if not, what stands
  between here and closure, in two sentences.
- **Run record** as a table: state root, clients and their commit, each
  campaign and attempt with its result, review, verifier, spend, PR, base
  and head.
- **Actions exercised** on real items, per device, and the ones not
  evidenced with why.
- **Findings table:** issue, what's wrong, severity, lane, disposition,
  reason or trigger.
- **Implementation order** for the exit set (Required and Investigate
  before exit; a diagnosis must finish before closure): Startable now,
  Mergeable next, typed chains, cross-cutting gates (contract serialization,
  refute-first, four-front cap), critical path, parallel lanes, and the
  Mermaid diagram. Derive it from the issues' Dependencies fields.
- **What was verified and what wasn't,** in `Passed:`, `Checked:`,
  `Not run:` form, including any evidence caveat (a store edit, a device
  that wasn't available).
- **Decision-note disposition:** whether the run produced a lasting
  decision that needs a devlog note, and in which PR.

Update the tracker body only once the owner has adopted the dispositions:
add or extend the exit follow-up unit list (which findings it admits
depends on the tracker kind; see the reference), amend the close condition
to reference the comment, and refresh the Implementation order projections,
as one edit. On a wave tracker a listed unit needs the phase milestone in
the same edit (milestone and listing publish together, AGENTS.md Work
Units), so a finding the owner defers stays off that list with its
`deferral` label. Read the body first and preserve
everything else (AGENTS.md, Forge Edits). Verify both saved results. From
then on the body is the live projection: merge cleanup (AGENTS.md, Merge
Cleanup) refreshes the body, not this comment. The comment is the dated
record of its run and is edited in place only for that run: recording the
owner's adopted dispositions, or an owner decision that changes its order,
goes in as a dated status revision at the top. A rerun posts its own
comment under its own run label and leaves the earlier ones untouched, so
the derivation step can read every prior run newest first.

## Close Out

- Open a PR for any in-run fix branch that is still local, and for a
  `references/prior-runs.md` update when the run taught something a future
  run should look for on purpose. Routine rows aren't needed; the tracker
  carries the record.
- Write a devlog note only when a decision-note trigger applies
  (devlog/README.md): a reinterpretation of what the exit proof requires,
  or an owner decision that would otherwise exist only in this chat. Run
  status never goes in a note.
- Report to the user: whether the tracker can close, the exit set with
  its critical path, what waits on the owner, and what waits on the
  operator. Do not claim, start, or plan any unit; `Plan #N` and
  `Handle #N` are the owner's.
- Suggest a fresh session for the follow-up units. This session's context
  is the run; the fix sessions need the issues.

## Synchronization Obligation

This skill restates rules that live elsewhere: the tracking-issue format and
lane table (docs/coordination.md), the harness environment
(`scripts/run-real-work.sh` header), the walkthrough and recovery steps
(docs/production-walkthrough.md), the Stop check
(docs/task-cancellation-check.md), and the Forge Edits and label rules
(AGENTS.md). Those documents are authoritative; when they change, mirror the
change here, and when this skill disagrees with them, they win.
