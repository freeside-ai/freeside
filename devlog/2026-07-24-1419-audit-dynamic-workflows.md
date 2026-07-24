# Audit Synthetic Dynamic Workflow Rows as Digest-Bound Evidence

Work unit: #273 (blocks #234). Trust-boundary change: relaxes a
fail-closed validation on rows returned by GitHub's workflows-listing
API, so this note is mandatory (returned-object trust boundary).

## Problem

GitHub's `/actions/workflows` listing includes platform-managed
synthetic rows (path prefix `dynamic/`: Dependabot version updates,
CodeQL default setup) alongside repository-defined workflows. The
auditor treated any row outside `.github/workflows/` as malformed, so
the audit failed closed on every Dependabot-enabled repository. Found
live during the #234 onboarding pass on `freeasinbird/gh-imgup`
(`dynamic/dependabot/dependabot-updates`).

## Decision

Accept `dynamic/`-prefixed rows and record their path and state in a
dedicated `dynamic_workflows` evidence field; derive no authority facts
from them. Their definitions are not fetchable as repository content at
the audited commit, so content analysis is impossible; instead the
evidence digest (which the trust profile binds exactly) pins their
identity and state, making appearance, disappearance, or state change a
drift that fails closed until a human re-reviews the profile. Rows
outside both path families still fail closed. The evidence encoding
version bumps to `freeside-workflow-audit/v2` because the evidence
shape changed.

## Rejected Alternatives

- **Known-family allowlist (accept Dependabot, reject others).**
  Hardcodes GitHub product knowledge and recreates the original
  fail-closed outage for every new platform family (CodeQL default
  setup would have been the next one); digest-pinning handles all
  families uniformly with the same human-review gate.
- **Fold dynamic rows into the existing `workflows` evidence list with
  empty SHA/content.** Avoids the version bump but conflates
  repo-content workflows with synthetic registry rows in one shape;
  explicit-over-implicit lost for no real gain, since no stored audit
  depends on v1 (the digest is the only persisted derivative and no
  daemon state exists yet anywhere).
- **Derive facts from known dynamic families (e.g. treat CodeQL
  default setup as PR-triggered `security-events: write`).** Would keep
  the machine-derived facts complete but rests on undocumented,
  version-drifting platform behavior the audit cannot attest from
  content; wrong-by-construction attestations are worse than an honest
  "not derivable, human-reviewed" boundary.

## Refute-First Verification (Required for This Risk Class)

An independent fresh-context lens was prompted to refute the change.

- **Rejected by verification** (no failure constructed): post-approval
  appearance of a dynamic row fails closed via the digest axis of
  `EvaluateTrustDrift` before any fact comparison; path tricks
  (`Dynamic/x`, `dynamic/../...`, non-canonical forms) land fail-closed;
  digest is order-stable (single sorted construction site) and the
  empty case encodes as `[]`, never `null`; pagination duplicate and
  short-page handling unchanged and fail-closed; the new return value
  is threaded through the single caller on all paths.
- **Confirmed, accepted by decision**: digest-bound dynamic rows (like
  the pre-existing branch-protection/ruleset evidence) are not surfaced
  to the human anywhere; the approving reviewer must inspect the live
  repository out of band, and a drift error shows two opaque digests.
  Accepted because the drift gate itself is sound and the #234 review
  records the observed rows on the issue; the visibility gap is filed
  as its own deferral unit rather than widening this fix into a
  contract change. Follow-up: #274.
- **Confirmed, accepted by decision**: the v1→v2 encoding bump moves
  the digest for every repository, so any profile approved under v1
  would drift and require re-approval. Accepted as a no-op today: no
  daemon state exists yet (the first profile, #234, is what this unit
  unblocks), so there are zero approved profiles anywhere.

## Revisit When

A platform-managed dynamic family is observed carrying PR-candidate
execution authority that the human-review-plus-digest-pin posture
handles poorly in practice (e.g. frequent benign state flapping of
CodeQL default setup forcing repeated re-approvals), or when audit
evidence becomes reviewable in-product; then reconsider deriving
conservative facts for known families.
