# macOS Executor Reference-Host Fixes

Decision record for #3009: the first real install and run of the macOS
verification executor (macOS 27.2, Xcode 27B52) hit defects in install,
the pre-job probe, and slot cleanup. The note is mandatory because
`daemon/internal/macverify/` is Tier A (the host verification executor).
Three of its choices reject a reasonable option. The fourth replaces the
issue contract's default after the host showed that default was false; the
owner chose the replacement in the implementing session. The fifth section
records what the fix doesn't cover.

## Install Waits for the Boot Job Instead of Retrying `pfctl`

Chose to wait, bounded at 30 seconds, after bootstrapping the boot job until
`launchctl print` shows its first run finished, and to fail install when
that run exits nonzero. Rejected retrying `pfctl -f` on `Resource busy`: a
retry still races the job's own load, and it hides a boot job that fails,
which is the job that loads the rules at every boot. `launchctl print`'s
format is undocumented, so install reads only the job's top-level `state`,
`runs`, and `last exit code` lines (the coalition blocks nested in the
output carry their own `state` lines), and treats anything it can't read
as an error, never as a finished run.

## The Pre-Job Probe Drops the Operator's Home

Chose to prove write confinement with `/private/tmp` and `/Users/Shared`
alone. Only the sandbox can refuse a slot's write to a world-writable
directory, so `EPERM` there shows the sandbox. The operator's home showed
nothing more: file permissions already refuse the slot there, as `EACCES`
under the common `0750` home, which failed every job. Rejected accepting
`EACCES` for the home only: that keeps a check that can't tell the sandbox
from the home's mode.

## Cleanup Empties the Protected Temp Entries and Proves Them Empty

The contract's default was to leave the entries macOS protects in a slot's
`DARWIN_USER_TEMP_DIR` and prove the slot can't write inside them. The
reference host showed the slot can. After one job running `swift-format`,
`swift test`, and `xcodebuild` on each slot, four entries survived root's
`rm`, on both slots:

| Entry | Protection | Slot writes inside | Listable | Removable |
| --- | --- | --- | --- | --- |
| `com.apple.trustd` | SIP (`com.apple.rootless`, `sunlnk`) | yes | yes | no |
| `com.apple.MTLCompilerService` | SIP (`com.apple.rootless`, `sunlnk`) | yes | yes | no |
| `com.apple.trustd/TemporaryItems` | none visible | yes | no, for root or slot | no, `rmdir` refused |
| `TemporaryItems` | `com.apple.quarantine` | yes | no, for root or slot | no, `rmdir` refused |

A file the slot wrote in either `TemporaryItems` read back in a later
command, so a job could leave state for the next. Root removed what the
slot wrote in the two listable entries, and the slot couldn't change their
mode. Each `TemporaryItems` directory's link count went 2, 3, 4 and back to
2 as a file and a directory were created and removed, so APFS counts every
entry, files included. The run's `DARWIN_USER_CACHE_DIR` held only
ordinary, removable entries.

The owner chose "empty, prove empty": `slot-run` deletes everything it can,
then accepts a survivor only when it is on that fixed list, is a directory,
and is empty, by its listing for the first two and by a link count of 2 for
each `TemporaryItems`. Anything else that survives, or anything left inside
them, fails `prepare` or `cleanup`, and the room quarantines the slot, so
nothing reaches the next job. The contract's acceptance line ("the slot
can't create a file anywhere inside it") becomes "every allowed survivor is
empty". Rejected keeping cleanup failing, which leaves a slot unusable after
any `xcrun`, `swift`, or `xcodebuild` job until a reinstall, and new
accounts per job, which the contract lists as a non-goal.

The listings in the new `slot-run` functions refuse when `find` fails,
because an empty listing from a failed `find` would read as empty. The
functions use only `ud_`-prefixed variables: `sh` has no locals, and the
first version overwrote `prepare`'s Swift cache `entry`, which the install
live suite caught. A helper test now fails when the functions change any
variable the rest of the script assigns.

## A Swift Job Can Still Quarantine a Slot

"Empty, prove empty" doesn't cover every Swift job. SwiftPM's atomic
writes go through the top-level `TemporaryItems`, and one `swift test` run
of three left an entry there (link count 3). No one can list or remove it,
so cleanup refused and `fvslot1` has been quarantined since. The other two
runs left it empty, and the slot took its next job. Two fixes were tried on
the host and rejected:

- **Denying the job writes into the four entries.** Cleanup then always
  passes, but `swift test` fails within 30 seconds ("unable to write
  manifest… Operation not permitted").
- **Retiring the whole temp directory.** Root would rename the slot's `T`
  into a root-only directory and let macOS make a fresh one. `T` itself
  carries `sunlnk` and `com.apple.rootless`, and root's rename failed with
  `Operation not permitted`.

So the contract's "takes its next job" line holds for `xcrun`, and for
`swift test` only when the run leaves `TemporaryItems` empty. That line
stays open on #3009. What remains untried: one account per job (a contract
non-goal), and whether a reboot clears a quarantined slot's entries.

The same runs explained why `swift test` exits 1 in the room at the Swift
live suite's pinned commit. The build succeeds and 1,869 of 1,872 tests
pass. Two `ComposerAttachmentIntakeTests` and one
`TaskPagePlaceHostedTests` test fail because they need a pasteboard or a
window server, which the room withholds on purpose. They pass outside the
room. The live suite can't pass until those tests or the recipe change,
which is outside `daemon/internal/macverify/`.

## Revisit When

- A macOS or Xcode update leaves a new entry in a slot's temp or cache
  directory. Cleanup then fails closed and quarantines the slot, and the
  list grows only with the same evidence: what protects the entry, and
  whether its emptiness can be proven.
- The executor runs on a volume that isn't APFS, where a directory's link
  count may not count files.
- trustd or Metal can be pointed away from the account's own temp
  directory, which would let the entries stay out of the slot's reach
  instead of being tolerated.
