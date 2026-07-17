# Persist the ntfy topic key across daemon restarts (issue #133)

Follows up #131's refute-first finding
(`devlog/2026-07-16-1819-pairing-ntfy-subscription.md`): the
`freeside-signet-dev` composition minted a fresh 32-byte topic key on every
process start, so reusing its SQLite store across a restart stranded every
already-paired device on a topic the daemon no longer publishes to (topics are
`HMAC-SHA256(topicKey, deviceID)`, `daemon/internal/signet/ntfy.go`).

## Where the key lives: a credential file, not a store row

The key is daemon-held secret material that derives *every* device's private
capability topic, and issue #133 criterion 2 requires it to stay out of
backups and workspace mounts. In this codebase backup exclusion is achieved by
*physical separation from the store file*: the SQLite store under `-db` is the
checkpoint/backup and workspace-mount surface (plan §5.10, §5.4), and it is why
the GitHub App private key lives in a disjoint credentials dir, not in the
store (`daemon/internal/publish/keystore.go`). So the key goes in its own 0600
file addressed by a new `-topic-key-file`, disjoint from `-db`. This holds on
the merits independent of lane discipline: even owning the store outright,
putting a derive-all-topics secret inside the backup surface would violate
criterion 2, and the key needs real secret entropy that can't be derived from
public device IDs, so *some* persistent secret file is unavoidable.

The hardened file pattern (0600 + `O_EXCL` create + fsync file and dir, perm
and symlink re-assertion on every load, fail-closed sentinels that never embed
key bytes) is **mirrored** from `publish.Keystore`, not imported: signet
already mirrors publish's `Secret` for the same reason
(`daemon/internal/signet/secret.go:11-18`) — the discipline is shared, the
lane is not, and a cross-lane import would couple signet releases to publish.

## Fail-closed state table (criterion 3)

`loadOrCreateTopicKey(path, storePreexisting)`:

| State | Disposition |
|-------|-------------|
| present, private, exactly 32 bytes | load |
| absent + store never opened | mint + persist (genuine first run) |
| absent + store pre-existing | **fail closed** (would silently rekey) |
| present but symlink/dir/widened/short/long | **fail closed** |

The "may already hold paired devices" signal is `os.Stat(cfg.DBPath)` sampled
*before* `store.Open` (which creates the file). Rejected alternative: a real
`store.ReadTx.CountDevices`. It gives exact semantics but touches spine-owned
`daemon/internal/store`, turning a single-directory harness fix into a
`kind:contract` unit (serialized, own PR, consumer adapter). The `os.Stat`
proxy cannot *under*-report (devices live inside the very file it stats), only
over-refuse a pre-existing-but-empty store, which fails safe. Decision: ship
the proxy; file the `CountDevices` contract issue only if the over-refusal
ever actually bites. The composition also refuses `-topic-key-file` unset
against a pre-existing store, so criterion 3 holds even without the flag; an
unset flag against a fresh store keeps the historical per-process key (the #72
convergence suite uses a fresh temp store per run and stays green).

## Refute-first verification

Credential-leak + fail-closed lens, fresh context, given only the diff and the
stated intent, prompted to disprove each guarantee.

- **No blockers.** Rejected-by-verification (attempted and could not
  construct): key leak into logs/errors/readiness/API/store (errors reference
  only path, mode, length); a silent rekey via the `os.Stat` proxy (it cannot
  under-report — devices live in the very file it stats; over-refusal fails
  safe); an `O_EXCL` clobber or concurrent-create race (loser gets `EEXIST`,
  fails closed); a vacuous restart test (delivery submit asserts 200 + exactly
  one publish to the HMAC-derived grant topic; a dead device would 404/503);
  backward-incompat with the #72 convergence path (fresh store per run, no
  flag → ephemeral, unchanged); scope creep (diff touches only
  `daemon/cmd/freeside-signet-dev`).
- **Confirmed and fixed:** a TOCTOU window between `Lstat` and `ReadFile`
  (a symlink swapped in after the kind/perm check could redirect the read).
  Closed by opening with `O_NOFOLLOW` and asserting kind/perm from the
  descriptor's `fstat` before reading; `ELOOP` maps to the permissions
  sentinel. This is stricter than the keystore pattern it mirrors.
- **Accepted by decision:** a `0000`-mode key file passes the `&0o077` perm
  gate (no group/other bits) and instead fails one step later at open/read
  with `EACCES`. Still fail-closed on a credential we cannot read; no special
  case added.

## Automated review (Codex)

Two confirmed findings, both fixed by folding into the original commit:

- **P1 — key path disjointness was documented, not enforced.** The flag help
  and note said the key must sit outside `-db`/`.blobs`, but nothing rejected a
  path inside them; a digest-shaped name under the blob tree could even be
  served by the attachment handler. Added `ensureTopicKeyDisjoint`: the key
  path may not coincide with the store file or its SQLite sidecars, nor land
  within the `.blobs` tree. Swept the whole store surface, not just the cited
  `.blobs`.
