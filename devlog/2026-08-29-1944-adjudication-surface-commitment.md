# Adjudication Surface Commitment

**Work unit:** #1019, Wave 7 contract-chain position 4.

## Decision

`FindingAdjudication` carries an optional `decision_surface_digest` in its
content-addressed body. A producer writes the prospective item's decision
surface digest before finalizing the artifact. Recommendation derivation then
requires strict equality between that commitment and the item's current
decision surface. An empty commitment is valid for mechanical callers and
stored artifacts that have no recommendation producer, but it never
authenticates a recommendation.

The successor constructor accepts its own prospective commitment rather than
inheriting the predecessor's. A successor opens a replacement item surface, so
the predecessor's commitment would authenticate the wrong epoch.

Chose one optional constructor parameter over a committed-only constructor
because two constructors would create two ways to build the same artifact.
Rejected requiring a non-empty commitment because #1002, not this contract
unit, owns prospective-surface computation at the engine sites. Rejected a
"non-empty and differs" derivation rule because it would allow an uncommitted
artifact to render a recommendation.

## Compatibility Evidence

The commitment uses `omitempty` in both canonical preimages and encoded body
shapes. The existing revision-1 and successor goldens remain byte-identical
and decode through content-digest revalidation. New committed goldens pin the
field immediately after `resolved_policy_digest`. A raw pre-change stored body
also reconstructs with an empty commitment and its original content digest.
No migration or encoding-version change is needed because the JSON body is the
authority and the store extracts no commitment column.

## Refute-First Findings

The deserialization boundary accepts only an empty commitment or a valid
content address, then recomputes the artifact digest. Tests cover legacy raw
rows, malformed commitments, committed round trips, predecessor/successor
commitment changes, and recommendation reconstruction with empty, foreign, and
matching commitments. An independent fresh-context review found no actionable
correctness, security, or compatibility defect. No decoded commitment grants
recommendation authority without strict equality to the daemon-owned current
surface.

## Revisit When

- #961 changes the persisted finding-adjudication body.
- #1002 finds that the engine cannot compute the prospective decision surface
  before finalizing the artifact.
