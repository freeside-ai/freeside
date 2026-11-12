# The Trust Profile Accepts issue_comment Workflows by Content

Issue #1435, plan §5.5 and §5.11. A contract and safety-policy change in a
Tier A unit, so a mandatory note.

Chose to bind the `issue_comment` workflows a repository runs into the trust
profile as an audited set of path and closure digest, added without an
encoding bump, over the v7 bump the issue contract specified. The bump
stranded in-flight publication across the upgrade; the digest-stable form
keeps the same comment gate without that cost.

## Owner Decision

The contract required `freeside-trust-profile/v7`. Implementing it failed the
stored-state corpus (`daemon/README.md`, Stored-State Corpus): a publication
task recorded before the upgrade reads its admitted profile by digest, the v6
row no longer validates, and the publication reconciler records a durable
stop for the whole daemon. Re-approving the profile does not clear it,
because the task stays bound to the old digest. That also contradicts plan
§5.5, "The key never affects pull-request publication."

Asked on 2026-11-12, the owner delegated the choice: take the path that best
balances safety against UX and smooth running. This note records that
delegation and the choice made under it.

## Decision

- **No bump; both members omitted when false or empty.** The profile gains
  `allow_issue_comment_workflows` and `issue_comment_workflows`, digested,
  and omitted from the canonical and stored form when false or empty, as
  `external_reviewers` was. A profile approved before they existed keeps its
  bytes and digest, so publication, in-flight or new, is unaffected.
  Rejected: the contract's v7 bump, for the durable stop above. Rejected:
  keeping v7 behind an engine change that lets an in-flight publication
  survive any encoding bump. That change is a Tier A rework of the
  publication binding, and this unit would wait on it with no gain for the
  comment gate; the defect is #3171.
- **The key is the acceptance, and may sit over an empty set.** True means
  the owner accepted the comment operation over exactly the listed
  workflows, none included. False, which is every profile reviewed before
  the key existed, approves no comment at all. This is what lets the digest
  stay put: the contract's objection to omission was that an empty set
  would mean both "reviewed and found none" and "never looked", and the key
  now tells them apart. Plan §5.5 holds as written: a profile reviewed
  before revision 66 approves no comment until the owner reviews it again
  under its new digest. `Validate` rejects a listed set under a false key.
- **Contract departures.** Three acceptance criteria change with this
  choice: the v7 pin and the v6-fails-closed test become a pin that the
  profile without the keys keeps its pre-change digest; "the key is true
  exactly when the set is non-empty" becomes "a listed set requires the
  key"; and the predicate refuses a false key even when nothing is
  observed, where the contract approved it.
- **The recheck takes a full audit.** `RecheckIssueCommentWorkflows` runs
  the ordinary `WorkflowAuditor.Audit` at the given ref and compares only the
  set. Rejected: a narrower fetch of just the workflow and action files. The
  full audit already has the ref-stability retry and one fail-closed parse
  of every active workflow; a second fetch path would duplicate both and
  could disagree with the audit publication uses.
- **Onboarding proposes the comment operation over the observation.** The
  proposed profile sets the key and copies the audited set, as it mirrors
  every other observed privilege. The owner declines by not approving the
  digest. Rejected: an owner flag to refuse the found workflows, which would
  record a profile that disagrees with what the audit saw and gives the
  consumers nothing they can act on. After the upgrade, a rerun of
  `freesided onboard` shows a review with the new keys; until the owner
  approves it, publication continues and comments stay refused.
- **The set never gates publication.** `EvaluateTrustDrift` does not
  compare it; `EvaluateIssueCommentWorkflowDrift` does, with its own
  sentinel. An edit to one of these workflows still drifts publication
  through `workflow_audit_digest`, as before.
- **The closure follows only local `uses`.** It digests the workflow file,
  local reusable workflows from `jobs.*.uses`, and every audited file in the
  directory of each local composite action from `steps.*.uses`,
  transitively, with a seen-set as the cycle guard. A missing local
  dependency or a non-composite local action fails the audit closed,
  matching the privilege analysis. Remote references, checkout scripts, and
  `workflow_run` chains stay the residuals plan §5.11 and §14 name.

## Refute-First Findings

Two fresh-context reviews tried to prove the diff wrong, the first on the v7
form and the second on the digest-stable one. The second found no defect in
the encoding or the predicate; it found the nested-action gap below.

- **Confirmed and fixed: action scripts were outside the closure.** The
  digest covered a composite action's `action.yml` only, so an edit to a
  script the action runs from `github.action_path` went unseen. The closure
  now digests every audited file in the action's directory.
- **Confirmed and fixed: a nested action's references were skipped.** The
  cycle guard keyed on files already in the closure, and an action's
  directory sweep adds a nested action's metadata before that action is
  followed, so an action the nested one used never entered the digest. The
  guard is now a separate set of files whose references were read.
- **Confirmed and fixed: a YAML merge key in `on:` hid its triggers.**
  `workflowEvents` read `<<` as an event name. It now fails closed, in the
  privilege analysis too.
- **Confirmed and fixed: the recheck's repository and audit-validation
  branches had no test.** Both now do.
- **Allowed by decision: the recheck trusts its caller for activation and
  ref.** `Validate` proves the content matches the digest, not that the owner
  activated the profile, and the function audits the ref it is given. The
  contract fixes the signature; the doc comment tells callers to pass the
  active profile and the default branch.
- **Allowed by decision: the recheck matches the repository by name.**
  `WorkflowAudit` carries no repository ID, the same limit publication's
  drift check has; closing it is an audit contract change outside this unit.
- **Allowed by decision: a broken `issue_comment` workflow fails the whole
  audit, publication included.** The contract requires the audit to fail
  closed on a listed workflow it cannot read or that names an absent local
  dependency. GitHub fails such a workflow too, and the owner fixes it.
- **Allowed by decision: onboarding offers no way to accept publication but
  decline the comment workflows.** Declining the digest declines both, as
  plan §5.5 intends.

## Revisit When

- A future field cannot be added with "absent means exactly as before", so
  the profile needs a real encoding bump. Until in-flight publication
  survives a bump, that bump durable-stops a daemon with publications open;
  the engine defect is #3171.
- A consumer (#1437, #3147, #3150) needs a narrower or cheaper recheck than
  a full audit, measured, not assumed.
- #3140's post-merge acceptance is extended to this set; plan §5.5
  currently keeps the comment recheck unchanged under it.
