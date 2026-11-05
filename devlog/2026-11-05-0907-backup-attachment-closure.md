# Attachment Blobs in the Backup Closure

#2635 adds attachment digests to the local backup's artifact closure, from
both places a store names one: the `task_attachments` rows (#2587) and the
stage-input snapshot's `Attachments` list (#2588,
`devlog/2026-11-05-0809-stage-input-attachments.md`). It is Tier A: it
changes what a restore must prove before it replaces a store.

## Finding

- **The backup checks blobs by reference and never copies them.** A
  checkpoint holds the database and seals a manifest of the blob digests
  that database needs. The blobs stay in the daemon's content-addressed
  store, which has no delete. Backup health and restore verify every digest
  in that closure. Attachment blobs already live in the same store, so the
  gap was only that `checkpointArtifactDigests` never named them: a missing
  attachment blob left health healthy, the manifest omitted it, and restore
  accepted a checkpoint whose attachment rows it never checked. The fix is
  a closure entry, not a copy.

## Decisions

- **Chose the validated per-task read over a bare digest query.** The scan
  lists the task IDs that have attachments, then reads each list with
  `ReadTx.TaskAttachments`, which rechecks positions and every list rule. A
  `SELECT digest FROM task_attachments` would be one query and would still
  catch a missing or edited digest, but it would pass a row whose name,
  type, or position was edited, and the closure treats every other kind of
  owned state as fail-loud. Only the outbox scan tolerates a bad row,
  because only there is the row another binary's payload. A bad attachment
  row therefore fails the producer's next seal with a hard error, not a
  closure gap, so the daemon enters a durable stop rather than sealing a
  checkpoint over it; a checkpoint that carries one refuses restore. Backup
  health reads the sealed checkpoint, not the live rows, so it does not see
  a row edited after the last seal: the durable stop is the signal.
- **Chose no reseal migration.** The sealed manifest grows on any store
  with attachments, so a checkpoint sealed before this change no longer
  matches the recomputed closure. That is already the manifest-mismatch
  path: health reports the checkpoint unhealthy, which holds unattended
  admission, and the next producer pass treats it as absent and seals a
  new one. The window is at most one maintenance interval after upgrade.

Revisit when attachment blobs move out of the shared content-addressed
store, or when the backup starts copying blob bytes.
