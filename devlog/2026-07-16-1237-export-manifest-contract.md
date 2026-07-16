# Export manifest contract v1 and the trusted helper's shape

Gauntlet unit #73 (trusted export helper). Basis: plan §5.6 (two
channels, regular-files-only manifest, importer never trusts workspace
state), the workspace-handoff spike's Required backend contract check
6, and the #73/#76 seam declared at Wave 1 planning
(2026-07-14-2113-wave1-planning.md). This note records the wire-format
decisions the manifest+blob contract bakes in; the importer (#74) is
their consumer.

## Decisions

- **Full-snapshot manifest, not a change manifest.** The helper emits
  every entry it sees; the importer derives the change set against the
  exact trusted base SHA. Rejected: diffing in the helper. The only
  base state visible inside the exporter VM is the workspace's own
  `.git`, which §5.6 forbids trusting, and the VM has no network to
  fetch a trusted base; a helper-side diff would launder untrusted
  parentage into "changes". The workspace-copy cost this implies was
  already accepted at planning (first-repository record, revisit
  conditions there).
- **The workspace `.git` is one `git_dir` entry, never walked.**
  Directory or linked-worktree file form. Rejected: omitting it
  silently (the manifest should say it saw one) and blobbing it
  (importer must never receive workspace git state as content).
- **Submodules: record, don't descend.** A non-root directory carrying
  its own `.git` entry becomes one `submodule` entry with no digest;
  its content is another repo's working tree, and a pointer commit
  could only come from untrusted `.git`. Rejected: descending and
  blobbing the nested tree; the importer blocks submodule changes
  regardless, so the blobs would be dead weight collected from a
  hostile source.
- **Oversized files: digest and size recorded, blob omitted.** Above a
  flag-set cap (default 100MiB, pinned by the exporter image
  invocation so identical workspaces export identically), the entry
  sets `blob_omitted`. Owner decision this session. Rejected: always
  shipping blobs (unbounded exporter-rootfs growth under a hostile
  workspace, and the ward collects the whole rootfs via container
  export); the importer treats a needed-but-omitted blob as
  publish-blocking.
- **Aggregate blob budget beside the per-file cap** (Codex review
  finding, accepted): many under-cap files could still exhaust the
  exporter rootfs, so a second flag-set budget (default 1GiB) bounds
  total bytes written under `blobs/`, charged by bytes actually
  written; a dedup hit is free and never marked omitted. Entries are
  processed in manifest order, so which blobs a budget omits is
  deterministic. Rejected: relying on the ward's bounded workspace
  volume alone (the helper is a trust-boundary component and should
  not assume its caller's limits).
- **Entry cap fails closed; blob dedup trusts only this run's writes**
  (Codex round-2 findings, both accepted). Blobless entries (empty
  files, symlinks, invalid names) evade the blob budgets, so a
  flag-set entry cap (default 1,000,000) aborts the walk instead of
  accumulating an unbounded manifest; failing loud beats truncating,
  which would silently drop entries. And stored-ness is tracked in
  the run's own written set with the output directory required empty
  at start, so a stale or corrupt pre-existing path at a digest name
  can never satisfy a manifest entry; the collected output holds
  exactly manifest.json plus the blobs the manifest references.
- **Git mode normalization; special bits are their own kind.** Regular
  modes collapse to 0644/0755 on the owner-exec bit, like git's index.
  setuid/setgid/sticky files become `unusual_mode` entries recorded
  unnormalized and unblobbed, so the importer sees exactly the risk
  class §5.6 names publish-blocking.
- **Non-representable names get a lossless `path_hex` form.**
  encoding/json folds invalid UTF-8 to U+FFFD, so two distinct hostile
  names could collide in the manifest; `invalid_path` entries carry
  the raw bytes hex-encoded instead, and a directory of that kind is
  recorded but not descended. NUL joins non-UTF-8 in the same class
  (Codex review finding, accepted): `fs.ValidPath` and
  `utf8.ValidString` both accept it though no real path can carry it,
  so the shared canonical-path gate rejects it in the validator and
  the walker alike. Symlink *targets* stay best-effort strings: every
  symlink is publish-blocking downstream, so target bytes are
  informational, not identity.
- **Empty directories are unrepresentable**, as in git: directories
  are implied by their children. The importer must not expect them.

## Verification findings

- Determinism held by construction and by test: entries sorted by raw
  name bytes, no timestamps/uid/gid/host fields, double-build
  double-export compares manifest bytes and full blob trees.
- No-execution held twice: sentinel fixtures (hooks, fake `git`, an
  executable script) and a structural test that the package never
  imports os/exec.
- APFS rejects non-UTF-8 filenames, so hostile-name and device/socket
  coverage runs over fstest.MapFS fakes (ReadLinkFS-conformant since
  Go 1.25); real-filesystem coverage (symlinks, FIFO, setuid) runs in
  a temp dir. Device-node creation skips on EPERM everywhere
  unprivileged.
- Static linux/arm64 build verified locally (CGO_ENABLED=0; the module
  is pure Go). A CI cross-compile step is out of this unit's declared
  paths; deferral filed.

## Refute pass (trust boundary)

Codex serialized four review rounds; six findings confirmed and folded
into their commits: NUL path rejection, the aggregate blob budget, the
manifest entry cap, output-trust hardening (dedup trusts only this
run's writes; output must start empty), lstat-mode classification
authority (never DirEntry.Type()), and overflow-safe budget arithmetic.
Then three fresh-context refute lenses (execution/symlink safety,
resource exhaustion, manifest integrity), each tasked to disprove:

- **Confirmed by decision (documentation honesty, no code change):**
  the `MaxEntries` cap bounds entry *count*, not memory bytes; peak
  memory scales with total path-name length and doubles during Encode.
  Over-cap files are still fully streamed through the hash to record a
  digest, so bytes *read* equal the workspace size regardless of the
  blob caps. Both are bounded by the ward's bounded workspace volume
  (§5.7), not by these caps; the Options docs now state that honestly
  rather than claim the caps bound memory or read cost.
- **Rejected by verification (raised, then disproved; not re-open):**
  no execution or symlink-follow path exists (only lstat-proven regular
  files are opened; `.git`, submodules, symlinks, specials are recorded
  unopened); walkDir recursion is bounded by PATH_MAX, not MaxEntries,
  so deep nesting errors closed at ~2K frames; temp blobs are removed on
  every error path; manifest determinism, digest-to-blob binding, and
  `Validate` completeness all held (Path vs decoded-PathHex sort keys
  are provably disjoint, so no collision-DoS or ambiguous pass).
- **Accepted residual (already in-design):** a non-UTF-8 symlink target
  folds to U+FFFD under json, so two such targets collide in the encoded
  manifest. Accepted because every symlink is publish-blocking
  downstream, so the target is informational, never an importer
  identity; now pinned by a test so the behavior can't drift silently.

On failure the export returns before writing manifest.json, so `/handoff`
can hold committed blobs without a manifest; the exporter is single-shot
and destroyed, and the ward collects only a successful export, so the
partial output is discarded with the VM (noted for the #76 seam).

Revisit when: the importer (#74) lands and stresses the contract (any
gap becomes a v2 or an in-place widening while the format is still
consumer-free); or a real repository trips the 100MiB default cap in
practice; or a workspace filesystem yields paths valid per fs.ValidPath
but hostile in ways the canonical-path validation misses.
