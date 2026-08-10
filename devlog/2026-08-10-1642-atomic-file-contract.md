# Atomic File Durability Contract

Chose one leaf `internal/atomicfile` package over package-local copies because
the file-sync, rename, and directory-sync sequence is a single durability
contract, and the existing copies had already diverged on both missing syncs
and close-error handling. The helper reports joined sync and close errors,
offers replace and no-replace byte-slice writes, and keeps streaming writes
explicit for content-addressed callers that learn their target only after
hashing.

Chose to add full file and directory syncs to the exporter and fake driver
instead of documenting them as intentionally weaker. Export is not a hot path,
and making ward verification defense in depth avoids assigning durability to a
distant consumer. The fake driver now matches the production stage driver's
restart boundary.

Current `main` changed after issue #565 was elaborated: the Claude intent
writer moved to `internal/exec/stage`, and `cmd/freesided` gained a second topic
key durability sequence. Chose to consolidate both in this work unit so the
issue's no-duplicate-helper acceptance remains true, rather than preserve a
known new copy because its path was absent from the original scope list.

Kept the fake-publication `os.Link` installation separate because it reports a
race loser while retaining the source, a different contract from replace and
no-replace rename. Its surrounding directory barriers use the shared helper.

The refute-first review confirmed four gaps in the first implementation and
changed the contract accordingly. A failed temporary-file cleanup now
supersedes `fs.ErrExist`, so a losing backup key is never classified as a clean
race while secret material remains. Streaming commit accepts a narrow
caller-supplied directory barrier so signet's fault injection replaces the real
barrier instead of adding a second one. The fake driver durably publishes each
new state-directory ancestor before its first file, and streaming creation
rejects an empty directory instead of silently staging in the system temp
directory. The review rejected concerns about the retained `os.Link` contract,
the moved platform syscalls, and export-tree durability after verifying their
existing semantics remained intact.

Revisit when a supported platform cannot provide atomic no-replace rename, or
measurement shows per-blob exporter syncs materially constrain the handoff.

Issue: #565
