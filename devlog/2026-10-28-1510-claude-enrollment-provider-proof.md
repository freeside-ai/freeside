# Prove Claude Setup Tokens Before Enrollment

Work unit #1647 replaces the provider-check deferral recorded in
`2026-09-30-1047-auth-add-list.md`. The missing prerequisite now exists:
`ProbeLineupImage` runs one minimal Claude call through the ward's
provider-only proxy. The enrollment still records no probe-result contract
or account identity, and adoption remains unchanged.

## Probe The Authored Credential

Chose to read the token back through `ReadSetupToken` and compare its digest
with the enrollment's manifest digest before probing. Probing only the
operator's input would leave the authored volume untested. The exporter
reads the volume without network access; the probe receives the read-back
token through a private staged file copied into its root filesystem. It
loads and deletes that file before invoking Claude with the token in the
CLI process's environment. The container spec carries no token. An export
that retains the staged credential is refused by the existing prober.

The digest-pinned agent image is required before the enrollment records
anything. Chose the existing probe with no model or effort override so a
subscription's model restrictions cannot be mistaken for a rejected token.
The request uses one turn and no tools. It proves the token can answer a
minimal call, not that every lineup model will work.

## Refuse Every Unproven Token

Chose to delete the new credential volume and append no generation for
authentication refusal, provider unavailability, or a runtime failure.
Enrolling after an inconclusive check would preserve the deferred first-run
failure this unit removes. The generation-less enrollment remains retryable
under the existing mutation lease.

Authentication refusal asks the operator to mint another setup token;
network errors, overload, usage limits, and other provider failures ask for
a retry. A refusal can be attributed to the provider only after the proxy
observes bytes from it. The shared lineup verdict remains unchanged.

Refusal text that carries the token is withheld. The check also covers
12-byte runs beyond the public prefix, because the shared judge truncates
long refusal text and could leave only part of the token visible.

## Refute-First Evidence

- **Independent review:** A fresh-context reviewer found no reachable
  credential leak, failure-cleanup defect, or probe-ordering regression.
- **Confirmed and fixed:** A real proxy listener prevents a `synctest`
  bubble from advancing. The runtime-surface test runs the real probe
  outside the bubble; the other enrollment tests stub only the probe.
- **Disproved by checks:** Probe failures delete the created volume and
  append no generation. A read-back digest mismatch refuses before probing.
  Missing or unpinned agent images refuse before recording, and CLI flag
  failures do not open the store.
- **Disproved by checks:** The real probe path's container specs, returned
  result or error, and captured outputs carry no token on success or
  refusal. Classification tests with whole and truncated token echoes
  withhold the message while retaining the failure class.
- **Observed live:** Claude Code 2.1.282 reports an invalid setup token as
  "Failed to authenticate. API Error: 401 OAuth access token is invalid."
  The provider heartbeat is present and the enrollment classifier returns
  `ErrSetupTokenRejected`.

## Revisit When

Revisit the refusal phrases when the pinned Claude CLI changes its auth-error
format. Account discovery and recording the probe result on a generation
remain separate contract changes.
