# Cordoned Verifier on the Host

- Status: complete
- Tested: 2026-11-20 (UTC)
- macOS: 27.2 (26B123)
- Xcode: 27.2 (27B52); Apple Swift 6.4.2; iOS Simulator runtime 27.2 (24B8117)
- Go: 1.27.4, the installed executor's toolchain (`/Library/Freeside/macverify/go-cache/go1.27.4`)
- CLI: the owner's installed `claude` 2.1.327, for the one CLI writer run
- Host: Mac Studio (Mac13,1), Apple M1 Max, 10 cores (8 performance, 2
  efficiency), 32 GiB, the owner's account
- Identity: the owner's enrolled `claude-bnw` setup token, read from its
  credential volume and passed to the CLI on fd 3
- Base: `9cf375955`, the `main` tip the jobs and writer worktrees were cut from
- Issue: #3526

## Conclusion

**It doesn't work: a job on the owner's account can't get a verdict as
trustworthy as a slot's.** Everything runs. The whole `macos_host` recipe
(format, `app test`, the Mac and iOS builds, convergence) passed under the
final verifier file as the owner, with the owner's home unreadable (one
load timeout in `app test` passed on rerun). So did the in-process renders
and the built Mac app with a window capture; a Simulator can't run, since
the file has to deny it. Most of what a writer could do to a running job, and a job to a writer, the
daemon, or another job, closes with a rule. What stays open comes from
running as the same user, and no rule file closes it:

1. **Xcode's tools share two per-user places with every writer.** The
   Swift driver, SwiftPM, `xcodebuild`, and `xcrun` are platform binaries:
   they ignore `TMPDIR` and the per-job suffix and use the owner's own
   temporary directory and Clang module cache. The recipe fails without
   both. A writer under the final writer file creates files in both, and so
   does another job. The module cache feeds the package-manifest compile,
   whose output the job runs. Closing them for writers breaks a writer's own
   Swift build as soon as the cache misses. Write access was shown; no
   poisoned module was built.
2. **Any process on the account reads any other's environment.** A writer
   read a job's environment, and a job read the stand-in daemon's, through
   `sysctl(KERN_PROCARGS2)`, the call `ps eww` makes. Seatbelt has no
   operation for it. A slot job can't read the owner's processes at all.
3. **The owner's pasteboard and screen stay open to the job.** Both are
   session services that act for the job outside its rule file. Denying the
   pasteboard service fails two of the app's own tests, which use private
   named pasteboards through the same service, so a job reads and replaces
   what the owner copied. Capturing the app's own window needs the window
   server, and the same access captures the whole screen.

Three more gaps can be closed or contained, at a cost:

- **The Simulator** runs its devices outside the rule file, as the owner:
  from a job, in-device processes listed `~/.ssh` and `~/Library/Keychains`
  and wrote to `/private/tmp`. The final file denies the CoreSimulator
  services, and the recipe still passes, since it builds for the Simulator
  but runs none. A cordoned job then captures no Simulator.
- **No packet filter** as the owner: Seatbelt is the only network layer.
- **No uid to reap by:** a sweep by sandbox membership finds every
  survivor that the other finders miss.

**Recommendation: keep verification on the slot executor.** The two rule
files are worth keeping as the cordon's record and as defense in depth for
a job that must run as the owner, but they don't replace the slot's uid.
What the slot buys, the owner's account can't: its own per-user
directories, its own process table view, and its own session services.
Whether a slot can run a Simulator of its own is untested: no step here ran
one as a slot account.

## The Rule Files

[`cordoned-verifier.sb`](cordoned-verifier.sb) is the verifier file the spike
ended with, and
[`cordoned-writer-additions.sb`](cordoned-writer-additions.sb) the rules a
writer needs on top of [`cordoned-writer.sb`](cordoned-writer.sb), which
stays as #3463 left it. A writer runs under the writer file and the
additions concatenated, the additions last, so their rules win. Both take
paths as `sandbox-exec -D` parameters, resolved, because Seatbelt matches
resolved paths.

The verifier file starts from the slot profile,
`daemon/internal/macverify/profile.sb`, rule for rule, with the slot
parameter names kept (`JOB_DIR`, `SLOT_HOME`, `SLOT_TMP`, `SLOT_CACHE`,
`PORT_1` to `PORT_4`). What it adds, and why:

