# Bind a No-Image Project's Recipe by a Settings Section the Admission Pins

Issue #3620. Tier A (recipe approval and stored run settings), kind:contract.

## Decision

A project with no project image binds its verification recipe with a new
project-settings section, `recipe_binding` (`repository`, `repository_id`,
`commit_sha`, `recipe_digest`). A project sets exactly one of `agent_image`
and `recipe_binding`. A writer admission pins the binding by value in a new
optional field, encoded under `freeside.execution.admission/v8`, and every
proof that names the recipe reads it through one resolver,
`productionBinding.boundRecipe()`. The digest's approval is re-checked
against the current approved recipes at each use, never trusted from the
stored binding. A warded writer is refused for a no-image project.

A no-image project does not run yet. The installed daemon composes no driver
for it, the stored project authority every gate reads (#3659) refuses it so
its tasks hold under another project's driver, and a fresh verification
refuses before it builds a room, because there is no room to verify in
until #3627 builds one. The authority refusal was added at rebase: once the
gates read each task's own project, a manual submission for a no-image
project could otherwise be admitted under an image project's driver and
reach the room refusal.

## Why a Settings Section, Not a Table

- **The binding is run configuration.** It changes when the project's
  approved recipe changes, as `agent_image` does. Settings revisions already
  give it append-only history, `set_by`, and the loader's re-check against
  current policy.
- **A table would duplicate that.** It would need its own migration,
  history, and gate, and a second place the loader reads a project's recipe
  source from.

## Why the Admission Pins It by Value

- **A settings change can't move an in-flight run's recipe.** The proofs a
  run builds after admission (handoff, readiness, re-entry, closure) bind the
  digest the admission recorded, the way an image run binds its admitted
  image. Reading current settings at proof time would let a `set` between
  admission and publication change what a run is verified against.
- **A new label keeps existing identities.** The field is omitted when
  unset and selects v8 only when set, so every existing admission keeps its
  bytes and ID, and the existing goldens are unchanged.

## Why Exactly One Source

Both set would make the resolver choose between two recipes, and neither
leaves a run with no recipe to bind. The store refuses an append that would
leave both, the loader refuses neither and names both sections, and the
settings gate re-runs both rules. Switching is clear one, then set the other.

## Why Approval Is Checked at Use

The binding records which recipe a project means, not that it is approved.
Every decision that binds `boundRecipe()` checks it against `w.recipes` where
it uses it, so revoking the recipe holds a no-image run's publication exactly
as it holds an image run's (`HoldRecipeRevoked`). The admission's decode
doesn't refuse a revoked binding, just as a project-image row isn't refused
on read: a candidate already ready can still finish after its recipe is
revoked, as an image run's can. At load, the gate also
refuses a binding whose digest is not `approved_recipe` or whose repository
is not the project's. The engine's approved set is wider: it keeps any
image-policy adoptions the repository recorded while it had an image, as an
image run's does.

## Rejected Options

- **A placeholder image record for a no-image project.** It would satisfy
  the image lookups without an image to run, and every check that reads the
  image as real would need to know it is fake.
- **Holding or retrying a no-image verification.** The missing room is not
  transient, so the refusal (`errNoVerificationRoom`) is unclassified: it
  stops the publication lane loudly, for every run, instead of parking one.
  That is acceptable only while nothing admits a no-image project's task:
  no driver composes for one, and the stored project authority refuses
  one. #3627 must lift both refusals together with this one.

## Refute-First Findings

One fresh-context reviewer tried to prove the change wrong and found no
blocker.

- **The room refusal stops the lane: confirmed, allowed.** The decision
  above; unreachable while nothing admits a no-image project's task. The
  rebase onto #3659 made it reachable through another project's driver,
  which the authority refusal closes.
- **The remediation source compares its own image's recipe: confirmed,
  allowed.** The issue keeps that image-to-image read, so a no-image
  remediation fails its identity check closed. #3627 must change it, with
  the handoff stop account's `binding.image.ImageRef` and the re-entry
  reads that use the image.
- **The gate comment called `approved_recipe` the whole approved set:
  confirmed, fixed.** A repository's old image-policy adoptions still
  approve.
- **Clearing `agent_image` alone leaves neither source: confirmed,
  allowed.** It is the documented first step of a switch, and the loader
  refuses the result and names both sections.
- **No engine test drives the revoked-recipe hold or the first-cycle
  checkpoint loader for a no-image run: confirmed, allowed.** Both read the
  same `boundRecipe()` lookup the resolver test pins, no engine-level
  revoked-hold test exists for image runs to extend, and a no-image run
  can't be imported or verified yet. The re-entry checkpoint loader test
  covers a stored verdict with another digest.

Revisit when #3627 builds a verification room for a no-image project: the
room refusal and the driverless start go, and the room's recipe read must
match `boundRecipe()`.
