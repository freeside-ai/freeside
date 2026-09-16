# Separate New Work From Manual Submission Retry

The owner chose a fresh task for every deliberate submission in #1366 on
September 16, 2026: “There are no retries outside of manual retries- new task
submission is for new tasks.” The exit exercise had shown that matching
source text silently reopened old work.

This revises the source-key reuse decision in the
[client-submission note](2026-09-12-1950-client-task-submission.md).
Follow-up: [#1387](https://github.com/freeside-ai/freeside/issues/1387) promotes
the decision to an ADR in a separate reviewed change.
The [manual configuration note](2026-09-15-1315-manual-submission-configuration.md)
still governs startup policy. Its replay-before-configuration rule now applies
only to recorded submission identities, never to a new same-source command.

## Decisions

- Keep source artifacts digest-addressed, but key manual intake by a namespaced
  submission identity. Bind the request, task, specification run, and campaign
  inside one accepting transaction. Label intake and execution retry/revision
  keep their existing task inheritance.
- Bind new client requests to decoded device, project, source, and submitted
  optional name. Keep request fingerprints private. Old command bodies stored
  the resulting display name, so reconstructing the requested name would
  fabricate history. Preserve their original replay checks and revisions.
- Save client commands before sending, separately from the attention-item
  decision ledger. Restore by device and daemon ownership without sending.
  An untrusted success response leaves the submission unresolved, because it
  does not prove that acceptance failed.
- Keep recovery outside the New Task form. On September 16 the owner rejected
  using the same form for Retry because it complicates submission. Tasks now
  opens a separate read-only list of unconfirmed requests with explicit Retry
  actions. New Task never selects a saved command or offers Retry.
- Save CLI input snapshots before acceptance and pass a prepared identity
  through preflight and submit. Manual Retry loads those snapshots. A legacy
  run lookup cannot create a new run. Retained-session attachment still makes
  no submission.
- Extend the existing cache format with optional fields. The read snapshots
  remain compatible; a version bump that discards them buys no safety. Old
  caches naturally contain no pending submission ledger.

## Refutation Findings

Independent review confirmed an epoch-eviction data-loss window: deleting the
cache before a failed replacement save erased an already durable submission.
The cache now replaces snapshots atomically without deleting the last command
copy. The epoch-change/save-failure regression restores the original command
and leaves old snapshots unvalidated. Independent re-review found that fix sound.

Review also found that prepared CLI redelivery rejected invalid UTF-8 which
initial acceptance tolerates in publication, policy, and work-unit JSON. Replay
comparison now uses each role's acceptance posture, with composition remaining
strict. A regression submits and redelivers the same accepted publication bytes.

Automated review found that CLI validation failures occupied prepared identities
with permanently invalid recovery journals. Inputs now freeze in memory, pass
validation, and reach the journal before the database opens. Corrected invalid
requests can reuse their prepared identity; a journal-write failure still
prevents acceptance. Equivalent redelivery derives digests from the first
journal's original bytes.

A failed client-cache save after a known response cannot durably record that
response. Keeping the older unresolved entry preserves all pending commands;
deleting the cache would lose them. After relaunch, only manual Retry resends
the original command, and daemon replay returns its original result and
revision without creating work. Review retained this recovery behavior.

Authentication failure cannot settle a saved submission: 401 and 403 can occur
before the daemon reaches recorded-result replay. The client now keeps that
command unresolved and marks authentication unavailable. Restart still sends
nothing, and explicit Retry after same-device authentication recovery uses the
original command. Re-pairing as a new device does not transplant that command.

Call-path review disproved policy-dependent replay, reconstructed legacy names,
source-file-dependent CLI retry, revision-advancing replay, source-key task reuse,
and retained-session resubmission. Migration tests compare pre-existing bodies,
results, revisions, intake keys, runs, campaigns, and attention rows unchanged.
The real-daemon convergence case drops a committed response, restores the client,
and proves that only manual Retry resends the saved command.

## Revisit When

The product adds task merging, intentional execution reruns, or migration of
pending commands between paired devices or daemon deployments. None is implied
by a submission Retry.
