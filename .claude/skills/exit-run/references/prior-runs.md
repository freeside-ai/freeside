# Prior Exit Runs

A digest of the exit runs so far: what each proved, what broke, and the
defects that recur. The tracker comments are the record; this file is the
pattern list a new run reads before preflight so it looks for known failure
modes on purpose. Update it when a run teaches a new pitfall or changes a
pattern; a routine run needs no row.

## Runs

| Run | Tracker | Target and result | Mode | What it taught |
| --- | --- | --- | --- | --- |
| Wave 6 exit, 2026-08-28 | #835 | gh-imgup#72 to PR #103, merged by the owner | CLI (label intake) | The review was clean, so adjudication, remediation, and re-review ran as no-ops; the shadow arm had no evidence because its configuration was never approved. One harness defect (#996: supervision never reached `published`). The daemon had stopped before the merge, so no completion fact was recorded. |
| Wave 7 exit run 1, 2026-09-04 | #1001 | gh-imgup#80 to PR #105, merged by the owner | CLI | Every exit-proof checklist item passed while the operator could not tell which card or run was live; the owner adopted a second closure core of seven legibility units. Specification rejected for a trailing newline (#1123, fixed in-run: the daemon was rebuilt from `f5fb30d1` before attempt 2). Attempt 1 failed export on gitignored `dist/` (#1132). Ready-card actions lost because the PR was merged first. About USD 6 of Claude spend. |
| Wave 7 exit run 2, 2026-09-06 | #1001 | gh-imgup#78 to PR #106, merged | CLI | A crash between ward export and the export-record commit stranded a dispatched invocation that held the execution slot for hours with no log or item (#1181). Review completion evidence resolved to nothing, so a clean pass and a silent no-op were indistinguishable (#1182). Unblocking needed a direct `outbox` edit, which made `state-80` unclean evidence. Ready-card actions lost the same way a second time. |
| Wave 7 blockers run, posted 2026-09-07 | #1001 | gh-imgup#77 to PR #107, merged | CLI | The reviewer could not reach the repository, returned exit 0 with empty findings, and Freeside accepted it as clean (#1212). Six exit blockers (#1212 to #1216, #1187): review progress on the clients, docs-versus-allowed-paths conflicts, prospective verification claims, branch conventions, and the walkthrough and restore path. |
| Wave 7 rerun, 2026-09-08 | #1001 | gh-imgup#79 to PR #108, left unmerged | CLI | The first attempt never ran: a historical-data compatibility regression kept backup health from becoming ready (#1226). Preflight rejected the inherited reviewer-configuration approval, correctly. A specifier decode rejection (`unknown field "comment"`) cost one attempt. The rerun gave the first live `open_pr` and `mark_seen` on both clients; the return-to-agent feedback path failed (#1234, #1235). Supervised restoration timed out with `registered=false`, and the new-input preparation reused a stale publication body. |
| Wave 7 controlled challenge, 2026-09-12 | #1001 | freeside-review-challenge#1, left unmerged | CLI, dedicated challenge repository | The first full live chain with a genuine Codex finding: classification, adjudication, remediation, re-review, clean final round, publication, verifier (#1275). The host killed the harness and `real-work-session.sh recover` completed the session as designed. Relaunching the Mac client re-registered the normal daemon beside the campaign daemon. The remediator prompt package described a workspace state the driver does not produce. |
| #1211 run-82, 2026-09-15 to 16 | #1211 | gh-imgup#82 to PR #110, merged | Client target | Preparation was blocked twice before launch: no production submission-policy configuration (#1360), then an expired Codex token at composition preflight (the recovery gap is #1363). The client-created run produced 19 findings mapped to 21 units, 13 required for exit. The PR merged without the `Closes #82` its template required, which started tracker #1445. |
| #1211 run-70, 2026-09-21 | #1211 | gh-imgup#70 to PR #111, ready for review | Client target | Implementation aborted at ten minutes on the writer stop timeout (fixed in-run, PR #1456, validated on a resumed implementation of about 29 minutes). Publication blocked because a stale implementer prompt package was reused. Nine findings (#1457 to #1465); the recommendation held exit only on Stop working from a real client (#1457). The owner then narrowed the gate to a live Stop check and made the next run also the #1445 proof. |
| #1445 run-113 after #1535, 2026-09-25 | #1445 | gh-imgup#113 to PR #120, merged | Client target | The first passing policy-approved closure proof: `verify-source` receipt, stored authoring, a `propose_site` proposal, a policy approval, and publisher-written `Closes #113`; the merge closed the issue. The first session died at preflight because Claude Code auto-update pruned the pinned judgment CLI. `return_to_agent` worked live for the first time: the feedback run republished onto the same PR. Two clean reviews still passed a PR missing the decision note its repository's AGENTS.md required, because the review rubric only admits defects with a failure path (#1542). The successor publication raised a self-clearing "repair external state" card (#1544). Restore stalled at `registered=false` again. |
| #1445 run-114 human gate, 2026-09-25 | #1445, #1211 | gh-imgup#114 to PR #121, merged by the owner | Client target | The human-gated proof passed and closed #1445: the PR stayed a draft with `Refs #114` until the closure card was approved on the iPhone, then the publisher wrote `Closes #114`. The first session died at preflight because a review-prompt bump (PR #1546) invalidated the onboarded reviewer digest. The reviewer flagged the missing decision note live (#1542 working), but the adjudicator recommended parking an in-scope fix twice; two Discuss rounds reached `remediate` (#1550). A #1211 sketch exercise afterwards bricked the daemon with a durable stop once its clarified spec was ready (#1556). Restore stalled at `registered=false` a third time, probably because nobody was handed the menu Start step. |
| Wave 8 exit run 2, 2026-10-12 to 13 | #1613 | gh-imgup#116 to PR #127, merged by the owner | CLI | The pull request waited overnight for its merge, and the run's daemon stopped itself twice meanwhile: a GitHub 500 on the janitor's token mint (#1997), then the backup producer refusing the run's own effect-proposal artifact twelve hours after start (#1996). Stopped, it could not see the merge, recorded no completion, and so could not raise review growth. That stop was the first real stopped-state evidence on an exit run. The first real external review superseded the ready card and reached the Mac as a card, but the adjudicator's batch parked, no remediator ran, and the card offered only discuss and stop (#1989, #1990). The spine drove the Mac client through computer use; the iPhone never connected (#1514), so no phone proof exists. Three owner decisions changed this skill (devlog/2026-10-13-1002-exit-run-owner-decisions.md). |
| Wave 8 exit run 3, 2026-10-14 to 16 | #1613 | gh-imgup#117, no pull request; ended final with `base_advanced` | CLI | The run reached a second internal review round and the harness's observation deadline expired while the run waited on the review-dispute card; `--resume-session` then refused because the run had not published (#2040, #2041). The per-finding review-dispute decision ran live for the first time, on the internal review, with a remediation and re-review behind it. A retained root's open system-health items hold the next run's admission (#2043), and `inspect` cannot list items on a stopped root (#2042). Resumed on 2026-10-16 after those fixes merged: the verifier refused the retained checkpoint on a drifted backup payload map (#2053, fixed in-run), a decline-only final round durably stopped the daemon at publication (#2055, fixed in-run), and the third resume ended final because gh-imgup's `main` had advanced after admission and an unpublished run has no base-advance re-entry (#2058, owner: Required). The first phone-side per-finding decision ran on the iPhone. |

