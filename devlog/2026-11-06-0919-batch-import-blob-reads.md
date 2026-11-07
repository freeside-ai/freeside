# Batch Import Blob Reads

Issue #2681 moves the importer's base-blob reads onto `gitrun.ObjectReader`,
the batch reader #2669 added
([note](2026-11-05-2020-batch-git-object-reads.md)). The importer used to
start one or two `git cat-file` processes per base blob it checked. Now it
starts at most two per import: `--batch-check` for sizes and types,
`--batch` for content. In a 60-file unchanged import, that's 120 processes
down to 2. The importer is Tier A, so the refute-first findings are
recorded below.

## Decisions

- **One reader per import, owned by the import's runner.** `gitRunner`
  builds the reader when it pins the checkout. `Import` defers its close
  right after the runner exists, so no `cat-file` process outlives the
  import on any path. The defer runs before the scratch removal, which was
  registered earlier. A reader per call would bring back the per-object
  process cost this change removes.
- **A base entry read as a file must name a blob.** This tightens behavior
  on purpose. A regular, symlink, or `.gitignore` base entry whose object is
  a tag, tree, or commit now fails the import with `ErrGitPlumbing` at the
  size check, before any content read.
  - **Why it matters:** Before, `cat-file -s` reported a tag's own small
    size and `cat-file blob` then peeled the tag. A tag could carry a blob
    of any size past the size gate, and a size claim matching the tag let
    the import read the peeled blob in full: hashed for an omitted entry,
    buffered for a symlink target or an ignore file.
  - **Rejected option:** Peeling the tag in the importer would keep a
    reachable bypass of the size gate, and no git tool writes a tag into a
    blob entry.
  - **Side effects:** A non-blob whose size differed from the manifest's
    claim used to yield a finding; it now fails the import. With a matching
    claim, the old code could even elide an omitted entry as unchanged
    because the peeled content matched. Only a hand-built tree reaches
    either case. #2669 made the same choice for the agent tree.
  - **Not a definitive rejection:** `ErrGitPlumbing` is outside the stage
    pipeline's definitive import rejections
    (`daemon/internal/exec/stage/pipeline.go`), so the completed export is
    kept as retryable rather than discarded, like any plumbing failure.
    The fault lies in the base, not the agent's export, so that fits.
- **The callers stay as they were.** `derive.go`, `ignored.go`, and
  `verifyIngestedBlobs` still call `blobSize` before `blobDigest` or
  `blobContent`, in the same order, with the same SHA-256 checks and error
  classes. Only the three methods underneath changed.

## Refute-First Findings

Each claim below was tried against the change before commit:

- **Every content read follows a size check the manifest can't bypass.**
  Disproved by check. Sizes come only from `Info` (`--batch-check`), and a
  non-blob fails there. Tamper tests use a logging git wrapper to show that
  a mismatched size claim (omitted regular entry, symlink), a tag in a
  regular, symlink, or `.gitignore` entry, and an oversized base ignore
  file never start `cat-file --batch`. The tag tests fail on the old code.
- **The new reads differ from the old ones.** Disproved by check. A scratch
  comparison ran the old per-call methods and the new ones in interleaved
  random order over 300 generated blobs: sizes 0 through 3 MiB, binary and
  text, 900 reads in all. Sizes, digests, and content all matched. Trees,
  commits, and tags fail with `ErrGitPlumbing` under the new methods, and a
  missing object fails with the same class under both.
- **Content can be left unread, wedging the reader.** Allowed by an explicit
  decision. `blobDigest` and `blobContent` read to `io.EOF`. A refused
  non-blob leaves its content unread, which fails the reader's next call,
  but every caller returns the error at once.
- **A caller swallows a reader error and keeps going.** Disproved by
  reading. Every call site returns the error immediately.
- **A cancelled or failed import leaves `cat-file` running.** Disproved by
  check. No child process remained after a successful import, a refused
  tag, or a context cancelled mid-derive. The cancel surfaced as
  `ErrGitPlumbing` wrapping `context.Canceled`.
- **The reader refuses IDs the old path accepted.** Allowed. It takes only
  full lowercase hex IDs, and the importer passes only `ls-tree` and
  `hash-object` output, which are always in that form.

Revisit when importer derivation becomes concurrent: the reader serves one
goroutine, so each goroutine would need its own reader.
