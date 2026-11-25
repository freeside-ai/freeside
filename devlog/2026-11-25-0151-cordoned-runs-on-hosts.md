# Cordoned Runs on Several Hosts

Chose to run cordoned writers and their checks on every enrolled host, as
that host's run account, over the plan's ward runners with pairing and
per-host token volumes. More machines are for capacity first, and a cordoned
run needs no egress proxy, handoff gate, or container runtime on the host, so
the runner design served wards only. Plan revision 137 rewrites §5.7's
Cordoned Runs, Cordoned Verification, and Execution Hosts, and the matching
rules in §§2, 3, 5.4 to 5.9, 5.14, 5.15, 6, 8, 10, 11, and 14 (#3597,
tracker #3462). Nothing is built yet.

## The Owner's Decisions

From 2026-11-24, recorded on #3597:

- **One daemon, on the Mac Studio.** No daemon per host.
- **Runs on every host are cordoned,** under Freeside's rule file with the
  agent CLI's own sandbox off. Cordoned is the first runner class; warded is
  second.
- **A host needs a desktop session.** There is no desktop-less mode.
- **On an extra host, runs execute as the standard account `agent`,** the
  account the owner's own unconfined agent sessions already use there.
- **Every stage is independent in principle** and runs on any capable host,
  except where locality argues otherwise.
- **Local verification is the agent checking its own work.** The
  independent check is the project's CI and the owner's review.
- **Scale:** two or three hosts now, six or seven at most. A step a person
  repeats per host or per run is a defect.
- **Each project's own files pick tool versions** through the native
  manager; Freeside resolves none, and no manager reads a repository's
  config outside a confined run.
- **The day-one list:** screenshot a Mac app window, use the iOS Simulator,
  view a web page, click and type, build and test with warm caches, fetch
  over an open network, keep helpers alive across a run, and let the owner
  watch and steer.
- **Fleet setup is out of scope;** a separate fleet repository owns
  accounts, installs, OS updates, disk unlock, and restarts.

The owner also accepted the design on the issue: no stored credential on a
host, the Simulator and desktop control as standing project settings,
verification after import on a fresh checkout, steering brought forward,
the placement rule, image-free onboarding, a fixed version-manager `PATH`,
a per-host count with a default and a pressure hold, host trouble as a
state, and host loss as the owner's call.

## Open Decisions and How the Text Settles Them

The owner didn't answer the issue's open decisions before implementation,
so the revision writes each recommended answer, and the PR lists them for
the owner to overrule:

- **Network:** the open network stays the profile on every host; #3476 and
  #3477 are held until extra hosts run.
- **Reaching a host:** mechanism, kept out of the plan (below).
- **The host binary:** the plan commits to the result. A host runs the
  daemon's build or is ineligible, and no run can replace the program that
  starts runs, directly or through a file it wrote. #3112 shrinks to a
  version check.
- **What holds a check:** the writer's cordon, as the run account, with no
  credential and no agent, its end proven as a writer's is. Revision 135's
  class, its four capabilities, and the writer denials it added for the
  verifier's ports, preferences, and toolchain leave the plan.
- **A command that names the project image's room** stays in a ward on the
  Studio; a project that wants every check on a host onboards with no image.
- **Which candidates get the self-check:** revision 135's rule stays for a
  project with an image. A candidate an uncordoned run helped write is
  checked on a host whose run account isn't the owner's, recorded as
  uncordoned. A project with no image sends every candidate to the
  self-check.
- **Later checks** (remediation, re-entry, a moved tip's handoff) are fresh
  checkouts and go to any eligible host by placement.
- **Evidence capture** doesn't change; #472 keeps verifier screenshots.
- **Enrollment:** the operator names each host once, the daemon
  authenticates a host before each launch, and nothing a run can read poses
  as the host or the daemon. §5.9's host key doesn't apply.
- **Warded launches on another host** leave the plan body; they run on the
  Studio until a later revision (tracker #2161).
- **Uncordoned admission:** `unattended` admits one on a host whose run
  account isn't the owner's, on its own proof keeping every denial the
  setting doesn't lift; the Studio is never eligible; no eligible host means
  a visible hold.
- **The count** is per instance and per host.
- **Self-hosted Actions runners** are prohibited on every enrolled host.
- **Steering** comes forward on a tracker started by fiat, as revision 132
  did for the live view; ACP interactive attachment stays in Phase 3.
- **Desktop control:** a rule and a fallback. The rule: a run without the
  setting can't capture or post input, and the host's proof shows it. The
  fallback until #3602 proves that: a host holding the approvals runs only
  projects with the setting.
- **Capture and input** are one setting. The daemon's host never honors it,
  so it runs no cordoned work while the program that starts runs there
  holds either approval (found in review).
- **Console steps** are fleet setup that macOS requires, shown as host
  states; desktop control runs only while the run account holds the console.
- **A Simulator run** exports only after the host shuts down and deletes its
  devices, beside the run-end proof; the two surviving launchd services are
  a residual.
- **mise** waits on #3599 and #3601; a project that needs it isn't run
  cordoned until both land.
- **The owner's files on `agent`:** #3600 and #3603, merged while this
  revision was written, deny what the spike found, so the first extra-host
  run waits on #3602 alone. The plan says Freeside stores no credential on
  an extra host, not that a host holds none.
- **The browser:** a headless browser is standard, with its own sandbox off
  and the cordon as its only wall; one clicked on screen is desktop control.
- **The launcher** can't be reached by a cordoned run, proven by the suite,
  and nothing on the run account can replace it. The owner's unconfined
  sessions are inside the host's trust, and so is code a run plants for
  them, such as a shell startup file, and what an uncordoned run starts
  outside the rule file: the plan names that as the shared-account
  residual rather than claim no run reaches the launcher.
- **Tools Freeside runs on a host** come from paths no run can write; a
  check's own commands use the run's `PATH`, which is why it proves no clean
  room.

## Mechanism the Plan Leaves Out

The plan says only that the daemon reaches an authenticated host. The #3596
spike measured the mechanism the unit that builds it should use:

- **Transport:** a `freesided` LaunchAgent in `agent`'s desktop session
  serves the cordon backend on a Unix socket, reached through an SSH socket
  forward (`ssh -N -L`), which runs no login shell. The daemon's key goes in
  the root-owned `/etc/ssh/authorized_keys/agent`.
- **Install:** the fleet repository installs the program that starts runs
  root-owned, outside every path the run account can write, so nothing that
  runs as `agent`, a run or a file a run wrote that an unconfined session
  later sources, can replace it.
- **Signing:** the program keeps a stable signing identity and identifier,
  since macOS ties the screen and input approvals to both and a change costs
  two console visits per host.

## Earlier Decisions This Revises

- **Cordoned runs on the daemon's host only, as the owner** (revision 134,
  [note](2026-11-20-2237-cordoned-runs.md)). The owner's decision above.
- **Execution hosts through a dial-out runner, pairing, and per-host token
  volumes; placement preferring the daemon's host and banning cordoned
  placement** (revision 94,
  [note](2026-10-22-1042-execution-hosts.md)). Those parts served wards; the
  design stays in the history for warded launches.
- **Cordoned verification on the owner's account under a verifier rule
  file** (revision 135, [note](2026-11-21-0854-cordoned-verification.md)),
  and the clean-room goal in §2 and §6. The owner's statement that local
  verification is a self-check replaces them.
- **Revision 135 rejected verification on the Mac mini** as not its first
  job, and sent a candidate an uncordoned run helped write to the slot
  executor. Checks now run on hosts, and such a candidate is checked on a
  host whose run account isn't the owner's.
- **The Simulator and desktop control as per-run grants** (revision 134,
  #3473, #3478). The reason was the owner account's reach (#3526); on
  `agent` the reach is what the owner's unconfined sessions already have.
- **One operator-set cordoned cap per instance with no default**
  ([note](2026-11-22-2055-writer-class-and-launch-cap.md)). Decided for one
  host; at seven hosts it is seven hand-set numbers.
- **The live view is read-only and steering stays in Phase 3**
  ([note](2026-11-19-1526-live-writer-view.md)). The owner named steering a
  day-one need.
- **Onboarding always builds a project image**
  ([note](2026-07-26-2130-project-images-belong-to-onboarding.md)). Python,
  bash, Rust, and Elixir projects have no builder, and a cordoned-only
  project needs no image.
- **A host needs only the runner protocol version, not the daemon's
  build** (revision 116). The owner's design has the fleet install the
  program that starts runs, so a host runs the daemon's build or takes no
  new launch, and #3112 shrinks to a version check. The fleet updates a
  host only once it holds no launch, so its runs drain; a run the daemon
  can't read back across builds is recovered as a returning host's are.
- **The Mac mini's first job is wards, once a start trigger holds**
  ([note](2026-11-18-2202-mac-mini-first-host.md)). Its first job is now
  cordoned runs.

## Rejected

- **The plan's dial-out runner with pairing.** More than a cordoned run
  needs, and a host credential that unconfined sessions on `agent` could
  read.
- **A command over SSH per launch.** sshd runs it through the login shell,
  and a cordoned run can write shell startup files, so a later launch's
  token would pass through code a run wrote.
- **Per-run grants for the Simulator and the desktop.** Recurring owner work
  on the common path.
- **A broker for Simulator operations.** `xcodebuild` and `simctl` talk to
  the Simulator directly.
- **A per-run Simulator device set as isolation.** Nothing makes a run use
  it.
- **Verification in the writer's worktree first.** It needs new binding
  logic where a fresh checkout reuses the verifier.
- **Ending a run lost automatically when its host is unreachable.** A
  network fault at the Studio would duplicate every running writer.
- **Killing a run under memory pressure.**
- **Capability labels and tool probes per host.** The fleet keeps hosts
  uniform, and every run's record names its host.

## Evidence

**The council's confirmed findings,** from Opus 5.5, GPT-6.2 Sol, and Gemini
4 Neon in two rounds, checked in the code at `949ea7f6`:

- **SSH runs a login shell a run can rewrite:** `daemon/internal/cordon/rules.sb`
  denies no shell startup file.
- **The candidate commit is authored at import** and a verdict is bound to
  it (`daemon/internal/engine/production_verification.go:540`), so a check
  before import can't name the published head.
- **The verifier already builds a fresh workspace per command**
  (`daemon/internal/verify/verify.go:230`).
- **Proofs take the recipe digest from the project image**
  (`production_verification.go:540`,
  `daemon/internal/engine/production_handoff.go:347`), so image-free
  onboarding needs a recipe binding of its own.
- **The run-end proof finds only sandbox members**
  (`daemon/internal/cordon/runend.go:128`), so Simulator devices and typed
  commands escape it.
- **A pass needs no unsettled process anywhere on the host**
  (`runend.go:136`); #3596 measured it settling on a busy host.
- **Conformance records carry no host**
  (`daemon/internal/store/conformance.go:43`), so identical hosts would
  share one pass; the plan binds each pass to its host.
- **A blocking `system_health` item stops all unattended admission**
  (`daemon/internal/store/unattended.go:401`), so host items are advisory.
- **The suite expects the Simulator's service denied**
  (`daemon/internal/cordon/suite.go:71`), so the Simulator setting needs its
  own suite variant.
- **A cordoned launch is a one-shot `-p` run**
  (`daemon/internal/exec/claude/spec.go:794`), so steering needs streamed
  input and a supervisor path.

**What the #3596 spike measured**
([report](../docs/spikes/cordoned-shared-host.md)), on the Mac mini as
`agent`, with stand-ins for the daemon's launch:

- **Holds:** the SSH socket forward to a LaunchAgent with no login shell
  (item 1); the conformance suite as `agent` (item 2); the run-end proof for
  200 of 200 ends beside unconfined builds, at most 666 ms, killing only the
  run's processes (item 3); a Simulator build, boot, and screenshot under a
  rule file without the Simulator denial (item 6); a headless browser
  screenshot (item 7); Go, uv, and cargo fetching and building (item 8);
  memory pressure reading the same inside a launch (item 9).
- **Departs from the design:** the screen and input approvals belong to the
  program that starts runs, so they reach every run (items 4 and 5, #3602);
  a person is needed at the console for the approvals and after each
  restart (items 1 and 4); a Simulator device outlives the proven end
  (item 6); mise can't install under the cordon (#3599) and leaves a trust
  entry (#3601); the rule file misses files the owner keeps on `agent`
  (#3600, #3603, since fixed); a run could reach the stand-in launcher (#3602); and the
  browser runs with its own sandbox off (item 7).
- **Left unmeasured:** a real launch (no agent CLI, provider token, or
  verifier ran on the host, and no steering); desktop capture and input off
  the console; tool versions beyond the three runs; denials the account
  can't show; Chromium, an Apple-issued signing identity, and how often the
  screen-capture prompt returns.

The plan rests each sentence on a measured item, or says it is unmeasured
and names its issue.

## Refute-First Review

One fresh-context reviewer read the uncommitted diff and tried to refute it,
beside three checks: a grep sweep of the plan for every stale phrase the
issue named, a script that resolved every link and anchor in the changed
files, and `bash scripts/check.sh docs`. The checks found nothing. Each of
the reviewer's findings, and its outcome:

- **Confirmed, fixed: who can replace the launcher.** Execution Hosts said
  the owner's sessions can replace it and no run can, through a file it
  wrote; with a root-owned install neither holds, and a run's startup file
  or an uncordoned run's processes reach it through those sessions. The
  plan now says nothing on the run account can replace it, that code a run
  plants for the owner's sessions and an uncordoned run's processes are
  inside the host's trust, and that a cordoned run itself can't reach it.
  §13 and the §14 row match.
- **Confirmed, fixed: "no host stores a credential"** was false for the
  daemon's host, which is the first execution host. Five places now say an
  extra host.
- **Confirmed, fixed: an approved host off the console** read as open to
  work without the desktop setting. The fallback now holds whatever the
  console state.
- **Confirmed, fixed: one desktop run at a time** couldn't hold while the
  approvals reach every run. The daemon admits one run with the setting per
  host, and the plan says an approved host therefore runs one run at a
  time until #3602.
- **Confirmed, fixed:** the Actions-runner ban said "enrolled host", which
  §5.9 now defines narrowly; it says execution host. The count of host-side
  exceptions read two, two plus one, and three; all say three.
  `supports_cordoned_daemon_protection` and the §14 row had dropped clean
  verification from what a run can't write, though the slot executor still
  verifies on the daemon's host; restored. A host declared lost now also
  shows its lost Simulator runs' devices gone. §4's `advisory` posture now
  allows an item to clear when its diagnostic ends.
- **Confirmed, fixed: revision 116.** Requiring the daemon's build
  reverses revision 116's protocol-version rule. The plan says so, makes
  another build a fourth host state, keeps a launch held on an older build
  under the host-lost rule, and this note lists it.
- **Confirmed, fixed: §11's revision 96 paragraph** still named the
  runner's verification room and a second Mac as an execution host for the
  UI-writer tier; it now points to revision 137.
- **Confirmed, fixed: stale dependencies.** #3600 and #3603 merged on
  `main` while this was written; the plan names #3602 as the only wait.
- **Second round, at the pre-push review (no blocker):** code a cordoned
  run plants in a shell startup file runs with the account's whole reach,
  other runs' checkouts, later runs' tokens, and the host's reports
  included; the §14 rows now say so instead of "a run that escaped the
  cordon". The daemon's host had no rule for an approved launcher; it now
  runs no cordoned work while that program holds either approval, and the
  §14 row names input beside the screen. Denial 5 lists clean
  verification again; a host on another build drains before the fleet
  updates it; the enrollment sentence no longer says nothing on a host can
  reach the launcher; and the browser's two preference files are named.
- **Allowed, in the PR for the owner:** a self-check's capture isn't
  clean-room, so a render can carry what the run account can read; capture
  rule 1 and the §14 row name it, and the issue's evidence decision left
  the capture rules unchanged. Denying writes to shell startup files on
  extra hosts would narrow the planted-code path, and is the owner's call.
  Which image a no-image project's warded specifier and reviewers use is
  left to the unit that builds image-free onboarding.
- **Checked clean:** every spike-backed sentence against the report's
  tables, the single-writer rule, the publication re-gate, the
  non-waivable list, and the history entry against §13.

## Revisit When

- **#3602 lands:** if a host proves that runs without desktop control can't
  capture or type, the fallback that dedicates an approved host to
  desktop-control projects goes.
- **Rebuild time costs real capacity:** checking in the writer's own
  worktree returns as the first step.
- **A host the owner doesn't physically hold** (#3421): the trust a host
  gets, and who is inside it, need revisiting.
- **The network rule:** when extra hosts run, the owner revisits #3476 and
  #3477 against "search and read anything, send nothing" (#3464).