## Recurring Pitfalls

Look for each of these before and during the run; most have bitten twice.

- **Merging before the pre-merge evidence.** The ready card resolves on
  merge and takes its actions with it, and an external review can no longer
  be posted. Collect what the contract needs from the open pull request
  before handing the owner the link.
- **An in-run fix the run never ran.** "Fixed in-run" is earned only when
  the fix is merged and the exercised runtime carries it, with the affected
  evidence repeated: Wave 7 run 1 rebuilt the daemon from `f5fb30d1` before
  attempt 2, and run-70 validated #1456 on a resumed implementation. A fix
  that is only an open PR leaves the finding suggested to block on and the
  evidence on the SHA that ran.
- **Stale inputs.** A prompt package, harness flag, or client build from an
  earlier run proves the earlier run. Take prompts from the approved
  default-branch commit and rebuild both clients from the tip.
- **Credentials that expire between runs.** Check the Codex token and the
  provider identity before onboarding; preflight rejects them after the
  expensive steps.
- **Old state roots.** A clean-break migration retires a retained root at
  daemon startup. Decide fresh-versus-retained in preflight. A retained root
  also carries its open health items; see "A retained root holds admission
  on old health items" below.
- **Stranded submissions.** Before the per-session `submission-id`
  (2026-09-16), identical inputs re-attached to an earlier run and runs
  escaped with an attempt marker in the specification body. Now a lost
  submit is recovered with `--retry-submission-id`; an edited resubmit
  creates separate work, and a store edit is never the answer.
