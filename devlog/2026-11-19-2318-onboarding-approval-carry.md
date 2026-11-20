# Onboarding Carries an Approval Forward When Only the Commit Moved

`freesided onboard` now completes a review pass without an owner approval
when nothing the active approval trusts changed: the installation is
already trusted, the proposed profile's digest equals the active profile's,
and the admitted image (the Claude harness pointer) serves the new commit.
The pass re-checks the installation binding and the janitor's grant as an
applying pass does, then re-activates the same profile. An applying pass
reuses a serving image instead of building. Every other review still needs
the owner and now lists what changed. Contract: #3435. It came from #2382's
exit run, which needed six approvals and as many image builds for merges
that touched neither workflows nor dependency manifests.

## Chose Comparing Against Current State Over Storing Approvals

The carry compares the fresh review with what is already authoritative: the
active trust profile and the admitted-image pointer. The alternative was a
stored onboarding-approval record that a later review could match. That
needs a new domain type, a migration, and a contract unit, and it would be
a second authority beside the profile activation that runs already read.
Comparing against current state needs none of that, and the activation row
plus the fresh workflow-audit row are together the record of a carry.

Re-activating the same digest appends an activation row and prunes
workflow-audit evidence for other digests, the same thing the original
activation did. Nothing reads an activation's time as a fresh owner review
(checked at `44c56e778`: the store's activation queries select the latest
row by id, and none reads `activated_at`).

## Chose Keeping the Commit in the Approval Digest

The approval digest (`freeside.onboard-approval/v2`) still binds the image
request, commit included, and the audit still covers only the live tip. A
digest without the commit would let an approval issued for one tree apply
to another on the applying pass, where a build follows. Instead the review
pass decides the carry, from the profile digest, which already excludes the
commit: plan §5.5 binds the owner's review to that digest. A moved tip now
fails with the exact review command to run next (same arguments, `-commit`
at the tip, no `-approve`).

## What "the Image Serves the Commit" Requires

The image bakes the requested recipe digest on the requested agent base,
sits at the requested registry and image name (`ValidatePublishedRef`),
names the requested repository and the fixed preparation command, and is
admissible at the commit (`ProjectImage.AdmissibleAt`, over inputs read
from the `-source` checkout). This is the same admissibility rule run
admission applies, so a carried image is one a run would admit anyway.

The agent added one check the issue contract did not list: the image's Go
module and Swift package directories must equal the request's. Without it,
an npm-only image would carry onto a request that newly declares a Go
module, because `AdmissibleAt` reads inputs under the image's layout, not
the request's. A changed image request still needs the owner (the
contract's first non-goal), so the check belongs to the rule.

The fresh-context review found a second gap: `AdmissibleAt` accepts any
image at its own build commit without reading its environment, so a
same-commit review after a `freesided` upgrade would have carried an image
built by the old toolchain. The rule therefore also requires the image to
record this binary's preparation digest, as every build onboarding admits
already must.

The rule does not notice a commit that changes which toolchain a fresh
build would select (a root `package.json` added beside a root Go module):
the inputs are read under the image's layout. Run admission has the same
gap, so a carry admits nothing a run wouldn't; closing it belongs to the
shared admissibility rule, not to onboarding (follow-up: #3508).

Fields of the image request that only shape a build (DNS, build proxy, base
build ref, ref tag) don't block reuse: no build runs, and the image is
digest-pinned.

## Chose Asking the Owner on a Manifest Change

A commit that changes `go.mod`, `go.sum`, `package-lock.json`, or a Swift
manifest makes the admitted image inadmissible, so onboarding builds and
asks the owner as before. Carrying across it would mean onboarding builds
an image no one approved. Image upkeep's policy-gated rebuild stays the
hands-free path for manifest changes.

## A Trusted Installation Is Not Re-Approved

The carry needs the installation to be trusted, not unchanged since the
last approval. A trusted binding exists only after an owner approved its
promotion, and runs already use the current trusted binding with the
active profile, so carrying adds no authority a run doesn't already have.
A pending intent never carries: only an approval promotes it.

A carry reads the active profile and later re-activates it without a
compare-and-swap. A concurrent approval can't slip between the two: all of
`onboard` runs under the App authority writer lock, which a second
`onboard` or a deputy run can't take.

Revisit when a stored onboarding-approval record lands, when the approval
digest stops binding the commit, or when run admission's image rule and
this one diverge.
