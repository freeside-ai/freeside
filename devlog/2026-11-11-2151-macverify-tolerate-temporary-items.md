# Tolerate Leftover Entries in a Slot's Top-Level TemporaryItems

Work unit: #3009, its last open acceptance line ("a slot that has run
`swift test` takes its next job"). Mandatory note: Tier A by path
(`internal/macverify/`), and an owner decision that loosens a check the
previous note (`devlog/2026-11-10-1256-macverify-reference-host-fixes.md`)
set.

## Decision

The owner chose to keep a slot usable when a job leaves entries in the top
level of its `DARWIN_USER_TEMP_DIR/TemporaryItems`. `slot-run`'s reset
still requires that directory to be a real directory, not a symlink, but
no longer requires it to be empty. Every other rule stands: the two SIP
entries must be empty, trustd's `TemporaryItems` must have a link count of
2, a `TemporaryItems` in the cache directory must too, and any other
survivor fails the reset and quarantines the slot.

This replaces the wording the previous note gave #3009's acceptance line
("every allowed survivor is empty"): every allowed survivor but the
temporary directory's top-level `TemporaryItems` is empty.

## Why

- **The leak can't be cleared in place.** On the reference host on
  2026-11-11, `fvslot1` was quarantined again by a `swift test` job (link
  count 3). Neither root nor the slot could list or rename `TemporaryItems`,
  from a plain `sudo -u` or from the slot's own launchd context, and the
  unified log showed no sandbox report for the refusal.
- **Its parents can't be retired.** The bucket directory, the per-user
  directory, and `T` all carry `com.apple.rootless` and `sunlnk`, so moving
  any of them aside fails as the earlier rename of `T` did.
- **What the strict check protected is narrow.** `TemporaryItems` is
  Foundation's item-replacement directory, and Foundation names each entry
  at random. No one can list it, so a later job reaches an entry only by a
  name it already knows. An honest job can't pick up a planted entry; only
  two jobs written to cooperate can pass data through it. Every job on the
  executor is the operator's own.

## Rejected

- **Rotate the slot account's `GeneratedUID` on a leak,** so macOS derives
  a fresh per-user directory, and deny jobs reads of every other per-user
  `T` and `C`. It needs root to change a directory-service record on every
  leak plus a tighter sandbox profile, a lot of new Tier A machinery to
  close the same narrow channel. It was not tried on the host.
- **Keep quarantining.** A slot stays unusable after about one `swift test`
  run in three until a reinstall, so the `macos_host` app steps can't run.
- **A macOS VM per job,** which removes this class of problem by starting
  every job from a clean clone. It is the better long-term design, but it
  replaces the slot-account executor and is a plan-level change, so it is
  proposed separately as a spike.

## Risks and Revisit When

- **Disk growth.** An untrusted job can now leave data the reset can't
  remove, and nothing can measure it. Assumed, not verified: macOS's
  `dirhelper` cleans old per-user temporary entries at boot.
- **Naming assumption.** The safety argument relies on writers into the
  top-level `TemporaryItems` naming entries at random. Revisit if a tool is
  found that reads a fixed name there.
- **Guessing an honest job's leftover.** A later job could try to read an
  entry an honest job left by guessing its random name. That is only as
  hard as Foundation's random suffix is long, which was not measured.
- **Revisit** when the VM-per-job spike reports, or if trustd's
  `TemporaryItems` starts leaking too; it stays strict until then, since
  trustd's naming is unknown.
