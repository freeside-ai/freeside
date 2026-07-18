---
run: issue-78
stage: ward-networkless-export
date: 2026-07-17
branch: feat/network-free-exporter
---

# Networkless exporter proof

## Decision

- **Chose Apple container 1.1.0's public `--network none` mechanism** over a
  named isolated network or private runtime-state modification. The pinned
  source maps `none` to an empty attachment set, while an isolated network is
  a host-only or NAT topology rather than an absence of networking.
- **Named the property `supports_networkless_export`.** The capability states
  the security boundary policy needs, not the Apple-specific mechanism that
  realizes it. Its shared vocabulary lands separately in #155.
- **Require structural and behavioral evidence.** The exporter spec always
  requests no network, pre-start inspection must explicitly report zero
  attachments, and the full suite runs both DNS and direct-IP connection
  attempts with tool-presence guards. A new backend and any failed Full rerun
  omit the capability, so unattended admission and PreJob fail closed.

## Verification finding

On the reference host, Apple container 1.1.0 (commit `5973b9c`) ran a fresh
Alpine 3.22 VM with `--network none`; `configuration.networks` was an explicit
empty array, only loopback was present, and both `nslookup example.com` and a
TCP connect to `1.1.1.1:443` failed. The uniquely named probe containers were
removed after inspection. The permanent live `Suite.Full` and subsequent
`Suite.PreJob` passed with the new probe, and post-run runtime listings showed
no surviving containers or volumes. The first live run also exposed Apple
container's 64-byte ID ceiling; the suite now uses a shorter common prefix and
pins every current role against the longest valid run ID.

## Refute-first findings

- **Confirmed:** the public no-network sentinel and the inspected empty set
  agree on 1.1.0, and both live egress attempts fail with the required tools
  present.
- **Rejected by verification:** an omitted `networks` field, any nonzero
  attachment count, a missing or malformed behavioral proof, cleanup failure,
  and a panic all keep the capability absent. Repeated race-enabled runs kept
  the capability state and admission result consistent. Automated review found
  that the actual handoff allowlist initially relied on the CLI's aggregate
  field-presence bit; the independent `NetworksObserved` gate and its negative
  fixture now make the same fail-closed rule binding for every Runtime. A later
  review found the behavioral proof's fixed path could accept a preexisting
  image file; both its path and exact content now carry the invocation's
  unpredictable ownership token, and a stale-file fixture is refused. A
  subsequent review found that the finite behavioral payload could self-mask
  eager execution on a runtime that treats only network-disabled creates
  specially. The refute-first pass then found that a flag-only liveness probe
  still missed runtimes specialized on the production exporter image and
  read-only workspace mount; the separate nonterminating probe now mirrors that
  full create topology before the finite egress attempts run. The next review
  found that a present but invocation-incompatible `nc` could turn a usage
  failure into a false blocked-egress witness; the probe now requires the
  pinned BusyBox `-w` and `-z` help contract and rejects usage/invalid-option
  diagnostics before writing proof. The following pass found the same class in
  the DNS witness, which now requires the pinned `nslookup` help contract and
  the reference runtime's explicit no-server timeout diagnostic. It also found
  that overlapping Full passes could let an older success republish after a
  newer failure; generation-checked publication now lets only the newest pass
  change the capability state. Refute-first follow-up also found that a panic
  from a late cleanup defer could publish after the nominal proof completed;
  the publication defer now records failure and re-panics before any
  capability can escape. A controlled two-run overlap and a late-cleanup panic
  regression pin both orderings.
- **Accepted by decision:** the DNS name and direct-IP endpoint are behavioral
  witnesses, not availability authorities. Endpoint failure alone is
  insufficient; the explicit empty attachment set is the load-bearing proof.
  An exact-output-spoofing fixture image remains outside this probe's boundary:
  `SuiteFixture.AgentImage` is a trusted benign input, and a malicious image
  could forge every guest-produced witness, so its provenance belongs at the
  trusted configuration boundary rather than in output-string heuristics.

Revisit when the reference runtime changes its no-network CLI or inspected
configuration shape, or when another backend needs a different mechanism to
prove the same capability.
