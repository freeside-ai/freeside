# Production Walkthrough Ownership And Recovery

Work unit: #1187.

Keep one foreground harness owner through final verification and the operator
walkthrough. Stopping at publication disconnected the same paired endpoint just
when the operator needed its ready card. Restarting another daemon or restoring
the default service does not preserve that run's live endpoint. Completion is
an explicit request to the owner; EOF and the production deadline cannot grant it.

Separate writable identity setup from final read-only verification. The latter
uses the same policy, backup-health and evidence gates with `store.OpenReadOnly`.
Existing backup keys and blob directories are prerequisites, not missing state
the final verifier may repair. Use the existing encrypted-backup constructor
with the established key/checkpoint paths, avoiding a shared store API change.

Preserve the safety decision in
`2026-08-15-1058-production-rig-lease.md`: a bare signal cannot erase the lease
gate before exact-resource cleanup. Normal harness interruption attempts cleanup;
an already dead holder requires checked `rig recover -confirm`. A request file
does not transfer process ownership. Recovery never signals a stored PID.
The retained binary and inputs survive failures so recovery commands stay usable.

Restore through the app's SMAppService controls, after rig release. Clearing a
launchd disable flag is insufficient after bootout. Require service registration
and a successful default endpoint health response, with pending approval and
timeouts reported as incomplete. Do not silently change client pairing.

The trusted-local-operator boundary from
`2026-08-18-1300-composition-preflight-salvage.md` still applies. Filesystem or
environment manipulation by a hostile operator is outside this unit. Reachable
interruption, child-process lifetime and partial cleanup failures remain in scope.

## Refutation Findings

- Confirmed: a retained session under the checkout makes the harness's own
  clean-checkout check fail. Default sessions now live under the operator's
  log directory, outside the repository.
- Confirmed: matching multiple child PIDs as a space-delimited string fails
  because `jobs` separates entries with newlines. Grouping the builtins in a
  pipeline also loses the job table in macOS Bash. A standalone shell check
  demonstrated the failure; direct capture plus line matching preserves the
  parent's actual child ownership.
- Disproved by fixtures: stale recovery could clear a gate despite a live
  database/listener/holder, or after failed/timed-out cleanup. The helper keeps
  the rig command as authority, retains diagnostics and never starts restoration
  after its refusal. The command's existing tests supply the real safety gates;
  shell fixtures verify ordering and failure propagation.
- Confirmed by independent review: TERM during the recovery helper could leave
  its separate cleanup group running after the helper dropped its lock. Recovery
  now defers interruption until bounded group cleanup finishes, then reports
  failure without claiming restoration.
- Confirmed by the detached-runtime fixture: ignoring TERM in the foreground
  cleanup trap also passed an ignored disposition to shell cleanup children.
  The foreground owner now defers exit by recording the signal status, keeping
  child cancellation handlers usable while the bounded cleanup finishes.
- Confirmed by independent review: acquisition timeout could retain the binary
  without its recovery root and bound. Those inputs are now written before the
  holder starts, then refreshed with canonical rig resources after acquisition.

Revisit when the harness needs detached ownership, the rig adds another resource
class, the backup layout changes, or the app's registration controls change.
