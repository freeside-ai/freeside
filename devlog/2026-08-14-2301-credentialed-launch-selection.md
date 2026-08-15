# Credentialed Launch Selection

Work unit: #782 (`kind:fix`, `lane:saddle`). This mandatory note records the
owner-delegated deployment-selection decision.

## Decision

Chose the deployment that already holds this device's credential when local
readiness and the persisted server URL disagree. Readiness still wins on a
fresh install and whenever the local deployment is paired; it loses only when
the readiness deployment is unpaired and the persisted deployment is paired.
Explicit launch modes remain higher precedence.

The credential probe stays at the composition boundary and returns only
presence to the pure launch selector. Credential loading and request
authentication remain scoped by the selected deployment key, so selection
does not expose a token or permit one deployment's token to reach another.
An unreadable credential counts as absent, matching pairing recovery.

## Rejected Alternative

Rejected a two-deployment chooser because credential presence resolves the
observed ambiguity deterministically. Multi-deployment management UI is beyond
this work unit's disambiguation scope.

## Refute-First Findings

The credential-isolation lens found that Foundation decodes a percent-encoded
slash in `URL.path`, so the prior deployment key collided for `/a%2Fb` and
`/a/b` even though requests preserve those distinct base URLs. It also found
that `URL(string:)` accepts relative, opaque, and non-HTTP values, while the
new malformed-input test let readiness short-circuit before evaluating the
persisted candidate. Both findings were confirmed and fixed: deployment keys
now retain the percent-encoded path, persisted selection accepts only absolute
HTTP(S) URLs with an authority host, and the regression test makes readiness
uncredentialed so malformed persisted input reaches the selection boundary.
The follow-up lens found that Foundation also accepts out-of-range explicit
ports; the validator now requires ports in `1...65535`, and a call-recording
test proves malformed candidates never reach the credential probe.

Rejected compatibility lookup or migration from the former decoded-path
Keychain service when the encoded and decoded keys differ. The old item does
not record its deployment URL, so a key such as `/a/b` cannot prove whether
the credential was minted for `/a/b` or `/a%2Fb`; consulting it could disclose
a bearer token to the wrong deployment. The change therefore fails closed:
the ambiguous legacy item remains untouched, the encoded-path deployment
requires pairing under its distinct key, and the old device stays available
for explicit server-side revocation.

The lens confirmed that valid noncolliding URLs follow the intended truth
table, explicit modes retain precedence, the selector receives no token bytes,
and selecting the persisted deployment drops the readiness pairing code.

## Revisit When

The client supports more than one paired deployment as a first-class operator
workflow, or credential presence no longer identifies the intended deployment.
