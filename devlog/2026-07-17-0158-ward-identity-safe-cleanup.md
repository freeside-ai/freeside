# Ward cleanup binds destruction to observed identity, not names

Work unit: #138 (contract and safety-policy change on a destructive
path; mandatory note). Stacked on #129's branch; supersedes the
name-authority half of round 12 in
`2026-07-16-1720-ward-handoff-backend.md`, whose per-object
ownership model this unit keeps and tightens.

## The gap

PR #129 treated a successful create as permanent authority over the
deterministic name (`freeside-handoff-<runID>-{ws,agent,exporter}`):
teardown reaped an owned name with no fresh evidence, the list-failure
fallback deleted on identity alone, and a same-name row appearing in
the delete-to-absence window failed the run while the claim stayed
owned, so deferred teardown then destroyed the replacement. An external
delete-and-recreate between create and deferred cleanup could therefore
lose an object this run never made. Confirmed by a mutation check: with
the old absence semantics restored, the new delete-window regression
test observes teardown deleting the replacement.

## What the runtime can and cannot offer

Verified against Apple container 1.1.0 on the reference host (and
pinned by decode fixtures): a container's `id` IS its caller-chosen
name and volumes have only a name, so no immutable runtime ID exists;
`delete`/`volume delete` take a bare name with no precondition or
compare-and-delete; labels are set only at creation. The one immutable
attribute the CLI emits is `configuration.creationDate`, present on
container inspect, `list --all`, `volume list`, and `volume inspect`
(verb verified live this unit; fixture `cli-volume-inspect.json`
captured from the real CLI).

## Chosen design: creation-instant fingerprint plus fresh-evidence gate

The seam surfaces `CreationDate` as an opaque, equality-only
fingerprint (never parsed: parsing could normalize distinct raw values
into a false match) and gains `InspectVolume`, giving volumes the same
per-object observation containers had. Right after each successful
create, the gate captures the fingerprint from an observation that must
itself carry the invocation's unpredictable 128-bit ownership label; a
labeled observation with no reported instant leaves the claim
fingerprintless and cleanup degrades to fresh label evidence (the
runtime-without-dates case, and acceptance criterion 4's "where safe
identity evidence exists").

Every destructive or absence decision routes through one classifier
(`classifyEvidence`). The fingerprint is a veto, never proof by itself:
a differing instant is a foreign replacement, left untouched and
counted as absent even when it copies this run's labels; a matching
instant still needs the token to corroborate it (instants are
second-granular, so a same-second replacement would match), and an
observation that can show neither withholds the delete and fails
teardown. With no usable instant comparison the token decides alone.
`waitStopped` re-classifies every poll against the claim, so check 3's
stopped proof is about the one VM the gate started even on a runtime
that reports no instants (the token is then the whole evidence), and
the happy-path delete always follows a just-verified observation. The
delete-to-absence window fix falls out: a foreign same-name row now
counts as absent, the claim is cleared, and teardown never touches the
name again. Fingerprints are captured only after the report's identity
is verified (allowlist ID checks for containers, an explicit name check
for the workspace observation), so a wrong-object report cannot bind a
fingerprint that would later misclassify this run's own object.

The unit folds in and extends the base branch's rounds 27-28: the
list-failure fallback evidence-gates owned and ambiguous claims through
one inspect-based reap (round 27's ambiguous-container reap, extended
to ambiguous volumes now that `InspectVolume` exists), and round 28's
ownership downgrade after the happy-path delete is subsumed because no
path reaps by create-success identity anymore.

Rejected: an adapter-side "verified delete" (inspect-then-delete inside
the Runtime implementation) — it would relocate the same TOCTOU while
advertising an atomicity the runtime cannot provide, and duplicate
policy into every implementation; policy stays in the gate, evidence
stays seam data, and the fake and CLI honor one contract. Rejected: a
host-level lease/lock object — it cannot exclude other host processes
from the runtime namespace and adds a liveness failure mode without
closing the window.

## Refute-first pass (mandatory for this risk class)

Two independent refute lenses (lifecycle interleavings; evidence
calculus and decoders) ran against the change before handoff.

Confirmed and fixed:

- The draft classifier returned ours for a matching instant with
  unobserved labels; a same-second no-label replacement (the shape a
  label-less foreign object actually decodes to) would have been
  deleted. Fixed: a matching instant never proves ours without the
  token; pinned in the classifier table.
- `waitStopped` in the fingerprintless degraded mode compared "" == ""
  and would have accepted any same-name stopped container before the
  happy-path delete. Fixed: every poll runs the full classifier, so the
  token is required even without instants; regression test added.
- The post-create capture sites trusted the report's identity at the
  gate layer (the CLI decoders check it, another Runtime might not).
  Fixed: capture moved after the allowlist identity checks, plus an
  explicit name check on the workspace observation; test added.
- Cross-endpoint fingerprint consistency (inspect vs list raw
  creationDate) is load-bearing and was unasserted: a format drift
  would silently classify every own object foreign and leak it with
  teardown green. Byte-equality assertions added to the live test.
  Rejected alternative: classifying instant-mismatch-with-token as
  unprovable would make drift loud but break the replacement-spared
  property for token-copying replacements; the live assertion covers
  drift instead.

Rejected by verification: a copied-token impersonation reaching ours
requires reading and re-applying labels, same-UID namespace authority
already outside the threat model; sibling freeside runs cannot collide
on the token (per-invocation crypto/rand, independent of RunID).

## Residual assumptions (accepted, recorded in doc.go and the seam)

- The verify-to-act window on a name-addressed stop/delete is
  irreducible at CLI 1.1.0. The `freeside-handoff-` prefix is a
  daemon-owned namespace; a host actor mutating it holds the same
  user's full runtime authority and is outside the threat model.
- `creationDate` has second granularity; a same-second replacement that
  also copies the unpredictable token classifies as ours, which is
  already namespace-authority behavior above. A same-second replacement
  without the token is unprovable (fail closed), never ours.
- Same-RunID collision between two live runs remains a caller contract
  violation (round 10 of the #129 note), unchanged here.
- An ambiguous claim whose object was never actually created fails the
  list-failure fallback loudly (the fallback inspect cannot find it: a
  teardown problem, never a delete); claim state is verified, not
  assumed.

Revisit when: Apple container exposes immutable runtime IDs,
conditional deletion, or sub-second creation instants; or when a second
backend (linux_vm) needs the same identity contract and the classifier
should move behind the seam.
