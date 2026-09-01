# Capability Manifest Retry Authority

Issue: #921

Chose predefined, versioned, content-addressed capability manifests over
operator-authored execution settings. The first manifest contract carries only
an egress profile because that is the only capability dimension the current
composition can independently enforce at admission. Its digest covers the
version, name, and egress profile; the resolved-policy value is a strict,
canonical JSON array of those authenticated manifests.

Chose to treat the card's offered manifests as a projection, never as execution
authority. Signet accepts only one offered digest, the retry attempt binds that
digest to the accepted command and failed invocation, and admission reconstructs
the manifest again from the retry run's resolved policy before selecting its
egress profile. A missing, changed, malformed, or unenforceable manifest refuses
admission with no fallback to the original profile.

Chose the accepted command id as the durable retry idempotency key instead of a
new top-level transition record. The campaign allocator searches existing
attempts for that exact command binding and reuses the same attempt and run. An
incomplete retry may be recovered only when all operator bindings match, which
closes the item-conclusion/attempt-allocation crash window without widening the
durable transition vocabulary.

Rejected trusting the selected profile from the client, trusting the card after
acceptance, accepting an arbitrary manifest body, and silently falling back when
the selected environment cannot be materialized. Each would let stale or
caller-controlled data choose execution authority.

The refute-first pass exposed several reachable boundary gaps before commit.
The final design preserves typed-field presence through HTTP decoding, lets the
capability path carry only its canonical digest, authenticates every
operator-bound attempt against the accepted command, concluded card, failed
admission, and parent run policy at both write and reconstruction, and requires
exact full bindings before adopting an incomplete retry. Admission also checks
the retry attempt and admission reciprocally, re-applies the manifest to every
production-stage invocation in the retry run, and rejects a profile equal to
the failed admission.

The same pass changed the composition declaration from a bare profile promise
into a validated promise: every declared provider profile must have an auth
identity, while a clean-verification composition must have none. Because one
current admitter cannot construct both identity shapes, it cannot truthfully
declare both. Manifest names use printable ASCII so Go and Swift compare,
deduplicate, and order them identically. The Swift mock hashes the exact Go v1
canonical manifest bytes and recomputes that digest when validating fixtures.

The operator picker now captures the reviewed snapshot and submits against
those exact bindings. A card that advances while the picker is open cannot
launder its old manifest choice through a newer snapshot. Focused tests pin
these corrected boundaries, and the integration test covers offered selection,
Signet acceptance, reconciler allocation, and idempotent replay.

Revisit when a composition can enforce capability dimensions beyond egress, or
when capability retries need an independently queryable lifecycle beyond their
accepted command and production-attempt lineage.
