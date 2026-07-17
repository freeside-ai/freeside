# Clean verifier: trusted recipes, head binding, and the verifier evidence channel

Work unit: #75 (gauntlet lane, 1A.1). Scope: `daemon/internal/verify`.

## Decisions

- **Recipe wire format is JSON, not the plan's §5.12 YAML config
  syntax.** The daemon carries no YAML dependency and the control-plane
  config format (`policy/`) is not yet initialized; adding a dependency
  for a one-file provisional format was rejected. Owner-confirmed
  2026-07-16. Revisit when `policy/` initializes its config format: the
  recipe should then adopt the control-plane's format and loader.
- **Recipe digest is sha256 over the exact trusted bytes as loaded,
  never a canonical re-encoding.** Approvals bind to byte digests
  (§5.12); re-encoding would let distinct byte forms alias one approved
  digest. Rejected alternative: digest over a normalized JSON encoding.
- **Commands are strict argv; no shell ever.** The wire command strings
  are whitespace-split and every shell metacharacter and control
  character is rejected at parse. Rejected alternative: `sh -c`
  execution, which would make the recipe a shell-injection surface and
  its behaviour host-dependent.
- **Divergence semantics**: a candidate-head copy of the recipe path
  that differs (modified, non-blob, oversized) is always flagged;
  absence is a divergence only for the base-commit source, since a
  config-sourced recipe need not exist in-tree. Execution proceeds on
  the trusted bytes in every case; the finding exists to make the
  attempted swap visible, not to block (§5.6 flags, the publication
  gate gates).
