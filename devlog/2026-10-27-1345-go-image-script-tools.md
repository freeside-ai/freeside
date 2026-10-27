# Script Tools in the Go Project Image

Work unit: #2376. Plan: `docs/plan.md` §5.7 (Toolchains), §11 (revision
92). Builds on `devlog/2026-10-27-1204-freeside-protected-paths.md`.

## Decisions

**The Go image bakes procps from Debian packages (owner decision,
2026-10-27).** The planned tools were vacuum, shellcheck, and Python. Running
`check.sh scripts` in Freeside's image showed a fourth gap:
`scripts/check-agent-image.sh` reads process groups with `ps`, and its suite
also runs `ps -p`. The base has no `ps`, and BusyBox's applet rejects `-p`.
procps publishes no release binary, so the plan's rule (a pinned upstream
archive, or stop and report) had no archive to pin. The owner chose
Debian trixie's `procps` and `libproc2-0` 4.0.4-9 arm64 packages, pinned by
SHA-256 and fetched from a fixed snapshot.debian.org timestamp. They enter
the image like the other archives: compressed bytes, a label, a provenance
check, and the shared launcher, which extracts them with BusyBox
`dpkg-deb` and runs `ps` with `LD_LIBRARY_PATH` on the package's library.
`ps` also needs the base's `libsystemd0`, which every current agent base
carries; a base without it fails the build's `ps --version` check.

Rejected:

- **Installing procps in the agent bases.** The builder's rule is that
  verification tools belong to the project image, and the issue keeps
  `images/` out of scope.
- **Onboarding `api/` alone and deferring `scripts/`.** It leaves the unit's
  `scripts/` acceptance unmet for a tool that a pinned package covers.
- **A BusyBox `ps` launcher.** It passes the checker but not its suite,
  which runs `ps -p`.

**`python3` and `ps` launchers live in `/usr/bin`.** `scripts/test-check.sh`
runs `check.sh daemon test` with `PATH` cut to `/usr/bin:/bin` and expects
the slot limiter, which needs `python3`, to take a slot. Debian installs
both tools there, so the launchers do too; vacuum and shellcheck stay in
`/usr/local/bin` beside the Go launchers.

**The new tools extract once per room and exec; Go is unchanged.** One
launcher serves every name. The Go names keep their per-process-tree root,
which the launcher removes after the tool exits, so it runs the tool as a
child. The first build's proof failed on that shape: `scripts/test-check.sh`
starts `python3 -c 'os.setsid(); os.execvp(...)'` in the background and then
kills the process group named by `$!`, which only works when `python3` keeps
the launcher's PID. So vacuum, shellcheck, python3, and ps extract into a
fixed `/tmp/freeside-project-<tool>` tree on first use and `exec` the tool.
Concurrent first calls each extract into a staging directory and publish it
with BusyBox `mv -T`, a rename that fails onto a populated tree, so the loser
discards its copy. This also saves the Python extraction (about 0.9 s) on
every later call. The tree persists for the room's life on the container's
root filesystem, which is disk, not a tmpfs.

Rejected: leaking a per-call tree (about 84 MiB per Python call), and a
detached reaper that removes the tree after the tool exits (it would inherit
the caller's descriptors, including `check.sh`'s slot lock and capture
pipes). The fixed path gives code that ran earlier in the same room a way to
plant a tree later commands use. That adds no new power: the same code can
already poison `$HOME/.cache/go-build` or set an inherited Go root for its
own descendants, and verification evidence is not a boundary against the
code it verifies.

**The recipe's scripts command leaves out `vocabulary`.** The second
build's proof failed there: `check-vocabulary.sh` lists files with `git
ls-files`, and the proof workspace, like every verification room, holds the
commit's exact tree without `.git` (`verify.MaterializeExact`). So the step
fails in every room whatever the change, and the recipe names the other four
steps. The required CI check still runs it. Making the script work without
`.git` is a `scripts/` change outside this unit, filed as #2399.

Rejected: running `git init` and `git add` inside the recipe command, which
writes into the workspace under test, and keeping the step, which makes every
room fail.

## Refute-First Review

A fresh-context reviewer tried to break the launcher, Containerfile,
provenance, and digest coverage. Outcomes:

- **Confirmed and fixed:** two doc gaps, an unrecorded measurement and a plan
  line that claimed full `scripts/` coverage.
- **Allowed by decision:** the launcher runs a complete
  `/tmp/freeside-project-<tool>` tree it did not extract, so a tree baked into
  an image layer would bypass the archive hash. The build removes its own
  trees, so only an approved base or a tampered layer could carry one, and
  provenance already trusts the base's other files (libc, the loader). Room
  code that plants a tree reaches only later calls in the same command, where
  it already runs.
- **Allowed by decision:** the concurrent first-use path has no executed
  test, only a string pin. It relies on BusyBox `mv -T` refusing a non-empty
  target, checked by hand in the image; if the applet lacked `-T`, every
  call would exit 126 and the build's version checks would fail.
- **Disproved by check:** the publish race, PID preservation under `exec`,
  quoting, unverified downloads, label and ARG mismatches, provenance modes
  and size caps, and digest coverage of every new pin.

Revisit when the agent bases change their Debian release or drop
`libsystemd0`, or when a second Go project needs a different tool set
(opt-in tools need a `ProjectImageEnvironment` field and a contract unit).
