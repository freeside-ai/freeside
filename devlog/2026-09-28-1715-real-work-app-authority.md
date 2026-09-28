# Real-Work Runs Share the Prod App's Authority

Work unit #1517, plan revision 71. Owner decisions of 2026-09-28. This
revises the prod-App exception in
`devlog/2026-09-23-0939-environment-tiers.md` (revision 69) after the finding
in `devlog/2026-09-23-1245-shared-app-janitor-interference.md` (#1511).
Follow-up: #1583.

## Changed Assumption

Revision 69 let an attended real-work run enroll the `prod` App's key into its
own `ephemeral` store. It assumed a separate authority store could safely
hold a shared App's key. It cannot. The janitor lists every installation of
the App and deletes or quarantines each one its local authority
(`installation-authority.json`, the pending envelope, and the janitor journal
under `-publication-state-dir`) does not name. So a second authority for one
App uninstalls the first's installations. It does so at the later daemon's
startup, so stopping the other daemon does not help.

## Chose One Authority per App

Chose to have an attended real-work run use `prod`'s publication authority
state directory and credentials directory, only while it holds the production
rig lease, over three alternatives:

- **A separate App for real-work runs.** Rejected because real-work runs do
  real work on real, often public, repositories and should publish as the
  real App.
- **A janitor that tolerates installations its authority does not name.**
  Rejected because it weakens a guard on a destructive path.
- **Running real work through `prod`'s own store.** Rejected because the
  harness isolates a run's database and state from `prod` on purpose. Only
  the two App directories are shared.

This holds even though `prod` does not publish today: its LaunchAgent runs
with no driver or publication flags, and it holds no App state. The App's only
authority lives in the operator's `FREESIDE_REAL_RUN_APP_STATE` and
`FREESIDE_REAL_RUN_APP_CREDS` directories. `prod`'s directories become where
the single authority lives, so the follow-up code unit moves that state there
once, authority and credentials together. A run pointed at `prod`'s
directories before the move finds no authority and must refuse to start.

A real-work build is usually newer than the installed `prod` build, so the
plan requires it to read and write the authority and journal formats that
`prod` accepts. A mismatch fails closed but would stop `prod` publishing. The
follow-up unit enforces the check.

## Kept the Prod Stop and the Lease Decline

Chose to keep `prod` stopped during a real-work run over running both at once.
`freesided rig hold`, `rig recover`, and preflight already refuse while
`ai.freeside.daemon` is loaded, so the stop costs nothing new. They check
only when they run, though, so a mid-run **Start** would go unrefused; the
follow-up unit enforces the stop for the rest of the run. Concurrency is
deferred to #1568.

The GitHub-side lease that revision 69 declined stays declined, on a new
basis. The old basis was that attendance removed contention. The new one:
`prod` stays stopped for the whole run, and with one shared authority the
janitors agree on which installations to keep. A lease alone never fixed
divergent bindings, because the later daemon's janitor deletes at startup.

Publication identity is content-derived and names neither daemon nor App, so
two daemons that publish the same candidate for one repository to the same
head branch converge on one PR. With `prod` stopped this can't happen between
`prod` and a run; the operator also never runs one work unit in both while
either has it active.

## Kept Real-Work Records in the Run's Database

Chose to keep real-work task records in the run's own database over folding
them into `prod`'s history. Only App authority needs to be shared to stop the
janitor interference.

## Interim Risk

Until the follow-up code unit lands, the guards still refuse `prod`'s
directories, so runs keep their own App directories. Two runs with different
App directories trust different installation sets, and the later run's
janitor can uninstall installations only the earlier one trusted. The
walkthrough requires every run to reuse one App state directory and one App
credentials directory until then.

## Revisit When

- `prod` and a real-work run need to operate at the same time (#1568).
  Shared authority then needs concurrent-writer rules for the authority file
  and journal, and the publication-identity convergence needs a work-unit or
  target rule.
- Real-work task records should appear in `prod`'s history.
- The follow-up unit cannot enforce the format compatibility check; then the
  plan must say how the operator verifies it instead.
- `prod`'s LaunchAgent gains publication flags before the move lands; then
  the migration must reconcile two authorities instead of moving one.
- The hardened dedicated-user mode is scheduled. The operator's run can no
  longer open `prod`'s directories, so it needs another path to the one
  authority.
- The janitor stops reconciling App-wide installations against local
  authority, or publication identity gains an instance boundary.
