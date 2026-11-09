# The macOS Executor Installs Through freesided macverify, With a Boot LaunchDaemon for pf

Work unit: #2878. Mandatory note: Tier A (`internal/macverify/` creates and
deletes accounts and trees as root). The plan comment on #2878 proposed these
choices; this note records what the implementation settled.

## Decision

- **Chose a boot LaunchDaemon over editing Apple's pf files.** Stock macOS
  loads `/etc/pf.conf` at boot (`com.apple.pfctl` runs `pfctl -f` only) and
  never enables pf, and an anchor loaded by hand is gone after a reboot.
  Editing `/etc/pf.conf` or `/etc/pf.anchors/com.apple` would persist the
  anchor, but those files are Apple's: a system update can rewrite them, and
  removal would have to edit them back. A root-owned LaunchDaemon that runs
  `pfctl -E -a com.apple/<anchor> -f <root>/pf/anchor.conf` at load is a file
  install owns outright and removal deletes. The anchor file lives in the
  executor root, so the whole executor stays one tree.
- **Chose a `check` verb over `freesided doctor`.** `doctor` reads the store
  through `internal/operations` and forwards to a running daemon, and nothing
  in the daemon builds a macverify backend yet. `check` reads the host only,
  runs without root (reporting the sudoers drop-in and pf as `unread`), and
  exits nonzero on any missing, misconfigured, or foreign piece. Folding its
  findings into `doctor` waits until the daemon wires a `macverify.Config`.
- **The adoption rule is shape, not a marker.** An account is a slot install
  may adopt, repair, or delete only when its primary group id is the spec's
  and its home is `<root>/homes/<name>`; the group is install's only when it
  has the spec's id and every member and primary-group account is a slot. That
  adopts the hand setup `daemon/README.md` described before this unit and
  refuses a real user who shares a slot's name. A marker attribute was
  rejected: the hand setup has none, and a marker proves no more than the
  home path does, since only root can write either. Install never renumbers
  an account; a slot-shaped account with another uid refuses the install.
- **Removal matches content, not names.** The sudoers drop-in and the
  LaunchDaemon are deleted only when their bytes are what install writes (the
  sudoers match accepts any operator and the hand setup's header-less rule);
  the root only when it holds a `slot-run` helper and every ancestor passes
  install's check, so a mistyped `-root` names nothing to delete and no
  non-root user can swap a path above the deletion. Accounts come only from
  the root's `slots.conf` and must pass the adoption rule.
- **Every decision lives in `internal/macverify`.** `Install`, `Inspect`, and
  `Remove` act through a `Host` interface; `cmd/freesided/macverify.go` only
  parses flags and prints, so it stays off the Tier A list.
- **The install runs as one-shot `sudo freesided macverify ...`.** The daemon
  never holds root, which is how this unit reads plan §10's narrow elevation:
  no long-running privileged helper beyond `slot-run` itself.
- **One renderer for the anchor.** `PacketFilterRules` names accounts, not
  uids, so the bytes depend only on the slot set and the configuration digest
  stays stable across id choices. `InstallSpec.Config` hands the same bytes to
  `Config.PacketFilterRules`.

## Refute-First Findings

A fresh-context reviewer read the diff against the issue contract and tried
to refute each gate. Fixed:

- **Blocking: install adopted an existing tree as the root, and removal then
  deleted it.** `-root /usr/local` passed the ancestor check, install chowned
  it and wrote a helper, and removal's helper check then allowed
  `RemoveAll`. Now a tree directory owned by another user, or holding any
  entry install does not write, is foreign; removal deletes only install's
  named entries and removes the root only if that empties it.
- **Blocking: removal deleted any empty group it was named.** `remove -group
  everyone -first-id 12` would have deleted a stock group with no members.
  Now the group goes only when removal found a root-owned `slots.conf` or
  helper.
- **Removal flushed the anchor before stopping the slots,** opening the pf
  layer under a live recipe process; and it could delete accounts without
  flushing, leaving rules on a uid a later account reuses. Slots now stop
  first, and any deleted account forces the flush.
- **A stopped install could wedge.** A record `dscl` stopped writing made
  every later read fail, and accounts were created before `slots.conf`
  named them. An incomplete record (each of uid, gid, and home unset or
  install's) is now repaired, and the tree, helper, and `slots.conf` come
  before the accounts.
- **Check counted distinct rule keys,** so mixed name and uid forms could
  pass with each slot missing a direction. Rules are now normalized to uids.
- **An adopted slot in `admin`** was accepted; it is now foreign (slot-run
  also refuses it at run time).

Declined:

- **`check` exits 0 with pf unread when run without root.** The contract
  makes missing and misconfigured pieces fail; `unread` says what was not
  read, and `sudo freesided macverify check` reads it.
- **Checking that the main ruleset still has `anchor "com.apple/*"`.** The
  #2879 pre-job probe proves egress is blocked, which covers any cause.
- **Default ids 460 to 462 sit below 500.** They match the hand setup this
  install adopts, and a collision with any account or group is refused as
  foreign.
- **Removal leaves pf enabled and the slots' `/var/folders` directories.**
  Documented in `daemon/README.md`; pf runs only stock rules after the next
  reboot.

## Revisit When

- A reboot leaves the anchor unloaded or pf disabled, which would mean
  `com.apple.pfctl` runs after the boot job and flushes `com.apple` children,
  or a pf enable reference does not outlive `pfctl`.
- The daemon wires a `macverify.Config`, at which point `check`'s findings
  belong in `freesided doctor`.
- `slot-run.sh` changes its layout or account checks.
