# Cordoned Runs on a Shared Host Account

- Status: complete
- Tested: 2026-11-25 (UTC)
- Host: Mac mini `mini-m6-24-01` (Mac18,5), Apple M6, 12 cores (2 super,
  4 performance, 6 efficiency), 24 GiB, macOS 27.2.1 (26B131), FileVault on
- Account: `agent` (uid 502), a standard account, not an administrator. The
  owner's account `b6n` was logged in at the same time through fast user
  switching, and fleet automation ran commands as `agent` over SSH throughout.
- Tools: Xcode 27.2 (27B52); iOS Simulator runtime 27.2 (24B8117), downloaded
  during the spike (item 6); Go 1.27.4 (Homebrew); rustup 1.29.3 with cargo
  1.100.0 (Homebrew `rustup`, stable toolchain already installed); uv 0.13.9;
  mise 2026.11.10; Google Chrome 158.0.8127.89 (Homebrew cask), used in place
  of Chromium, which the host doesn't have
- Signing: the self-signed identity "Fleet Code Signing mini-m6-24-01", made
  on the host by the fleet repository. An Apple-issued identity may behave
  differently with the permission checks in item 4.
- Cordon code: `8946f9c2`
- Issue: #3596; it feeds the plan revision #3597
- Records: [`cordoned-shared-host.zsh`](cordoned-shared-host.zsh) (one
  subcommand per item), [`cordoned-shared-host-helper.go`](cordoned-shared-host-helper.go)
  (the stand-in LaunchAgent and supervisor), and
  [`cordoned-shared-host-cordon_test.go.txt`](cordoned-shared-host-cordon_test.go.txt)
  (the throwaway test the record copies into `daemon/internal/cordon/` for a
  run and removes afterward)

## Verdict

**Reaching a host: an SSH socket forward to a LaunchAgent works, and it runs
no login shell.** A plain `ssh host command` ran `zsh` and sourced
`~/.zshenv` (item 1). An `ssh -N -L` Unix-socket forward reached a LaunchAgent
in `agent`'s `gui/502` (Aqua) domain and started no shell at all. Four
conditions come with it:

- **Key.** The daemon's key has to go in the root-owned
  `/etc/ssh/authorized_keys/agent`, because sshd reads no key from `~/.ssh`.
- **Login.** The `gui` domain exists only while `agent` is logged in at the
  console. After a restart, the fleet unlocked FileVault over SSH with
  `agent`'s password, but that logged nobody in, so a person still logs
  `agent` in at the console after every restart.
- **Unconfined writers.** The LaunchAgent's binary and plist are writable by
  every unconfined session on `agent`, so they're trusted exactly as far as
  those sessions are.
- **Runs.** The supervisor's socket, binary, and plist must sit where the
  rule file denies runs. The spike's stand-in sat outside every protected
  root. Any cordoned run could have connected to it and asked it to start a
  command under a rule file of the run's own choosing (#3602).

**The run-end proof holds on a shared host.** Every end proved, all with
the same result (item 3):

- 50 of 50 idle;
- 50 of 50 beside `go test ./...` and `xcodebuild` in loops;
- 100 of 100 with a `cargo build` loop added, at a load average of about 25
  on 12 cores.

Each end killed exactly the canary and the three survivors. The proof alone
took at most 666 ms, and a whole launch at most 906 ms, against the
one-minute bound. A standard account's membership query answered for every
root process and every other account's process without an error.

What the proof can't see is what launchd starts: the Simulator's device and
an app opened through Launch Services (items 5 and 6). The plan already
records such a run as uncordoned.

**Seeing and driving the desktop costs console visits, one of them recurring,
and as measured it reaches every run.** macOS charges Screen Recording and
Accessibility to the LaunchAgent's program, not to `screencapture`,
`sandbox-exec`, or the run's tools (item 4).

- **Each approval needs an administrator at the console.** All four switch
  changes asked for an administrator's name and password, because `agent` is
  a standard account.
- **They survive a same-identity rebuild.** Both approvals survived replacing
  the program with another build signed with the same identity and
  identifier. So they cost one console visit per host, plus one whenever the
  identity or identifier changes.
