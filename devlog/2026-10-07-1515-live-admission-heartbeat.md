# Live Admission Verdict on the Heartbeat

Issue #1707 chooses the current unattended-admission snapshot on the revision
heartbeat over advancing the sync revision when backup health changes. Backup
health depends on checkpoint files and the clock, including checkpoint and
restore-test age limits. Its verdict can change without a store transaction.
A revision-based solution would need a stored last verdict and either a write
from a read path or a periodic writer, adding persistence and coordination.

The heartbeat reads the same admission gate as bootstrap, in the transaction
that reads the epoch and revision. It carries every stop, because the cause
can change while admission remains stopped. A failed gate read fails the
heartbeat instead of returning a success without its verdict.

The client compares the whole verdict with its adopted snapshot and
bootstraps on a mismatch. Only that bootstrap replaces cached state. If it
fails, the old verdict remains available but is no longer marked fresh. This
keeps the existing cache format and revision rules while detecting live gate
changes on the existing 15-second foreground heartbeat.

Independent review found no reachable defect in the transaction, error, or
adoption paths. The failure checks disprove two possible false-success paths:
an inconsistent attention row cannot produce a successful heartbeat, and a
failed bootstrap after a verdict mismatch cannot leave the old cache fresh.
An unreadable backup-health source still produces a blocking stop under the
existing gate policy; it is distinct from failure to read the gate itself.

Revisit when gate inputs become fully revisioned state, or measurements show
the heartbeat's backup-health reads need a separate performance work unit.
