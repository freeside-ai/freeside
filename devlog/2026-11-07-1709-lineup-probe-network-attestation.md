# Lineup Probe Network Attestation

Issue #2772. Tier A by path (`daemon/internal/ward/`).

## Decision

**Chose to attest the lineup probe's network name and host-only mode
after `InspectNetwork` and before its CONNECT proxy starts, over trusting
`CLIRuntime.CreateNetwork` to pass `--internal`.** The probe copies a real
provider credential (a Claude setup token or a Codex credential) into its
container, yet it checked only the network's ownership fingerprint. The
package rule is that the gate trusts nothing a Runtime returns, and
`devlog/2026-07-27-0130-exact-base-seeding-and-egress.md` already rejected
"trusting `--internal` without attesting the returned mode". The probe now
makes the same two checks as the handoff's provider network
(`prepareProviderEgress` in `egress.go`), in the same place: before the
ownership fingerprint and the proxy.

The gap was not reachable through today's runtime, since `CLIRuntime` is the
only production Runtime and always passes `--internal`. It would open if the
`container` binary's behavior drifted.

**Chose one check before the proxy over also re-checking before credential
delivery.** The issue asks for the handoff's pre-proxy check. The handoff's
second check before its writer runs (`handoff.go`) guards a longer window
between network creation and the writer; whether the probe needs one too is a
separate decision, left to the issue tracker if it comes up.

**Kept the errors plain.** The probe has never returned `CheckFailure`, and
its callers in `cmd/freesided` print the error as is.

## Refute-First Record

A fresh-context reviewer tried to prove the change wrong; the race detector
ran the probe tests three times.

- **A path to `CopyIntoContainer` that skips the checks: disproved.** Every
  caller (`probe-lineup`, Claude and Codex model discovery, Claude auth
  enrollment through model discovery) goes through `probeLineupImage` into
  `lineupProbeRun.probe`, whose order is create, inspect, attest, proxy,
  container, deliver.
- **A report that passes but names another network: disproved.** Passing
  needs the probe's own name, host-only mode, and then the run's random
  ownership label in the fingerprint. A Runtime lying in every field is
  outside the package's model, as for every gate in it.
- **Teardown after a refusal deletes the wrong thing or leaks: disproved.**
  With no fingerprint recorded, teardown proves ownership from the label on a
  fresh listing (`classifyEvidence`), the same state as a fingerprint failure
  before this change. The tests show no network or container left behind.
- **The test proves ordering: confirmed.** It blanks `IPv4Gateway`, so a
  proxy started before the check fails with "start lineup probe proxy". Run
  against the unfixed code, all four cases failed that way.

Revisit when `startConnectProxy` stops rejecting a blank gateway (the test's
ordering proof rests on it), or when the probe's network setup moves into a
shared helper-role runner (#2776).
