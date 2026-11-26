# Conformance per Host

Issue #2165 binds every conformance record to the execution host whose suite
wrote it, and runs the cordoned suite on each host on its own (plan §5.7,
Execution Hosts, Conformance per host). Source note:
`devlog/2026-11-25-0151-cordoned-runs-on-hosts.md`. Tier A by path, so this
note is mandatory.

## Choices

- **A column default over a row rewrite or a foreign key.** Migration 0152
  adds `backend_conformance_records.host_id` as `NOT NULL DEFAULT 'daemon'`,
  so every existing row reads as the daemon's host's with no `UPDATE`, and
  the migration stays an add-only Tier B one. A `REFERENCES execution_hosts`
  key was rejected: SQLite refuses to add a column that references a table
  and has a non-null default while foreign keys are on, so the key would
  take a table rebuild, a data-rewriting migration. The store checks that
  the host exists when it records a pass instead, and a removed host's
  records stay as history. The row id stays the proof generation; every
  read filters by host and orders by id, so it rises within each host.
- **The daemon binds the host; the store takes it from the admission.** A
  suite builds its record with no host, as it builds it with no generation,
  and the store refuses a record that already names one. The host comes
  from the recorder the daemon hands the suite
  (`StoreConformanceRecorder.Host`). The three admission gates keep their
  signatures and read the host from the admission inside the store
  (`admissionHost`), so no caller can pair an admission with another host's
  proof. An admission records no host until #3641, so that is the daemon's
  host today; #2166 switches it to the recorded host. Passing the host as a
  gate argument was rejected for that reason.
- **The proof condition is read from the newest record.** The runner sets
  `proof_not_current` on a host exactly when the host's newest cordoned
  record isn't a pass, after each full pass and each failed probe ends, by
  reading the record rather than carrying the outcome in memory. That keeps
  the condition a projection of the durable log: one rule covers a pass, a
  failure, a probe's withdrawal, and a recheck's marker. A recheck in
  progress doesn't set it: a settle outside a pass leaves the condition
  alone while the newest record is a recheck's marker, because the pass
  settles the host when it ends. Every settle runs past its caller's
  cancel, as the suite records its own steps, so each pass does settle,
  and a record a canceled job's probe wrote isn't left unsettled. It reads
  first and writes only when the condition changes, because every Write
  moves the server revision. Only cordoned records count. The record and
  the condition are two transactions; a crash between them leaves the
  condition stale until the host's next pass, which runs at start.
  Accepted: the condition gates nothing yet (#2166 reads it), and the start
  pass repairs it.
- **A digest check before a job over a poller.** The runner reruns a host's
  full pass before a job when the host's configuration digest differs from
  the one its last completed pass ran under, then runs the probe. A pass
  that completed and failed counts as run under its digest, so a host with
  a failing configuration isn't re-proved before every job; the schedule
  retries it. A poller that watches each host's configuration was rejected
  as machinery #3625 can add if a host's configuration changes between
  jobs often enough to matter. The daemon's own host's digest is fixed for
  the process's life, so the check matters only once #3625 composes
  another host.
- **A failed launch that required proof settles the host.** The backend's
  launch runs the probe again under `RequireProof`, and its failure writes
  a `failed` record the runner never saw. The gate settles the launch's
  host after such a launch fails, so the condition follows that record
  too. Settling is a read when nothing changed, so it runs after any
  failed launch that required proof.

## Refute-First Findings

One fresh-context reviewer tried to refute the diff (trust boundary,
runner, migration, and unchanged test assertions). It found nothing in the
trust boundary, the migration, or the existing tests.

- **Confirmed, fixed: a recheck under way set the condition.** A probe a
  beginning pass refused records nothing, so its settle read the pass's
  marker and set `proof_not_current` until the pass ended. A settle
  outside a pass now leaves the condition alone on a marker.
- **Confirmed, fixed: a canceled job's red probe went unsettled.** The
  suite records a probe's failure past the job's cancel, but settle
  skipped a canceled ctx. Settle now runs past the cancel.
- **Confirmed, fixed: two jobs could rerun one digest change's pass
  twice.** The digest was compared outside the pass lock; the second
  pass's marker would refuse the first job's probe. The comparison and the
  pass now share one hold of the lock. Unreachable today, since the
  daemon's host's digest is fixed for the process.
- **Confirmed, fixed: a pass that succeeded as ctx ended returned ctx's
  error.** The outcome switch again checks success first, as `runPass`
  did.
- **Confirmed, fixed: a stale comment** in `claude_driver.go` named only
  the record error among what the scheduled pass returns.
- **Allowed: a removed host's id can be added again and inherit its
  records.** Nothing adds or removes a host outside tests, and admission
  reads only the daemon's host until #2166. Recorded under Revisit When for
  #3625, which adds the operator's remove command.
- **Missing tests, added:** the gate settling after a failed launch that
  required proof, the host verification gate on host B's record, and the
  three runner fixes above.

## Revisit When

- #2166 reads the host from the admission: `admissionHost` should then
  return it, and the gate's probe and settle should name the launch's
  recorded host.
- #3625 composes a host whose configuration changes between jobs: weigh a
  poller against the pre-job digest check.
- #3625 lets an operator remove a host: a removed id added again would
  read the old host's conformance records as its own, so removal should
  retire the id or the records should bind to the host's enrollment.
