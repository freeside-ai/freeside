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

## Recurring Pitfalls

Look for each of these before and during the run; most have bitten twice.

- **Merging before the walkthrough.** The ready card resolves on merge and
  takes `open_pr`, `mark_seen`, `dismiss`, and `return_to_agent` with it.
  Hand the operator the action list before the PR link.
- **An in-run fix the run never ran.** "Fixed in-run" is earned only when
  the fix is merged and the exercised runtime carries it, with the affected
  evidence repeated: Wave 7 run 1 rebuilt the daemon from `f5fb30d1` before
  attempt 2, and run-70 validated #1456 on a resumed implementation. A fix
  that is only an open PR leaves the finding Required and the evidence on
  the SHA that ran.
- **Stale inputs.** A prompt package, harness flag, or client build from an
  earlier run proves the earlier run. Take prompts from the approved
  default-branch commit and rebuild both clients from the tip.
- **Credentials that expire between runs.** Check the Codex token and the
  provider identity before onboarding; preflight rejects them after the
  expensive steps.
- **Old state roots.** A clean-break migration retires a retained root at
  daemon startup. Decide fresh-versus-retained in preflight.
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
- **A killed harness is recoverable.** `real-work-session.sh recover`
  completes a session the host killed; a fresh session would be a different
  run.
- **The normal daemon can come back mid-run.** Relaunching the installed Mac
  client re-registers its LaunchAgent, so the normal daemon runs beside the
  campaign daemon. Stop it again and record it.
- **Inherited approvals get rejected.** Preflight refuses a reviewer
  configuration approval from an earlier deployment; expect to approve the
  current proposal before the first submission.
- **Private details leak through the run record.** The first Wave 7 comment
  posted a tailnet address and a device name; the template now asks for
  build commits only.

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
