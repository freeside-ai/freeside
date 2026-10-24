---
name: exit-run
description: Run a live exit run, the real-backlog production exercise that closes a wave tracker or an ad hoc tracker. Drive freesided deputy against a real issue in a real project from the default-branch tip, walk the operator through the Mac and iPhone checks, file an issue per finding, propose exit dispositions and which findings to block exit on, and record the run, the dispositions, and the start order as a tracking comment on the tracker. Use when the user asks to "run the exit run", "run the exit exercise", "do the exit run for #N", "close out wave N with a real run", "prove the close condition", "run gh-imgup#<n> through production for #1211", or names a tracker plus a target issue to run, even without the words "exit run". Also use to re-run a prior exit run or to file and disposition a finished run's findings. Not for the fresh-context adversarial review (review-wave), wave planning (plan-wave), implementing an exit-required unit (ordinary work-unit flow), or a hermetic test run.
---

# Exit Run (Spine)

You are the spine role (AGENTS.md, Coordination). An exit run is the
close-condition proof for a tracker: the production pipeline runs on a real
issue in a real project, from the current default-branch tip, and the run's
evidence and findings decide whether the tracker can close. This session
owns the whole run: preparation, driving the deputy, collecting evidence,
filing findings, proposing exit dispositions, and recording everything on the
tracker. Three parties share it:

- **You** drive the deputy and the forge, and write every record.
- **The operator** does what needs a human or a physical device: pairs the
  Mac and iPhone, approves the specification in a client, and exercises
  cards. Hand each of these over as an explicit
  checkpoint and wait; never assume it happened.
- **The owner** decides which findings block exit and merges the run's pull
  request. You propose dispositions; the owner adopts, amends, or rejects;
  only then do you record them as decided. The owner merges every real-work
  pull request: never merge one, and don't ask who will.

Stop and report when a precondition fails, when the deputy's self-check
fails before its daemon serves, when a fix would need a production store mutation, or
when an exit disposition needs a call only the owner can make. A partial
record on the tracker is better than a guessed one.

## Inputs