- **Silent capacity holds.** A stranded dispatched invocation or an orphaned
  rig-cleanup reservation refuses every later run with no signal. Inspect
  the store and the rig before starting.
- **Network in the way.** A Mac VPN can block the tailnet range the iPhone
  pairs through.
- **Clean reviews prove less than they look.** Zero findings leaves
  adjudication, remediation, and re-review unexercised. Plan a controlled
  defect when a gate needs a non-empty round, and check that the review's
  completion evidence actually resolves.
- **Two runs of Stop, duplicate submit, and retry.** These are the
  task-lifecycle gates a client-target run is asked to prove; each needs a
  real item and its own recorded evidence.
- **Store mutations taint evidence.** A direct write to the production store
  leaves the state root inconsistent; back it up, record the edit, and carry
  the caveat in the tracker comment.
- **Restore is not proved by asking.** Supervised restoration has timed out
  with `registered=false` more than once; verify registration and `/health`
  on the normal port before calling the session complete.
- **Watch the automatic restore result.** `complete` only requests completion;
  the foreground harness then stops the campaign daemon, releases the rig,
  and runs the restore helper. Run-114 stalled at the former menu Start
  handoff with `registered=false`. The helper now runs the installed app's
  `daemon start` itself, with one stop/start retry for an unresponsive
  registered daemon. Watch for a nonzero result: approval requires the
  operator, and a missing or old app requires an install. Verify registration
  and health after the helper returns. Never start production before rig
  release.
- **Auto-update prunes a pinned CLI.** A judgment pin that points into
  Claude Code's auto-updated `versions/` directory disappears between runs.
  Copy the binary into the run root and pin that copy by path and SHA256.
- **`open -b` can launch a stale build.** Several app bundles share the
  bundle ID, and `open -b ai.freeside.app.macos` once launched an old wave7
  build whose daemon refused the current store. Launch
  `~/Applications/Freeside.app` by path.
- **Restoring from `registered=false`.** Reinstalling the Mac app from the
  run's commit into a fresh build directory, launching it by path, and
  then running `real-work-session.sh recover` restored the normal daemon.
- **Run the harness as a tracked background task.** Started that way (not
  with `nohup` or `disown`), it kept its rig holder for the whole
  multi-hour run.
- **Clean review doesn't mean the repository's rules were followed.** The
  reviewer is told the target's AGENTS.md but admits only defects with a
  failure path, so a missing required note or doc passes. Check the
  target's mandatory-note list yourself before handing over the merge.
- **The operator's Mac is shared.** When the operator is working on the Mac,
  windows move and a synthetic click can land in their terminal. Hand the
  Mac actions to them instead.
- **A killed harness is recoverable.** `real-work-session.sh recover`
  completes a session the host killed; a fresh session would be a different
  run.
- **The normal daemon can come back mid-run.** Relaunching the installed Mac
  client re-registers its LaunchAgent, so the normal daemon runs beside the
  campaign daemon. Stop it again and record it.
- **Inherited approvals get rejected.** Preflight refuses a reviewer
  configuration approval from an earlier deployment; expect to approve the
  current proposal before the first submission. A review-prompt version bump
  changes the reviewer configuration digest, so check `git log` for prompt
  changes since the last onboarding and re-run onboarding with the new
  `-review-config-digest` before launch (run-114 lost a session to this).
- **A recommended park route can be wrong.** An adjudication card's
  recommendation isn't proof that accepting it moves the run. Per the engine
  code (not yet observed live), accepting `park_revision` or
  `park_separate_work` records no disposition and starts no remediation, so
  the run would stall. When the fix fits the declared paths,
  answer with Discuss and ask the adjudicator to return compatibility and
  route `null`, so the engine's `allowed` verdict gives `remediate` (#1550).