- **One prompt recurs.** After the first capture, macOS asked for permission
  to "bypass the system private window picker". That prompt needs a person at
  the screen, and macOS repeats it on a schedule this spike couldn't observe
  (inferred).
- **The approvals reach every run.** Every cordoned run the approved
  program starts can capture the screen and post clicks and keystrokes,
  under the default rule file too (item 5). In the shared session, that
  includes typing into the owner's own unconfined Terminal. The plan's
  project setting can't hold under this layout (#3602).
- **The approvals survive a restart.** After a restart and a fresh console
  login, both approvals held with no prompt (item 4).

## Findings That Need Work Outside This Spike

- **#3599:** mise can't download a tool under the cordon. Its TLS check needs
  the `com.apple.SecurityServer` lookup the cordon denies (item 8).
- **#3600:** a cordoned run can read another Claude configuration directory
  on the account, `~/.claude-bnw`, which holds the owner's session
  transcripts (item 2).
- **#3601:** a plain `mise install` trusts the project's config and writes
  that trust to `~/.local/state/mise`, where it outlives the run (item 8).
- **#3602:** the host's screen and input approvals reach every cordoned run,
  and the stand-in supervisor was reachable from inside a run (items 1, 4,
  and 5).
- **#3603:** a cordoned run can read the fleet's signing key and its keychain
  password, and write the `sign` script and the agent prompt clones under
  `~/proj/freeasinbird/` (item 2).

## Item 1: Reaching the Desktop Session Without the Login Shell

**Observed.** The stand-in helper ran as a LaunchAgent (`gui/502`,
`RunAtLoad`, `KeepAlive`). It answered on a Unix socket under
`~/.spike-helper/`, and `launchctl managername` reported `Aqua`. A marker
line in `~/.zshenv` logged every zsh that read it, with `ssh=yes` when sshd
started it. From the Studio, the owner ran both connections with the fleet's
`agent` key:

| Connection | Result | Marker line |
| --- | --- | --- |
| `ssh -N -L /tmp/spike-h.sock:~agent/.spike-helper/h.sock` | `whoami` answered: version v1, uid 502, parent 1, `Aqua` | none |
| `ssh agent@mini true` | exit 0 | `argv0=zsh ssh=yes cmd=true` |

`agent`'s login shell is `/bin/zsh`. Fleet automation over SSH ran about 40
`zsh -c` commands during the spike's half hour, and each one sourced
`~/.zshenv` too.

**sshd.** `sshd_config.d/050-fleet.conf` sets `AuthorizedKeysFile
/etc/ssh/authorized_keys/%u`, and the first value wins. A loopback-only key
added to `~agent/.ssh/authorized_keys` was refused with `Permission denied
(publickey)` (`item1-loopback`). So the loopback measurement the owner
approved couldn't run, and the Studio run replaced it.
`AllowStreamLocalForwarding` is unset, which defaults to yes, and the forward
worked. There is no `ForceCommand`, `~/.ssh/rc`, or `/etc/ssh/sshrc`.

**Inferred.**

