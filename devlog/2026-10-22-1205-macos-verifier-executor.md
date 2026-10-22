# Linux-First Cores and a Credential-Free macOS Verifier

Plan revision 96. Mandatory note: an architecture and safety-policy change.
A host-side executor is admitted under `unattended`, where revision 92's
§10 rule allowed none, and the macOS execution class that revision 92 left
open is decided. Owner direction of 2026-10-21 and 2026-10-22; the spikes
below ran on the reference host on 2026-10-22. Rebased at the lander's
hand-back over revision 94 (execution hosts) and revision 95 (proxy
credential injection): the executor is placed as a runner's verification
room, and the case against a host-side writer now rests on the agent it
runs, not on the token it would hold.

## Decisions

- **Linux-first.** Chose to keep agent development for a Swift project in
  Linux wards and to use macOS only to verify, as briefly as possible, over
  a macOS execution class that writers run in. The deciding facts: the
  macOS two-guest VM limit is a kernel quota per Apple host
  (`hv_apple_isa_vm_quota`), not only a license term, and its one bypass, a
  KDK kernel with SIP off, is rejected; and the reference host (Mac Studio
  M1 Max, 10 cores, 32 GiB, 82 GiB free) cannot hold even two Xcode guests
  beside the daemon and its Linux wards. A writer ward is where the provider
  credential lives, so it stays in the proven Linux boundary.
- **One credential-free macOS verification executor, pooled slots.** Chose
  one host-side installation reused for every managed project, with
  concurrency from pooled unprivileged accounts (one per slot) rather than
  from machines or VMs. Verification holds no provider or GitHub credential
  and no placeholder, and runs no agent, so it sits outside the credential
  modes; that is why it is admitted now, where revision 95 makes a host-run
  writer context defensible only under `subscription_injected`. The
  executor is the runner's verification room on a macOS execution host
  (revision 94): the daemon host in-process, or a second Mac enrolled in
  the runner role. Each job is confined by per-uid packet-filter
  rules, a sandbox profile that denies network and confines writes to the
  job directory, a digest-keyed offline dependency cache that fails closed
  when the workspace's `Package.resolved` does not match, and a home reset
  between jobs. Rejected: verifying in macOS VMs (the quota and memory
  above); a host-side writer now (it runs an agent outside the ward
  boundary and holds the token as a file until injection lands); sharing
  one account across slots (no uid boundary between concurrent candidates).
- **Admitted in `unattended`.** Chose to name the executor as the one
  exception to §10's "host-side runners are `ephemeral` only" and to require
  its own capability declaration, `supports_credential_free_host_verification`,
  from the full conformance pass, with the pre-job probe rechecking egress
  and write confinement. Rejected: keeping it `attended_dev`-only, which
  would make every Swift project's unattended runs impossible, defeating the
  point of the class.
- **UI writers wait.** Writer work that needs macOS itself (running the app,
  driving its UI, an Xcode-only build) takes a VM or a second Mac enrolled as
  an execution host (revision 94). A writer in a VM guest holds its token
  under `subscription_contained` as a ward does; a host-run writer context
  waits for `subscription_injected` (revision 95). Neither is scheduled,
  Phase 4 keeps it, and the bootstrap exception narrows to SwiftUI work that
  needs a macOS writer.
- **Swift is a third project-image toolchain**, keyed by `Package.swift` and
  `Package.resolved`, with the closure baked as bare mirrors discovered by
  one proxied resolution. Rejected: trusting `Package.resolved` as the whole
  closure. The Straylight experiment showed it omits OpenCombine (a
  Linux-only transitive dependency of combine-schedulers) and GRDB's
  SQLiteLib submodule.

## Spike Evidence

Both gates were spiked before any plan text, on the reference host (macOS
26.7.1, Xcode 26.6, Swift 6.3.3), as a freshly created standard account with
no login session.

### Spike 1: ImageRenderer Without a Window-Server Session

Passed. A probe binary mirroring `app/Tests/FreesideCoreTests/
ScreenshotCapture.bitmap` (an explicit `CGContext` through
`ImageRenderer.render`) and the `cgImage` path ran as the new account
`fsverify` (uid 502) in three ways. Under `sudo -u` from the operator's GUI
session the process still saw a CG session and one display, so that launch
is not a valid test. Under `launchctl asuser 502`, the account's own
launchd namespace with no Aqua session, `CGSessionCopyCurrentDictionary`
returned nil and `CGGetActiveDisplayList` counted zero displays; both
raster paths produced a non-blank 420×83 image and a PNG, and the same held
inside `sandbox-exec -p '(version 1)(allow default)(deny network*)'`.
`NSWorkspace.shared.accessibilityDisplayShouldIncreaseContrast`, which the
suite reads, answered `false` rather than failing. The executor's launch
path must therefore be the slot's own launchd context (a per-uid
`launchctl asuser` or a LaunchDaemon-owned child), never a child of the
operator's GUI session.

### Spike 2: Per-Uid Egress Enforcement

Partial, and the design changed accordingly. `pf.conf` `user` rules parsed
(`pfctl -nf`) and loaded into an anchor under `com.apple/*`, which the
stock main ruleset still evaluates beside Mullvad's anchor; pf was already
enabled, and the enable reference was released after the test. With
`block drop out quick user fsverify` and the `in` twin loaded, the slot
uid's `curl` to `https://github.com` resolved and then timed out on
connect, direct-IP HTTP timed out, and direct UDP DNS to 1.1.1.1 got no
answer, while the operator uid's control run succeeded on the first two
(direct DNS is blocked by the VPN for every uid). Two channels stayed open
for the slot uid: the system resolver (`dscacheutil`, served by
mDNSResponder's socket, not the uid's) still returned github.com's
address, and loopback TCP and HTTP to a listener another uid owned
succeeded. A follow-up loaded a plain `on lo0` port rule with no user
match, and it did not block either: pf does not filter loopback on this
host at all, so the gap is not a user-matching failure. The sandbox
profile `(deny network*)` closed both in the
operator-uid run earlier that day: the resolver returned nothing and the
loopback connection was refused. So the sandbox profile is the primary
network confinement and the packet filter is the second layer for off-host
traffic; the plan's probe refutes loopback and the resolver under the
sandbox and off-host reach under both.

## Linux Portability Evidence

Straylight (pure SwiftPM, macOS 15 / iOS 18) in the official Swift 6.3 image
under Apple container (6 CPUs, 8 GB): with the mirror recipe (bare clones of
every `Package.resolved` location plus OpenCombine and SQLiteLib,
`mirrors.json` for both URL spellings, git `insteadOf` and
`protocol.file.allow always` in the guest, `swift package resolve` then
`swift build --skip-update --target X`), `StraylightConfig` builds in 15 s;
Core, Persistence, and Sync fail on `import SwiftData` (11 files across 5
targets, 4 in Core); GRDB needs `libsqlite3-dev`. Straylight's split is
straylight-ai/straylight #379, #380, #381. Freeside's `app/` gates
`FreesideCore` off Linux wholesale with one `#if !os(Linux)`; `FreesideAPI`
builds on Linux; 39 of 75 `FreesideCore` files import a UI framework, and the
only other Apple-only imports are `Network`, `Security`, and `CryptoKit` in
four files.

## Revisit When

A macOS release changes `hv_apple_isa_vm_quota` or Apple documents a
supported way past it; a second Mac joins the deployment (the executor moves
there and the residual shrinks); a Swift project needs a macOS writer for
real work (schedule the UI-writer tier); or the executor's conformance
finds a confinement the spikes did not cover, such as a recipe that needs a
window-server session after all.
