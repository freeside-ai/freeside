# The Supervision Contract: Mode, Exit Discipline, Stop, Liveness

**Decision.** Plan revision 27 writes the §5.2 supervision contract
(#453, pulled forward from its wave-7 parking by owner fiat). Four
calls, all owner-decided in the 2026-08-04 session: (1) Mac-first
supervision is a per-user LaunchAgent registered by the Freeside Mac
app through `SMAppService`, superseding the LaunchDaemon-first
direction endorsed in `2026-08-01-2221-supervision-contract-gap.md`;
(2) exit discipline classifies every fatal-channel writer as durable
in-process stop, restart-safe exit, or involuntary exit, so
restart-always becomes safe; (3) the stop-wait fork closes on the
unlimited side; (4) liveness is an unauthenticated `GET /health`
(schema landed in this unit) plus a fixed supervised listen address
and a readiness file replacing the one-shot stdout line. The
implementation core (#454's daemon side, plus a new app-side
LaunchAgent/menu-bar unit) lands in wave 5; the external ntfy probe
stays in wave 7.

## Why the LaunchDaemon-First Endorsement Was Superseded

The prior note endorsed a `KeepAlive` LaunchDaemon under a dedicated
user via the §5.2 elevation helper. Three assumptions changed:

- **Timing.** Revision 26's wave-4 row starts real-backlog daily use
  at wave-4 close; the operator would live with a terminal-launched,
  ephemeral-port daemon for two waves before the wave-7 fix. The
  prior note's own revisit trigger ("sooner by owner fiat") fired.
- **Platform fit.** The daemon drives Apple `container`, per-user
  tooling with unverified behavior under a GUI-less service account;
  a dedicated-user LaunchDaemon may not even run the workload. The
  operator login session is where the workload already works.
- **Cost.** `SMAppService.agent` registration from a bundle-shipped
  plist needs no elevation helper at all, and the app (verified
  unsandboxed, no entitlements in `app/Freeside.xcodeproj`) can use
  it directly; the elevation helper drops off the Mac-first critical
  path. The Docker Desktop model (GUI app owns a launchd-supervised
  local backend, menu bar reflects its health) is the platform's
  proven idiom for exactly this shape.

The LaunchDaemon is retained as the hardened end state (boot-time
start, logout survival, operator isolation), not deleted. Accepted
cost, stated in §5.2: a LaunchAgent daemon dies at logout and returns
at login, so unattended operation assumes a logged-in operator, a
bound already true of the terminal process it replaces.

## Exit-Discipline Rationale

The classification (plan §5.2) was made over a complete inventory of
the fatal channel at 19cfe4f: seven writers (`d.errs`, buffer 7,
`daemon/cmd/freesided/main.go:598`; HTTP serve, workflow reconcile,
local backups, the two §5.16 scheduler arms carrying doctor/janitor,
the production-publication lane, the active-resource reconciler),
plus panics and the startup-failure family in `run()`. Key judgment
calls:

- **Durable-stop over exit for recurring-on-restart conditions**
  (store I/O, correctness invariants, backup maintenance, classified
  doctor/janitor causes): under restart-always, exiting on these is a
  respawn loop that hides the fault; the durable stop (close
  unattended admission, file `system_health`, keep serving reads)
  makes it visible and non-resumable without a human, per §4's
  restart-never-reopens rule. This resolves the doctor source-error
  posture deferred by `2026-07-30-2350-operational-command-packaging.md`
  as a durable stop (superseding its interim fatal, per that note's
  own "pending the supervisor contract").
- **Transient external failures never stop or exit.** A doctor or
  janitor pass failing on an ambient network blip must not close
  unattended admission overnight (attention-spam is the product's
  cardinal sin); persistence, or a definitive classification such as
  revoked GitHub App authority, is what escalates to a durable stop.
  The consecutive-failure threshold is #454's to set.
- **HTTP serve is a restart-safe exit, not a durable stop**: with the
  API surface down the daemon cannot serve even read-only state, so
  "keep serving" is incoherent; a fresh bind plausibly clears it.
- **Startup failures stay process exits** because a pre-store failure
  cannot record a durable stop by construction; crash-looping becomes
  observable via `started_at` churn on `/health` (and the wave-7
  probe alarms on it).

## Rejected Alternatives

- **Daemon-embedded status item (Go systray).** Icon⟺process fidelity
  is strongest, but it forces an AppKit main loop and GUI-session
  coupling into the Go daemon forever, contradicts the hardened
  LaunchDaemon end state, and cannot show the state that matters
  most ("not running"). The app-resident menu bar shows both states
  because the app, not the daemon, draws it.
- **App-spawned daemon child process.** Fragile lifetime (dies with
  the app or is orphaned), invisible to launchd, no crash restart; the
  app registering the launchd agent gets durability with the same
  one-click UX.
- **launchd socket activation** for address durability: solves the
  ephemeral port, but puts launchd knowledge inside `daemon/`,
  violating the boundary rule; a fixed configured address plus the
  readiness file achieves the same durability portably.
- **Bounded stop grace.** Any finite grace recreates SIGKILL-mid-lease
  on the credential teardown that is unbounded by design; bounded
  credential-safe teardown is real engineering, deferred as hardening,
  not a plist tunable.
- **Richer `/health` payload** (admission state, doctor summary):
  rejected to keep the unauthenticated surface at the minimum for a
  dead-or-alive verdict plus skew/crash-loop evidence; operational
  state stays behind authentication (§4).

## Verification Findings

- The fatal channel and graceful shutdown currently converge on
  identical teardown and exit code (exit 1 even after a clean SIGTERM
  if teardown errors, `main.go:262-278`); the contract therefore keys
  restart semantics off "any self-initiated exit restarts," not exit
  codes.
- `MockServerTransport` routes by `operationID` switch (no generated
  all-operations conformance), so the `/health` schema lands without
  forcing a mock handler; the mock's auth comment was the only echo
  of the "one unauthenticated operation" claim in `app/`, and
  `daemon/internal/signet/http.go:35` carries the same stale claim
  for #454 to align when it adds the route.

Revisit when: unattended away-from-host operation (logout survival,
boot-time start) or a multi-user host is actually needed — that is
the LaunchDaemon hardening trigger; or if Apple `container` proves
workable under a service account, which weakens the platform-fit
argument above.
