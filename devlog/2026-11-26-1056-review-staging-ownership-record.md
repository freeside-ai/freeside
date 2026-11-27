# Review Staging Is Reclaimed Only Through Its Ownership Record

**Decision (agent, 26 Nov 2026, #536):** each
`review-workspaces/staging-*` directory that `ensureReviewWorkspace`
creates now holds an ownership record, `staging-owner.json`, beside
`workspace/`. The record names the review invocation and the directory's
own name and is synced before materialization starts. When the same
invocation next prepares or removes its workspace, the daemon deletes every
staging directory whose record authenticates to it, and leaves every other
entry alone. This closes the "Confirmed and deferred" finding in
`devlog/2026-08-06-0828-raw-review-workspace-materialization.md`, which
rejected a fixed-path `RemoveAll` because it would make recovery a
recursive delete of whatever sat at that path.

## Why

- **Chose a record in the directory over a database row.** A row needs a
  migration, which makes this a contract unit, and the database can't see
  a directory replaced on disk anyway: the delete must still check what is
  on disk at the moment it acts. The record is that check, and it sits in
  the directory it vouches for.
- **Chose reclaim bound to the invocation over a start-up sweep.** A sweep
  can't tell a live staging directory from an abandoned one when another
  pass may be preparing its own. Reclaim runs only for the caller's own
  invocation, and only one pass works on an invocation at a time (see
  Refute-First Findings), so no staging directory reclaim can reach is in
  use.
- **Chose to leave unrecorded entries alone over reclaiming them by name.**
  Staging from a daemon older than this change, and an empty directory
  left by a crash before the record was synced, carry no proof of who made
  them. Deleting them by name is the recursive-delete boundary the earlier
  note rejected. A person removes pre-upgrade residue once.
- **The record stays outside `workspace/`.** Candidate content is
  materialized only under `workspace/`, so it can't write or replace the
  record, and the rename into `review-workspaces/<invocation>` never
  carries the record into the tree ward proves byte for byte.
- **The record is deleted last.** Reclaim removes everything else in an
  authenticated directory through a handle opened on that directory, then
  the record, then the empty directory with a non-recursive remove. A
  crash or I/O failure while clearing leaves a directory the next pass
  still authenticates. One failing between the record's removal and the
  final remove leaves an empty directory with no record, which stays like
  any other unrecorded entry; it holds nothing, and only a crash in that
  one step or another writer in the work directory can cause it.

## Accepted Residue

Staging for an invocation that never prepares or removes its workspace
again stays on disk. The contract accepts this; a sweep would reopen the
live-versus-abandoned question above.

## Refute-First Findings

The pass combined a fresh-context refuter, which ran scratch probes of
`os.Root` and mutations against a copy of the daemon, with the author's own
caller trace and mutation runs.

Disproved by a check:

- **Two passes on one invocation.** Every caller of `ensureReviewWorkspace`
  and `removeReviewWorkspace` runs inside `reconcileTask`. The pass
  dispatches pending tasks per run through `keyedrun`, which never runs a
  key twice at once, and takes the per-task flock. Both invocation-id
  derivations hash the run id, so one invocation belongs to one run.
  Reclaim also runs before the call claims its own staging directory.
- **`os.Root` following a link out.** On darwin (probe) and in the Go
  1.27.4 source shared by linux, `Root.RemoveAll` unlinks a symlink
  without following it, removes a tree holding links to outside files
  without touching them, and `OpenRoot` refuses a link that leaves the
  root. `OpenRoot` and `OpenFile` do follow a link that stays inside the
  root; the `Lstat` and `SameFile` checks reject that case.
- **A swap after authentication.** With the authenticated directory moved
  away and a victim directory put at its name, deletion through the held
  directory handle removed only the authenticated content, and the final
  identity check refused to remove the new entry.
- **Candidate content forging the record.** `RetainWorktree` admits only
  regular files into `workspace/` and refuses an existing destination, so
  candidate content can't plant a link or reach the record beside
  `workspace/`.
- **Tests that don't bite.** Removing the invocation check, the
  directory-name check, or the record-last order fails the suite. The
  refuter found that removing the version check, or the directory
  identity checks, did not; the table gained a wrong-version record and an
  in-root `staging-*` symlink, each shown to fail under its mutation.

Allowed by decision:

- **The window between the final identity check and the remove.** An entry
  swapped in there for a regular file or an empty directory is removed;
  a non-empty directory fails and a symlink is only unlinked, so nothing is
  deleted through. Only a writer with the daemon's uid inside its 0700
  work directory can make that swap, and that directory is trusted (AGENTS.md,
  Threat Model). Unix offers no remove-by-handle to close it.
- **No swap-injection test.** Covering the identity checks against a live
  race needs a hook in production code. The probe above stands in for it.
- **The `staging-` prefix is not reserved for invocation ids.** A
  retained workspace named `staging-…` would become a reclaim candidate
  whose root holds candidate content. No derivation can produce one: ids
  are `review-<hex>` or `shadow-review-<hex>`, either with
  `-adjudication-base`.
- **A transient open failure skips an owned entry.** Nothing is deleted,
  and the next call retries it.

## Revisit When

- Two passes can work on the same invocation at once (the lock or the
  per-run dispatch changes), since reclaim could then delete a live
  staging directory.
- The staging layout (`staging-*/workspace`) or its location under the
  publication work directory changes.
