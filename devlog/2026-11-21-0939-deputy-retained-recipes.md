# A Deputy Root Keeps the Recipes Its Approvals Replaced

Deputy settings gain an optional `retained_recipes` list. The deputy, its
preflight and prelaunch inspections, and the `auth adopt` it runs open the
store with `approved_recipe` plus every retained recipe. `exit-root.sh` adds
the replaced recipe to the list when an onboarding approval names a new one.
Contract: #3556, filed during #2382's routing rerun.

## The Failure

The deputy opened its store approving `approved_recipe` alone. The store
re-gates `publish_eligible` against that set on every artifact and attention
read, so evidence stored under the recipe an approval replaced failed closed
(`publish_eligible is inconsistent with provenance`). On the rerun's root
(`~/.freeside/exit-2382-real`) the approval at 9d6f4903 moved the recipe from
`0bc4fcdc` to `71b1bc7b`. The next start's local backup producer read a
verification transcript from the earlier run and exited the daemon, and the
preflight's Codex re-enrollment check reported a hold it could not verify.
A root that re-onboards to a new recipe could not start again.

## Chose a Retained List Over Widening `approved_recipe`

`approved_recipe` stays one digest because it is also the recipe the agent
image must carry (`claude_image_selection.go` compares the image's recipe to
it). A second key keeps that check unchanged and says which recipe new
bindings use. Prod already takes a repeated `-approved-recipe`, and so do
`auth adopt`, `renew-codex`, and the driverless daemon, so the store and
engine already handle a set; only the deputy's settings could not name one.

## Chose One Approved Set Over a Read-Only Set

The daemon passes one recipe set to both the store and the engine
(`daemoncompose.StartOptions.ApprovedRecipes`). A retained recipe therefore
stays approved for verification as well as for reads: a task already bound
to an image under it still verifies and can publish under it. That matches
prod's repeated flag and what the root allowed before the re-pin, since the
owner approved that recipe too. A split, where the store reads under the
union and the engine verifies only under `approved_recipe`, would need a
daemoncompose change and would strand in-flight tasks bound to the old
image. Rejected for this unit; revisit if a retained recipe ever needs to
read without verifying.

## Authority

`retained_recipes` is operator configuration in the same trusted settings
file as `approved_recipe`. Validation refuses a non-digest, a repeat, and the
approved recipe itself. `exit-root.sh` only ever adds a recipe that the root's
own earlier approval set as `approved_recipe`, and drops a retained recipe
that a later approval makes current again.

## Refute-First

- **Claim: a retained recipe lets new work run under an old recipe.**
  Disproved for new bindings: the agent image check still compares
  `approved_recipe` alone. Allowed by the decision above for tasks already
  bound to the old image.
- **Claim: `auth adopt` opens the store with a different set and fails on
  the same evidence.** Disproved by
  `TestDeputyStoreReadsEvidenceUnderARetainedRecipe`, which parses
  `deputyAdoptArgs` and checks it approves exactly `storeRecipes()`.
- **Claim: an inspection path still opens the store with one recipe.**
  Disproved by a sweep: every `store.Open`/`OpenReadOnly` in
  `cmd/freesided/deputy.go`, `preflight.go`, and `prelaunch.go` that took
  `ApprovedRecipe` now takes `storeRecipes()`. The standalone
  `freesided preflight` CLI still takes one `-approved-recipe`; it serves prod
  roots, not deputy settings, and is out of this unit's scope.
- **Claim: the deputy's preflight or daemon config drops the retained
  recipes.** Confirmed as untested by the fresh-context review; now pinned by
  `TestDeputyConfigsApproveTheRetainedRecipes`.
- **Claim: a root re-pinned before this change cannot recover through the
  script.** Confirmed: `pin_approved_image` runs only for a new approval, and
  `retained_recipes` is a script-set path a template may not supply. Allowed:
  such a root adds the replaced recipe to `retained_recipes` in
  `<root>/exit-root.json` by hand, as `daemon/README.md` now says. The
  script never prunes the list, so revoking a retained recipe is the same
  hand edit.
- **Claim: an operator following the prelaunch walk by hand reproduces the
  failure.** Confirmed; the walk now says to move the replaced recipe into
  `retained_recipes`.
- **Claim: the fresh render gains a key and breaks the settings key-set
  check.** Disproved by `scripts/test-exit-root.sh`: `render_settings` omits
  an empty list, and the fresh-render key set is unchanged.