`/exit-run <tracker> <target>`: the tracker issue number (a wave tracker
like #1001, or an ad hoc tracker like #1211 or #1445) and the target issue
URL in the managed repository (for example
`https://github.com/freeasinbird/gh-imgup/issues/82`). Missing either? Ask;
never pick a target by browsing the target repository's backlog.

The run **mode** is derived, not chosen: CLI mode (`freesided submit` seeds
the task) or client-target mode (the task comes from the client composer,
with the settings naming a manual-submission file). Read it from the tracker's Exit section and its latest
exit-gate comment, which may also pin daemon flags or target properties
(`references/prior-runs.md` lists the ones past trackers required).

Every run has two passes on the same `freesided` build SHA, or on a
newer one `scripts/runtime-diff.sh` carries the pass to: a rehearsal pass
on a disposable target (Rehearse Before The Operator), then the real run.
Derive the exit contract (below), then state the derived mode, its reason,
the expected spend of both passes, and every gate the contract leaves
without a trigger, and wait for the user's go-ahead before
preflight: a run costs real money, opens a real PR on someone's
repository, and takes the operator's afternoon, so a wrong reading should
cost one message, not a run.

## Derive the Exit Contract

Before preparing anything, write down what this run has to prove. Read, in
order:

1. The tracker body: its Exit gate, its Units, and the exit dispositions
   (`Exit required`, `Deferred beyond exit`, and so on) recorded in its
   Notes block. A tracker written before `docs/tracker-format.md` carries
   the close condition in its intro and the dispositions on its unit
   lines.
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

The written contract has six parts: the gates this run must satisfy; the
gate table (below); the mode; the actions that must be exercised on real items, on which device
(the ready card's own actions belong on the list only under the rule in
Walk Through below); the boundaries the record may rely on without proving
(an accepted policy, an inactive configuration, a clean-break decision);
and what a clean review would leave unproven. That last one matters: a run
whose review returns zero findings exercises adjudication, remediation, and
re-review as no-ops. If a gate needs a non-empty finding round, plan a
controlled defect in a dedicated challenge repository, never the real
target (#1275 is the precedent), or record the gate as not measurable,
before the run rather than after.

The **gate table** plans every check before launch, one row per gate the
tracker's Exit section names (template in `references/checkpoints.md`):
the trigger, the expected result on the Mac and on the iPhone, and the
point in the run where it must happen. A trigger is one of three things:

- **A real condition the run produces:** the specification card, the
  published pull request and its ready card, the source-issue closure card
  under the human gate, the completion fact.
- **A synthetic `freesided raise-item -alert <name>`:** for an alert that
  needs no real condition, only proof that it reaches the operator (a
  stalled writer, held work, a review that keeps growing). Build
  `freesided` now from a clean checkout of the tip the run will use (the
  rehearsal and the real run reuse that build), and take the names from its
  `raise-item -list`, never from memory or this file; `raise-item -h` says
  which need `-run`. The command raises the item each condition raises, on the
  run's own daemon. Mark each such row a **synthetic-trigger proof**: it
  proves the item is delivered and shown, and the Go tests, not the run,
  prove the condition raises it.
- **None.** A gate no real condition and no `raise-item` alert can reach
  goes into the contract and the go-ahead message as untriggered, with
  what would give it a trigger, before launch. Never discover it after.

The point in the run is **before the merge** for anything that needs the
ready pull request open (an external review, a ready-card action), or
**after the merge and the completion fact** for `review-growth`, which is
refused until the run's work unit is recorded complete, and the alerts
raised with it (Walk Through on Both Clients).

The closure card is **before the merge**, and first among those: under the
human gate (`gates.source_issue_closure` set to `true` in the policy file),
for a source issue in the managed repository, the pull request stays a
draft until the card is decided, so nothing can merge ahead of it. The
card is not in `inspect task`'s `items[]`, because its subject is a
proposal batch, not the task. Until #2226 adds a `freesided` read for it,
its evidence is the client inbox card and
`gh pr view <number> --repo <owner/repo> --json isDraft,body`.

## Preflight (Stop on Failure)

You run these checks yourself, before the rehearsal pass; none needs the
operator. Their environment checks come later, as one checkpoint list
(The Environment Checkpoint below).

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
- **The inputs are complete:** every row of the input checklist below for
  the derived mode, read against the daemon guide's settings table in this
  session (daemon/README.md, Run Real Work With The Deputy); the table is
  the authority and it changes.
- **Credentials are live:** the Codex reviewer token and the provider auth
  identity. Composition preflight's `claude_credentials` and
  `codex_credentials` checks are the authority, and the rehearsal pass runs
  them before the real run spends anything. An expired token fails there,
  after onboarding and image work are already spent (#1363); renew it
  (docs/production-walkthrough.md, Recover Expired Reviewer Credentials)
  and rerun the pass.
- **The state root is usable:** a fresh root, or a retained one the current
  daemon can open. Clean-break migrations retire old roots at startup; don't
  spend a run discovering that. A retained root also keeps every
  system-health item its earlier runs left open, and a restart doesn't close
  them. The blocking ones (not the advisory ones) hold the first run's
  admission with reason `blocking_system_health` until an operator resolves
  them in a client. List them before launch with `freesided inspect health
  -db <root>/daemon/freeside.db` (daemon/README.md, Read A Task's Record)
  and record each item whose `admission` is `holds`, with its `id` and
  `requested_decision`, and whether `operator_stop` is set. Plan to resolve
  them at the pairing checkpoint (Run the Deputy, step 2), or record why
  one stays open. With no daemon running, an item with a `supersession`
  reads as `holds` even when the daemon would waive it; the served read at
  the checkpoint decides it.
- **Nothing else holds the rig** or an execution slot: no stranded
  dispatched invocation, no orphaned cleanup reservation (#1181, #1465). A
  stale lease for this root is recovered at start; one for another root
  refuses the start (docs/production-walkthrough.md, Recover An Interrupted
  Deputy).

### Input Checklist

One table per mode. Each row names the input, where it lives, and the
check that refuses it, so a missing input fails at a known point instead of
during the run. The refusals stay in the daemon; this list makes sure you
never reach them.

**CLI mode.**

| Input | Where it lives | What refuses it |
| --- | --- | --- |
| Settings, every key | `<root>/daemon/settings.json`, mode `0600`: prompt packages from the approved default-branch commit, the approved recipe digest | The deputy, before it takes the lease: a missing, unknown, or repeated key (`requireEveryKey`, `daemon/cmd/freesided/deputy_settings.go`), a relative path, an invalid prompt package. A stale package passes every check; only you catch it (run-70 lost its publication to one). |
| Project | `submit -project`: the settings' `repository.project` | `freesided submit`, which requires it; composition preflight refuses a project the launch does not admit |
| Specification | `submit -task`: a file holding exactly the target's bare canonical issue URL, or a specification file | `freesided submit`, which requires it |
| Policy | `submit -policy`: the resolved policy keys, including `gates.spec_approval` and `gates.source_issue_closure` | `freesided submit`, which validates the keys and the specification policy (`specify.ParsePolicy`, `daemon/internal/specify/policy.go`) |
| Publication | `submit -publication`: for a bare-URL target, the client record with `source_issue` set to that same URL (docs/production-walkthrough.md, The Publication File And Work-Unit Declaration) | Composition preflight and `freesided submit` (`submittedSourceReference`, `daemon/cmd/freesided/submit.go`) |
| Work-unit declaration | `submit -work-unit`: at least `{"completion_criterion": "bound_pr_merged"}` | Composition preflight and `freesided submit` (`errWorkUnitRequired`, same file). A task accepted without one never records completion (#1931). |

A target in the managed repository becomes the pull request's closing
reference; one in another repository is linked and stays open.

**Client-target mode.** The settings and the seed's `submit` inputs are
the CLI-mode rows, for the visibility seed on a fresh root
(docs/production-walkthrough.md, The Visibility Seed). Two rows are added:

| Input | Where it lives | What refuses it |
| --- | --- | --- |
| Manual-submission file | `manual_submission_config` in the settings, the JSON in the daemon guide (Configure Client Task Submission) | The deputy at start, before the lease. A file with an empty `projects` array starts but accepts no client submission. |
| Composer source text | The pairing checkpoint list, which you write and the operator types | Nothing: `Please handle <URL>.` creates valid work with no saved source reference (#1928). When the contract requires source-issue closure, write the bare canonical issue URL, with no prose or trailing punctuation. |

The rehearsal pass exercises every CLI-mode row on its disposable target
before the operator is involved. It leaves `manual_submission_config`
empty, so in client-target mode the real run's deputy start is the first
check of that file; it still comes before the pairing checkpoint.

### The Environment Checkpoint

Hand the operator the environment list from `references/checkpoints.md`
after the rehearsal pass completes and before the real run starts, as one
message. It covers what past runs found live, and each row says what to do
when it fails:

- **The installed production daemon runs driverless** from an app that
  includes #2080, and the App authority lives in its directories
  (docs/production-walkthrough.md, Start And Verify). Leave it running: the
  deputy never stops or restarts it. Preflight's `prod_execution` check
  fails when it reports a driver, but a stopped `prod` passes, so the
  operator confirms it runs:
  `"$HOME/Applications/Freeside.app/Contents/MacOS/FreesideMac" daemon
  status` exits 0 and prints `health=reachable`. The command is the
  operator's (plan §10); ask for its output rather than running it. On
  failure: `daemon start` (app/README.md, Control The Supervised Daemon
  From A Command), or install the current app when it predates #2080.
- **Clients are rebuilt and installed from the tip** on both the Mac and the
  physical iPhone. A client built from an older commit proves the older
  commit. On failure: rebuild and install (app/README.md, Installing the
  Operator Client and Installing on an iOS Device).
- **The phone can reach the daemon.** Both devices are on the tailnet the
  daemon will listen on. A VPN on the Mac can cut the iPhone off (#1930):
  the operator turns it off or confirms the tailnet route. On failure every
  phone proof in the run is lost, so fix it before launch or record each
  iPhone row of the gate table as not measurable. Pairing itself happens
  after launch, against the deputy's `api_url` (the pairing checkpoint
  below); a fresh state root has no paired endpoint to check here.
- **Credentials need nothing from the operator** once the rehearsal pass
  passed composition preflight's `claude_credentials` and
  `codex_credentials`. A Codex chain the renewal reports as revoked needs
  `codex login` and a paired client's **Resolve re-enrollment**
  (docs/production-walkthrough.md, Replace A Revoked Chain And Resolve Its
  Hold): that is operator work before any pass can complete, so stop with
  an Exit Preparation comment and hand it over on its own, outside the run.
- **The liveness probe reaches the phone** when a tracker gate depends on
  it. The probe runs outside the daemon, so nothing in the run installs or
  tests it. You run `bash scripts/daemon-liveness-probe.sh check`; it
  must exit 0. Then run `bash scripts/daemon-liveness-probe.sh
  notify-test`, and the operator reports that the notice arrived on the
  iPhone. On failure: `bash scripts/daemon-liveness-probe.sh install`, or
  fix the phone's subscription, then repeat both.

## Rehearse Before The Operator

A rehearsal pass runs the whole pipeline once, unattended, on a disposable
target, so the launch, inputs, credentials, publication, and alerts fail
here and not in front of the operator. It is the first step after the
go-ahead and preflight, and the operator is asked for nothing before a pass
completes. docs/production-walkthrough.md (Rehearse Before The Real Run)
holds the procedure; this section says what makes a pass count.

1. **Same runtime, fresh root.** Build `freesided` from a clean checkout of
   the SHA the real run will use, with the version flag the walkthrough's
   Start And Verify build names (preflight's `daemon_build` check fails a
   plain `go build`), and make a fresh root (`mktemp -d` style). The pass
   proves that build's runtime only. When `main` advances before the real
   run launches, the run may start from the newer tip only if
   `bash scripts/runtime-diff.sh <pass-sha> <run-sha>` exits 0: the two
   SHAs build the same `freesided` and the diff touches no image,
   `prompts/`, or `policy/` source. The run record then names both SHAs
   and the script's verdict line. Any other exit means a new pass on the
   new tip. An in-run fix keeps its own rule (Run the Deputy).
2. **Settings that differ only where they must.** Copy the real run's
   settings and change only `listen`, `seed_root`, `repository.*` (the
   disposable repository, its project, base, and allowed paths),
   `images.agent` and `approved_recipe` (from the disposable repository's
   onboarding, step 3), `manual_submission_config` (empty), the effort for
   each role, and the `agent_tree.commit` the efforts require. Every role keeps the real run's
   model and runs at `low`, with one exception below: each writer in `writers.*`, `review.effort`, and
   `judgment.effort` when the real run sets `judgment`. A model the images,
   the subscription, or the pinned `judgment.claude_bin` refuses then fails
   in the rehearsal and not in the real run, and `low` keeps the spend
   bounded. A cheaper stand-in model would prove nothing about the real
   one. The pass does not prove a model accepts its real effort: a review
   `max` goes out as `xhigh`, which a model can refuse at its first review
   (daemon/README.md, Select Agents Through The Lineup). Writer roles on one
   model and effort share one agent, named for the first of them, so `low`
   can merge two writer agents the real tree keeps apart and drop a name a
   `--task-line` uses. Keep a writer at its real effort where `low` would
   merge its agent away: the rehearsal tree's writer agent names match the
   real tree's. Effort is part of the emitted agent tree, so the rehearsal
   has its own agent-tree commit, adopted once through the walkthrough's
   First Start procedure and reused by later rehearsals until an adoption
   input changes, which the deputy's self-check refuses.
3. **A disposable target.** An issue in a dedicated throwaway repository
   where the `prod` GitHub App is installed (the `freeside-review-challenge`
   precedent, #1275), small enough that a review comes back clean. Never
   the real target. It is onboarded on its own on the rehearsal root, which
   builds its project image and approves its recipe, and that onboarding
   takes the rehearsal's review-configuration digest: `low` review effort
   is a new review configuration. The walkthrough (Rehearse Before The Real
   Run, Review configuration) gives the order and the commands: agent tree,
   digest, onboarding, then `images.agent` and `approved_recipe`.
4. **Unattended inputs.** The policy file sets `gates.spec_approval` to
   `false` (`PolicySpecApproval`, `daemon/internal/specify/policy.go`) and
   leaves `gates.source_issue_closure` absent or `false`, its policy-approval
   default (`daemon/internal/engine/publication_closure.go`), so neither a
   specification nor a closure card waits for a person. The other inputs
   are built the way the real run builds them, naming the disposable issue
   and project instead of the target, so the pass exercises the CLI-mode
   input checklist. The submit carries every `--task-line` the real run
   will pass. A line resolves only when an attempt of its role is
   admitted, so a pass proves the specifier and implementer lines; a
   remediator line stays unproven when the review comes back clean. Start
   `freesided deputy -root <rehearsal-root>` as a tracked background task,
   submit with `--require-composition`, and record the build, the settings
   digest, and the composition result.
5. **Read, never ask.** Read `freesided inspect task -db
   <root>/daemon/freeside.db -task <task>` at each change of
   `as_of_revision` until the run's `publication.outcome` is set: status
   from `runs[].conclusion`, and the derived state from `freesided follow
   -db <db> -run <run> -snapshot`. A card that needs a person (a finding
   adjudication after a non-clean review, a closure approval) stops the
   pass. That stop is a rehearsal finding, because the contract keeps people
   on card decisions.
6. **Merge, then raise the alerts.** Merge the rehearsal pull request in the
   disposable repository yourself; the owner's merge rule covers real-work
   pull requests on real targets. Wait for the run's
   `publication.completion` in the record, then raise each alert the gate
   table triggers synthetically, with the commands in Walk Through on Both
   Clients. The run-bound alerts (`held-work`, `review-growth`) must show in
   `items[]`. The rehearsal root has no paired device, so their
   `deliveries` stay empty and the pass proves the daemon side only; a
   daemon-subject alert (`stall`) never shows in a task's record, so its
   proof is the raised item ID `raise-item` prints.
7. **End it.** Apply the four record checks in
   docs/production-walkthrough.md (Start And Verify), then end the deputy
   with Ctrl-C. It must exit 0. The file `rig-acquisition.json` is removed
   either way, so it proves nothing: a nonzero exit, or a line saying it
   `left the rig lease for the next start to recover`, means the lease was
   abandoned, and the real run's deputy would refuse that stale lease for
   another root. Then run `freesided rig recover -state-root
   <rehearsal-root>/daemon -confirm` before the real run starts, and record
   the abandonment as a rehearsal finding.

The pass counts as complete only when every step above passed. Record it as
the Rehearsal row of the run record (`references/tracker-comment.md`):
build SHA (and, when the real run starts from a newer tip, that SHA and
the `runtime-diff` verdict), root, settings digest, disposable target,
task and run ids, `billable_cost`, the task lines the pass proved, and the
result of the record checks, the alerts, and the lease release. A pass
that fails is an Exit Preparation comment on the tracker with the defect
filed, and no operator step happens until a later pass completes.

## Run the Deputy

Follow docs/production-walkthrough.md; this section only says where to stop
and what to write down. Keep the deputy's shell open the whole time. Say in a
line what you are about to start, because a run can take an hour and the
operator needs to know when their step is next.

Each operator step is one list from `references/checkpoints.md`, filled in
and sent as one message: pairing and health, specification approval, the
pre-merge walkthrough and merge, and the post-merge alerts. Between
checkpoints, ask the operator nothing. Status, cards, timeline, delivery,
and spend come from the record (Walk Through on Both Clients), never from
the operator.

1. Start only after a rehearsal pass completed on the same build, or on
   an older SHA that `scripts/runtime-diff.sh` carries to this one
   (Rehearse Before The Operator, step 1), and its lease is released. Build
   `freesided` from a clean checkout of the run's SHA and start
   `freesided deputy -root <root>` on the real run's own root as a tracked
   background task. Record the
   root, the build, the settings digest, and the composition-preflight
   result it prints; a failed self-check leaves execution disabled and
   stops the run. Then submit with `freesided submit --require-composition`
   from a second shell: the target in CLI mode, or the visibility seed on a
   fresh root in client-target mode (docs/production-walkthrough.md, The
   Visibility Seed). Record the submission id and task id. In CLI mode,
   check the publication file against what the pull request will carry
   (The Publication File And Work-Unit Declaration) before going on.
2. **Checkpoint: pairing and health.** Once the daemon is up, send the
   pairing list: pair both clients against the deputy's `api_url` (a fresh
   state root) or confirm both still reach the retained root's paired
   endpoint. A startup code that expires before the iPhone pairs is renewed
   with `freesided pairing-code -state-dir <root>/daemon`
   (docs/production-walkthrough.md, Start And Verify). Record a redacted
   receipt and which device paired, never the code.

   When both report paired, run `freesided notify-test -state-dir
   <root>/daemon` yourself. It prints `accepted by ntfy` or the failure per
   paired device, and the operator confirms each test notice arrived; a
   phone cannot send itself one yet (#1959). Before the first approval, the
   operator syncs the run's task on the iPhone, so the approval card is
   there to compare.

   On a retained root, before the first approval, open Inbox > System health
   in the Mac client and resolve each open item with the action its card
   offers (on run 117, More Actions > Resume Unattended closed the run-116
   `daemon_durable_stop` notice; Acknowledge only records, and the item
   stays open). Record each item's id and the action taken for the run
   record, then run `freesided inspect health -db <root>/daemon/freeside.db`
   and confirm no item's `admission` is `holds` and `operator_stop` is
   `null` before the first approval. Only an advisory item may stay open,
   since it never holds admission; record why. A blocking item that no offered action closes
   holds every run at admission: it is a defect that stops the run (below),
   so file it and stop rather than going on. A missed blocking item shows
   as the run's hold reason `blocking_system_health`. In CLI mode the submitted
   task's specification run is already waiting for admission, so that hold
   can show now; in client-target mode nothing waits until the operator
   composes a task.
3. **Client-target mode:** the pairing list's composer row hands the
   operator the exact source text to enter. Find the new task yourself with
   `freesided inspect -db <root>/daemon/freeside.db`, confirm with
   `freesided inspect task` that it is the client's task and not the
   seed's, and record it as the one target. If the derived exit contract requires source-issue closure,
   give the exact bare canonical GitHub issue URL before submission, with no
   prose or trailing punctuation. `Please handle <URL>.` creates valid work
   without a saved publication source reference.
4. **Checkpoint: specification approval.** Send the list once the record
   shows the open `spec_approval` item. The operator approves it in a
   client; record which device and which actions the card offered.
   Leave the human gate on unless the contract says otherwise.

   **For closure-specific client-target runs,** no `freesided` read shows
   the selected task's saved source binding before approval: `inspect task`
   does not report it, and the retired harness's `verify-source` receipt
   has no deputy equivalent. Record the pre-approval source proof as not
   run; neither displayed spec prose nor the seed's publication file is
   evidence of that binding. A wrong source is a finding, not something to
   repair: never edit the target task or clear its checkpoint, and compose
   a corrected task only with the owner's yes, recording it as the new one
   target and the first as superseded.

   The closure proof is then the live one: record the same task's actual
   closure proposal path, including its `propose_site` result; fallback or
   `has_proposal=false` does not satisfy that proof. The later proposal,
   policy or human approval, and publisher checks remain required.
5. Follow implementation to publication in the record: `freesided inspect
   task` for each run's `conclusion` (outcome, hold reason, terminal
   status), and `freesided follow -db <db> -run <run> -snapshot` for the
   derived supervision state. Record the specification and implementation
   run ids, the campaign and attempt, base and head SHAs, the review id,
   model, and outcome, the run's `publication.outcome`, and each run's
   `billable_cost`.
6. **Under the human closure gate,** for a source issue in the managed
   repository (one in another repository gets a `Source issue:` link and no
   card), the publisher opens the pull request as a draft carrying
   `Refs #N` and holds it there while the closure card is undecided. That
   card is an effect-proposal card in the client inbox, open beside the
   ready card; its statement names the merge and the issue.
   It opens the pre-merge checkpoint list (Walk Through on Both Clients),
   not a checkpoint of its own. Once it is decided, the publisher marks the
   pull request ready: with `Closes #N` on an approval that keeps the
   resolves flag, with `Refs #N` on a decline or an approval that turns the
   flag off. Read the draft state and the reference with
   `gh pr view <number> --repo <owner/repo> --json isDraft,body` before the
   decision and after it, and record both beside the device and the
   decision.

**One task per run.** Never resubmit. Each `submit` creates new work, and
a relaunch that leaves a second task for the target puts a second card in
the clients that looks like the first (#1922).

- **A deputy that dies** is restarted on the same root. It recovers its
  lease (docs/production-walkthrough.md, Recover An Interrupted Deputy),
  and the task resumes on its own record.
- **A lost `submit` result** is replayed by its saved identity:
  `freesided submit --db <database> --retry-submission-id <id>` reloads the
  saved inputs and never creates a replacement (docs/production-walkthrough.md,
  Start And Verify).
- **A refused duplicate** (`refuseUnfinishedDuplicate`,
  `daemon/cmd/freesided/submit.go`) names the first task. That task is the
  run's task.
- **The composer has no such guard:** each client submission creates a
  task. The operator composes once. When the client loses the result, find
  the task with `freesided inspect -db <database>`; never ask for a second
  composition.
- **Don't mutate the store,** and don't resubmit with edited inputs.

The record shows exactly one task for the target. A second one is a
finding.

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
  (docs/production-walkthrough.md, Replace The Runtime) and the
  affected evidence repeated on it. Otherwise the finding is suggested to
  block on with its PR named, and the run's evidence stays attributed to
  the SHA that ran. Both prior in-run fixes met this bar
  (`references/prior-runs.md`).
- Writing to the production store to unblock a run is the last resort and
  needs the owner's explicit yes. Back the store up first and record
  exactly what changed.
- A store edit makes the state root unclean evidence; the tracker comment
  carries that caveat (the run-78 `outbox` edit is the precedent and the
  warning).

## Walk Through on Both Clients

The run's PR is not the proof; the walkthrough is. The merge ends the part
of it that needs the ready pull request open: the ready card resolves on
merge, and an external review can only be posted before it. Under the human
closure gate the pull request stays a draft until the closure card is
decided, so that decision comes first.

**Read the record, not the operator.** Status, cards, timeline, delivery,
and spend come from `freesided inspect task`: `items[]` with each item's
`status`, `conclusion`, and per-device `deliveries`; the task `timeline`;
`runs[].conclusion`; `runs[].billable_cost`. `freesided follow -snapshot`
adds the derived state. The closure card is the exception: it is not in
`items[]`, so the operator's inbox report and the `gh pr view` read are
its record (Derive the Exit Contract). Ask the operator only what a screen
shows. On the Mac, prefer a window capture through computer use when the
operator has granted it: an agent can drive a card's buttons without the
pointer, but the card's More Actions menu and the menu-bar Start and Stop
need full-screen control (#1988). On the iPhone, the operator's screenshot
is the evidence, requested in the checkpoint list, never one question at a
time. So:

- **Checkpoint: pre-merge walkthrough and merge.** One list covers every
  action the contract lists before the merge, per device, and ends with
  the PR link for the owner. Under the human closure gate it starts with
  the closure card's rows. After the operator decides the card, confirm
  from the forge read that the pull request left draft and carries the
  final reference before the merge row; a pull request still in draft a
  few minutes after the decision is a finding (on gh-imgup#144 the rewrite
  took under a minute). Collect the operator's reports, check them
  against the record, then wait for the merge.
- The ready card's own actions (`open_pr`, `mark_seen`, `dismiss`,
  `return_to_agent`) are not a per-run obligation. Put one on the
  contract's list only when a tracker gate names it, a prior run left a gap
  on it, or it changed since it last ran live; exercise it before the
  merge. Otherwise record them as "not exercised, owner decision" beside
  what covers them: `open_pr` and `mark_seen` have run on real cards on
  both clients and `return_to_agent` has run live once
  (`references/prior-runs.md`), and `dismiss` rests on its engine and
  client tests.
- Record, per action: the card, the device, whether it actually ran, and
  the resulting state. "Offered" is not "exercised". An action the run
  produced no real item for is recorded as covered by its engine and client
  tests, never left blank.
- Cover the contract's action list, plus anything the tracker's gates name
  (a live Stop, a duplicate submission, a retry). A Stop check follows
  docs/task-cancellation-check.md and records its runtime evidence
  separately.
- After the merge, wait for and record the completion fact (the run's
  `publication.completion` in the record) and the resolved ready item.
  The daemon records that fact against the task's work-unit declaration:
  the one you passed in CLI mode, or the one the daemon wrote for the
  client-composed task in client-target mode. A daemon that stopped itself
  records no completion, and the review-growth alert depends on it, so the
  pre-merge list asks, right before the merge, whether the Mac shows the
  STOPPED banner or an open `daemon_durable_stop` notice. That notice
  names the daemon, not a task, so no `freesided` read shows it.
- Then raise the alert gates, while the run's daemon is still up. The
  review-growth alert is refused until the completion fact is recorded, and
  ending the deputy stops the daemon, so this is the only point where all
  three work. Use the deputy's build and the implementation run's id:

  ```sh
  db=<root>/daemon/freeside.db
  freesided raise-item -db "$db" -alert stall
  freesided raise-item -db "$db" -alert held-work -run <run-id>
  freesided raise-item -db "$db" -alert review-growth -run <run-id>
  ```

  Each prints the raised item's ID and type. Read delivery yourself: a
  run-bound item (`held-work`, `review-growth`) shows in `inspect task`'s
  `items[]` with a `deliveries` entry per paired device. The `stall` notice
  names the daemon, not a run, so no `freesided` read shows it; for that
  one the operator's report is the delivery evidence. Then send the
  **post-merge alerts** checkpoint list: the operator reports, per alert
  and per device, what the notification and the card show, and you record
  the item ID beside each report. Every one of these is a
  synthetic-trigger proof and the record says so: nothing stalled, nothing
  was held, and no review round stands behind the review card, so the
  operator reads the cards and decides nothing on them. A refusal (a
  daemon that is not `ephemeral`, no daemon, a run with no completion) is
  a finding, not something to work around.
  `TestRaiseItemSequenceSurvivesCompletionSweep` in
  `daemon/cmd/freesided/raise_item_test.go` runs this sequence on a
  completed fixture run through the completion sweep and a delivery pass,
  so a daemon change that breaks it fails `scripts/check.sh daemon` before
  an exit run; its order literal and this block change together.
- Read the final task record with `freesided inspect task` while the
  deputy still serves and apply the record checks in
  docs/production-walkthrough.md, Start And Verify:
  no read errors, both evidence fields true, matching heads, and the
  forge's pull request on that head and base. A record that fails one is a
  finding, not proof. Then end the deputy deliberately with Ctrl-C
  (docs/production-walkthrough.md, Complete Deliberately) and record that
  its cleanup proof passed. If the deputy died first, start it again on the
  same root rather than a fresh one: it recovers the stale lease
  (Recover An Interrupted Deputy), and a fresh root would not be this run's
  evidence.
- Every checkpoint list asks for screenshots of anything visually wrong;
  they attach to the finding issue and are the only evidence a client
  defect leaves.

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
- **Deputy and exercise defects** (the deputy command, its settings, the
  walkthrough procedure) are findings too, labeled by their lane. A
  procedure that lost evidence is a defect in the procedure, not operator
  error; say so and file it.
- **Keep private material out.** No pairing codes, credentials, device
  identifiers, tailnet addresses, or raw transcripts in issues or comments.
  Run and invocation ids, SHAs, and PR links are fine.

## Propose Exit Dispositions

Propose **Deferred beyond exit** for every finding that is not **Fixed
in-run**, and mark the ones you would block exit on as **Suggested to block
on**. The marker is a note on a Deferred finding, not a fourth disposition:
a `deferral` label never stops the owner choosing to block. The owner's
answer turns a finding into **Required** (merge before exit) or
**Investigate before exit** (a diagnosis that may spawn a required fix), or
leaves it Deferred; you propose neither of those two yourself.

The test for the marker is the tracker's stated outcome, not its checklist:
Wave 7's first run passed every checklist item while the operator could not
tell which card was live, and the owner adopted a second closure core for
that reason. Use these rules:

- A defect that contradicts the tracker's stated outcome or the plan
  premise it proves (unattended operation that silently bricks, a decision
  surface the operator can't read from the phone) is suggested to block on
  whatever its severity label.
- A known defect that stays reachable in the deployment under evaluation is
  not excused by "it didn't happen during this run".
- An accepted policy, an inactive configuration, or a recorded clean break
  is a legitimate boundary; name it in the record instead of proving
  through it.
- A finding whose fix is cheap and sits on the path the next run will take
  (a prompt rule, a settings key) is worth suggesting even when it doesn't
  contradict the outcome; say which it is.
- A cosmetic or ergonomic defect with no functional loss carries no marker;
  give the trigger that would promote it.

Write the proposal, then ask the owner. Put the suggested-to-block-on group
first, the reason in one line each, and the conditionally blocking items (a
check to run before deciding) as their own group. Record the owner's answer
on the tracker with its date; until then the comment says "proposal".

## Record on the Tracker

The tracker comment is the durable record; chat is not. Write it with the
template in `references/tracker-comment.md`. Its proposal diagram follows
`docs/tracker-format.md` (§status) with Freeside's additions in
docs/coordination.md (Tracking Issues), and it ends with the authority
note.

- **Bottom line first:** can the tracker close, and if not, what stands
  between here and closure, in two sentences.
- **Run record** as a table: the rehearsal pass, state root, clients and
  their commit, each campaign and attempt with its result, review, task
  record, spend, PR, base and head.
- **Gate table** from the contract, each row with its result.
- **Actions exercised** on real items, per device, and the ones not
  evidenced with why.
- **Synthetic-trigger proofs**, apart from the real items: per raised alert,
  the command, the item ID, and what each device received.
- **Findings table:** issue, what's wrong, severity, lane, disposition,
  reason or trigger, with the suggested-to-block-on group above it.
- **Start order** for the exit set the owner adopted (Required and
  Investigate before exit; a diagnosis must finish before closure). A
  proposal has no exit set, so add this section with the status revision
  that records the owner's answer: Startable now, typed chains,
  cross-cutting gates (contract serialization, refute-first, four-front
  cap), critical path, parallel lanes, and the Mermaid diagram. Derive it
  from the issues' Dependencies fields.
- **What was verified and what wasn't,** in `Passed:`, `Checked:`,
  `Not run:` form, including any evidence caveat (a store edit, a device
  that wasn't available).
- **Decision-note disposition:** whether the run produced a lasting
  decision that needs a devlog note, and in which PR.

Update the tracker body only once the owner has adopted the dispositions:
add the exit-set findings to Units (which findings it admits depends on
the tracker kind; see the reference), add an Exit bullet that references
the comment unless the exit set is empty, and refresh the Status section,
as one edit. On a wave
tracker a listed unit needs the phase milestone in the same edit
(milestone and listing publish together, AGENTS.md Work
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
lane table (docs/coordination.md), the deputy's settings (daemon/README.md,
Run Real Work With The Deputy), the walkthrough, rehearsal, and recovery
steps (docs/production-walkthrough.md, including Rehearse Before The Real
Run), the task record's fields (daemon/README.md, Read A Task's Record), the
Stop check (docs/task-cancellation-check.md), the `raise-item` command
(daemon/README.md, Operational Commands), and the Forge Edits and label
rules (AGENTS.md). Those documents are authoritative; when they change,
mirror the change here, and when this skill disagrees with them, they win.
`references/checkpoints.md` restates this skill's operator steps as lists;
change both in the same edit.
