# Keep Prod Running Driverless During A Real-Work Run

#1938 Unit 2 (#2080) lets the installed `prod` daemon keep running while a
real-work run holds the production rig lease. It revises the "Kept the Prod
Stop and the Lease Decline" decision in
[2026-09-28-1715-real-work-app-authority.md](2026-09-28-1715-real-work-app-authority.md)
and plan revision 71 item 2, which kept `prod` stopped for the whole run and
deferred running both to #1568. Plan revision 90 records the change.

**Run `prod` driverless instead of stopping it.** The operator used to stop
the installed daemon before a run and restore it after. The restore is the
step that timed out with `registered=false` in the 2026-10-10 exit run, and it
kept the deputy's first proving run (PR #2015) from showing "`prod` keeps
running". The stop protected one thing: the App authority's files (trusted
installations, pending envelope, janitor journal) must have one writer. A
`prod` without a driver never opens them. `main.go` passes
`-publication-state-dir` and `-publication-credentials-dir` only into the
Claude driver and the one-shot `-fake-publication` command, the janitor runs
only in the Claude wiring, and the installed LaunchAgent passes no `-driver`.
So the rule is: while a production rig lease exists, `prod` runs only without
a driver. The GitHub-side lease that revision 71 kept declined stays declined
on the same basis, since a driverless `prod` runs no janitor. `prod` still
writes its own database, readiness, and backups in `<root>/daemon/`; those
files are not the authority.

**Two sides, no locks.** The deputy checks the loaded `prod` only after it
holds the lease, and a `prod` that starts after the manifest exists refuses a
driver on its own (`checkProdDriverUnderLease`). The `-prod-app-authority`
daemon repeats the deputy's probe after it authenticates the lease
(`checkProdDriverless`), so a daemon started by hand without preflight meets
the same rule. Every launchd inspection and every `prod` database lock is
gone: `rig hold`, `rig recover`, the deputy, the `-prod-app-authority` daemon,
and `preflight` no longer refuse on a loaded `prod` or hold its lock. `rig
recover` lost its launchd refusal with the rest; it guarded the same stop and
nothing else.

**The signal is the control socket, not `/health`.** Every daemon serves
`GET /execution` on its private control socket, answering its tier and
`disabled`, `fake`, or `claude`. `/health` was rejected: its `HealthStatus`
schema is closed, the generated Swift client rejects an unknown key, and plan
§5.2 fixes its four fields, so a new field there is a paired-client contract
change. The control socket is same-host, same-user, and peer-UID checked,
which is the trust a local preflight needs. The preflight finds `prod`'s
socket through `<app_state_dir>/freeside.db.control.json`, because the App
state directory is `prod`'s `-state-dir`.

**Fail closed on an advertisement that does not answer.** No advertisement
means `prod` is not running: a daemon publishes it once it serves and removes
it on a clean stop. An advertisement whose socket does not answer, or a
daemon that does not serve the read (an installed build older than this
change), fails `prod_execution`, so execution stays disabled before any
spend. Reading it as "not running" was rejected because a live `prod` with a
driver behind a dead socket would pass. The cost: a crashed `prod` can leave
a stale advertisement until launchd's `KeepAlive` restarts it, and the
installed app must include this change before the first run.

**What the deputy's probe cannot see.** The probe sees a daemon that serves
its control socket. Three hand-run cases fall outside both sides, and stay
operator discipline, like the paths plan §10 already leaves unchecked:

- `freesided onboard` run by hand against `prod`'s App directories during a
  run opens the authority without a daemon.
- A one-shot `-environment prod -fake-publication` started before the lease
  publishes no advertisement while it runs. The old deputy lock on `prod`'s
  database caught it; nothing does now.
- A `prod` started by hand with a driver before the lease, still between its
  manifest check and its advertisement when the deputy probes, reads as not
  running. The installed LaunchAgent passes no driver, so only a hand start
  reaches this window.

Closing the last two needs either a momentary probe of `prod`'s database lock,
which the issue rules out because a `prod` starting at that instant would
exit, or `prod` publishing its advertisement before it starts any driver loop
and re-reading the manifest after. Neither is needed while `prod` has no
driver.

**Refute-first findings.** An independent reviewer tried to break the change
before commit and found no blocking defect. It confirmed that no driverless
code path opens the App authority, that the probe's canonical path matches
`prod`'s advertisement, and that the composition manifest needs no version
bump. Four findings changed the work. The `-prod-app-authority` daemon had
lost its only check of `prod` with the lock, so it now runs the probe itself.
A test now pins that a failing `prod_execution` blocks the App-authority
reads. A retained session saved before this change resumes through its own
binary, which still refuses a loaded `prod`, so the walkthrough says to stop
`prod` for that case. The stale-advertisement remediation no longer suggests
stopping a `prod` that is already dead.

The 2026-10-14 note's revisit condition ("Unit 2 lets `prod` keep running")
is met by this note; that note stays as written.

Revisit when `prod` gains a driver (#2083), since the hand-start window above
then becomes the LaunchAgent's own start; or when the control socket or its
advertisement moves.
