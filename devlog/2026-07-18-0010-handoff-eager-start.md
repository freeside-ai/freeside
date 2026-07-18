# Handoff eager-start regression

Work unit: #152. Mandatory note: credential-leak and returned-object-trust
boundary. Source: `devlog/2026-07-17-1001-ward-conformance-suite.md`.

## Keep the existing gate and add the missing lifecycle proof

Current `main` already fails closed when either pre-start `Inspect` report is
not `StateStopped`: `verifyAgentAllowlist` assigns the failure to
`control_plane_isolation`, and `verifyExporterAllowlist` assigns it to
`exporter_allowlist`. Those checks and their direct violation cases landed with
the original handoff backend, before #152 was filed. Chose a test-only change
over rewriting or duplicating the production checks because the remaining
acceptance gap was the issue's induced-failure fake test through `Handoff`.

The lifecycle regression makes the fake report `StateRunning` for the agent and
exporter in separate cases. Each case requires the expected typed conformance
failure, proves the affected `StartContainer` call never occurs, and proves
teardown reaps all containers and volumes. Covering both payloads closes the
same no-eager-start class at both pre-execution inspections, even though the
issue requires only the agent as its explicit induced failure.

## Refute-first verification

- Confirmed: fresh inspection of the production path found both stopped-state
  comparisons before their corresponding start calls. No runtime behavior
  change was needed.
- Rejected by execution: the induced `StateRunning` reports cannot reach an
  explicit start. The new full-handoff cases return the expected check-specific
  failures and observe no start call.
- Rejected by teardown evidence: both induced failures leave no fake runtime
  containers or volumes, so the new fail-closed path does not trade execution
  safety for a resource leak.
- Rejected by the complete daemon checks: build, tests, vet, and lint all pass
  with the regression present.

Revisit when a supported runtime has a distinct created-but-never-executed
state that is not `StateStopped`; that requires an explicit contract decision,
not widening this check implicitly.