- **No shell on the token's path.** A daemon that holds a forward to a
  LaunchAgent supervisor never passes a token through a shell a run could
  have written (plan §14's risk row).
- **The LaunchAgent is the trust anchor.** A run can't write
  `~/Library/LaunchAgents`, but any unconfined session on `agent` can.
- **The run can reach the stand-in.** Nothing in the rule file kept a run
  from the stand-in's socket under `~/.spike-helper/` or its binary under
  `/private/tmp`.
- **The stand-in accepts any rule file.** Its `run` request takes the rule
  file's path from the caller.

While items 6 to 9 ran, those runs were plain stand-in scripts, so nothing
used this. A real supervisor needs its socket and binary under a protected
root (#3602).

**Console:** none for the forward itself. The `gui` domain needs `agent`
logged in at the console after each restart. The Studio side needed the
owner (Where It Runs). The daemon's key in `/etc/ssh/authorized_keys/agent`
is a root step for the fleet repository, once per host.

## Item 2: The Conformance Suite as `agent`

**Observed.** Three suites ran from the desktop session and again over SSH
from the Studio, and all three passed:

- the live suite as written;
- the full suite under the configuration the daemon would build for `agent`
  (`TestSharedRealConfig`), which uses the real home, `~/.claude`, `~/proj`,
  and `getconf`'s `DARWIN_USER_TEMP_DIR` and `DARWIN_USER_CACHE_DIR`.

| Test | Desktop | SSH |
| --- | --- | --- |
| `TestLiveFullProvesTheCordonOnTheOwnersAccount` | pass | pass |
| `TestSharedRealConfig` (real per-user directories) | pass | pass |
| `TestLiveLaunchRefusesSessionServices` | pass; `container` skipped (not installed) | pass; `container` and the keychain probe skipped |
| `TestLiveLaunchCantWriteTheGlobalPreferences`, `…CantStopALaunchdJob` | pass | pass |
| `TestLiveCredentialLaunchAuthenticates` | skipped: no token, by design | skipped |

Over SSH the keychain probe was skipped because `security
show-keychain-info` fails unconfined there too ("User interaction is not
allowed"). So the keychain denial goes unproven when the suite runs outside
the desktop session. No check failed.

**Paths that prove nothing on this account.** For a path that doesn't exist,
the suite's read probe passes without proving anything.

- **Present:** `~/.ssh`, `~/.config/gh`, `~/.codex`, `~/.claude`,
  `~/.claude.json`, `~/Library/Keychains`, `~/Library/Safari`, and
  `~/Library/Cookies`. `~/.ssh` exists only because of this spike (Cleanup).
- **Absent:** `~/.git-credentials`, `~/.netrc`, `~/.aws`,
  `~/.config/gcloud`, `~/.azure`, `~/.kube`, `~/.docker/config.json`,
  `~/.freeside`, the browser profiles, the container volumes, and both
  Freeside application-support roots.
- **No `.env` files.** `~/proj` holds none, so the `.env` denial is unproven
  here too.

**Paths the rules miss.** Each was checked by opening the file under the
default rule file. Nothing was read out or written.

- **A second Claude configuration directory (#3600).** `~/.claude-bnw` holds
  `sessions/`, `projects/`, `.claude.json`, and `skills/`, and a cordoned run
  listed and read it. Its `settings.json` and `CLAUDE.md` are links into
  `~/.claude`, so those read as denied. It holds no credential file.
- **The fleet's signing files (#3603).** A run could open
  `~/.config/fleet/signing/key.pem`, `keychain.password`, and
  `fleet-signing.keychain-db` for reading, and the `sign` script for writing.
- **The prompt and skill clones (#3603).** A run could open a file in the
  clones under `~/proj/freeasinbird/` for writing. The rule file denies only
  removal and renaming under the projects directory. The owner's fleet thread
  raised both of these.

**Console:** none.

## Item 3: The Run-End Proof Beside Other Work

**Observed.** Each end ran twice:

- **Through the backend,** as production runs it: a launch's stand-in agent
  started three detached children and exited.
- **By hand,** under the package's own `endLaunch`, with a counting host that
  records every pass, not only the last.

The one-minute production bound applied to both. The backend time covers the
whole launch, from start to proven end. The by-hand time covers the proof
alone.

| Condition | Path | Proved | Survivors | ms min / median / p95 / max | Passes |
| --- | --- | --- | --- | --- | --- |
| idle | whole launch | 50/50 | 0 | 75 / 80 / 83 / 87 | |
| idle | proof alone | 50/50 | 0 | 23 / 25 / 25 / 26 | always 2 |
| go + xcodebuild | whole launch | 50/50 | 0 | 237 / 415 / 577 / 906 | |
| go + xcodebuild | proof alone | 50/50 | 0 | 31 / 45 / 126 / 666 | 2 to 9 |
| go + cargo + xcodebuild | whole launch | 100/100 | 0 | 141 / 232 / 415 / 456 | |
| go + cargo + xcodebuild | proof alone | 100/100 | 0 | 28 / 37 / 66 / 102 | 2 to 5 |

**Load.** The first loaded run meant to include cargo, but its cargo loop
never started because of a crate-name bug in the record, so it is labeled
go + xcodebuild. The load average was 23 two minutes in, and it reached 31
during that run and 25 during the second.

**What a pass saw.** Under load, a pass listed 834 to 933 processes, against
828 idle. Across the 150 loaded ends, the proof met two kinds of process it
can't judge yet, and waited out each one without timing out:

- 48 still being created or exiting;
- 102 gone or changed between the listing and the query.

**Survivors.** Every backend end wrote its three children's pids, and none
was alive afterwards. Every by-hand end killed exactly four processes: the
canary and the three children. So none of the owner's unconfined processes
was ever counted as a member.

**Queries across accounts.** One pass asked the proof's membership query
about every live process, with a tag no rule names:

- 189 to 208 of root's processes;
- 132 to 136 of other accounts', the owner's `b6n` and the system accounts;
- 489 to 562 of `agent`'s.

No query errored, idle or loaded. This shows only that a standard account
can query every process on the host. The kill counts above are the evidence
that the proof leaves other work alone.

**Inferred.** Load costs the proof extra passes, not correctness. Its tail
stays two orders of magnitude under the bound.

**Console:** none.

## Item 4: Screen and Input Permissions

**Observed.** The chain always started from the LaunchAgent, never from
Terminal or SSH:

1. the helper's `serve`;
2. the helper's `supervise`;
3. `sandbox-exec` with the rendered rule file;
4. `screencapture` or the probe tool.

The helper was signed with the fleet identity under the fixed identifier
`ai.freeside.spike.shared-host-helper`.

- **Which binary is charged.** The first capture attempt showed no prompt.
  System Settings then listed **helper** under Screen & System Audio
  Recording, switched off. The probe tool, `screencapture`, and
  `sandbox-exec` never appeared. The Accessibility request
  (`AXIsProcessTrustedWithOptions` with its prompt option, run inside the
  cordon) added **helper** to the list System Settings showed as Device
  Control and Data Access. The owner found no pane named Accessibility.
- **Admin credentials.** Every change to either switch, on or off, asked for
  an administrator's name and password: four times in all.
- **No restart of the helper.** Captures worked on the next request after the
  switch went on, without restarting the LaunchAgent.
- **The bypass prompt.** After Screen Recording was on, macOS showed
  "“helper” is requesting to bypass the system private window picker and
  directly access your screen and audio", and the owner chose Allow. On
  macOS 15 and later this prompt returns periodically. The spike ran too
  briefly to see it return, so its schedule here is inferred, not measured.
- **Replacing the binary.** A second build (`version=v2`) replaced the first
  in place. It had a different hash but the same identifier and certificate
  leaf, so the same designated requirement. After `launchctl kickstart -k`,
  capture, click, and typing all still worked. No prompt appeared and
  neither list changed.
- **Restart.** Both approvals survived a restart of the mini, with no
  prompt. The fleet session ran item 5's stage (`item5 s6-restart`) over SSH
  as `agent`, after `restore.sh` put the spike's files back and restarted
  the helper (still `v2`, in `Aqua`). The mini booted at 06:58:17 UTC and the
  stage ran from 07:02:04 to 07:02:22. `nolsopen open` and `default direct`
  each read both statuses true, captured the magenta center pixel, and
  logged the click and every key. `default open` failed with -54, as before.
  The owner saw no prompt at login or during the stage, and `helper` was
  still on in both lists. The disk was unlocked over SSH from the Studio with
  `agent`'s password, which logged nobody in; the owner then logged `agent`
  in at the console. Nothing touched TCC or the helper before the result was
  posted (<https://github.com/freeside-ai/freeside/pull/3604#issuecomment-7155450709>).
- **The log.** As `agent`, `log show` returns no `tccd` entries. So the
  attribution comes from System Settings, not from the log the plan named.

**Inferred.**

- **Updates must keep the identity.** The supervisor on each host has to keep
  a stable signing identity and identifier across updates. Otherwise every
  update costs two console visits with an administrator's password. An ad hoc
  signature would change with every build.
- **The approval travels with the identity.** A program signed with that
  identity and identifier inherits the approvals without a prompt. Any
  unconfined session on `agent` can sign one today, because the fleet's
  `sign` command works without a prompt.
- **A run could too.** A cordoned run can read the signing key (#3603).
- **The spike's approvals should go.** They stay until an administrator
  removes them (Cleanup).

**Console:** needed for every approval, with an administrator's
credentials. It repeats per host, per identity or identifier change, and
each time the bypass prompt returns, but not per restart.

## Item 5: Seeing and Driving the Desktop

**Observed.** A stand-in app, SpikeTarget, filled its window magenta and
logged every click and key it received. The run did four things:

1. started the app;
2. captured its window by id;
3. clicked the window's center;
4. typed `spike3596`.

Each effect was checked in the app's own log and in the capture's center
pixel, not by exit status.

| Screen Recording | Accessibility | Capture | Click and typing |
| --- | --- | --- | --- |
| off | off | fails: "could not create image from window" | dropped silently, exit 0 |
| on | off | works (magenta read back) | dropped silently, exit 0 |
| on | on | works | both arrive |
| off | on | fails | both arrive |

**What the matrix shows.**

- **Two separate gates.** Screen Recording gates capture and Accessibility
  gates input, each on its own.
- **The rule file doesn't matter.** The default file and the one without the
  Launch Services denial gave the same results. So the approvals reach every
  run the approved program starts (#3602).
- **Only `open` needs the denial dropped.** Under the default file, `open`
  fails with error -54. Starting the app's binary directly works.

**Side effects.**

- **Arguments are dropped.** An app opened through `open --args` from inside
  the cordon started with no arguments. The same command run unconfined
  passed them through.
- **Keystrokes land in the run's window.** While the stand-in window was
  frontmost on `agent`'s console, the owner's own keystrokes reached it.

**Before the console.** Before the owner switched the console to `agent`,
another session held it (`kCGSSessionOnConsoleKey` 0). The same actions
failed then too, but they ran with no permissions granted. So this spike
doesn't show whether a session off the console can capture once approved.

**Console:** a person approved each permission (item 4).

## Item 6: The Simulator

**Observed.** One launch ran under the rule file without the CoreSimulator
denial, and did all of this in about 90 s:

1. created a fresh iPhone 17 Pro on iOS 27.2;
2. booted it, in 19 s;
3. built `FreesideIOS` with the three sandbox-disabling settings, in 63 s;
4. installed and launched the app;
5. took a 1206 × 2622 screenshot.

The run's end proved. Just before the end, the proof saw five members: the
canary, the script's shell and its `sleep`, and two `ibtoold` processes.
`ibtoold`'s parent was launchd, but it ran under the run's rules, and the
proof killed it.

**What outlived the proven end.** With no `simctl shutdown`, these stayed
alive as `agent`, outside the rule file, and none read as a member:

- the device itself, still Booted, with its `launchd_sim`;
- about 285 of its iOS processes;
- the `FreesideIOS` app;
- `CoreSimulatorBridge`, `SimRenderServer`, and three `SimMetalHost`.

`simctl shutdown` ended the device's processes. Two launchd jobs the run had
started, `SimAudioProcessorService` and `SimStreamProcessorService`, outlived
both the shutdown and `simctl delete`. CoreSimulator's own services were
already running for `agent`, `b6n`, and root before the run.

**The runtime.** The host had no Simulator runtime. `xcodebuild
-downloadPlatform iOS`, run as the standard account, downloaded and installed
one: 8.3 GB in 5 minutes, with no administrator.

**Inferred.** A Simulator run needs its own teardown, a `simctl shutdown` and
`delete` of its device, because the proof can't end what launchd started. The
two leftover services are harmless but accumulate.

**Console:** none.

## Item 7: A Browser

**Observed.** Under the default rule file, headless Chrome loaded a page from
a local server the run started and took an 800 × 600 screenshot of it. The
screenshot's center pixel was the page's green.

| Flags | Result |
| --- | --- |
| none | exit 133: "sandbox initialization failed: Operation not permitted" |
| `--use-mock-keychain` | the same |
| `--no-sandbox` | screenshot |
| `--no-sandbox --use-mock-keychain` | screenshot |

- **One flag is needed.** That flag is `--no-sandbox`, because Chrome's own
  sandbox can't nest inside the cordon's.
- **The keychain errors don't stop it.** Chrome logged that its keychain
  lookup failed and that encryption was unavailable, which is the cordon's
  keychain denial, and it ran anyway.
- **It writes outside the run.** With `--user-data-dir` under the run, Chrome
  still wrote `~/Library/Preferences/com.google.Chrome.plist` and
  `com.google.Keystone.Agent.plist`.

**Not run.** Chromium itself, which the host lacks. Seatbelt's denials
couldn't be read either, because `log show` returns nothing to a standard
account. Chrome's own messages told the two failures apart instead.

**Console:** none.

## Item 8: Toolchains

**Observed.** One launch put the managers' directories ahead of the
fixture's `PATH`:

- `/opt/homebrew/bin`
- `/opt/homebrew/opt/rustup/bin`
- `~/.cargo/bin`
- `~/.local/bin`
- `~/.local/share/mise/shims`

Homebrew's rustup keeps its proxies in `/opt/homebrew/opt/rustup/bin`, not
`~/.cargo/bin`.

| Step | Result | Cache before |
| --- | --- | --- |
| `GOTOOLCHAIN=go1.26.5 go build` | downloaded go1.26.5, built, 5 s | build cache warm, toolchain cold |
| `uv sync --python 3.12` | downloaded CPython 3.12.16, made `.venv`, 2 s | cold |
| `cargo build` (one dependency) | fetched the index and the crate, built, 1 s | toolchain warm, registry cold |
| `mise install` (jq 1.7.1) | **failed**: "bad certificate format" from both mise's mirror and GitHub | cold |

Go took the toolchain from the environment rather than from a `toolchain`
line in `go.mod`. A `toolchain` line only makes Go download a release newer
than the installed one. Either way, Go downloads a toolchain and builds.

**mise's failure comes from the cordon.** The same install worked unconfined.
`mise-variants` ran it under rule-file variants, each with empty mise data
and cache directories so that every run downloaded:

| Rule file | Result |
| --- | --- |
| default | "bad certificate format" |
| without the keychain file denial | "bad certificate format" |
| default, with `SSL_CERT_FILE` set | "bad certificate format" |
| without the `com.apple.SecurityServer` and `securityd.xpc` lookup denial | installed |
| without both | installed |

So only the lookup denial matters (#3599).

**What the managers wrote outside the worktree.**

- **Go:** `~/go/pkg/mod`, 11,547 files including the toolchain module, and
  `~/Library/Caches/go-build`.
- **uv:** `~/.local/share/uv`, 1,705 files including the Python, and
  `~/.cache/uv`.
- **cargo:** a new `~/.cargo/registry`.
- **mise:** one new entry in `~/.local/state/mise/trusted-configs` naming the
  run's worktree, and one new link in `tracked-configs`.

**mise's trust (`mise-trust`, unconfined).** A plain `mise install` with no
trust given trusted the config itself and wrote a `trusted-configs` entry.
With `MISE_TRUSTED_CONFIG_PATHS` alone it wrote no trust entry, only a
`tracked-configs` link. So the run's trust entry came from its plain install
(inferred from the order of the steps), and it outlives the run (#3601).

**Console:** none.

## Item 9: Memory Pressure

**Observed.** `kern.memorystatus_vm_pressure_level` read the same unconfined,
inside a launch, and through the LaunchAgent chain:

| Condition | Unconfined | In a launch | LaunchAgent chain |
| --- | --- | --- | --- |
| idle | 1 | 1 | |
| build load (item 3) | 1 | 1 | |
| 16.25 GiB of incompressible memory held | 2 | 2 | 2 |

After the memory was released, it read 1 again within 5 s.

**Console:** none.

## Manual Steps for the Fleet Repository

| Step | Needs | Repeats |
| --- | --- | --- |
| Install the daemon's SSH key in `/etc/ssh/authorized_keys/agent` | root | once per host |
| Install the supervisor's LaunchAgent and binary, signed with a stable identity and identifier, where runs can't reach them | `agent` | on each update; a same-identity update keeps the approvals |
| Approve Screen Recording for that binary | admin credentials at the console | per host, per identity or identifier change |
| Approve Accessibility (Device Control and Data Access) for it | admin credentials at the console | the same |
| Answer the "bypass the private window picker" prompt | a person at the screen | on macOS's own schedule (inferred) |
| Unlock FileVault after a restart | `agent`'s password, over SSH; the fleet scripted it with no terminal in about 20 seconds | each restart |
| Log `agent` in at the console after the unlock | a person | each restart |
| Keep the signing key and the agent prompt clones out of a run's reach | fleet layout (#3603) | once |
| Download the iOS Simulator runtime | `agent`; no admin | per Xcode release |
| Install Chrome | admin (Homebrew cask) | Chrome updates itself |
| Sign with the fleet's `sign` command | `agent` | per build; it worked from the desktop session with no keychain prompt |

## Method

- **Throwaway test.** The record copied the test into the cordon package for
  each run and deleted it afterward. The test was gated on
  `FREESIDE_CORDON_SHARED_LIVE_TEST=1`, a `FREESIDE_*_LIVE*` name, so
  `scripts/check.sh` kept the real `HOME` and dropped its own sandbox.
- **No secrets.** Launches carried the package's fixture token. No step read
  a provider token, and no record holds a secret. Probes of secret paths
  report only whether a file opened.
- **Script launches.** Items 6 to 9 each ran a zsh script as one launch's
  agent, with a wrapper standing in for the agent CLI. The launch used the
  real configuration and the variant rule file the item names. Each variant
  is `rules.sb` with one line removed, swapped in through the package's
  `ruleTemplate` before `New`. Before releasing the run, the test listed the
  processes the proof would count as members.
- **Items 4 and 5.** The rule files and their `-D` parameters came from
  `RenderRules` and `sandboxParams` (`TestSharedRender`), and the helper
  started `sandbox-exec` with them.
- **Environment.** Credential-named variables were unset before every suite
  run, because the suite's environment check refuses them.
- **Load.** Each item 3 condition started its loops 90 to 120 s before the
  ends began, and the record logs the load average with each run.
- **Fixes after review.** A fresh-context review found three bugs in the
  record, and each was fixed:
  - **`rules-nosim.sb`.** It dropped both denials, not one. Item 6 renders its
    own variant, so no result used that file.
  - **`load-stop`.** It could leave a loop's last build running.
  - **`item5`.** It didn't rerun `open` under the default file. The
    dry run had.

## Cleanup

`zsh cordoned-shared-host.zsh cleanup` reverses what the spike can undo by
itself. The fleet session ran it after the restart check, and it exited 0:

- boots out the LaunchAgent and removes its plist and socket;
- restores `~/.zshenv` from its backup, checked with `cmp`;
- deletes the Simulator device and the copied test;
- removes mise's links to the spike's directories.

Also done after the restart check: `~/.freeside-spike-3596/`, the copy that
outlived `/private/tmp`, is deleted, and the owner removed `helper` from both
permission lists. The first run of `cleanup` left mise's empty
`incomplete-installs/jq` folder, because it used `rm -f` on it; the script
now uses `rmdir`, and the folder is gone.

These stay, and each needs the owner:

- **`~/.ssh/agent/`.** Launchd's `ssh-agent` created it while the spike's
  `~/.ssh` briefly existed, and still holds it open.
- **Caches the items filled:**
  - go1.26.5 in `~/go/pkg/mod`;
  - uv's CPython 3.12 and its cache;
  - `~/.cargo/registry`;
  - mise's jq installs in `~/.local/share/mise`;
  - Chrome's two preference files;
  - the 8.3 GB Simulator runtime;
  - the two leftover `Sim*Service` jobs.
