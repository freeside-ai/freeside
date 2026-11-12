# An App Decides Compatibility From a Level and a Floor

Issue #3109 (tracker #3113). Tier A by path (`daemon/internal/signet/http.go`),
so a mandatory note.

Chose a compatibility level and floor in the spec's `info`, reported on
`/health` and judged by the app alone, over matching contract digests,
because an app and a daemon from different commits are normal now and only
a change that breaks older apps should stop one.

## Decision

- **Two integers under `info`.** `x-compatibility-level` counts breaking
  changes and `x-compatibility-floor` is the lowest app level a daemon from
  the spec serves. Neither goes down, and the floor never exceeds the
  level. Both start at 0, the value every earlier build implicitly had, so
  a daemon that omits them reads as 0/0.
- **An app's minimum daemon level is its own level.** An app below the
  daemon's floor shows "this app is too old"; an app above the daemon's
  level shows "this daemon is too old". There is no second number for the
  app's minimum, because an app built against level N may use anything the
  level N spec has.
- **The app decides; the daemon never refuses on version.** The daemon only
  reports. Strict command decoding and digest-bound decisions already keep
  a stale app from doing harm (plan §5.14, Version skew).
- **The health read now runs ahead of each refresh round.** #1266 put it
  beside the round to save a round trip; the verdict now decides whether
  the round reads or resends anything, so it has to come first. The cost
  is one round trip per round. `.incompatible` is sticky: only a round's
  health read sets or clears it, so a read already in flight can't paint
  over the refusal, and sync resumes with no relaunch.
- **One refusal screen replaces the whole inbox, and the Mac's Settings
  window too.** The cache stays on disk but nothing reads as current or
  actionable while it shows. Settings sits outside the main window's root,
  and its panes read devices, resend routing and usage commands, and open
  System Health cards, so it takes the same screen.
- **`/health` itself never changes shape.** An app that can't decode it
  can't learn it was refused; an unanswered health read runs the round as
  before rather than blocking sync on a slow or flaky endpoint.
- **A `compat` step checks the level is honest.** It runs oasdiff v1.35.0
  between the merge base's spec and the head, and fails when an ERR-level
  change arrives without the level rising, or when either number goes
  down. The pin is a deliberate choice: v1.35.0 is older than two weeks,
  while v1.37.0 was the newest. CI installs a checksum-verified binary;
  local runs use `go run` of the same version.
- **`api/oasdiff-severity.txt` adjusts five rules.** A new `oneOf` member in
  a response body or property stays an error, since an older app can't
  decode it. A removed request property becomes an error, since the daemon
  rejects a request field it doesn't know and an older app still sends
  it. A new response enum value is demoted to info, because the
  generated client tolerates unknown members (#1266). That leaves one gap:
  widening a one-value response enum isn't caught, which api/README.md
  states.

## Not Verified

- **Calibration was not run.** The plan asked for oasdiff over the spec
  commits from 2026-11-05 to 2026-11-11 to check the severity file against
  real history. This session's permissions refused both downloading the
  binary and `go run` of the module, so neither that run nor a real `compat`
  run against this branch happened here. The script's logic is covered by
  `scripts/test-check.sh` with a stubbed oasdiff, and CI's api job runs the
  real binary.

## Refute-First Findings

A fresh-context reviewer tried to break the change before commit.

- **Confirmed and fixed:** the Mac Settings window still read from and
  wrote to an incompatible daemon; and models' direct `.unauthenticated`
  writes could clear the refusal. Settings now shows the refusal, and every
  freshness write outside the health read goes through
  `InboxStore.publish(_:)`, which keeps it.
- **Confirmed and fixed:** oasdiff rates `request-property-removed` WARN,
  so the check missed a break api/README.md names; it is pinned to `err`.
- **Allowed by decision:** an unanswered health read with no earlier
  verdict runs the round. Holding sync on it would turn a slow `/health`
  into an outage, and a compatible-looking daemon still meets strict
  command decoding. The shape freeze above covers the one way it could
  fail open for good.
- **Disproved as reachable:** the coordinator's public read entry points
  (`bootstrap()`, `refreshRuns()`, timelines) carry no gate of their own,
  but every production caller sits in a view the refusal replaces.

## Rejected

- **Keep the digest as the gate.** Every additive change would refuse every
  older app, which is the exact-match rule #3109 replaces.
- **A list of supported versions.** More to keep honest than two integers,
  and nothing needs it yet.
- **The daemon refusing old clients.** It would make a stale phone unable
  even to show why, and the existing command checks already make an old
  app harmless.
- **Read health beside the round, as before.** The round's reads and any
  resend would reach an incompatible daemon before the verdict lands.