- **The verifier consumes the importer's audited change account
  (`[]importer.Change`) instead of re-deriving the diff.** One audited
  derivation (the importer's, cross-checked against the built tree)
  stays the single source of truth; a second derivation could disagree
  and the disagreement would itself need adjudication. Rejected
  alternative: verifier-side diff-tree.
- **Verification-control class covers Go module pins, build
  entrypoints, lint config, and the recipe path; widen-only.**
  go.mod/go.sum flagged despite routine-work noise (owner-confirmed
  2026-07-16): the flag is non-blocking and dependency swapping is the
  `test: @echo passed` attack via modules. CI and reviewer-instruction
  paths are deliberately not duplicated: they are the importer's
  publish-blocking classes.
- **Hardened git runner and the glob/alias matcher are package-local
  copies of the importer's.** Both are unexported there and
  shared-package edits are outside this unit's scope; the copies must
  stay decision-identical. The runner copy drops the importer's
  commit-reproducibility pins (identity, dates, i18n.commitEncoding,
  commit.gpgsign) because the verifier authors no commits. Revisit
  when a third consumer of either appears: promote a shared internal
  helper as its own contract unit.
- **Evidence channel under capture "none" is exactly the verifier's own
  account**: a canonical timestamp-free JSON report and a bounded
  transcript, both stamped through `domain.NewArtifact` with verifier
  provenance (invocation, head-bound head, recipe digest),
  digest-derived IDs. The verifier never scans the workspace for
  evidence files, which is what makes "agent/candidate files cannot
  enter evidence" structural rather than filtered.
- **Fail-fast command execution**: a non-zero exit (including a
  timeout kill, surfaced as exit −1) fails verification and later
  commands do not run. Rejected alternative: run-all-and-report, more
  evidence per run but pays full recipe cost on already-failed heads.
- **cmd/ out-of-process worker wrapper re-deferred** (same grounds as
  the importer note): every acceptance criterion is package-testable
  and the issue's declared path is exactly `daemon/internal/verify`.
  Revisit when the daemon integration unit wires the gauntlet
  end-to-end; the wrapper belongs there.

## Named residual (§5.6)

Candidate test code executes inside the warded verifier: `go test`
runs the candidate's test functions by design. Containment is the
room's job. In this unit the room is a **process-level fake**
(`ProcRoom`), an explicitly weaker isolation class than the ward's
room: it scrubs the child environment (allowlist of PATH, scratch
HOME, LC_ALL) but cannot deny network or filesystem access. Its doc
comment says so; it is for tests and bring-up, never a silent
substitute where ward isolation is required (§5.7 no-silent-downgrade).
Real-room integration is 1A.1 exit work with #76.

## Refute-first verification pass

Two fresh adversarial lenses (trust boundaries; execution and matching
parity) ran over the package before handoff, prompted to disprove the
acceptance properties. Dispositions:

**Confirmed and fixed:**

- An in-tree `.gitattributes` (ident, text/eol, filter) made
  `checkout-index` write bytes other than the committed blob's, and the
  read-tree/write-tree cross-check could not see it (probe also showed
  the cross-check satisfiable via git's cache-tree for a malformed
  tree). Fixed with three layers: `GIT_ATTR_SOURCE` pinned to the empty
  tree plus `core.autocrlf/eol` pinned off, and `verifyMaterialized`
  byte-comparing every workspace entry against its blob object name
  (strays and gaps rejected), which fails closed even on a git without
  `GIT_ATTR_SOURCE`. `.gitattributes` joined the verification-control
  class.
- A grandchild holding the combined-output pipe kept `cmd.Run` blocked
  past the context kill (probe recorded a hung command as exit 0).
  Fixed: `WaitDelay` on both runners; a clean exit with force-closed
  pipes reads as a failed step, never passed.
- A `cat-file -t` failure was classified as path-absent, silently
  suppressing the config-source divergence flag on a transient error.
  Fixed: recipe reads go through `ls-tree`, which distinguishes genuine
  absence from failure; plumbing errors now propagate in every
  direction, and a symlinked recipe path is non-regular rather than
  read as target text.
- Transcript-cap truncation was unmarked (`transcript_truncated` added
  to report and result); the recipe path could carry glob
  metacharacters into the pattern set where it would fail open
  (validation now rejects them).

**Rejected by verification (properties that held):** candidate bytes
steering execution (all recipe sources trusted; replace objects
ignored; spec composition safe); head binding (full-SHA rev-parse
equality, sha1 format enforced); evidence forgery (publish_eligible
unreachable from input; IDs type-prefixed; workspace files never
gathered); environment scrubbing (allowlist env, no credential-helper
invocation); glob/alias matching decision-identity with the importer;
shell-metacharacter rejection as defense-in-depth over a no-shell
execution path.

**Accepted by decision:** ProcRoom cannot deny network or filesystem
access (the documented weaker isolation class; ward owns real denial);
`checkout-index` never writes through a symlink to escape the
workspace because an index cannot hold both a symlink and a path
beneath it, backed by protectHFS/NTFS and the stray walk.

## Automated review (Codex)

Many findings across a long review series (the first eight passes plus
several more after each base rebase), all confirmed (several by probe)
and fixed in their home commits, or declined with reasons (below).

Two P1s. First, evidence artifact IDs were purely content-derived, so
two runs emitting byte-identical content (a quiet transcript) with
differing provenance would collide on an ID the store persists
immutably, making the later run's evidence unstorable; identity is now
type + invocation + content digest, keeping runs distinct and
same-invocation replays idempotent. Second, a multi-command recipe
shared one workspace, so an earlier command's candidate code (`go
test` running the candidate's tests) could rewrite files a later
command reads while evidence still claimed the head; each command now
runs in its own freshly materialized workspace (clean-room, so recipe
commands cannot pass workspace state between one another).

The P2s, by area:

- **Descendant processes**: ProcRoom reaped only the direct child, so a
  candidate-spawned background process could outlive the step on the
  host (now: own process group, group-killed on cancel and reaped
  unconditionally).
- **Materialization runs no host code** (P1): `git checkout-index`
  runs any smudge/clean filter the checkout's attributes/config define,
  executing host code outside the Room during materialization (and
  `GIT_ATTR_SOURCE` does not suppress `.git/info/attributes`). Not
  candidate-reachable in the threat model (the filter definition is in
  the daemon-owned `.git/config`, and the candidate's in-tree
  attributes are neutralized), but the clean-room guarantee should be
  structural: the workspace is now materialized by extracting each blob
  with `cat-file` and writing it directly, which cannot run a filter,
  so materialization is pure data extraction. That rework itself
  introduced, and a follow-up pass caught, a path-escape: a malformed
  tree with a symlink `a` and a blob `a/b` would have the direct writer
  follow the symlink prefix and write outside the workspace, so
  materialize now rejects a tree where any entry is nested under
  another entry (ErrMalformedTree) before any write — which a
  well-formed tree never triggers.
- **Symlinked entrypoint prefixes**: the symlink-entrypoint check now
  rejects a symlink anywhere in a command path's prefix chain
  (`./run-check/verify.sh` with `run-check` -> `scripts`), not only the
  final component, since exec traverses either.
- **Stray workspace directories**: with per-command workspaces at
  predictable sibling paths, an earlier command could pre-create the
  next workspace (`mkdir workspace-1/extra`), checkout-index left the
  empty dir in place, and the stray-walk skipped directories, so a
  later command could observe `test -d extra`. Now materialize clears
  the destination before checkout (clean slate) and the stray-walk
  rejects any directory that is not a tree-entry ancestor or a gitlink
  placeholder, making verifyMaterialized an exact tree match including
  directories.
- **Materialized shape** (one class, widened over three passes): the
  workspace byte check compared blob content but not entry shape, so
  under `core.symlinks=false` a symlink materialized as a plain file
  passed, a 100755/100644 executable-bit mismatch passed the same way,
  and a gitlink's empty-directory shape (clone parity) was unpinned
  (now: on-disk shape must match the tree mode across the full
  enumeration git expresses, fail closed).
- **Base binding**: `ls-tree` accepts any tree-ish, so a tree object
  passed as the base would serve as a recipe source while the report
  claimed it as base_sha (now: the base binding is enforced like the
  head's, ErrBaseMismatch).
- **Recipe parsing**: a stray trailing closing delimiter (`{...}]`)
  slipped past `json.Decoder.More()` at top level (now: a second decode
  must return io.EOF, with the trailing-byte input space enumerated as
  tests).
- **Command entrypoints as control paths** (the class recurred over
  several passes as I pattern-widened; closed structurally): a recipe
  running a repo-local script (`./scripts/verify.sh`) executes a
  candidate-tamperable file that must be in the verification-control
  class. The root fix: `Recipe.CommandPaths` derives the repo-relative
  files the commands reference (excluding only a `...` package-pattern
  *segment*), and `flagControlPaths` matches them by **exact
  case/normalization fold against the canonical change path** — not
  through the importer's glob/alias machinery, which is for
  protected-name *patterns* and mangled entrypoint filenames one
  character class at a time (unclean `./`, glob metacharacters, three
  embedded dots, a colon). The entrypoint-filename input space is now
  an adversarial enumeration run once as tests, not a widening of the
  cited pattern. One further entrypoint case is handled by failing
  closed rather than by more matching: a **symlink** entrypoint
  (`./run-check` → `scripts/verify.sh`) is executed by following the
  link, so the target is the real control surface, not the lexical
  name; resolving symlink chains in the tree is disproportionate for a
  trusted recipe, so a symlink command entrypoint is rejected
  (ErrSymlinkEntrypoint) and the recipe must name the target directly.

Declined (out of scope, captured elsewhere):

- **Non-Go recipes** (a ninth pass, after the first handoff): an
  argv-array wire form (for space-containing args like xcodebuild's
  `-destination`) and Swift/Xcode control-file patterns. Both are
  app-lane verification, not this Go-first unit (§11); deferred to
  #140, with reasons on the threads.
- **Reaping setsid/daemonized descendants** (after the rebase onto
  #129): a candidate that escapes its process group with a new session
  survives ProcRoom's group reap. Containing an escaping descendant
  needs a PID-namespace/cgroup reaper, which is definitionally the
  ward room's job (§5.7), not a process-level fake's; the Room
  interface exists to swap the ward room in (its backend landed as
  #129). ProcRoom's doc now names this specific escape rather than
  leaving it implicit in "weaker isolation class".

## Revisit when ...

- `policy/` initializes its config format → recipe format and loader.
- A third consumer of the hardened git runner or the glob/alias
  matcher appears → promote a shared helper (contract unit).
- The daemon integration unit wires importer → verifier → publisher →
  the out-of-process worker wrapper and the real ward room replace
  ProcRoom.
