# Put the Version Managers' Directories on a Cordoned Run's PATH

Issue #3622. Tier A (runner containment). Implements plan §5.7, Tool
versions (revision 137), with the list the shared-host spike ran with
(#3596, `docs/spikes/cordoned-shared-host.md`, item 8).

## Decision

A cordoned launch's `PATH` is the pinned CLI's directory, then
`/opt/homebrew/bin`, `/opt/homebrew/opt/rustup/bin`, `~/.cargo/bin`,
`~/.local/bin`, and `~/.local/share/mise/shims` under the run account's
home, then `/usr/bin`, `/bin`, `/usr/sbin`, and `/sbin`, keeping the first
of any repeat. `cordon.LaunchPath` is the one definition, and
`daemoncompose.CordonConfig` calls it in place of its own system list.

## Why

- **One definition in `internal/cordon`, not a `daemoncompose`
  constant.** The daemon's backend builder and a host's launcher (#3623)
  both import `internal/cordon`, so no host can compose a different list,
  and the list sits beside the `Config.Path` it fills.
- **Entries are not resolved.** `/opt/homebrew/opt/rustup/bin` is a link
  Homebrew moves on every rustup upgrade. `Path` enters the configuration
  digest, so a resolved entry would change the digest, and force a new
  proof, with each upgrade. A directory a host lacks stays on the list and
  holds nothing, so every host's list is the same.
- **Freeside's own starts never resolve through this `PATH`.** A run can
  write `~/.cargo/bin`, `~/.local/bin`, and on a shared account
  `/opt/homebrew/bin`, so a later run's `PATH` holds what an earlier run
  left there (plan §14, Cordoned runs on a shared account). Reading the
  code found every program the daemon, the supervisor, and the suite start
  under the launch's environment named by absolute path, the supervisor
  itself started by absolute path with an empty environment, and the
  run-end proof starting nothing. The confined
  program matters most: `sandbox-exec` resolves its program argument
  through the launch's own `PATH`, so a bare canary name would run a
  decoy. Starting the canary as bare `cat` makes the decoy tests fail.

## Rejected

- **A conformance-suite probe that plants decoys.** The pass runs on the
  account's real home, and a decoy `git` in `~/.local/bin` would shadow
  the owner's own tools while it ran. A darwin test on the fixture's fake
  home plants a decoy for `git`, `sandbox-exec`, `cat`, `sleep`,
  `launchctl`, `xcrun`, and the CLI's name in each home directory on the
  list, and finds none ran through a launch to its end, a canceled launch,
  a recovered launch, and the suite's full pass and pre-job probe. A last
  launch whose agent runs `sandbox-exec` by name is the control: the decoy
  runs. The test can't plant in `/opt/homebrew/bin`, so a real tool there
  hides the decoy of the same name. A decoy catches only a start through
  the launch's `PATH`; a bare name the daemon starts resolves through its
  own `PATH` (Consequences).
- **Resolving each entry at composition**, as `CordonConfig` resolves its
  other paths: see the rustup link above.

## Consequences

- **Every host takes a new proof.** The digest's value changes on every
  host; its shape doesn't, so the configuration version stays `v2`.
- **Out of scope: the daemon's own `PATH`.** The installed LaunchAgent's
  `PATH` ends with `/usr/local/bin` and `/opt/homebrew/bin`, and the daemon
  runs `git` by name, which resolves to `/usr/bin/git` first. Whether any
  other name the daemon looks up resolves only from `/opt/homebrew/bin` is
  a separate question for a follow-up issue.