| Rule | Why |
|---|---|
| Deny contents and xattrs of `VERIFY_ROOT`, then allow the job's own directories | Other jobs' workspaces. Metadata stays open: denying it broke every path lookup under the job's own root (v3, v4) |
| Deny contents of `OWNER_HOME` | Whatever a job reads can reach its transcript, which the daemon stores and may publish. The recipe passed with all of `file-read*` denied there (v6); the final file keeps metadata so a job root under the home would still resolve |
| Allow read and write of `USER_TEMP`, except `xcrun_db` and other jobs' `freeside-verify-*` suffixes | SwiftPM and the Swift driver fail without the owner's temporary directory (`couldNotFindTmpDir`, then `permissionDenied`). `xcrun_db` is the shims' tool-path cache, which they run from; a job that can't write it runs uncached |
| Deny `USER_CACHE` but its own suffix, a look at the parent, and `clang/ModuleCache` | `xcodebuild`'s cache there (`com.apple.DeveloperTools`) can stay closed; the manifest compile's module cache can't (Mac build exit 74 without it, v4) |
| `(deny signal) (allow signal (target same-sandbox))` | A job stops only its own processes. launchd applies the same check, so `launchctl kickstart -k`, `kill`, `stop`, and `bootout` refuse too |
| `(deny mach-priv-task-port)` | No task port, so no debugger attach or memory access |
| Deny preference writes, and reads but the global domain | cfprefsd acts for the job outside its rule file: a job wrote the owner's global preferences and read 19 KB of Terminal's preferences around the home rule. The global domain stays readable for the appearance a render follows. A test's own preference suite then lives in memory, which the app's tests pass with |
| Deny `mach-lookup` of `com.apple.CoreSimulator.*` | The Simulator's devices run as the owner outside the rule file (Capture) |
| The writer file's secret-read denials, keychain and securityd lookups, and the Freeside app-support denials | A slot account held none of these; the owner's does |

The writer additions:

| Rule | Why |
|---|---|
| Deny writes to `VERIFY_ROOT`, `/Library/Freeside`, `DAEMON_TMP`, and the jobs' `freeside-verify-*` suffixes | A job's workspace, home, caches, the installed Go toolchain and both cache trees (owned by the owner, so writable under the writer file alone), and the daemon's scratch |
| Deny reads of `VERIFY_ROOT` | Another task's candidate |
| The same signal and task-port rules | Under the writer file alone a writer stopped a job and attached `lldb` to it |
| Deny `user-preference-write` to `kCFPreferencesAnyApplication` and writes to `~/Library/Preferences/.GlobalPreferences*` | The global domain is the one a job reads. `com.apple.universalaccess` (increase contrast) already refuses an ordinary process |
| Deny `network-bind` on the jobs' reserved ports | A writer that binds a job's port first fails its test or answers its client |

## The Recipe