- **Pair the Mac through loopback.** Run-114's Mac client couldn't pair
  against the campaign's tailnet URL; launched with
  `-FreesideServerURL http://127.0.0.1:<port>` it paired at once (#1557). The
  iPhone pairs through the tailnet URL as before.
- **Run a sketch exercise after every proof you need.** Answering a sketch's
  clarification questions put the daemon in a durable stop once the spec was
  ready (#1556). Until that's fixed, run sketch or clarification checks last
  in a session, and don't Acknowledge or doctor the stop: it's evidence.
- **A stopped daemon answers `/health` and records nothing.** A durable
  stop keeps reads serving, so `/health` stays 200 while the daemon cannot
  mint a GitHub token, see a merge, or record a completion. The stop state
  is the STOPPED banner in the client or an open `daemon_durable_stop`
  item; "janitor is not active" in the log is a cue to read it, not proof,
  because a janitor that is absent or still starting logs the same error.
  Read the stop state right before handing over the merge, again while the
  merge waits, and before waiting on the completion fact. Hand the link
  over with the deadline: until #1996 lands, a daemon that has made an
  effect proposal stops at its next backup pass, at most twelve hours after
  start. If it stops before the merge, tell the owner at once, because a
  merge the stopped daemon can't see leaves no completion fact.
- **Acknowledge does not clear a stop.** It is record-only. Resume is
  offered only after a fresh daemon start, and a restarted daemon stops
  again while the cause remains. Once a run's daemon has stopped for a
  cause the run can't remove, record the stop as evidence and wind the run
  down.
- **External review needs an admitted reviewer and the open pull request.**
  Admit the reviewer's login at onboarding and post the review before the
  merge; the ready card is superseded within seconds. Expect a parked
  batch: no remediator runs and the card offers only discuss and stop
  (#1989, #1990). The real target proves the alert; remediation proof needs
  the challenge repository.
- **Confirm the phone reaches the run daemon before the first submission.**
  A phone that cannot connect loses every phone proof in the run, and the
  daemon's log shows only that a notification was submitted, never that it
  arrived.
- **An agent can press a card's buttons, not the menus.** Background app
  control drives the Ephemeral client's cards without taking the pointer,
  so it is safe on a shared Mac. The card's More Actions menu and the
  menu-bar Start and Stop need full-screen control, which the owner
  approves each time. Use the installed app's `daemon stop` and `daemon
  start` commands for the exit run's supervised lifecycle steps (#1988).
  Record that the spine, not the operator, drove: a card the spine reads
  is not an operator's unprompted reading.
- **The liveness probe logs its own notices.** `probe.log` beside the
  installed probe script records "sent recovered notice"; read it to
  confirm the recovery notice after restoration.
- **Private details leak through the run record.** The first Wave 7 comment
  posted a tailnet address and a device name; the template now asks for
  build commits only.
- **Session logs carry the pairing code.** The daemon log prints the startup
  pairing code on one line. Filter it out (`grep -v pairing_code`) before
  tailing or grepping the log into a transcript. The harness's own failure
  paths dump an unfiltered `tail -50` of that log until #1559 lands, so
  keep a failure's stderr out of the record too.
- **Reading the store after the daemon stops.** A `?mode=ro` open fails once
  the daemon is gone. Copy `freeside.db` with its `-wal` and `-shm` files,
  if present (never the neighboring key files), to scratch and open the
  copy normally. Copying the database alone, or opening it with
  `?immutable=1`, skips commits still in the WAL after an unclean exit.
- **The observation deadline pauses only at decision gates.** Before
  #2041 (PR #2046), `FREESIDE_REAL_RUN_TIMEOUT_SECONDS` ran through the
  specification approval and every review-dispute card, and expired while
  the operator was away. It now pauses at the specification approval gate
  and at implementation attention cards such as a review dispute, but it
  still runs through a hold, and a specification attention card still ends
  supervision (`docs/production-walkthrough.md`). An admitted run
  interrupted before it publishes can be resumed (#2040, PR #2051;
  exercised live on gh-imgup#117); a run interrupted at the specification
  approval has no implementation admission and still cannot. Decide cards
  promptly anyway: a run that waits also gives the target's `main` time to
  move (next item).
- **The target's `main` must hold still until publication.** The
  publisher audits the target's base when it decides to publish. A merge to
  the target's default branch after admission ends an unpublished run final
  with `base_advanced` (trust rule `target_base_advanced`); only a published
  candidate re-enters on a base advance. Until #2058 lands, ask the target's
  maintainers not to merge while the run is open, and check `main` against
  the run's base before every resume. Run 117 lost its publication to a
  merge (gh-imgup#128) made the day before its resume.
- **Watch the items, not the log.** A `review_dispute` card after a re-review
  leaves no daemon log line, and the harness prints its state line once. A
  watch armed on new log lines can miss it; poll the task's attention items
  (or re-read the harness tail after every wake) so a card never sits
  unattended.
- **A retained root holds admission on old health items.** Open blocking
  system-health notices from earlier runs (a durable stop, for one) hold the
  next run at admission with `blocking_system_health` until an operator
  resolves them in a client; advisory notices never hold it. No read lists
  them before launch, and resolving one needs a client on the running
  daemon, so the SKILL.md Preflight bullet "The state root is usable" sets
  the expectation and Run the Harness step 2 (the pairing checkpoint) says
  how to resolve and record them before the first approval (#2043).
- **Resume replaces the production daemon.** A failed `--resume-session`
  leaves its session `recovery-required`, and its `recover` restores the
  supervised daemon only after the build the restore gate expects is
  installed with `install-mac-app.sh prod --prod --daemon-path <binary>`
  (the `--prod` flag is the non-interactive confirmation). The gate's
  diagnostic does not name the binary; the session's markers do: a session
  that started its own upgrade (`runtime-upgrade-started` present) expects
  `<session>/freesided`, and one that only inherited an earlier interrupted
  upgrade (`runtime-upgrade-inherited` without `runtime-upgrade-started`,
  the second consecutive failed resume) expects `<session>/restore-freesided`
  (`scripts/real-work-lifecycle.sh`, `real_work_restore_supervised`).
- **Recover before resuming on a new build.** An interrupted upgrade must
  retry the same build, so `--resume-session` from a `recovery-required`
  session onto a newer tip refuses at composition, and leaves a second
  `recovery-required` session behind. A same-build retry from a session
  whose rig release was verified is accepted; resuming onto a different
  build needs a completed predecessor (`scripts/real-work-retained.py`,
  `validate_session`). After an in-run fix merges: install the binary the
  stopped session's markers select (previous item: `<session>/freesided`
  for one that started its own upgrade, `<session>/restore-freesided` for
  one refused at composition before starting it) into the production app
  (`install-mac-app.sh prod --prod --daemon-path <binary>`,
  operator-approved), `recover` that session, stop the supervised daemon,
  then resume on the new tip.
- **Have a route ready for every parked finding.** A `review_dispute` card
  lists each finding's routes with none recommended; the adjudicator's lean,
  confidence, and rationale do not reach the client (#2059), and on run 117
  every finding was parked as `park_unknown`. The operator will ask which
  route to take. Read the finding, the target's scope, and its decision-note
  rules first, and give a recommendation with one line of reason.

## Mode And Flags Past Trackers Required

- #1530 found that `Please handle <URL>.` saves no publication source issue.
  For a closure-specific client-target run, give the bare canonical issue URL
  before submission and require the retained `verify-source` receipt before
  specification approval. Client URL provenance is `recommended`; the receipt
  proves eligibility, not closing authority or a later proposal. Correcting a
  source requires a fresh task/session, never editing the old run's binding.
- #1211 required the task to come from the client composer
  (`--client-target`) with the human specification gate on, and a live Stop
  from a real client.
- #1445 requires the daemon's four `-judgment-*` flags and a target whose
  PR template needs a close reference; without the flags the author falls
  back to recipe v1 and the proof cannot pass.

## What A Run Proves

- The deployment at one `main` SHA, on one target, through the human gates
  as configured. Record the SHA, and repeat integration evidence if the base
  moves before exit.
- Actions that ran on real items, on the devices they ran on. Test coverage
  stands in for actions the run produced no item for, and the record says
  which.
- Nothing about a known reachable defect that happened not to fire. The exit
  claim is about the deployment, not the sample.
