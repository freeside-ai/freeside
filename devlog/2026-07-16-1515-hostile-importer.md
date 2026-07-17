# The hostile importer: policy split, git mechanism, and the refute pass

Gauntlet unit #74 (the §5.6 hostile importer). Consumes the export
helper's manifest+blob wire contract (#73, PR #112, contract note
`2026-07-16-1237-export-manifest-contract.md`) and produces a
daemon-authored clean commit on a fresh daemon-owned checkout at an
enforced base SHA. This is a returned-object trust-boundary unit
(AGENTS.md finish line), so it carries a mandatory note with a
refute-first pass.

## Owner decisions (settled this session)

- **Two outcome classes, not one.** Integrity violations fail closed as
  typed errors with no Result and no commit (unreadable/oversized/invalid
  manifest, git-metadata path injection, file/dir conflict, missing/orphan
  blob, digest/size mismatch, base-SHA mismatch, tree mismatch). Policy
  violations accumulate as publish-blocking `Finding`s on the Result.
  Rejected: a single "reject" outcome — it conflates forgery (an honest
  exporter cannot produce it) with policy (a real change that must route
  through the control plane).
- **Publish-blocking findings still produce the commit when the tree can
  faithfully represent the candidate.** §5.5 routes blocked
  automation-control changes "through control-plane change", which needs
  the imported commit to exist; §12's safety failure is such a change
  reaching *publication*, not import; #75's verifier consumes the imported
  commit. The commit is withheld only when the tree cannot faithfully hold
  the candidate: a changed non-regular kind, an `invalid_path` entry, or a
  needed-but-omitted blob (`FindingKind.blocksCommit`). Rejected: blocking
  the commit on any finding — it would deny the human gate the concrete
  artifact and leave #75 nothing to verify.
- **Git mechanism: system git plumbing only.** `hash-object`/`read-tree`/
  `update-index`/`write-tree`/`commit-tree`/`update-ref` under a hardened
  context (git dir pinned once, scratch index and HOME, no user/system/
  workspace config, hooks and fsmonitor off, protocol denied, protectHFS/
  NTFS forced, sha1 object format required). Candidate bytes enter git only
  as stdin content or from the audited blob store, never argument vectors;
  paths travel NUL-terminated. Rejected: go-git (a heavy new dependency in
  a trust-boundary package with git-compat edge cases) and hand-rolled git
  objects (reimplements internals the project would then own).
