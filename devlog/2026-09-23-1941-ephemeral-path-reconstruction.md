# Reject Reconstructed Supervised Paths

Chose to re-check paths after reconstructing missing components and to compare
multiply linked SQLite sidecars with supervised-root files, rather than
assuming lexical resolution and sidecar enumeration provide isolation.

An adversarial review showed two admitted, material paths: a missing component
followed by `..` could expose a supervised child symlink after reconstruction,
and an outside WAL or SHM pathname could be hard linked to a supervised
sidecar. Both let an ephemeral daemon reach supervised state while its daemon
lock was absent. The guard now rejects the reconstructed alias and only scans
supervised roots when a sidecar's link count requires an inode comparison.

Rejected a blanket ban on all linked sidecars because an unrelated external
hard link does not reach supervised state. Rejected broader alias and layout
policy because #1523 and #1524 already own those decisions.

Revisit when the daemon supports a filesystem whose link-count metadata cannot
be inspected through `syscall.Stat_t`, or when a bounded supervised-file index
replaces the on-demand root walk.
