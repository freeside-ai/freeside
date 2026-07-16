# Public-repo settings: branch protection without named required checks

With the repository public (#101, the AGPL decision's follow-up), branch
protection on `main` became available and was enabled (owner decision):
require a pull request before merging with zero required approvals,
enforce for admins, and block force-pushes and deletions. Zero approvals
keeps the solo owner able to merge after the automated Codex review;
requiring a PR mechanically enforces the existing "never commit directly
to main" rule.

The protection also sets `required_status_checks` with `strict: true`
and an empty check list, but that strict bit is inert as configured:
GitHub documents the up-to-date-before-merge requirement as a mode of
required status checks, and with no required checks enabled a behind
branch can still merge. Base freshness therefore remains enforced only
by convention, by the merge-result-audit script and the handoff
procedure, not by the forge. The bit is left on so it takes effect if a
required context is ever added.

Rejected alternative: naming the four CI jobs as required status check
contexts. Every workflow is path-filtered per component, so a named
required context would never report on a PR that does not touch its
paths, and the merge would block forever. The known mitigation, no-op
twin workflows with matching job names and inverse path filters, was
judged not worth the machinery while `gh pr checks` polling plus the
merge-result-audit already cover the gap. A lighter variant, a single
trivial always-run PR-gate workflow named as the one required context,
would also activate the strict freshness gate; it was not part of this
decision and stays open under the revisit condition below.

Also enabled the free public-repo security features: secret scanning,
push protection, and Dependabot security updates (alerts were already
on). Non-provider-pattern scanning was attempted; the API accepts the
write but the setting stays disabled, as it is gated behind GitHub
Secret Protection, so it remains off. Merge-method settings needed no
change; they already matched the conventions (merge commits only,
title-only merge message, auto-delete head branches, update-branch
suggestions on).

Revisit when: forge-level freshness or check enforcement becomes worth
adding a required context (the always-run PR-gate workflow or the no-op
twins), a merge queue is adopted, or GitHub Secret Protection becomes
available to the org for non-provider patterns.
