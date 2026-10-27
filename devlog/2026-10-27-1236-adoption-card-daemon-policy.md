# Approving an Image-Policy Adoption Card Is the Adoption

Plan revision 101. Mandatory note: a material plan change (§5.7, §5.8, §11)
to a trust-policy path, decided by the owner on 2026-10-27 on #2335
(option A). The plan text is #2378; the store contract is #2379; the card
itself is #2335.

## Decision

Chose to have the approved card record the adoption in daemon-owned trusted
policy (option A) over first moving recipe approval and the registry set into
a repository-hosted control-plane tree and building the §4 policy-proposal PR
path (option B). Approving an `image_policy_adoption` card records the exact
recipe digest or registry hosts it is bound to, for the card's repository
only, and upkeep then builds a new image at the tip under it, as onboarding
does. A declined card records nothing. §5.8 now names image policy (recipe
approval and the registry set) beside host vendor instructions as a class
not loaded from a default-branch commit; both keys live in operator host
configuration today.

## Why

- **Revision 98's route has no target.** It had the card's approval open a §4
  policy-proposal PR and made the merge the adoption. At `5a1fd118c` the
  registry set (`execution.registry_set`) and recipe approval
  (`approved_recipe`) are operator host configuration, not files in any
  repository, and no daemon code implements the §4 path. There is nothing for
  that PR to change.
- **Option B costs a new policy tree and a new path.** It needs a
  repository-hosted home for both keys, the loader that reads it from an
  approved default-branch commit, and the PR path, all before the first card
  can land. The card already carries the reviewed decision: a person approves
  one digest or one host set bound to a recorded policy-class refusal.
- **The card's scope keeps it narrow.** One repository, only the exact digest
  or hosts the refusal's findings name, re-derived from the tree at the
  refusal's tip; a stale card can record only what its refusal named. The
  daemon never edits the operator's configuration; the effective set for a
  repository is the operator's set plus its adoptions (#2379).
- **An adopted host meets the same bar as the operator's own addition.** The
  registry set is also the writer's `provider_registry` egress, and §5.4
  admits a per-project addition only for a public package registry. The
  card is that reviewed operator change, so the approver applies the same
  criterion, and any other authority stays a `provider_web_read` decision.
  Rejected: letting the card admit whatever host the refusal names, which
  would widen writer egress past §5.4 on one tap.

## Reversal

This reverses revision 98's departure from the owner's choice on #1999. That
choice named a one-tap, digest-bound `effect_proposal` into the trust
profile; revision 98 routed it through the §4 PR instead, on the reading
that §5.8 allowed policy changes only by an operator-authored PR or the §4
path
(`devlog/2026-10-26-1057-project-image-upkeep.md`, "The adoption goes
through the control-plane gate"). Revision 101 adds the approved card to
§5.8's reviewed changes for these two keys, so the gate still holds, and the
card returns to what the owner first chose.

Revisit when project policy moves into a repository-hosted control-plane
tree; adoption could then become a PR again.