- **Package only, no `cmd/` binary this unit.** All acceptance is testable
  at package level; the out-of-process worker wrapper lands with the daemon
  integration that wires the gauntlet end to end (post-#75). Keeps the
  issue's declared path (`daemon/internal/importer`) exact.
- **Secret scanning: a small in-house high-signal detector**, no
  dependency. §5.4 honest scope: added-or-modified regular UTF-8 content
  under a per-file cap; GitHub tokens/PATs, AWS access-key ids, Slack
  tokens, PEM private-key headers, GCP service-account key ids. Findings
  carry path + rule + line, never the matched bytes. Rejected: gitleaks as
  a library (heavyweight dependency in a trust-boundary package, churning
  upstream rules).
- **`golang.org/x/text` added** (the daemon's first x/text direct
  dependency) for NFC-collision detection: the reference deployment is APFS
  (case- and normalization-insensitive), so two tree paths that fold
  together resolve to one file and which content wins is filesystem-defined.
  Rejected: case-fold-only detection, leaving the NFC gap on the project's
  own filesystem.

## Contract observations (#73 first-consumer)

The importer is the manifest format's first consumer, so a gap could be
fixed in place as a v1 widening (contract note's revisit condition). None
was needed: the wire contract held under the full adversarial suite. Two
non-blocking observations, both handled importer-side as intended by #73's
design (the helper records, the importer enforces):

- `fs.ValidPath` permits `\n` and `\t` in a canonical path. Inert in the
  importer's NUL-delimited plumbing channel (proven by the exec lens), so
  not a v1 change; a path with control characters can still be committed.
  Not currently flagged; a candidate follow-up if the plan wants control
  characters treated as a policy class. Filed as an observation, not open
  work.
- An `invalid_path` entry is opaque exactly like a submodule; the importer,
  not the helper, owes deletion suppression beneath it (see F1 below).

## Refute pass (trust boundary)

Three fresh-context lenses, each tasked only to disprove, given the code
and the export contract but not this design history.

- **git/exec execution and hook/config influence** — no escape found. No
  manifest-derived byte reaches a git argument vector; the `-z` NUL channel
  makes `\n`/`\t` in a path inert; env hardening leaves no config, hook,
  filter, or protocol surface; the sha1 object format is required and fails
  closed otherwise. Two independent cross-checks (ingested-oid equality and
  the exact-tree diff) fail closed on any divergence.
- **integrity binding** — no commit-integrity break, no blocking-finding
  bypass. Committed content is provably the verified bytes (ingested oid
  cross-checked against the pure-Go derivation from the sha256-verifying
  stream; exact-tree diff re-confirms). One hardening note accepted and
  fixed (see secret-scan binding below).
- **change derivation** — one real finding (F1) and two minor ones, all
  fixed; the exact-tree acceptance bijection and deletion-suppression under
  submodule blindness held under empirical git-plumbing probing.

Dispositions:

- **F1 (confirmed, fixed): `invalid_path` directory blindness produced
  phantom deletions.** Only submodule entries were added to the opaque
  set; an `invalid_path` directory (a non-UTF-8 name the exporter records
  without descending) left base content beneath it deriving as deletions.
  Contained today by the blocking bit (no commit), but `Result.Changes`
  misreported and the phantom deletions fed the policy/collision/secret
  passes. Fixed: an `invalid_path` entry's decoded raw bytes join the
  opaque prefix set and mark the exact base path consumed, exactly like a
  submodule. Regression test with a base tree carrying a non-UTF-8
  directory (`TestImportInvalidPathDirectorySuppressesDeletions`).
- **F2 (confirmed, fixed): mode-only change on an omitted blob was blocked
  with a false detail.** A base regular file chmod'd but byte-identical,
  whose blob the export caps omitted, was misclassified as changed content.
  Fixed: the omitted branch compares against base content regardless of
  mode; an identical-content mode change reuses the base object
  (`plannedChange.fromBase`, which construction neither ingests nor
  cross-checks against a handoff blob) and produces no finding. Regression
  test `TestImportOmittedModeOnlyChange`.
- **F3 (confirmed, fixed): `sortFindings` comment contradicted the code.**
  An empty-Path invalid_path finding sorts first, not "after representable
  ones". Comment corrected; ordering was already total and deterministic.
- **Secret-scan digest binding (accepted, fixed).** The scan read handoff
  bytes without binding them to the verified digest; under a hypothetical
  concurrent writer on the handoff a "scanned clean, committed dirty" split
  could open. In the real design the handoff is a static, daemon-owned
  directory collected from a destroyed single-shot exporter, so no live
  writer exists, but binding is cheap and correct for a trust-boundary
  component: `readScanBlob` now re-hashes and fails closed on mismatch.
- **Rejected-by-verification (raised, disproved; do not re-open):**
  argument-vector option injection, `--stdin-paths` / `--index-info`
  newline/tab smuggling, workspace `.git`/hook/config influence, base-SHA
  spoofing, TOCTOU blob swap between verify and ingest, dir↔file shape
  changes evading the exact-tree check, `underAny` prefix false-matches,
  spurious-change / silent-revert in elision, and blocking-finding desync.
  Each is blocked by a named defense the lenses could not break.
- **Residual, revised by Codex pass 4 (below):** a changed regular file
  between `SecretMaxScanBytes` and `MaxBlobBytes` was originally left
  unscanned and unflagged (§5.4 best-effort; `secret` is non-blocking). The
  silent skip was the wrong shape — it let a findings-free import imply
  "scanned clean" — so it now surfaces a non-blocking `secret_scan_skipped`
  finding instead.

## Automated review (Codex + human, findings across fifteen passes, all fixed)

Every finding was real and folded into its owning commit. Codex ran
eight automated passes (nine P2s); a ninth pass plus a human review
round then surfaced the batch recorded under "Round 9" below.

- **Pass 1 — overlong secret-scan line.** `scanText` used
  `bufio.Scanner`, which stops at `ErrTooLong` on a line at or above its
  buffer cap, and the loop never checked `sc.Err()`, so a token padded onto
  one very long line (a minified or env line) within the configured scan
  size yielded no finding while the import still committed. Fixed by
  splitting the already-bounded in-memory buffer on newlines directly, with
  no per-line cap, so the scan cannot fail closed-open. Test
  `TestScanTextLongLine`.
- **Pass 2 — special file at an untrusted-boundary path.** `os.Open` on
  `manifest.json` ran before any type check, so a FIFO there blocked
  `Import` indefinitely and a symlink read outside the handoff. Fixed as a
  class: a single hardened `openRegular` (O_NOFOLLOW, O_NONBLOCK, then
  fstat-regular, failing closed) now guards every untrusted-boundary open —
  the manifest, each verified blob, and each scanned blob — rather than only
  the cited line. Tests `TestImportRejectsSpecialManifest` (FIFO and
  symlink) and `TestImportRejectsSpecialBlob`.
- **Pass 2 — blob-present mode-only change not marked fromBase.** The
  sibling of the refute pass's F2: F2 fixed the omitted-blob branch, but a
  blob-*present* file identical to base with only a mode change fell through
  with `fromBase=false`, so `scanSecrets` re-scanned it and a chmod on a
  token-bearing file spuriously flagged a secret. Fixed by marking the
  content-identical mode-only change `fromBase` in the blob-present branch
  too (content already in base, nothing new to scan). Test
  `TestImportBlobPresentModeOnlyChange`. Lesson recorded: F2's class sweep
  should have covered both branches at once.
- **Pass 3 — unbounded blob read.** `verifyBlob` streamed the whole blob
  file through `io.Copy` before the `n != size` length check, so a hostile
  blob file far larger than its claimed manifest size consumed arbitrary
  disk I/O before rejection. Fixed by reading at most `size+1` bytes
  (`io.LimitReader`), so an oversized file fails closed on length without a
  full stream; the sibling reads (manifest intake, secret scan) were already
  `LimitReader`-bounded. Test: the `oversized blob` case in
  `TestVerifyBlobsRejects`.
- **Swept proactively with pass 3 (same resource-exhaustion class).** Two
  hostile-handoff resource findings in a row (blocking open, unbounded read)
  made the class worth sweeping ahead of the reviewer: `auditHandoffLayout`
  used `os.ReadDir`, which loads a whole directory listing, so a handoff that
  stuffs `blobs/sha256/` with millions of names was a memory DoS before the
  audit could reject it. Replaced with a batched `scanDirBatched` (bounded
  `ReadDir(n)` per syscall, aborting on the first unexpected entry), matching
  the export walker's batched-read discipline. Tests
  `TestVerifyBlobsManyBatches` and `TestVerifyBlobsRejectsOrphanAmongMany`.
- **Pass 4 — silent over-cap secret-scan skip.** An added or modified file
  between `SecretMaxScanBytes` (1 MiB) and `MaxBlobBytes` (100 MiB) was
  skipped by the scan with no finding, so a token in a 2 MiB text file gave
  a findings-free "clean" import. Fixed by surfacing the skip as a
  non-blocking `secret_scan_skipped` finding: the gap is now visible to the
  publication gate rather than implied clean. Supersedes the revised residual
  above. Test `TestImportSecretScanOverCapSurfaced`.
- **Pass 4 — fromBase chmod size-accounted.** A `fromBase` mode-only change
  is bytes already in the trusted base, but `applyPolicy` still counted the
  base blob's size against the per-file and total caps, so a chmod on a large
  tracked file tripped a spurious `size_violation`. Fixed by excluding
  `fromBase` from size accounting alongside deletions (size policy bounds
  introduced content). Test `TestApplyPolicySizeExcludesFromBase`. Same
  fromBase-scope class as the pass-2 scan finding.
- **Swept proactively after pass 4 (fromBase-scope class, second
  recurrence).** Two Codex findings on "a `fromBase` change treated as
  introducing content" (pass 2 scan, pass 4 size) made the class worth
  closing at its real width: every site that decides "did the candidate
  introduce this". The last latent member was `detectCollisions`, which
  treated a modified path as introducing a collision — so a chmod (or any
  modify) of a path that already fold-collides with a base sibling would
  flag a pre-existing base condition as the candidate's. Fixed by keying
  collision introduction on `ChangeAdded` only (a modify keeps an existing
  path and cannot introduce a new fold-collision). Test
  `TestDetectCollisionsIgnoresModifies`.
- **Pass 5 — file/directory fold-collision missed.** Collision detection
  compared only complete folded paths, so a file and a directory whose
  folded names coincide slipped through (base file `README` + candidate dir
  `readme/config.yml`; base dir `foo/` + candidate file `FOO`) — APFS can
  materialize neither. Fixed by flagging an added regular file whose folded
  path equals another leaf, is a directory another path occupies, or has a
  folded ancestor another path occupies as a file (the ancestor-storage
  mechanism this first used was replaced by the second refute pass below to
  bound memory). Restricted the introducing side to regular-file adds
  in the same change (a non-regular add is already publish-blocking, so a
  collision finding layered on it is redundant — it surfaced on the
  submodule fixture before the restriction). Tests
  `TestDetectCollisionsFileDirectory`.
- **Pass 6 — stored-blob size caps enforced only after streaming.** The
  `size+1` read bound (pass 3) caught a file larger than its *claimed* size,
  but a manifest whose stored blob *declares* a size over policy (a 50 GB
  blob, or many blobs summing past the total) still streamed fully through
  `verifyBlob` before the non-blocking size finding ran. Since an honest
  exporter omits blobs over its caps, a *stored* blob past the importer's
  matching cap is contract-impossible; `verifyBlobs` now rejects it fail
  closed (`ErrBlobTooLarge`), per-file and overflow-safe total, before
  opening or hashing anything, bounding bytes read at the untrusted boundary.
  The publish-level `size_violation` finding stays as the routable
  publish-policy signal. Tests `TestVerifyBlobsRejectsOverCap`.
- **Pass 7 — checkout-local git config leaked into the commit.** The
  hardening neutralized user and system config but not the daemon-owned
  checkout's own `.git/config`, so `commit-tree` honored a local
  `i18n.commitEncoding` (writing an `encoding` header) or `commit.gpgsign`,
  making the commit object depend on checkout-local settings rather than
  only base+change+options. Fixed by pinning the commit-affecting keys via
  `-c` overrides (which outrank local config) in `hardenedConfig`, and
  correcting the overstated "no workspace config" doc claim. Test
  `TestCommitIgnoresLocalConfig`.
- **Pass 8 — omitted-entry base blob hashed before its size checked.** The
  resource class once more, on the base side the refute pass wrongly
  cleared: `deriveRegular`'s omitted branch streamed the whole base blob
  through sha256 (`blobDigest`) before comparing sizes, and the manifest
  chooses which base blob that examines, so an omitted entry claiming
  `size: 1` for a multi-GB base file forced hashing all of it. Fixed by
  checking the base blob's size first (`cat-file -s`, via a new `blobSize`)
  and hashing only on a size match; a mismatch decides "changed" cheaply.
  Test `TestImportOmittedSizeMismatchIsChanged`. (The during-review
  resource lens had inspected `blobDigest` and dismissed it as "base
  trusted" — it missed that the untrusted manifest selects the base blob;
  recorded so the miss is not repeated.)

## Second refute pass (during review, three read-only lenses)

Fired when the resource-exhaustion class recurred across Codex passes 3
and 6 (per the review-response class-sweep discipline): three
fresh-context lenses, read-only, tasked to disprove — resource/DoS
bounds, collision/derivation correctness, and validation-order/fail-open.
The boundary lens found nothing (every validation-order and cross-check
defense held). The other two found two real defects, both fixed in this
push, folded into their commits:

- **Path-length DoS (HIGH).** No cap bounded a single entry's path
  length or depth, and both `gatePaths` (ancestor walk) and
  `detectCollisions` (ancestor-prefix storage) are superlinear in one
  path, so a ~2 MB path (far under the 256 MB manifest cap) forced
  O(L²) memory/CPU — an OOM/hang. Fixed with intake caps (own commit,
  `MaxPathBytes`/`MaxPathDepth`) and by rewriting `detectCollisions` to
  store only leaves (a sorted-leaf binary search for the directory
  direction, a length-capped ancestor walk for the file direction), so
  no ancestor prefixes are materialized: memory is now O(manifest bytes).
- **Wrong case fold (HIGH + MEDIUM).** `foldedComponents` used
  `strings.ToLower` (simple case mapping); APFS uses full case folding,
  verified on a real APFS volume. `ß`/`ss` and the `ﬁ` ligature/`fi` are
  one file on APFS but folded distinct here (a missed collision, HIGH),
  while `İ` (U+0130)/`i` are distinct on APFS but both lowercased to `i`
  (a false collision, MEDIUM). Fixed by folding with
  `golang.org/x/text/cases.Fold()` (full folding) over the NFC form,
  pinned by `TestFoldedComponentsFullCaseFold`.
- **Rejected-by-verification (this pass):** no unbounded read/allocation
  survived the caps (blob reads, dir enumeration, base-blob streaming all
  bounded); no validation-order or fail-open path; the two git
  cross-checks and `openRegular`'s symlinked-ancestor case all held.

## Round 9 (human review + Codex pass 9, six findings, all fixed)

- **Mandatory gates were configurable away (P1).** The §5.5/§5.8 pattern
  accessors substituted a caller's override for the defaults, so an empty
  or partial list disabled a mandatory safety gate (the §12 failure). Fixed
  to immutable minimums: the defaults always apply and caller patterns
  (renamed `Extra*`) are *added*, widen-only. Invalid custom globs, which
  `matchAny` silently treated as no-match (fail open), are now rejected at
  `Options.validate` (`validGlob`). Tests `TestMandatoryGatesImmutable`,
  `TestInvalidGlobFailsClosed`.
- **Missing agent-control surfaces (P1).** Added current auto-loaded
  instruction/skill/hook locations to the classes: `.github/agents/**`,
  `.github/skills/**`, `.agents/skills/**`, `.windsurf/rules/**`
  (reviewer-instruction) and `.github/hooks/**` (automation-control). Test
  `TestNewAgentControlSurfaces`.
- **Opaque-prefix suppression was O(base × opaque) (P2).** The deletion
  pass tested every base path against every opaque prefix — up to a
  million manifest entries times the base tree, a CPU-exhaustion
  cross-product the path caps do not bound. Replaced with an opaque *set*
  and an ancestor walk over each base path's own components
  (`underAnyOpaque`, O(base × depth)). Test `TestUnderAnyOpaque`.
- **Git replacement objects bypassed base enforcement (P2, found by both
  the human review and Codex pass 9).** A `refs/replace/*` in the checkout
  let `rev-parse` (base enforcement) see the real SHA while
  `ls-tree`/`cat-file` (derivation) read the substituted object. Fixed by
  setting `GIT_NO_REPLACE_OBJECTS=1` in the plumbing env. Test
  `TestIgnoresReplaceObjects`.
- **Omitted entry replacing a non-regular base slot lost its
  classification (P2, Codex pass 9).** The omitted branch emitted only
  `blob_omitted` when it replaced a symlink/gitlink, dropping the §5.6
  `non_regular_change` signal the stored-blob branch keeps. Fixed by
  returning both findings (`deriveRegular`/`deriveSymlink` now return a
  finding slice). Test `TestImportOmittedReplacesNonRegular`.
- **Stale PR body (P2).** The PR body's base SHA, merge-audit path count,
  commit map, and config claim had drifted across the rebases; refreshed at
  handoff.

## Round 10–13 (Codex passes 10–13, four findings, fixed)

- **Deletion of a non-representable base path was lossy (P2).** When the
  trusted base tracks a non-UTF-8 name the candidate removes (absent from
  the manifest), the deletion fell through as an ordinary change carrying
  the raw bytes, so `Result.Changes` held invalid UTF-8 (lossy/ambiguous in
  JSON, and allowlist matching on it is unreliable). Fixed to match the
  `invalid_path` entry handling: a non-representable base deletion is
  publish-blocking (`invalid_path_entry`) and reported losslessly by raw
  `PathHex` (added to `Change` and `plannedChange`; `path` still holds the
  raw bytes for git's NUL-safe channels). Test
  `TestImportDeletesNonRepresentableBasePath`.
- **Policy findings on a non-representable path were still lossy (P2,
  pass 11).** The pass-10 fix made the `Change` lossless but `applyPolicy`
  still emitted its class findings (a `bad\xe9/AGENTS.md` reviewer-instruction
  match) with the raw `Path`. Fixed with a `plannedChange.finding` helper
  that reports `PathHex` when the change is non-representable, mirroring
  `public()`. Test `TestApplyPolicyPathHexLossless`.
- **Protected-path aliases bypassed the mandatory classes (P2, pass
  12).** A canonical candidate path like `.gitmodules ` (trailing space),
  `.gitattributes.` (trailing dot), or `.git‌modules` (zero-width) passes
  `gatePaths` but only case-folded before matching `**/.gitmodules` etc., so
  it missed the class while still materializing as the protected name on a
  downstream NTFS/HFS checkout — the same alias class `gitUnsafeComponent`
  rejects for `.git`, and it applies to all three mandatory classes
  (`AGENTS.md `, `action.yml `). Fixed with a `normalizeAliases` (trailing
  dot/space trim + HFS-ignorable strip) applied to the path before the
  mandatory-class match; the finding still reports the actual candidate
  path. Test `TestApplyPolicyAliasNormalization`.
- **Base-identical elision trusted git SHA-1 alone (P2, pass 13).** The
  blob-present branch elided (or marked `fromBase`) on `be.oid ==
  info.gitOID` — git's SHA-1 object identity — while the checkout uses the
  sha1 object format. A candidate blob crafted to collide with a base
  blob's SHA-1 would be treated as unchanged (base content retained, scan
  skipped) though the manifest's SHA-256 proves the bytes differ. The
  importer holds that independent SHA-256 evidence git does not, so it now
  verifies the base blob against the manifest digest (`baseMatchesDigest`,
  size-checked first) before eliding, and fails closed on a mismatch (a
  differing base/candidate SHA-1 collision is an attack on the object
  format). This streams the base blob when git OIDs match — accepted for the
  security property, and the same order as the candidate content the import
  already streams. Test `TestBaseMatchesDigest` (the collision path itself
  is not unit-testable without a real SHA-1 collision). Revisit if the base
  ever moves to git's sha256 object format, which would make git identity
  collision-resistant and retire this check.

## Round 14 (Codex pass 14, two findings, fixed as one ingestion class)

- **Ingested objects still trusted SHA-1 identity (P2).** The round-13
  base shortcut checked the independent SHA-256, but construction still
  accepted `hash-object` output by SHA-1 alone. If the object database
  already held different bytes at a colliding name, or two handoff blobs
  had different SHA-256 digests but the same SHA-1, the tree could point
  at bytes other than those verified and scanned. Fixed by re-reading
  every ingested object through `cat-file`, size-first, and matching its
  content to the manifest SHA-256 before it can enter the index. A helper
  test models the collision identity without requiring real collision
  fixture bytes (`TestVerifyIngestedBlobsRejectsSHA1Collision`).
- **Git re-opened the untrusted blob pathname without a bound (P2).**
  `hash-object --stdin-paths` opened each handoff path after verification
  and secret scanning, so a replacement race could substitute a FIFO or
  huge regular file and block or stream it before the OID mismatch. Fixed
  as the same ingestion class: each needed blob is re-opened with
  `openRegular`, read through the manifest `size+1` bound, re-bound to its
  SHA-256, and copied in that same stream to a daemon-private scratch
  snapshot; the one batched `hash-object` invocation reads only those
  snapshots. Tests cover same-size, oversized, and FIFO replacements
  (`TestIngestBlobsUsesVerifiedSnapshotAfterHandoffReplacement`).

## Round 15 (Codex pass 15, two findings, fixed)

- **Layout-audit directory opens were not hardened (P2).** The audit
  validated `blobs` and `blobs/sha256` in their parent listings, then
  re-opened them with bare `os.Open`; a replacement race could substitute
  a FIFO (blocking the import) or symlink (redirecting enumeration) before
  that open. Added `openDirectory`, the directory counterpart to
  `openRegular`: `O_NOFOLLOW`, `O_NONBLOCK`, `O_DIRECTORY`, then fstat and
  require a directory. The audit retains each opened parent descriptor
  across its listing and opens the next single child with `openat`, so a
  swapped intermediate `blobs` pathname cannot redirect the later
  `sha256` traversal either (the initial final-component-only patch was
  widened during self-review). Every handoff directory open now uses this
  descriptor chain. `TestScanDirBatchedRejectsSpecialDirectory` covers
  symlink/FIFO final components; `TestOpenDirectoryAtPinsAndRejectsChildren`
  covers a renamed-and-replaced intermediate parent. This promotes the
  already-resolved `golang.org/x/sys` module from indirect to direct use
  for portable `unix.Openat` on the Linux/macOS reference platforms;
  rejected: duplicating raw, build-tagged syscall wrappers.
- **NTFS alternate-data-stream aliases bypassed protected classes (P2).**
  The round-12 normalizer handled trailing dot/space and HFS-ignorable
  aliases but left `name:stream` / `name::$DATA`; NTFS materializes those
  as a stream of `name`, so `.gitmodules::$DATA`, `AGENTS.md::$DATA`, and
  `action.yml:payload` evaded their mandatory findings. Component alias
  normalization now strips the ADS suffix before matching all three
  classes. The same helper closes the more severe structural sibling:
  `.git::$DATA` now fails the git-metadata path gate. Tests cover unnamed
  and named streams across every class and the structural gate.
- **Refute pass widening: post-audit reads and APFS full folding (P1s).**
  The fresh read-only lens found that the initial directory fix pinned the
  audit but later verification, scanning, and ingestion still re-resolved
  `handoffDir/blobs/sha256/<digest>`; `O_NOFOLLOW` on the digest protected
  only the final component, so a swapped intermediate `blobs`/`sha256`
  symlink could redirect those reads. Widened the boundary: the audit
  returns its pinned `sha256` descriptor, content verification opens each
  digest with `openat`, and that same bounded SHA-256-verifying stream
  writes the daemon-private snapshot consumed by both scanning and Git.
  After verification returns, no stage reads a handoff path. The lens also
  found that mandatory class matching still used simple `strings.ToLower`,
  despite the package's own APFS evidence that full folding maps `ﬁ`→`fi`.
  `matchAny` now applies the collision model's NFC + Unicode full fold;
  `Jenkinsﬁle` is pinned as an automation-control regression. Both were
  confirmed and fixed; no speculative lens findings were retained.

## Round 16 (Codex pass 16, one finding, fixed)

- **Base symlink content was buffered before its size was checked (P2).**
  `deriveSymlink` read the complete base object before comparing it with
  the manifest target. Although the base commit is enforced, the untrusted
  manifest selects which base path is examined, and a malformed/no-checkout
  base can label an arbitrarily large blob mode `120000`; the comparison
  therefore exposed unbounded memory and I/O. The importer now asks
  `cat-file -s` first and reads content only when the size equals the target
  length, matching the omitted-regular branch's established size-first
  rule. The mechanical class sweep found no other production
  `blobContent` caller. Test `TestImportSymlinkSizeMismatchIsChanged`
  constructs a large mode-120000 base object without checking it out and
  pins the changed, publish-blocking result.

## Human follow-up after round 16 (two fixed; four non-findings verified)

- **One invalid UTF-8 byte silently disabled the secret scan (P2).** The
  scanner read and re-hashed an under-cap candidate blob, then skipped it
  without a finding when `utf8.Valid` failed. Prefixing an otherwise
  textual token file with one invalid byte therefore produced a
  findings-free import. Refined the earlier "UTF-8 content" scope decision:
  the six narrow ASCII RE2 rules now run over the bounded raw bytes. This
  closes the evasion without treating every binary add as an unscanned-file
  finding; arbitrary binary content matches only the same high-signal token
  structures. Test `TestImportSecretScanInvalidUTF8`.
- **Three-way collision evidence selected a random partner (P3).** The
  post-import path set began with randomized base-map iteration, while each
  fold retained only its first two owners. When two base paths and one add
  shared a fold, the finding's `Detail` could name a different partner on
  repeated imports. The path set is now bytewise sorted before owner
  retention, making the evidence deterministic. Test
  `TestDetectCollisionsDeterministicPartner` repeats the three-way case and
  pins the lexicographically first partner.
- **Verified non-findings.** Commit messages come only from the
  daemon-supplied `Options.CommitMessage`, never the manifest; every secret
  expression uses Go's linear-time RE2 engine; Git replacement objects,
  system/user config, signing, and commit encoding are all explicitly
  neutralized or pinned; mandatory path matching applies alias
  normalization plus NFC/full-fold to both the pattern and candidate path.
  No change retained for these four checks.

## Round 17 (Codex pass 17, two findings, fixed)

- **Duplicate digest references multiplied secret-scan work (P2).** A
  hostile manifest could point many added paths at one under-cap digest;
  blob verification deduplicated its bytes, but secret scanning reopened,
  re-hashed, and regex-scanned the private snapshot once per path. The scan
  now caches path-independent rule/line matches by digest and replays copies
  with each candidate path, bounding content work to unique stored blobs
  while preserving one located finding per path. Test
  `TestImportSecretScanReplaysDeduplicatedBlobFindings`.
- **Lossy symlink targets could falsely compare unchanged (P2).** The v1
  export contract records a symlink target as a best-effort JSON string;
  invalid UTF-8 is rendered as U+FFFD. An invalid-byte workspace target
  could therefore compare equal to a base target containing a literal
  replacement character, suppressing the non-regular finding. A manifest
  target containing U+FFFD is now treated as ambiguous and never used as
  unchanged-identity evidence; this can conservatively flag an unchanged
  literal-U+FFFD target, but cannot silently discard a changed non-regular
  entry. Test `TestImportLossySymlinkTargetNeverElides` exercises the real
  export/JSON/import path.

## Revisit when

- The verifier (#75) or the daemon integration wires the importer into a
  process boundary and needs the `cmd/` worker wrapper.
- The plan decides control characters in canonical paths (`\n`/`\t`, which
  `fs.ValidPath` permits) warrant a policy class; today they commit
  unflagged but inert.
- A real repository or a later export widening stresses the manifest
  contract in a way this first consumer did not.
