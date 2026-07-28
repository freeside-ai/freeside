# Canonical Repository ID Binding on Daemon Checkouts

Work unit: #341. Mandatory note: trust-boundary work (a push re-gate
that decides against on-disk state), plus an owner-visible deviation
from the issue's letter.

## The Stamp Is the File Ward Consumes, Not a Config Key

#341's text asks for the ID "beside the name in the checkout's config".
That wording predates the mechanism its own dependency landed: #329's
later review rounds bound ward's seeding gate to the canonical pair of
owner/name from local config plus a positive canonical-decimal ID at
`.git/freeside-repository-id`, and left the producer side open.
Stamping a new config key instead would be refused outright by both
config gates (publish's `pristineConfigKeys` and ward's seeding
allowlist are exact-key allowlists) and would need both widened, buying
no property the file does not already have: the file influences no git
behavior, and its integrity is proved by the explicit re-gates that
read it. `FetchBase` therefore writes the file ward already parses,
byte-for-byte in the same grammar (≤64 bytes, canonical positive
decimal, one trailing newline), with the literals deliberately
duplicated rather than imported across lanes, matching the existing
`freeside.transport.repo` precedent.

## Three Comparisons, Not One

Rejected: verifying disk against the current trusted binding alone. The
stamp lives in a directory later pipeline stages write into, so
disk-vs-trust collapses two different questions ("is the stamp intact"
and "does the name still mean the same repository") into one check an
on-disk rewrite could satisfy. The sealed `Checkout` capability now
carries the ID `FetchBase` stamped, and `PushHead` splits the re-gate:

- disk == capability, pre-token: stamp integrity; refuses absent,
  malformed, oversized, and tampered stamps, including every checkout
  materialized before the stamp existed, so staleness cannot bypass the
  tightening;
- capability == trusted binding (`InstallationToken.RepositoryID`,
  sourced from the validated stored trust profile), post-token: trust
  continuity; refuses a name transferred and reused between fetch and
  push instead of following it onto the other repository.

Transitivity closes the triangle disk == trust without ever trusting
the disk for it.

## The Continuity Check Necessarily Follows the Token

The trusted binding arrives with the token, so the one re-gate that
compares against it cannot run before `Token(...)`. The prior "a
rejected checkout never causes an audited mint" comment was scoped to
what it always meant: gates on the call's own arguments and local state
stay pre-token, and the continuity refusal still precedes every
authenticated network operation, so no credential is exposed for a
refused push. An audit row recording the mint beside a rebind refusal
is evidence of the anomaly, not noise. Rejected: widening `TokenSource`
with a resolve-only method to preserve the letter of the old comment;
one unused-credential row on an anomalous path does not justify a wider
seam.

## Refute-First Findings

- **Rejected by verification (parse divergence):** an independent
  refute pass compared `readRepoIDBinding` decision-for-decision
  against ward's ID-file section and found them identical (cap,
  newline, canonicity, positivity); nothing publish writes is refusable
  by ward, and the adversarial table (sign, leading zero, trailing
  junk, double newline, oversize, NUL) is pinned as tests.
- **Rejected by verification (credential exposure):** the mint-count
  test proves a stamp-refused checkout acquires no token; the
  continuity gate precedes the first authenticated operation.
- **Confirmed and fixed:** `FetchBase`'s pre-existing mint-ordering
  comment became overbroad once the ID refusal moved behind the token;
  reworded rather than reordered.
- **Accepted by decision:** the rebind test pins the refusal but not
  "no authenticated operation ran after it"; that ordering is enforced
  by code position and reviewed, and a recording-runner harness for it
  is not worth its weight while the gate sits eight lines above the
  first authed call.

**Revisit when** a checkout materialization path other than `FetchBase`
appears (the #237/#238 seeding wiring must hand ward a tree that
preserves `.git/freeside-repository-id`; ward refuses its absence, so
the gap fails closed rather than silently).
