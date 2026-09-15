# Renew Codex Credentials Without Accepting A Recovery Hold

Issue: [#1363](https://github.com/freeside-ai/freeside/issues/1363).

Chose an operator renewal command using review launch's existing durable
refresh transaction. Production preflight intentionally only inspects the
credential. Requiring a daemon launch to refresh it creates a startup cycle
when that inspection refuses an expired token. Moving the same lease-held
refresh to `renew-codex` breaks the cycle without weakening admission.

Renewal uses the existing refresh identity gates, which do not require the
identity's `Enabled` field. The existing production identity has that flag
disabled while review launch and preflight accept its configured store.
Adding that gate only to renewal blocked the
repair path; renewal does not admit execution or change the identity flag.

Renewal creates no hold. The existing revoked-chain marker belongs to a
review run; operator renewal has no run. Revoked or uncertain chains instead
lead to login and enrollment. Pending-response recovery runs before any new
provider call, preserving the transaction's single-use refresh semantics.

Rejected an operator-shell hold resolver because recovery requires a persisted
command from a paired device. The harness recovery mode starts the existing
daemon with execution disabled so that signet can process that command. It
reuses rig ownership, explicit walkthrough completion, and cleanup instead of
introducing another service launcher. It neither submits nor enables client
task submission.

Both repair paths refuse schema changes. Renewal opens the existing store
without migration, and recovery checks the schema read-only before starting
the disabled daemon under the rig. Rejected silently upgrading the database:
the cleanup path may restore an older installed daemon. Schema upgrades remain
the existing checkpoint and runtime-install workflow's responsibility.

## Refutation Findings

- Provider-response leakage was disproved by tracing the shared transaction's
  fixed error messages and by token-exclusion assertions on command output.
- Lease and hold bypass were disproved by refusal-before-acquisition tests,
  lease-loss tests, and the existing transactional filesystem mutation guard.
- Re-spending an interrupted refresh was disproved by pending and committed
  recovery fixtures that require zero provider calls.
- A disabled daemon's inability to resolve a hold was disproved by exercising
  enrollment followed by signet's persisted device command with no engine.
- UTC journal timestamps are required by the existing refresh transaction.
  The command fixture caught a local-clock default; renewal now uses UTC like
  enrollment and review launch.
- A renewal-only `Enabled` requirement was confirmed to reject the existing
  production identity before refresh. Ward and real-store command regressions
  now use that legacy disabled state and prove renewal and repeat readiness.
- Review confirmed that the initial writable open and recovery startup could
  migrate ahead of the restored daemon. Schema-refusal regressions now prove
  renewal avoids provider calls and recovery avoids daemon startup.
- Recovery must receive every approved recipe represented by retained evidence,
  not only the exercise's current recipe. Repeatable recovery-only arguments
  preserve those explicit approvals for paired-client reconstruction.

Revisit when #867 changes provider enrollment or the host auth-store contract.
