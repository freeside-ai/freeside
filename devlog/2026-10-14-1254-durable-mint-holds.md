# Keep Janitor Mint Holds In The Database

Work unit: #2004. Scope: `daemon/migrations/`, `daemon/internal/store/`,
`daemon/internal/publish/`, `daemon/cmd/freesided/`, and this note.

This revises one scope decision in
[the token-hold note](2026-10-13-1015-janitor-token-holds.md): a hold now
survives a daemon restart. The hold's length, its triggers, how a held pass
is counted, and supervision are unchanged.

## Decision

A row in `publish_installation_mint_holds` decides whether an installation is
held. The janitor commits the row before the mint request leaves, and a
failed write stops the pass as the daemon's own safety failure with nothing
sent. The janitor reads the row before every mint, so nothing loads holds at
startup and no in-memory copy can drift from it.

A row is deleted only on a proven outcome: GitHub refused the request (a
4xx), the request never left (it could not be built, or the pass was
canceled after the hold write), or the token was revoked. Otherwise it stays
until `held_until`, the request time plus 69 minutes. A failed delete keeps
the row and stops the pass; it only holds longer.

No token is persisted. The table has no token column, and a test scans the
database and its write-ahead log for the token value. A token whose revoke
failed stays in memory only, for revoke retries in that process. After a
restart the hold runs to its deadline without a retry, because the token is
gone.

## Crash Windows

Each window leaves a row and no token, and a fresh janitor refuses to mint
until the deadline:

- After the hold write and before or during the request.
- A request whose connection drops before a response.
- A 201 whose revoke fails.
- A successful revoke whose release never ran.

The first and last leave a row with no live token. They cost coverage for
that installation until the deadline, the price the owner accepted for an
unknown outcome.

A graceful shutdown is not a crash and leaves no row it can avoid. A pass
canceled before the request releases the hold. Once the janitor's last
cancellation check passes, the mint request runs detached from the pass,
bounded at 30 seconds like the production client, so a shutdown that lands
just before dispatch cannot fail the request unsent and leave a hold with no
token behind it. The rejected alternative traced whether the request was
written (`httptrace`) and released on an unwritten failure; it depends on
transport internals to prove "never sent", where finishing the request
yields a known outcome directly.

## Rejected Options

- **The janitor journal.** The destructive-action journal is rewritten whole
  under `flock` on every change, so it would be rewritten twice per
  installation per 30-second pass. The owner's contract named a store
  migration, and the encrypted checkpoint already covers the database.
- **A holds file in the publication state directory.** It would let the
  attended real-work run (`scripts/run-real-work.sh`), which opens prod's App
  authority but its own database, see prod's holds and the reverse. The two
  never run at once and the handover is attended, so the owner left this out
  of scope. It would need the `publication-formats` gate.
- **Loading holds into memory at startup.** A second source of truth adds a
  drift case and saves one indexed read per installation per pass.

## Refute-First Findings

An independent reviewer tried to break the change before commit.

- **Confirmed, fixed:** six store migration tests skip later migrations by
  name and did not skip 0099. They now do.
- **Confirmed, fixed:** a shutdown that cancels the pass during the hold
  write made the request fail as an unknown outcome, holding the installation
  for 69 minutes though nothing was sent. The janitor now checks the context
  after the write and releases the row if the pass is canceled.
- **Confirmed, fixed:** read and release failures were untested, so a version
  that read a failed lookup as "not held" would have passed. Tests now pin
  both as fail-closed.
- **Allowed: wall-clock deadline.** `held_until` is a parsed instant with no
  monotonic reading, so a forward clock jump of more than the nine-minute
  skew ends a hold early, now also within one process. GitHub's expiry is
  wall-clock time too, so wall time is the right measure.
- **Allowed: two janitors on one database.** The check, record, and release
  are separate transactions and the row names no owner, so two janitors can
  both mint, and one's release can clear the other's hold. Prod and dev lock
  the database exclusively, so this needs the ephemeral environment. Before
  this change two processes had fully separate holds, so it is no regression.
- **Allowed: restoring an older checkpoint.** It loses holds taken after the
  checkpoint, as a restore loses all later state.
- **Allowed: the deadline starts before the commit.** The hold write's
  latency shortens the hold measured from the actual send; the nine-minute
  skew absorbs it.

Revisit when the real-work run shares a database with prod, a double mint
across a daemon handover is observed, or two janitors routinely share one
database.
