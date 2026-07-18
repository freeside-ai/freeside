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
  the capability state and admission result consistent.
- **Accepted by decision:** the DNS name and direct-IP endpoint are behavioral
  witnesses, not availability authorities. Endpoint failure alone is
  insufficient; the explicit empty attachment set is the load-bearing proof.

Revisit when the reference runtime changes its no-network CLI or inspected
configuration shape, or when another backend needs a different mechanism to
prove the same capability.
