# Verify Batch Reads

Issue #2680 moves the verifier's object reads onto `gitrun.ObjectReader`
(`devlog/2026-11-05-2020-batch-git-object-reads.md`). Materialization
started one `git cat-file blob` per tree entry for every recipe command,
and `blobAt`, which serves `CommitReader` and the verifier's recipe reads,
started one per read. Both read untrusted candidate trees, so this is
Tier A.

## Decisions

- **One reader per `gitRunner`, under its creator's context.** Chose to
  build the reader in `newGitRunner`, after the checkout is pinned and its
  format checked, over a reader per call. Its process runs under the
  context `newGitRunner` was given, so every creator closes it before that
  context ends: `Verify` and `MaterializeExact` with a deferred close, and
  `CommitReader.Close`. `OpenCommitReader`'s context therefore bounds the
  reader's life. Every caller today opens, reads, and closes under one
  context; one that cancels the open context and keeps reading would get
  `ErrGitPlumbing`.
- **Stream regular files.** Chose `root.OpenFile` with `root.WriteFile`'s
  flags and `io.Copy` over buffering each blob. Symlink targets are still
  read whole. Neither path gains a size cap (a non-goal of #2680).
- **A wrong object type is `ErrMalformedTree`.** The batch read does not
  peel tags, so the verifier now checks each entry's object. A blob-mode or
  symlink entry whose object is not a blob, or a `blobAt` read whose object
  size differs from the listing, is refused before its bytes are used.
  Chose `ErrMalformedTree` over `ErrCommitFileUnreadable` because
  `ObserveBaseInputs` reads past `ErrCommitFileUnreadable`, and a refusal
  leaves the object's content unread, which usually ends the reader. The
  engine already treats `ErrMalformedTree` as an unverifiable tree.
- **The `blobAt` tag bypass is closed.** `ls-tree -l` lists a blob-mode
  entry naming a tag at the tag's own size, so the size cap passed, and
  `cat-file blob` followed the tag and returned the tagged blob whatever
  its size. `TestCommitReaderRefusesNonBlobEntries` returned 4,096 bytes
  past a 1,024-byte cap on the old code. A tag at the trusted base's recipe
  path now fails verification instead of supplying the recipe; no git tool
  writes such a tree.

## Refute-First Findings

- **Old and new materialization agree.** Disproved by a check: a scratch
  comparison ran the old and new `MaterializeExact` over one generated
  commit of 507 entries (461 random binary files from 0 to 300 KB, CRLF and
  `$Id$` text under `text`, `ident`, and `filter` attributes, 40
  executables, five symlinks including absolute, dangling, and
  tab-containing targets, and a gitlink). The two workspaces matched byte
  for byte, and in mode, size, and link target, across all 611 paths.
- **A reader error leaves a process running.** Disproved: the reader starts
  nothing until its first read, stops its process on any error, and every
  `gitRunner` creator closes it. Test runners close it through `t.Cleanup`.
- **A partly written workspace reaches a later command.** Disproved: each
  recipe command materializes into a fresh path, `Verify` returns on any
  materialization error, and `projectimage`'s `Copy` returns the error.
- **The reader writes bytes the tree does not hold.** Disproved:
  `verifyMaterialized` is unchanged and re-hashes every entry against the
  tree, and the reader checks each object's size and delimiter.
- **Concurrent use of one reader.** Disproved: the verify package starts no
  goroutines, and `CommitReader` is documented as single-goroutine.
- **An idle batch process outlives its use.** Allowed: during `Verify` the
  process stays open while recipe commands run, holding only the pinned
  repository, and stops when `Verify` returns.
- **A malformed trusted base is reported as the head's tree.** Allowed:
  re-entry words `ErrMalformedTree` as a fault in the evaluated tree, so a
  tag entry on the trusted base's recipe path or image inputs would be
  misattributed. Reaching it needs a hand-built tree on the default branch,
  which before this change was silently followed instead.
