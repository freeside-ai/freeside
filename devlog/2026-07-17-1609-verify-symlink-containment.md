# Clean verifier: containment against case-folding symlink escape

Work unit for #145 (gauntlet, kind:fix): close a workspace-escape in the
clean verifier's tree materialization (`daemon/internal/verify/materialize.go`)
on **case-insensitive filesystems** (macOS APFS default). A malformed tree
craftable with `git hash-object -t tree --literally` pairs a symlink entry
`link` (mode 120000, target an absolute path outside the workspace) with a
blob entry `LINK/pwned` (mode 100644). The exact-string prefix guard from
#140 (`rejectPrefixConflicts`) sees `link` and `LINK/pwned` as distinct, but
the runtime FS folds `LINK` onto `link`, so when the symlink is materialized
first, `os.MkdirAll(dest/LINK)` resolves the folded symlink and
`os.WriteFile(dest/LINK/pwned, …)` lands attacker-chosen bytes at an
attacker-chosen host path outside `dest`. Pre-existing since #137; #140 closed
only the case-sensitive / exact-string variant; surfaced by #140's mandatory
refute-first pass. Trust class: returned-object-trust-boundary +
path-materialization.

## Decisions

- **Chose per-component containment (`os.Root` bound to `dest`) over folding
  a normalization into the dedup key.** Every materialization write now goes
  through `os.OpenRoot(dest)` (`materialize` and `writeTreeEntry`), which
  refuses to traverse any symlink component whose target escapes the root and
  refuses absolute symlinks, per-component and independent of map-iteration
  order. Rationale (issue steer, owner-filed): a dedup-key fold covers only
  the one normalization it models, and APFS normalizes Unicode as well as
  ASCII case, so a fold would leave the NFC/NFD variant open. `os.Root` never
  lets the kernel silently resolve an escaping component, so it is robust
  beyond ASCII case by construction. It is also an established codebase idiom:
  the hostile importer already contains writes with `os.Root`
  (`internal/importer/blobs.go`). Rejected: making `rejectPrefixConflicts`
  case/Unicode-folding-aware.

- **Kept `rejectPrefixConflicts` unchanged (case-sensitive).** It stays as the
  cheap exact-string early guard that yields a clean `ErrMalformedTree` before
  any write for the case-sensitive variant; `os.Root` is the robust
  containment behind it. Deliberately not made folding-aware, per the decision
  above.

- **`root.Symlink` creates the symlink without validating its target.** Go's
  `os.Root.Symlink` does not reject an absolute or outside-tree target at
  creation, only later traversal through it fails closed. This is correct: a
  legitimate committed symlink may point outside the tree, and it must still
  materialize; the escape is a later *write that traverses* it, which `os.Root`
  refuses. Preserves `TestMaterializePreservesSymlinks`.

- **Left the `verifyMaterialized` read path unchanged (considered, declined).**
  The escape is a *write*. `materializedBlobOID` lstat's before any read and
  rejects a symlink for a regular entry (never follows a symlink to read
  external content for OID), and `walkForStrays` uses `WalkDir` (lstat, no
  follow). Routing reads through `os.Root` too would only add churn for no
  additional containment, so the diff stays scoped to the write path.

## Tradeoff recorded

With `os.Root` the harmless in-`dest` `link` symlink is written before the
`LINK/pwned` write is refused, so the fix is not literally "before any
host-filesystem write" for the case-insensitive variant — but nothing lands
**outside** `dest` (the security boundary the acceptance asserts), and `dest`
is `RemoveAll`'d on the next attempt. This is the explicit, accepted
consequence of preferring traversal-safe writes over an up-front normalization
fold.

## Refute-first verification

An independent lens (fresh-context subagent, prompted only to *disprove*
containment) built a throwaway harness driving `materialize` 64× per vector
against pre-existing external target dirs, asserting no out-of-tree artifact.

- **Confirmed the fix holds (all REFUTED):** Unicode NFC/NFD fold
  (`café` U+00E9 symlink + `café` U+0301 blob), relative escaping symlink
  (`../escape`), deep prefix chain (`a/b/link` + `a/b/LINK/pwned`), and a
  double-symlink fold (`LINK/sneak` symlink). None escaped across 64 iterations
  each; APFS case- and Unicode-folding were verified live.
- **Harness soundness proven:** with the fix stashed, each soundness-bearing
  vector escaped on the first iteration (wrote bytes outside `dest`), stopped
  solely by `os.OpenRoot` — `rejectPrefixConflicts`'s exact-string compare let
  every folded vector past. This attributes the containment to `os.Root`, not
  an incidental guard.
- **Not soundness-bearing (still refuted):** the gitlink-fold vector cannot
  escape by construction (`MkdirAll` of a dangling external target is EEXIST;
  of an existing dir is a no-op creating no new artifact).

Regression: `TestMaterializeRejectsCaseFoldingSymlinkCollision` loop-drives the
`link`+`LINK/pwned` tree with a pre-existing external target dir, asserts no
out-of-tree write on every FS, and additionally asserts a containment error on
a detected case-insensitive FS. Verified to fail on the unfixed code (escapes
on iteration 0) and pass with the fix.

## Revisit when

A future Go release changes `os.Root` traversal semantics (e.g. it starts
following in-root symlinks that then escape, or relaxes the absolute-symlink
refusal), or the materializer gains a write path that bypasses the bound
`os.Root` — either would reopen the escape and needs the refute harness re-run.
