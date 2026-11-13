# An Over-Long Review Guide Is Trimmed, Not Refused

#3180 changes what the importer does with a review guide that breaks a size
cap. The #2753 exit run wrote a finished change and a guide whose one
excerpt ran 116 lines against the 80-line cap; the import refused the guide,
and the whole run was discarded with `cause: unknown`. The owner decided on
2026-11-13 (issue #3180, Owner Decision) that size caps trim and never fail
the run. This note revises the first decision in
`devlog/2026-11-07-1832-review-guide-producer.md` ("fail a malformed guide
closed over dropping it") and records what the refute-first pass found. Plan
revision 124 carries the commitment.

## Decision

- **Chose to trim a size-cap breach and record it over failing the run.**
  The importer keeps the first stops, excerpts per stop, and questions up to
  their caps, keeps the first 80 lines of an over-long range, and cuts an
  over-long title, summary, caption, question, or notes to its cap, ending
  in "…". Rejected: keeping the fail-closed rule. The guide is an optional
  card section, so spending a finished run on its overrun costs far more
  than the overrun does.
- **Chose to record every trim on the ready item over trimming silently.**
  `review_guide_notice` lists each cap with its place, the limit kept, and
  the size found, and the card shows it above the stops. Rejected: a silent
  trim, which would show the operator a guide that differs from the agent's
  without saying so. The notice also tells the operator the agent overran,
  which the old refusal did through a failed run.
- **Chose to drop a file over the 64 KiB read bound over failing the run or
  trimming it.** The importer can't decode a file it won't read, so there is
  nothing to trim; dropping it keeps the run, and the notice's omission says
  why the card has no guide. Rejected: failing the run, for the same reason
  as above.
- **Chose to keep every other fault fail-closed.** Malformed JSON, unknown or
  missing keys, zero stops, a bad attention or side, a bad or over-long
  path, a bad range, and declaration, digest, and secret faults still refuse
  the import. A path cap stays a fault because a path names a file and a cut
  path names a different one. Every element is shape-checked before any
  trim, including elements a count cap then drops, so a trim never admits a
  guide whose dropped part was malformed.
- **Chose a typed cause, `invalid_evidence`, over leaving those refusals
  `unknown`.** `ErrEvidenceInvalid` and `ErrEvidenceMediaMismatch` now
  record `invalid_evidence` on the live pipeline and on both recovery paths,
  with an attention disposition and no alternate agent: the fault is in what
  the run wrote, so another agent's credential doesn't escape it. Other
  import refusals stay `unknown`.
- **Chose to carry the notice on the ready item only.** The blocked item and
  the hold comparison don't show a guide, so they carry no notice.

## Refute-First Findings

- **A dropped guide's bytes stay stored but reach no client.** The stage
  pipeline still persists every evidence blob, because the verification
  replay re-materializes all of them; dropping the oversize blob would break
  the replay. The attachment route serves any stored digest a client names,
  so the guard is that no record names this one: no claim names it, and the
  pipeline leaves it out of the stage's returned artifacts, which a feedback
  stage's reply would otherwise attach. The fresh-context review found that
  second path. A second review found that the shorter list broke the
  scope-conflict rewrite, which indexed it by manifest position: the guide
  sorts just before the conflict, so the rewrite panicked or overwrote the
  summary's slot. The live list, that rewrite, and the list recovery
  recomputes from the durable export now all index one list of returned
  entries, built through `importer.ReviewGuideOmitted`. The integration test
  asserts no claim on the ready item names the blob; stage tests assert no
  artifact does, including beside a scope conflict.
- **A dropped guide is still scanned.** The importer can't decode it, but it
  scans the raw body when it fits the secret-scan bound (1 MiB by default),
  as it scans a markdown entry, so a credential in an oversize guide still
  refuses the import. A body past that bound goes unscanned, like any other
  evidence blob past it; it reaches no client either way.
- **The secret scan covers what a trim cuts.** The importer scans the raw
  body and then the decoded strings of the guide as written, before
  trimming, so a credential past a cut point or in a dropped stop still
  refuses the import.
- **The notice is derived, never trusted from input.** The importer computes
  it; the verification checkpoint re-checks that a stored notice agrees with
  its stored guide (an omission beside no guide, trims beside one) and fails
  with a parent-key mismatch otherwise; domain validation binds each trim's
  indexes to places the stored guide has.
- **Old digests stay stable.** The import result's new field is
  `omitempty`, and the fake-publication terminal digest shadows it the same
  way, so results and terminals bound before the field existed keep their
  digests.

## Revisit When

Agents trim often enough that the caps, not the agents, look wrong; or a
guide fault other than size starts costing runs in the same way.
