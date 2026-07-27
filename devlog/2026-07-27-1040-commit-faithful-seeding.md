# Commit-Faithful Workspace Seeding

Work unit: #330. Mandatory note: this changes the seeded-workspace trust
boundary that decides whether pre-existing differences can become publishable
agent output.

## Decision

Chose a Git-backed read-only observer over a host-side Git check or a new tree
object parser. The existing observer image now carries a version-pinned Git
package. Ward resolves the detached `HEAD`, enumerates its tree, hashes each
mounted regular file as raw bytes without Git attribute filters, and compares
the blob identity and executable mode before the credential-bearing writer
starts. It also rejects every extra regular file.

The raw object comparison is load-bearing. Trusting the copied index would let
`assume-unchanged` or `skip-worktree` bits hide an edited file, while porcelain
status would apply `.gitattributes` clean or end-of-line conversions and could
accept worktree bytes that differ from the commit. Enumerating all regular
files independently also catches ignored paths, because a path absent from the
commit is still extra seed content even when `.gitignore` names it.

Every object lookup runs with replacement interpretation disabled. The
observer separately rejects replacement refs and legacy graft metadata, so
the writer cannot later see an object graph different from the one the gate
proved.

The existing host/observer digest remains a separate proof. Git binds the
worktree to the commit; the digest binds the read-only volume to the exact
snapshot the host approved. Neither substitutes for the other.

The exporter build assembles a private two-file context rather than placing the
generated helper beside the checked-in Containerfile. Apple `container` honors
the source context's ignore rule and otherwise omits that intentionally
untracked binary, producing a late `COPY` failure. The private context is
removed file-by-file and must be empty before `rmdir` succeeds.

## Rejected Alternatives

- Running Git on the host was rejected because it would check a mutable source
  path before the runtime copy, rather than the independent read-only volume
  the writer will receive. It would also introduce a second host command
  execution surface outside `CLIRuntime`.
- Trusting `git status` against the copied index was rejected because index
  flags can suppress worktree inspection and attributes can transform bytes
  before comparison.
- Reimplementing Git tree resolution in Go was rejected because an honest
  implementation must parse loose objects, packs, object-format extensions,
  file modes, and path ordering. The pinned Git implementation already owns
  those semantics.
- A separate observer-only image was rejected because ward already pins and
  allowlists the exporter image for the seeder and observer roles. Extending
  that generic internal image keeps one supply-chain identity and does not
  enter #334's project-image builder surface.

## Changed Assumption

#329 conservatively refused every source with no ordinary worktree file because
the observer could not distinguish an unmaterialized non-empty checkout from a
legitimately empty commit. Git now makes that distinction. The host permits the
candidate, and the observer accepts it only when the commit tree is actually
empty.

## Verification Findings

The executable Git comparison passed for a normal clean checkout and a
legitimately empty commit. It rejected an edited tracked file even after the
source index marked that path `assume-unchanged`, rejected a mode-only edit
under `core.filemode=false`, rejected worktree bytes hidden by end-of-line
attributes, rejected an extra ignored file, and rejected replacement-object
metadata. These last two cases were added after automated review identified
that semantic status and replacement-aware object resolution were weaker than
the raw commit identity the boundary claims. The strict proof parser still
rejects dirty or failed Git observations without echoing proof content. Shell
syntax and lint checks cover the private image-build context's cleanup path.
Linux CI additionally exposed that `read -d` is a BusyBox and Bash extension,
not POSIX shell. The tree parser now uses `xargs -0` batches, and its executable
test prefers `dash` when available so macOS cannot mask that portability
requirement.

A later review reproduced two more Git-plumbing traps. `hash-object
--stdin-paths` decodes quoted-looking input names, so hashes now receive literal
NUL-delimited paths as `argv` batches instead. Also, a raw `HEAD` holding an
annotated-tag object can peel to the expected commit while leaving the writer
unable to commit; the proof now requires the resolved commit identity to equal
the raw detached `HEAD`. Regression cases pin both behaviors, and a
network-disabled Alpine 3.22 smoke test confirmed its BusyBox `xargs` preserves
a quote-bearing NUL-delimited argument literally.

Revisit when ward stops reusing the exporter image for seeding roles, or when
the repository format expands beyond the Git object formats the pinned
observer supports.
