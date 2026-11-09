# macOS Verification Slots Launch Through a Sudo Helper and Prove Absence by Uid

Work unit: #2877. Mandatory note: Tier A (`internal/macverify/` runs
verification, and its helper deletes and resets as root). The owner chose the
launch path at planning on 2026-11-09; this note records it with what the
implementation settled.

## Decision

- **Chose a root-owned helper behind one sudoers rule over a LaunchDaemon
  broker.** `freesided` runs as the operator's LaunchAgent, so it cannot
  change uid, chown into a slot, or run `launchctl asuser`. A broker is a
  long-running root process with its own socket protocol, and its launch
  context was never spiked. `slot-run` is a short-lived script that launches
  with `launchctl asuser <uid> sudo -u <slot>`, the path spike 1 proved free
  of the operator's window-server session
  (`2026-10-22-1205-macos-verifier-executor.md`).
- **The helper decides which accounts it touches.** It reads `slots.conf`
  beside it and refuses any other name, root, the calling user, an
  administrator, and an account whose directory-service home is not
  `<root>/homes/<slot>`. The executor root is the helper's own grandparent
  directory, never an argument, and every directory from the helper up to `/`
  must be root-owned and not group- or world-writable. That last check is
  what defeats a hard link to the helper from a directory the operator owns:
  sudo matches a command by basename and inode, so such a link passes the
  sudoers rule and would otherwise bring its own `slots.conf`.
- **The daemon checks the helper's bytes, not a configured digest.** The
  room refuses a job unless the installed helper is root-owned, not group- or
  world-writable, and byte-identical to the embedded copy. The daemon binary
  is already in the configuration digest, so `Config` gained no field.
- **Root never reads for the caller or writes for the slot.** `prepare`
  copies the workspace with `tar` created as the calling user and extracted
  as the slot. Root copying the daemon-named directory itself would let the
  operator read any root-readable file through a slot. Before root deletes a
  slot-written tree, the slot clears its own immutable flags, so root never
  changes flags on a file the slot merely hard-linked.
- **Destructive work runs only while the slot owns no process.** `prepare`
  and `cleanup` refuse otherwise, so the slot cannot race root's `rm -rf`
  with a symlink swap.
- **Absence is proved by uid, not process group.** The daemon cannot signal
  the root `sudo` process or the slot's processes, so cancellation and the
  timeout call `reap`. Each slot runs one job at a time, so "the slot's uid,
  effective or real, owns no live process" is the absence proof, read by the
  daemon through the `kern.proc.uid` and `kern.proc.ruid` sysctls, which need
  no privilege. Unlike `ProcRoom`'s process-group reap, it catches a
  descendant that left its session. The proof runs after the reap and again
  after cleanup, since cleanup runs short-lived processes as the slot.
- **A slot without that proof is quarantined.** `Run` returns an error
  wrapping `ErrAbsenceUnproven`, and the pool never leases the slot again in
  this daemon process. After a restart, `prepare` refuses it until it is
  empty. The pool lives on the `Backend`, so every room over one
  configuration shares one lease and quarantine set.
- **A helper the daemon stopped waiting for quarantines without teardown.**
  The helper runs as root, so the daemon cannot stop it. When a canceled
  command's helper does not exit within the wait delay plus a grace, `Run`
  returns `ErrAbsenceUnproven` and skips teardown, since `cleanup` would race
  a live `run`. `prepare` runs under its own bounded context rather than the
  step's, so cancellation never abandons it mid-reset; a cancellation seen
  after it returns tears down normally and runs no command.
- **A refused run is told from the command by a reserved status.** Every
  failure in the helper before it hands over exits 120 (an `EXIT` trap maps
  any `set -e` failure there), and the daemon reads 120 from `run` as a room
  error. A command that itself exits 120 reads the same way. That is
  accepted: `prepare` has already passed every check `run` repeats, so the
  status is rare and costs only a room error, never a false verdict. After
  hand-over the helper has exec'd, so a failure in `launchctl`, `sudo`, or
  `sandbox-exec` itself (a missing `argv[0]` reads 71 or 127) reads as the
  command's exit status: a failed step, never a false pass.
- **`prepare` applies the profile once before the job.** It runs
  `sandbox-exec` with the job's profile and parameters on `/usr/bin/true` as
  the slot, so a profile the kernel rejects is a room error at `prepare`
  rather than the recipe command's exit 65.
- **The profile denies launchd job creation and Launch Services opens.** A
  job the command submits runs outside the sandbox under launchd and could
  outlive the job; a local check confirmed `launchctl submit` is refused
  under the profile.
- **The room renders the environment and sandbox parameters; the helper
  derives and compares them.** The daemon names the exact environment and
  `-D` values it expects, from what `prepare` reported, and the helper refuses
  the run unless they equal what it derives. The helper stays authoritative
  for every path it resets, and the daemon's record names the confinement.
- **Writes are confined to per-job state that is reset**, the plan's reading
  of §5.7: the job directory plus the slot's home, `DARWIN_USER_TEMP_DIR`,
  and `DARWIN_USER_CACHE_DIR`, each reset before and after every job.

## Findings

- **SwiftPM cannot nest its sandbox.** Under the default profile, `swift
  test` fails with `sandbox_apply: Operation not permitted` when SwiftPM
  sandboxes its manifest build; `swift test --disable-sandbox` passes. A
  recipe's SwiftPM commands must pass the flag.
- **Toolchains find the home through the directory service.** SwiftPM read
  `/Users/<operator>/Library` with `HOME` pointing elsewhere, so the slot
  account's `NFSHomeDirectory` must be the reset home, which the helper now
  checks.

## Refute-First Findings

A fresh-context reviewer read the helper, profile, and room against the
issue contract and tried to refute each gate. Fixed:

- **Blocking: an abandoned helper let the slot be released.** A canceled
  `prepare` or a `run` that outlived the grace went on to teardown and
  release while root could still be acting on the slot. Now quarantined
  without teardown, as above.
- **The profile allowed launchd jobs**, an escape from both the sandbox and
  the absence proof. Now denied.
- **A malformed profile read as the command's exit 65.** Now caught by the
  apply check in `prepare`.
- **The pool was per room**, so two rooms over one configuration could lease
  the same slot. Now on the `Backend`.
- **Locale-dependent tool output**, **an admin check that passed on a failed
  group lookup**, **a workdir existence check run as root** (an oracle for
  paths only root can see), and **stale job directories from a crashed
  daemon** were fixed in the helper: `LC_ALL=C`, fail-closed group lookup,
  the check runs as the caller, and `prepare` empties the slot's job area.

Declined:

- **ACLs on the helper or `slots.conf`.** Only root can set an ACL on a
  root-owned file, so the ownership and mode checks already cover them.
- **Unit tests of the helper's refusals.** They need root and slot accounts;
  the live suite carries them instead (`TestLiveHelperRefuses*`), and one
  unprivileged test proves the ancestor-ownership refusal.

## Revisit When

- The live suite shows `launchctl asuser` or `sudo` turning a signal death
  into an ordinary exit status, or a toolchain needs a writable path that
  cannot be reset between jobs.
- #2878 installs the executor, or a second Mac (#2161) moves the executor off
  the daemon host.
