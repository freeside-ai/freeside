# Keep Stop Delivery Separate From Runtime Confirmation

For #1369, retain exact Stop commands in a coordinator-owned ledger separate
from submission and attention commands. The cache section is additive, bound
to daemon and paired device, and survives read-cache eviction. Persist before
POST; failed persistence prevents sending. Restoration validates the payload,
identity, version, epoch and any receipt, but never sends. A manual retry uses
the same command body. This rejects generalizing the existing submission ledger,
whose command and conflict semantics differ.

Confirmation binds the observed task version and epoch. Recheck both against
fresh task state before saving. A trusted stale-task conflict rejects that
prepared request; refresh canonically and ask for confirmation again. Conflict
rows never replace local authority. All successful receipts pass the existing
command trust gate. Ambiguous and authentication failures retain exact identity.
Known definitive daemon rejections (400 and 404) remove the pending command and
refresh before another confirmation. Other undocumented statuses remain
uncertain; a timeout or rate limit must not erase an earlier attempt's identity.

Keep accepted receipts only until authenticated sync supplies current task
state. Receipt replay cannot update cancellation or WIP. Failure means execution
may continue; transport Retry recovers a receipt and never promises a provider
restart. Finished and abandoned labels do not prove quiescence, so only a
cancellation fence removes the new Stop action. Pending recovery remains in the
Tasks toolbar even when its row is absent.

## Refutation

- **Confirmed and fixed:** A definitive 400/404 rejection could leave a saved
  request permanently blocking a fresh Stop, including after daemon restoration
  removes its target. Initial sends and restored retries now clear the durable
  entry on those responses; authentication, timeout, rate-limit, and server
  failures preserve it.
- **Confirmed and fixed:** An accepted ledger restored beside current cached
  cancellation could remain stuck on unchanged heartbeats. Freshness validation
  now settles it even without a bootstrap; a relaunch regression covers this.
- **Confirmed and fixed:** Accepted-delivery text could contradict a newer
  confirmed or failed state after refresh failure. Synced cancellation takes
  precedence, and unresolved receipt recovery explicitly cannot restart work.
- **Disproved by tests:** Concurrent local sends, disk failure sending anyway,
  stale confirmation rebinding, wrong-device/daemon restore, forged receipt
  acceptance, and automatic relaunch replay. Coordinator ownership serializes
  local sends; validated persistence and exact replay preserve command identity.
- **Disproved by tests:** A delayed receipt overwrites newer failure or
  confirmation. Receipt handling never writes task snapshots; canonical sync is
  the only task authority. A conflict likewise cannot install replacement rows.

Revisit when the daemon adds a provider-cancellation retry or resume capability,
or when deployment identity changes from the paired daemon address. Neither is
introduced by a presentation control.
