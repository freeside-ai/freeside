# An Onboarding Approval Applies at a Moved Tip When What It Covers Is Unchanged

`freesided onboard -approve` no longer fails just because the base ref's tip
moved between the review and the approval. The applying pass audits the live
tip, rebuilds the proposed profile from that audit, recomputes the approval
digest over the request as reviewed (reviewed commit included), and reads the
tip's verification recipe. It accepts only when that digest equals the
owner's and the tip's recipe digest equals the reviewed one, then reuses or
builds the image at the tip. Anything else is refused with `MovedTipError`,
whose `Changes` name what differs. Contract: #3545, filed by the owner after
#2382's routing rerun, where three merges that touched neither `.freeside/`
nor `.github/` voided an approval.

This revises "Chose Keeping the Commit in the Approval Digest" in
`devlog/2026-11-19-2318-onboarding-approval-carry.md`, which kept the commit
so an approval for one tree couldn't apply to another on the applying pass.
The owner asked for exactly that application when the reviewed content is
unchanged. The digest still binds the commit; what changed is that the
applying pass compares everything else at the live tip instead of requiring
the tip to be the reviewed commit.

## Chose Recomputing at the Tip Over Dropping the Commit From the Digest

Dropping the commit from `freeside.onboard-approval/v2` would let an approval
match at any tip without the applying pass checking anything there. Keeping
the format and recomputing the digest from the tip's audit, with only the
commit and recipe held at their reviewed values, means acceptance is a fresh
equality check over the live state: the profile (every audited privilege and
`workflow_audit_digest`), the installation (including a pending intent's
revision and expiry), the base image, the destination, and the toolchain
directories. The audit used is always the tip's. No new record, migration, or
digest version was needed.

## Chose a Separate Recipe Comparison

The digest's recipe digest comes from the reviewed commit's recipe, because
that is what the owner saw. A recipe changed at the tip would therefore pass
the digest check. The applying pass reads the tip's recipe (from `-source`,
which must have fetched the tip, or from the `-recipe` file, which is the
recipe at every commit) and requires its digest to equal the reviewed one.
Dropping this comparison would let a changed `.freeside/verify.json` through.

## Chose Naming Changes From the Reviewed Audit, Never Deciding From It

A refusal names changed profile fields by rebuilding the reviewed profile
from the latest audit recorded at the reviewed commit and diffing it with the
tip's. That stored audit only explains a refusal; acceptance never reads it,
so a stale or missing row can't make an approval pass. When nothing is named
(a changed onboarding flag or installation, or no audit recorded at the
reviewed commit), the refusal says the digest doesn't match the review
recomputed at the tip.

## What Isn't Bound

Dependency manifests aren't part of what the owner reviews. A reused image
must still be admissible at the tip (`AdmissibleAt` over the tip's inputs),
and a build happens at the tip, as on any approval. A reused image keeps its
own build commit in `project_image.commit_sha`; `image_request.commit_sha`
is the tip. The tip need not descend from the reviewed commit: a rewound or
force-pushed base ref is accepted on the same equalities. A review pass at a
moved tip still refuses; only an approval carries.

`scripts/exit-root.sh` fetches the base ref before the approval command so
the checkout holds the tip, and pins the deputy to the commit a completed
approval records.

## Review Findings

Refute-first and one fresh-context review found no input that accepts while
a reviewed field differs, and no write before a refusal: the decision runs
before the audit is recorded and before promotion, build, or activation.

- **Confirmed and fixed:** no test pinned that a refusal leaves a pending
  intent unpromoted; one now does.
- **Allowed, diagnostics only:** a transient recipe-read failure (a canceled
  context) is reported as a moved-tip refusal with a review command, and a
  refusal that names profile fields doesn't also say an installation or
  flag changed. Both fail closed.
- **Allowed, narrow:** an exit-root state recorded before
  `template_base_sha` existed reviews once more after an approval at a moved
  tip, because the check falls back to the rewritten `base_sha`. It fails
  safe, and every review the script now runs records `template_base_sha`.

Revisit when the owner wants the dependency manifests bound to the approval,
or when the approval digest stops binding the commit.