`recipe ID SBFILE TAG` runs the workspace's `macos_host` commands from
`.freeside/verify.json` in a job built the way `slot-run prepare` builds one:
the workspace from `git archive` of the base, the Swift seed from the
installed cache entry its `Package.resolved` matches, and job-private home,
temporary, and cache directories. The environment is `commandEnv`'s plus
`CFFIXED_USER_HOME` (moves Foundation's home, which SwiftPM follows),
`DIRHELPER_USER_DIR_SUFFIX` (moves confstr's directories for tools that
aren't platform binaries), and `CLANG_MODULE_CACHE_PATH`. A failing step
reruns with the same environment and no rule file, to tell a rule from an
environment failure.

| Pass | Rule file | format | app test | build-mac | build-ios | convergence |
|---|---|---|---|---|---|---|
| v1 | slot rules, owner's T and C closed, no env overrides | fails | fails | fails | fails | fails |
| v4 | v1 plus T open, own suffix, other jobs closed by `file-read*` | 0 | fails | fails | fails | fails |
| v5 | v4 with other jobs closed by contents only, plus the module cache | 0, 19 s | 0, 910 s | 0, 121 s | 0, 107 s | 0, 200 s |
| v6 | v5 plus all reads of the owner's home denied | 0, 20 s | 0, 870 s | 0, 93 s | 0, 79 s | 0, 321 s |
| v8 | v6's home rule as contents only, plus preference and pasteboard denies | 0, 48 s | 3 tests fail | 0, 99 s | 0, 83 s | 0, 286 s |
| v9 | v8 plus the CoreSimulator deny | 0, 21 s | 2 tests fail | 0, 103 s | 0, 95 s | 0, 279 s |
| v10 | the final file: v9 without the pasteboard deny | 0, 21 s | 1 test fails; rerun 0, 418 s | 0, 76 s | 0, 83 s | 0, 148 s |
| none | v4's job with no rule file | 0 | 0, 551 s | 0, 110 s | 0, 84 s | 0, 21 s |

Times are on a host other agents loaded throughout (load average 100 to
320 until the v10 rerun), with two recipes often running at once; they
show that the steps finish, not how fast. Each failure a rule caused
matched a denial in the kernel log: v1's on the owner's T and C, v4's on path lookups under the job root
and then on `C/clang/ModuleCache`. v8's and v9's were the two
`ComposerAttachmentIntakeTests` that write and read a pasteboard named
with a fresh UUID: the pasteboard deny refuses every pasteboard, named or
not, so the final file drops it. v8's `app test` also had one 30-second
wait time out (`RunTimelineRefetchTests`), which passed under v9 and
unconfined; that is the load. v10's one failure was the same kind, a
30-second wait in `TaskPagePlaceHostedTests` at load average near 100; the
whole `app test` rerun in the same job under the same file at load 12 to 18
passed all 1,365 tests and the 303 and 23 beside them.

What the final file still refuses during a passing recipe, all harmless:
`xcodebuild` reads and writes of `C/com.apple.DeveloperTools/27.2-27B52`
(it rebuilds the cache in memory), the `xcrun` and `git` shims' writes of
`T/xcrun_db`, reads of `~/.CFUserTextEncoding` (v6), `xcodebuild`'s reads
of its own preference domains, and the screenshot tests' writes to their
per-test preference suites.

**Home-read deny: works.** v6 denied every read under the owner's home,
metadata included, and the whole recipe passed. The final file denies
contents and keeps metadata, a strictly smaller denial.

## Session Services

The rule file confines the job's own process, but some services the
account shares act for the job from outside it. `services` probes them
from a job; a write probe uses the spike's own preference key and the
find pasteboard, saved and restored, and the screen capture is deleted
unseen.

| Probe | v5 (no service rules) | v6 (home reads denied) | final |
|---|---|---|---|
| Write a preference, the spike's domain and the global one | landed | landed | refused |
| Read Terminal's preferences: bytes through cfprefsd, then from its plist | | 19,358, 0 | 0, 0 |
| Read the general pasteboard | 46 bytes | 46 bytes | 45 bytes |
| Write the find pasteboard | landed | landed | landed |
| `screencapture -x`, the whole screen | | exit 0, 3.7 MB | exit 0, 4.3 MB |

The home rule refuses the plist but doesn't reach cfprefsd: the job asks
the service, and the service reads the file. Preference rules close it. v8
also denied the pasteboard service (both pasteboard probes refused), which
broke the app's tests (The Recipe). No rule tells window capture from screen capture.

## A Writer Against a Running Job

`writer-vs-job` starts the spike's target program in a job under the
verifier file, listening on the job's first port with a marker in its
environment, then aims each probe at it from a writer under the writer file
alone (`base`) and with the additions (`final`). Each probe prints an exit
status or a yes and no, never what it read.

| Probe | base | final | Closing rule |
|---|---|---|---|
| Write and read the job's workspace | yes | refused | additions: `VERIFY_ROOT` write and read |
| Create a file in the job's home | yes | refused | additions: `VERIFY_ROOT` write |
| Create a file in the installed Go toolchain's `bin`, a Go module cache entry, a Swift cache entry | yes | refused | additions: `/Library/Freeside` write |
| Create a file in the job's own per-user suffix | yes | refused | additions: suffix regex |
| Create a file in the daemon's scratch | yes | refused | additions: `DAEMON_TMP` write |
| Create a file in the owner's T and in `C/clang/ModuleCache` | yes | **yes** | none (Residuals) |
| `SIGSTOP` the job's process | yes | refused | additions: signal rule |
| `lldb -p` attach | yes | refused | additions: `mach-priv-task-port` |
| Read the job's environment (`KERN_PROCARGS2`) | yes | **yes** | none (Residuals) |
| Bind the job's second port on `127.0.0.1` and `0.0.0.0` | yes | refused | additions: `network-bind` |
| Write `AppleInterfaceStyle`-style keys in the global domain, through cfprefsd and to its file | yes | refused | additions: preference rules |
| Write `com.apple.universalaccess` | no | no | the OS refuses it |

What a writer loses under the final file: nothing it ran. `go build ./...`,
`go test ./internal/golden/...`, a `git commit`, and stopping its own child
all passed. `task_for_pid` returned 5 in every case; the attach that worked
went through `lldb`'s `debugserver` after the owner had approved the
developer-tools prompt once, which macOS honors for ten hours.

**Appearance.** A job reads the owner's `AppleInterfaceStyle` (Dark) with
or without `CFFIXED_USER_HOME`, so an unpinned render would follow the
owner's setting. The screenshot suite pins its color scheme, so this
matters only for a render that doesn't.

**One real CLI writer** (`writer-cli`) ran under the final writer file,
beside a running job, with the token on fd 3 and stdin at `/dev/null`. It
did its ordinary work, `go build` and a commit, then from its Bash tool
tried the job: workspace write and read exit 1, signal refused, environment
readable. CLI exit 0 in 52 s, 2 turns, $0.03; a rerun after review widened
the leak check, 9 s with a warm build cache, gave the same results. That
check found the token in 0 files under the whole spike root (runs, jobs,
logs) and the CLI's cache, 0 lines of the commits on any ref, with 0 paths
it couldn't read.

## A Job Against Another Job

`job-vs-job` runs job B's probes against job A's target, both under the
verifier file. Read and write of A's workspace, A's per-user suffix, a
signal, `lldb` attach, binding A's port, and connecting to A's port are all
refused. B reads A's environment, and reads and writes the owner's T and
module cache, which A's Xcode tools also use. Before the other-jobs and
signal rules (v3), B read A's workspace, stopped A, and attached to it.

## Against the Daemon

`writer-vs-daemon` runs the spike's target program as a launchd service
under its own label, `com.freeside.spike.daemon`, standing in for
`freesided`: no probe can reach the owner's daemons. Neither binary has the
hardened runtime or `get-task-allow`, so the OS treats them alike.

| Probe | writer, base | writer, final | job, verifier file |
|---|---|---|---|
| Create a file in the daemon's scratch | yes | refused | refused |
| `SIGSTOP` | yes | refused | refused |
| `lldb -p` attach | yes | refused | refused |
| Read its environment | yes | **yes** | **yes** |
| `launchctl kickstart -k` | restarted | refused | refused |
| `launchctl kill TERM` | restarted | refused | refused |
| `launchctl stop` | exit 1 | refused | refused |
| `launchctl bootout` | removed | refused | refused |

launchd asks whether the caller may signal the service's process, so the
signal rule closes all four `launchctl` verbs. `launchctl stop`, the legacy
verb, failed even unconfined.

**The daemon-protection rules #3470 needs**, whatever runs beside the
daemon on its account, writer or job:

- `(deny signal) (allow signal (target same-sandbox))`, which also closes
  every `launchctl` verb on the daemon's label.
- `(deny mach-priv-task-port)`, so no attach, even after the owner has
  approved the developer-tools prompt.
- A write deny on the daemon's scratch (`DAEMON_TMP`), its state and
  database (the writer file's Freeside app-support rules), and
  `/Library/Freeside`.

No rule keeps another process from reading the daemon's environment, so
the daemon must start with nothing secret in it. The probe script's `env`
mode, aimed at a process the suite started, reports whether a given marker
is in that environment, never the environment itself. The verifier file
already denies all three writes through the slot write rule.

## Teardown Without a Uid

A slot job's processes all run as the slot, and teardown reaps the uid.
On the owner's account there's no uid to reap. `teardown` starts four
survivors from a job, each writing a heartbeat: `nohup`, a double fork, a
`setsid`, and one that `setsid`s, `cd`s to `/`, closes its descriptors, and
execs with its argv rewritten to `idle`. Then it tries each finder, kills
what it found, and checks which survivors still beat.

| Finder | Survivors it misses |
|---|---|
| The job's process group | setsid, rewrite |
| `pgrep -f` on the job root | rewrite |
| `lsof +D` on the job root | rewrite |
| A per-job launchd service, booted out | setsid, rewrite |
| Sandbox membership | none |

**Sandbox membership finds every survivor.** Seatbelt passes the rule file
from parent to child, and a process can't leave it. `sandbox_check(pid,
"file-write-create", SANDBOX_FILTER_PATH, path)` on every owner process
returns allowed for the job's own directory and denied for `/private/tmp`
only for a process in that job's sandbox: an unconfined process is allowed
both, a writer or another job is denied the first. It needs an existing
path; a missing one reads as denied. The sweep (`sbfind`, a small C
program, since the call is variadic) found all four survivors with no
command line, directory, or group to go on.

**A launchd service ends its own process group, nothing more.** When the
job's program exited, launchd killed the `nohup` and double-fork survivors
still in its group. `bootout` reached nothing else.

## Network Without pf

`network` runs the slot probe's own sandbox checks (`freesided macverify
probe sandbox`) from a job under the verifier file. Every network check
passed: loopback to another port refused, the job's four reserved ports
bound, a foreign port refused for bind and connect, DNS empty, TCP and UDP
out refused, and `curl https://github.com` exit 6. A unix-socket connect to
a listener outside the job and a bind inside it were both refused, as on a
slot.

The probe's one failure is by design: its write check on the owner's T
succeeds, because the final file opens T.

**No packet filter as the owner.** The slot executor also loads per-slot
pf rules that drop the slot uid's traffic; pf can't match on a process,
and `pfctl` needs root (`/dev/pf: Permission denied`). The probe's
`packet-filter` mode passed as the owner only because `192.0.2.1` is
unroutable: its UDP left through the owner's address. A cordoned job has
Seatbelt alone where a slot has two layers.

## Capture

| Capture | Result |
|---|---|
| In-process renders: the screenshot suite's selected-key probe (`decision-system_health-large`) through `swift test` | Exit 0, 960×499. The same digest before and after the final file's preference rules |
| The built Mac app, launched directly from the job (no Launch Services), then `screencapture -l` of its window | Exit 0. Under the final file the window opens at its default size: it can no longer read the owner's saved state through cfprefsd |
| Before the CoreSimulator deny: a Simulator in a device set inside the job: create, boot, install the recipe's iOS build, launch, screenshot | Exit 0 at every step, 1206×2622 |
| Before the CoreSimulator deny: in-device `simctl spawn` lists `~/.ssh` and `~/Library/Keychains`, touches `/private/tmp` and the job | **All succeeded** |
| The final file: the same Simulator steps | `simctl create` fails: `CoreSimulatorService connection became invalid` |
| The whole screen, `screencapture -x` | **Exit 0**, under every verifier variant |

**The Simulator is an escape.** `CoreSimulatorService`, `launchd_sim`, and
everything in the device have launchd as parent, so none carries the rule
file, and they run as the owner. Whether a slot's device would run as the
slot is untested. The final file denies `mach-lookup` of every
`com.apple.CoreSimulator.*` service, so `simctl` fails from the job, and
the recipe's iOS build still passes (v9, v10). Simulator capture would need
the job to reach the service, which is the escape.

**Window capture reaches every window.** The Mac app's capture needs the
window server, and the same access captures the whole screen: the owner's
other apps, every agent's terminal. No Seatbelt rule tells the two apart.
The app's window also appears on the owner's display while the job runs.

**launchd-started helpers.** During a build, `ibtoold` ran with launchd as
parent, outside any rule file. Whether a job can make it write outside the
job wasn't tested.

## Density

`density N` runs the recipe's Mac build in N jobs at once under the final
file, beside four writers under the final writer file, each running
`go build ./...` and the `golden` and `domain` tests from a cold build
cache. Every job and every writer exited 0 at each density.

| Jobs | Job time | Writer time | Load at start, at end |
|---|---|---|---|
| 1 | 108 s | 63 to 64 s | 15, 57 |
| 2 | 140 s each | 72 to 74 s | 54, 139 |
| 4 | 192 to 193 s each | 85 s | 123, 253 |

Four jobs cost each job about 1.8 times one job's time, and each writer
about 1.3 times, with no failure, refusal, or cross-job interference. These
runs came after other agents' load had fallen (it was 15 at the first);
earlier runs at load 100 to 135, with the writers' build cache left warm,
also all passed (one job: 142 s; two: 106 and 110 s). The slot executor
has two slots, so it runs two jobs at most.

## Per-Command Overhead

`overhead 20`, at load average 189:

| Path | ms per command |
|---|---|
| `/usr/bin/true`, no rule file | 26 |
| `/usr/bin/true` under the verifier file | 41 |
| `go version`, no rule file | 39 |
| `go version` under the verifier file | 51 |
| `/usr/bin/true` through the slot path, `Room.Run` (10 runs, empty workspace) | 4,264 |

A rule file costs about 12 to 15 ms per process start. The slot path is
the production room's `Room.Run`, run from a scratch live test in a copy of
the daemon outside the repository, through the installed helper, while no
daemon served the slots. It costs seconds because each call is a whole
verification step: the helper and toolchain checks, a pre-job probe on the
leased slot, `prepare`, the command, and a teardown that proves the slot's
uid has no process left.

**Only the empty workspace was timed.** The plan asked for a second slot
run on the full export, which wasn't measured, so the figure leaves out the
cost of copying a real tree into the slot.

**Caveat on the slot run.** It ran under a guard that looked only for a
`-macverify-slots` flag. Neither daemon's argv carried any `-macverify`
flag and no deputy ran, but a daemon can also take the executor from its
stored instance settings, which the spike didn't read (doing so means
asking the owner's running daemon). `fvslot2`'s job directory changed at
17:23 UTC, which the spike didn't cause; the run used only `fvslot1`, it
passed, and nothing showed a collision. The script now runs the slot path
only when the operator sets `SLOTS_IDLE=1` after checking, and no slot
process is running. A cordoned step trades that for one
`sandbox-exec` per command plus a sandbox-membership sweep. The slot pays
its cost once per recipe step, a few times per verification, against
steps that take minutes, so overhead decides nothing here.

## Residuals

What a cordoned verification job can't be promised, beyond what
`cordoned-writer.md` lists for a writer. Against the three things the slot
executor promises today: **uid teardown** becomes a sweep by sandbox
membership; **the pf layer** is gone, leaving Seatbelt alone; and **no
login session** is lost, since the job runs inside the owner's session and
its services (cfprefsd, the pasteboard, the window server, CoreSimulator)
act for it outside its rule file. In detail:

- **Its Xcode steps' inputs.** The owner's T and Clang module cache are
  writable by every writer and every job on the account. Closing them for
  writers breaks a writer's Swift compile on a cache miss
  (`writer-xcode`: `swiftc` exit 1, `unable to open output file
  .../C/clang/ModuleCache/...pcm`).
- **Its environment, and everyone else's.** A writer reads the job's; the
  job reads every owner process's, the daemon's and every agent CLI's
  included, and can print it into its transcript.
- **The owner's pasteboard.** A job reads what the owner copied and can
  replace it; denying the service fails the app's own pasteboard tests.
- **The screen,** whenever the job may capture a window.
- **Simulator capture.** The final file denies the Simulator, since its
  devices run as the owner outside any rule file.
- **Two network layers.** Seatbelt only.
- **Teardown by uid.** A sweep by sandbox membership replaces it.
- **Services launchd starts for it,** such as `ibtoold`, which run
  unconfined.

## Method

The script is [`cordoned-verifier.zsh`](cordoned-verifier.zsh), a zsh
measurement record kept beside this doc as `cordoned-writer.zsh` is.
[`cordoned-verifier-probe.zsh`](cordoned-verifier-probe.zsh) holds the
cross-process probes on their own, so #3470's conformance suite can aim
them. Run the script on the reference host as the owner:

```sh
export ROOT=$(mktemp -d /tmp/cordon-verify.XXXXXX)   # outside any repository and TMPDIR
export REPO=<the Freeside checkout to cut jobs and worktrees from>
token() { container run --rm --no-dns -v <enrollment volume>:/cred:ro alpine:3.22 cat /cred/token; }
zsh docs/spikes/cordoned-verifier.zsh setup
zsh docs/spikes/cordoned-verifier.zsh recipe final
zsh docs/spikes/cordoned-verifier.zsh network
zsh docs/spikes/cordoned-verifier.zsh services
zsh docs/spikes/cordoned-verifier.zsh writer-vs-job
token | zsh docs/spikes/cordoned-verifier.zsh writer-cli
zsh docs/spikes/cordoned-verifier.zsh writer-xcode
zsh docs/spikes/cordoned-verifier.zsh job-vs-job
zsh docs/spikes/cordoned-verifier.zsh writer-vs-daemon
zsh docs/spikes/cordoned-verifier.zsh teardown
zsh docs/spikes/cordoned-verifier.zsh capture renders
zsh docs/spikes/cordoned-verifier.zsh capture macapp "" final
zsh docs/spikes/cordoned-verifier.zsh capture sim "" final
zsh docs/spikes/cordoned-verifier.zsh density 4
SLOTS_IDLE=1 zsh docs/spikes/cordoned-verifier.zsh overhead   # only once no daemon serves the slots
zsh docs/spikes/cordoned-verifier.zsh cleanup all
```

- **Variants.** A verifier variant is a file passed as `SBFILE`; the
  passes above were the folded file at each stage plus one rule, kept in
  `$ROOT/sb/`. A writer variant is a file in `$ROOT/variants/` named after
  `final`. A rule file is read at each `sandbox-exec` start, so a run never
  edits the file it runs under.
- **Pool.** Each job takes one of four entries: a per-user suffix and four
  ports from 47100, outside the slots' 47000 to 47007. dirhelper flags a
  suffix directory so the owner can empty it but never remove it, so the
  pool is fixed rather than one per job.
- **Only the spike's processes.** Every pid probe acts only when the
  target's start time matches the one read when the spike started it, and
  every `launchctl` probe only on a `com.freeside.spike.` label. Nothing
  aimed at the owner's daemons, apps, or sessions.
- **The token.** As `cordoned-writer.zsh`: read from stdin into shell memory,
  given to the CLI on fd 3 by the `print` builtin, never in an argv or the
  environment, stdin at `/dev/null`. The script refuses to run traced
  (`zsh -x`), and the leak check runs before anything the CLI wrote is
  printed.
- **Denials.** `/usr/bin/log show --predicate 'sender == "Sandbox"'` reads
  them (zsh has a `log` builtin). The log is system-wide, so a step's
  summary also holds other processes' denials; attribute by process name.
- **Fixes made during the run.** Early teardown rounds used the harness's
  own process group, and timed launchd's round before the survivors had
  started on the loaded host; the stand-in daemon's job round read its pid
  before launchd restarted it; the first in-device probes ran `/bin/sh`,
  which the iOS runtime lacks; the first writer manifest compile tried to
  nest SwiftPM's sandbox. The numbers above come from the fixed runs.
- **Host load.** Other agents ran the daemon's test suites and their own
  builds throughout, at load averages of 150 to 320.

## Cleanup

`cleanup all` boots out the spike's `com.freeside.spike.*` services, ends
any process whose command line names the spike root, deletes the job
roots' Simulator device sets, removes the spike's preference key, the
writer worktrees (through `git worktree remove`), the jobs, and the
contents of the suffix pool, then checks the host and removes the root.
Its check after the last run:

- **Preferences:** `AppleInterfaceStyle` Dark and increase contrast unset,
  as before the spike; the spike's own key is gone.
- **Processes and services:** none under the spike root, no
  `com.freeside.spike.*` label, no process as either slot account. The
  owner's shared `CoreSimulatorService` was left running; whether the
  capture runs started it isn't known.
- **Worktrees:** none of the spike's left in the checkout.
- **The installed executor:** the helper `bin/slot-run` byte-identical to
  `main`'s `daemon/internal/macverify/slot-run.sh`, both slots' job
  directories empty, and the same 28,737 entries under
  `/Library/Freeside/macverify`'s `bin`, `go-cache`, and `swift-cache`,
  same sizes and modes. Three directories' modification times moved, at
  18:36:29 UTC: the Go toolchain's `bin`, a Go module cache entry, and a
  Swift cache entry, where the base writer round created a probe file and
  removed it. The executor doesn't read modification times.
- **What can't be removed:** the per-user suffix directories
  `freeside-verify-spike-1` to `-4` in the owner's T and C, empty, and
  `T/spike-x` from an early probe. dirhelper flags them `sunlnk` under
  System Integrity Protection, so the owner can empty them but not remove
  them.

SwiftPM also leaves lock files in the owner's T named after the paths it
built (`_private_tmp_..._Package.resolved.lock`); this run left none, and
the ones there now come from the earlier writer spike.
