# The Advisory Stance Is Read From the Bound Profile

Issue #3125, building plan revision 118
(`2026-11-12-0840-advisory-automation-control.md`). Tier A (import,
publication decisions, the publish token, a stored-authority re-check), so a
mandatory note.

Chose to recompute the `candidate_automation_changes` stance from the bound
trust profile at every gate over storing it as authority on a task,
checkpoint, or authorization, because the profile is already digest-bound
and re-read at each gate, while a stored stance would be one more bit to
forge or let go stale.

## Decision

- **Every gate reads the stance from the profile.** The importer policy,
  the stage's finding partition, the publication replay, the verification
  checkpoint, and the remediation source each take the bound profile's
  stance. The replay's import options do carry it, but only so the
  existing whole-options comparison refuses a payload that claims
  `advisory` under a `block` profile; nothing trusts the payload's value.
- **`Validate` checks representability; `ValidateAdvisoryStance` checks
  the stance.** An advisory finding is valid when some stance makes its
  category advisory, so a stored authorization stays decodable after the
  owner changes stance. The verification checkpoint and the remediation
  source, which rebuild an authorization from a stored payload, re-check
  its advisory findings against the bound profile's stance, so a checkpoint
  carrying an advisory workflow finding under `block` is refused even with
  a consistent content ID. Every other read binds the authorization to its
  profile digest, which fixes the stance.
- **`block` normalizes to the empty value in the import policy.** The
  policy's JSON omits an empty stance, so every replay payload recorded
  before this change, and every one recorded under `block` after it,
  stays byte-identical.
- **No profile encoding bump.** The canonical encoding already carried
  `candidate_automation_changes`; `advisory` is a new value, not a new
  field, so `block` profiles keep their digests and no profile needs a
  fresh review.
- **The token's permissions follow the stance, not the candidate.** Under
  `advisory` every publication token asks for `workflows: write`; under
  `block` none does. The token cache keys on the stance, so a `block`
  token is never reused once the profile turns `advisory`. The App
  registration requests the permission up front.
- **The mint audit records `workflows` in its own columns.** Migration
  0136 adds `requested_workflows` and `granted_workflows`, following the
  column-per-scope shape #1768 chose for `issues`, so the audit stays
  lossless; legacy rows read as empty.
- **The advisories section names the workflow risk only when it applies.**
  A workflow-configuration advisory adds one sentence saying GitHub runs
  the edit before review. Without one the section keeps its old bytes, so
  earlier candidates replay identically.

## Rejected

- **Choosing `workflows: write` per publication.** Asking for it only when
  the candidate edits a workflow narrows the token, but needs a second
  token-source method and a bit on the gated head saying which set to
  mint. Per stance needs only the mint and its cache key, and the plan
  allows it. The cost: until the installation accepts the permission,
  every publication under `advisory` is refused, not only workflow edits.
  The README says to accept it before approving the profile.
- **Storing the stance on the authorization or checkpoint.** It would let
  a gate skip the profile read, but a stored bit is one more thing a
  reconstruction gate must re-check against current policy; reading the
  profile is that re-check.

## Refute-First Findings

A fresh-context reviewer tried to break the change. None of its findings
lets `block` publish an automation-control edit.

- **The publisher's final gate doesn't call `ValidateAdvisoryStance`.**
  Allowed. The gate binds the authorization to its profile digest and
  requires the current profile to be that one or a review-only
  supersession, which keeps the stance. Only verification creates an
  authorization, under the bound stance, so a mismatched row needs a write
  to the daemon's own database, which the threat model trusts. Under
  `block` the token also lacks `workflows: write`, so GitHub would refuse
  the push.
- **An advisory finding beside an allowlist violation skips a
  remediation's graceful dissent.** Allowed here. It fails closed with a
  parent-key mismatch, and reviewer-instruction advisories already reached
  it before this change.
- **Re-running trust evaluation across a stance change quarantines the
  run.** Allowed. It fails closed, as a change to any other profile field
  in the replay options already does.
- **The checkpoint tamper test would pass without the gate.** Disproved:
  with the stance check removed, the test fails, because the forged
  authorization travels past the checkpoint.

Revisit when a stance needs a third value, or a token must vary by
candidate content rather than by profile.