- **P2 — a FIFO at the key path blocked the open forever** before `fstat`
  could reject the non-regular file, hanging startup instead of failing
  closed. Added `O_NONBLOCK` to the load open (a no-op on the regular file we
  expect), so a FIFO/device returns immediately and is rejected as
  non-regular.
- **P1 round 2 — the disjointness check was lexical**, so a symlinked parent
  directory (or a case-only alias on macOS APFS) reaching the blob store
  bypassed it. Widened to the proven keystore approach: resolve each path
  through its deepest existing ancestor (`resolveExisting`, mirrored from
  `publish.Keystore`) and case-fold before comparing. This is a class
  recurrence, so a second refute pass was spent on the resolution logic before
  re-handoff (below).

- **P1 round 3 — SQLite sidecars sit beside the resolved db target**, not the
  raw `-db` symlink name, so the raw-only sidecar loop missed them. Made the
  fix terminal for the class: `storeSurface` now derives every store path (db,
  `-wal`/`-shm`/`-journal`, `.blobs`) two ways and unions them: from the raw
  strings the composition passes and from the symlink-resolved database target.
  This closes the raw-vs-resolved ambiguity by construction; over-inclusion is
  the fail-closed direction. Regression `TestResolveTopicKeyRejectsResolvedSidecars`.

- **P2 round 4 — the load read was unbounded.** `io.ReadAll` slurped an
  oversized private file whole before the length check could reject it. Now the
  descriptor's `fstat` size gates before reading and the read is bounded to
  exactly `topicKeyLen` via `io.ReadFull`. Regression extends the wrong-length
  test with one-over and 8 MiB cases.

- **P2 round 5 (two).** (a) `store.Open` created/migrated a fresh db *before*
  `resolveTopicKey` ran, so a bad `-topic-key-file` left a store behind that
  flipped `storePreexisting` true on the corrected retry and refused the still
  absent key — a stranded, delete-to-recover setup. Moved topic-key resolution
  ahead of `store.Open`, so a bad key path fails before any store mutation
  (`TestRunWithBadTopicKeyLeavesNoStore`). (b) `storeSurface` added
  `resolvedDB + suffix` without resolving that combined path, so a sidecar that
  is itself a symlink onto the key evaded the gate; now every candidate
  (raw and resolved-derived) is run through `resolveExisting`
  (`TestResolveTopicKeyRejectsSidecarSymlink`).

- **P2 round 6 — dangling store-surface symlink.** `resolveExisting` treated a
  dangling symlink (existing link, missing target) as an absent component and
  rejoined it lexically, so a store sidecar dangling onto the not-yet-created
  key (`db-wal -> creds/topic.key`) passed disjointness and `createTopicKey`
  would then materialize the target, aliasing the sidecar onto the key. Fixed
  at the source: `resolveExisting` now `Lstat`s an `ErrNotExist` component and
  fails closed when it exists but will not resolve. This also promotes the
  earlier dangling-symlink item from accepted-by-decision to fully handled, on
  both the key and store-surface sides. Regression
  `TestResolveTopicKeyRejectsDanglingStoreSymlink`.

- **P1 round 7 — hard-linked key.** A distinct aliasing class no path check
  can see: an existing 0600 regular key file can carry a second hard link under
  the blob tree (a digest-shaped name the attachment handler serves), placing
  the same key bytes on the store surface while `-topic-key-file` points
  outside it. `loadTopicKey` now rejects a loaded key whose inode has more than
  one link (`st.Nlink > 1`): a credential file must have exactly one name.
  Regression `TestLoadOrCreateTopicKeyStates/a_hard-linked_key_fails_closed`.

### Refute pass on the path-custody resolution

Fresh-context lens over `ensureTopicKeyDisjoint` / `resolveExisting` /
`within`, prompted to reach the store surface anyway. It found and I fixed one
**blocker**: the check computed the blob tree as `resolveExisting(dbPath) +
".blobs"` (resolve, then append), while `main.go` opens the blob store from the
raw `dbPath + ".blobs"` string; when `-db` is a symlink these diverge, so a key
in the real blob tree passed. Fixed by resolving each concrete store path
(db, sidecars, `.blobs`) as a whole string, exactly as the composition opens
it. Regression `TestResolveTopicKeyResolvesSymlinkedDB` (proven non-vacuous
against the old logic). The dangling-symlink case this pass first noted as
accepted-by-decision was later closed outright (round 6 above). One residual
remains accepted by decision: an ancestor-directory swap between the
disjointness check and the open is a TOCTOU outside the operator-owned
dev-harness threat model, matching the residual the mirrored keystore
acknowledges.

## Revisit when

The product daemon `freesided` (plan §10) gains its own credential management:
durable topic-key custody, and any real rotation/re-keying UX, belong there;
this harness helper is the stopgap for the component that exhibits the bug
today. Also revisit if `store.CountDevices` ever lands for another reason, at
which point the `os.Stat` proxy can be replaced with an exact check.
