# Verification Scratch Under the Daemon's Root

Chose to stage every verification's scratch under `<state dir>/verify-scratch`
over the daemon account's temporary directory and over the seed root
(#3549). The scratch holds each command's workspace after its exact-tree
check, and the room then copies it (the slot helper's `prepare`) or mounts it
(the ward's project-image room). A cordoned writer runs as the daemon's
account and must be able to write the temporary directory, because Swift's
tools fail without it (PR #3542). In the temporary directory, a writer could
change a staged file between the check and the copy, and the job would verify
bytes that aren't the candidate's.

## Decisions

- **The state dir, not the seed root.** The cordon rule file (#3466) denies
  writes under the daemon's root, and the state dir sits inside it:
  `<root>/daemon/` in the derived layouts, and the deputy's `-root` itself.
  The seed root is an operator setting (`seed_root`) that can sit outside
  the root, so it would protect nothing there.
- **Required, with no default.** `verify.Options.ScratchParent` and
  `engine.ProductionPublicationConfig.VerifyScratchDir` refuse an empty value,
  and the verifier also refuses a relative or unclean one. A silent fall back
  to the temporary directory is the exact failure this unit removes.
- **The startup sweep removes only `freeside-verify-*` entries.** Each
  verification removes its own scratch on return, but a stopped daemon can
  leave one behind. Building the production publication workflow sweeps the
  parent; composition runs under the daemon lock, before any verification
  starts. Removing the whole parent was rejected: a parent named by mistake
  would lose unrelated files. A prefixed symlink is removed, never followed,
  and a parent that is itself a symlink is refused.
- **The sweep is best effort, over failing startup.** An entry it can't
  remove is logged and left. Leftover scratch costs only disk: every
  verification stages in a fresh, uniquely named directory, so residue never
  reaches a later run. Failing startup instead would let one stuck directory
  stop the daemon until someone removed it by hand.
- **Removal restores owner bits and retries.** The ward's room mounts the
  workspace read-write, and candidate code (or a Go module cache kept in the
  repository) can leave a directory without its owner's write bit, which
  stops `os.RemoveAll`. Both the per-verification cleanup and the sweep
  chmod each real directory to 0700, never through a symlink, and retry.

## Refute-First Findings

Two rounds of fresh-context review tried to break the change; the second
found no blocker.

- **Confirmed and fixed: a read-only leftover would stop startup.** With a
  failing sweep, a `0555` directory a candidate left in its workspace made
  `RemoveAll` fail (reproduced) and the daemon refuse to start. Fixed by the
  two decisions above; tests leave a `0555` and a `0000` directory in a
  workspace and in a stale entry.
- **Confirmed, allowed: a crashed daemon's container may still write.** The
  sweep runs at composition, before recovery stops containers the stopped
  daemon left running, so one may still write into its workspace during the
  sweep. The best-effort sweep logs that entry and moves on; recovery stops
  the container. The chmod pass can then race a container that swaps a
  directory for a symlink, which at most gives the owner full bits on a
  directory the daemon's account already owns; accepted.
- **Confirmed, filed separately: the hard-link guard can skip a root.**
  `linkedUnderSupervisedRoot` (`cmd/freesided/environment.go`) treats any
  not-exist error from its walk as an absent root, so a verification
  workspace removed mid-walk skips the whole root's check. The guard
  predates this unit, but scratch churn under the root makes the race
  reachable. Follow-up: #3562 (outside this unit's paths).
- **Declined: refuse a parent containing `:`.** The ward's room refuses a
  workspace path with `:`, so such a state dir fails every ward
  verification. That check already fails closed and names the cause, and
  the verifier shouldn't encode one room's argv syntax.

## Limits

- **Protection depends on #3466.** Until its rule file denies writes under
  the daemon's root, the move is harmless and protects nothing. If #3466 names
  a narrower denied set, this parent moves with it.
- **A run recorded as uncordoned is unprotected.** No rule binds it (#3473,
  #3478).
- **An ephemeral daemon's state dir can sit anywhere,** the temporary
  directory included, so its scratch is protected only when its operator
  places it under a denied path.
- **Only the parent's last component is checked for a symlink.** A
  symlinked `<root>` or `<root>/daemon` would put the scratch outside the
  root. Both are operator-controlled; #3466's rule must match resolved
  paths anyway (`/var` is `/private/var` on macOS).
- **Other scratch stays in the temporary directory:** `MaterializeExact`
  (project-image source), `OpenCommitReader` (recipe reads), and the
  `macverify` cache populate. None stages a workspace a room copies.

Revisit when #3466 settles its denied paths, or when a cordoned verification
room is built and copies from this scratch.
