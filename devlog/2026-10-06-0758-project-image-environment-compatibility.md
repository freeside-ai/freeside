# Project-Image Environment Compatibility

Issue #1230 lets one project image serve every base that leaves its
environment unchanged. It is a contract unit: `domain.ProjectImage`, its ID
preimage, a `project_images` column, and the four boundaries that bind an
image to a base move together. Plan §5.7 carries the rule (revision 81).

## Environment Compatibility Replaces Exact-Base Equality

Chose "the image's recorded environment inputs are unchanged at the base"
over "the base is the image's build commit". Equality was the only rule the
old record could support, and it made every base advance cost a rebuild and
a re-pin, including a README edit.

An image is admissible at a base when the base is its build commit, or when
the image carries an environment record and at the base:

- `package.json` and `package-lock.json` hash to the recorded SHA-256 values.
- Neither `npm-shrinkwrap.json` nor `.npmrc` exists, in any shape.
- The tree declares no recipe other than the baked one.
- The recorded preparation digest equals the running binary's.

Rejected: comparing resolved package versions. That misses installation
configuration, the Node toolchain, and the preparation helper, each of which
changes what the image provides.

`BaseImageRef` needs no clause. Reuse never crosses image records, so a
changed agent base is a different image, never a compatible one.

## The Recipe Clause Refuses a Contradiction, Not an Absence

The owner decided this on 2026-10-06, in session. The issue contract said
"the recipe at B (`.freeside/verify.json`) has the image's `RecipeDigest`".
That assumed the recipe lives in the tree. It does not for `gh-imgup`, the
repository the unit's acceptance names: onboarding accepts a recipe file
supplied outside the tree, the builder bakes it, and verification reads it
from the image. The literal rule would have refused every reuse there.

Chose "a base whose tree declares a different recipe is refused; a base that
declares none is compatible" over two alternatives:

- **The literal rule.** It leaves externally-onboarded repositories
  exact-commit-only until they commit a recipe and rebuild.
- **Recording the build commit's in-tree recipe state** and requiring the
  base to match it. It also refuses a recipe detected at the build commit
  and deleted at the base, at the cost of one more preimage field that the
  image itself cannot prove.

The reasoning: the image runs the recipe it baked, and every boundary still
checks that digest against current approval. A base that declares nothing
leaves the approved baked recipe as the only statement of how to verify. A
base that declares a different recipe says verification changed, and the
image would run the stale one.

The accepted consequence: a recipe detected in-tree at the build commit and
then deleted at the base is admitted. The run executes the same approved
recipe either way.

## The Preparation Digest

One digest covers the builder's fixed sources: the prepare script, the
toolchain launcher, the Containerfile template, and the Node version and
archive SHA-256. The builder records the running binary's value; each
boundary compares the record with its own binary's.

- **An older binary's image stays usable at its build commit.** The
  exact-commit rule never consults the digest, so upgrading the daemon does
  not strand an image pinned at its build commit. The upgrade does end that
  image's use at any other commit: a new run, a re-entry onto an advanced
  base, and a reused task still awaiting verification are all refused until
  one rebuild.
- **A pin test holds the digest value.** Editing any covered source changes
  it and fails the test, so the cost (every earlier image becomes
  exact-commit-only) is a visible decision in the change that incurs it.
- **The digest is a length-prefixed SHA-256, not canonical JSON.** The plan
  proposed canonical JSON; the hash has no failure path, so a package-level
  function can return it without an error.

## Two ID Preimages Over a Separate Evidence Table

Chose `freeside.project-image/v2`, which adds the environment to the ID
preimage, with `v1` kept for a record that has no environment. Rejected a
side table keyed by image ID: the environment would then be mutable beside an
immutable record, and a boundary would have to decide what a missing row
means.

- **Legacy rows still decode and validate.** Their stored bodies lack the
  `environment` key and their IDs still verify under `v1`.
- **`environment_digest` is a cross-checked column, `NULL` exactly when the
  body has no environment.** A row cannot gain or lose evidence through the
  column or the body alone; disagreement is a row-inconsistency refusal.
- **The image proves its own manifests.** The provenance probe reads both
  seed files from the built image and compares their hashes with the spec,
  so the record's hashes are what the image holds, not what the builder
  intended.

## The Verdict Is Re-Derived, Never Stored

Chose re-deriving admissibility at preflight, daemon start, publication, and
re-entry over persisting a verdict on `ExecutionAdmission`. The admission
already names the image and the base; the pair plus the immutable record
determines the answer. A stored "compatible" bit would be a decoded trust
bit, which the daemon conventions forbid trusting at reconstruction.

`ExecutionAdmission` does not change.

- **An exact-commit image reads no base tree.** Every boundary skips the
  observation when the commits are equal. Daemon start therefore fetches
  the base only for an image built elsewhere, and exact-commit behavior is
  byte-for-byte what it was.
- **Daemon start fetches the base once.** It has no checkout, so it uses the
  publication transport's exact-base fetch into private scratch. Rejected a
  required `-repository-checkout` flag on `freesided`: a new production
  precondition for the app-launched daemon.
- **A base that cannot be read is not an incompatible image.** At daemon
  start and preflight the read error is reported as itself. In the
  publication lane a plumbing fault stays a loud failure, while a manifest
  in a shape no image could have baked (a symlink, an oversized blob) is the
  same refusal as an incompatible image.

## What a Refusal Ends in the Publication Lane

The plan put the check right after the base fetch and made every refusal a
durable-authority disagreement (`ErrParentKeyMismatch`). The refute-first
review showed what that costs: the lane treats that error as fatal, so one
refused task stops publication for every run on every pass and restart. A
base advance under a ready pull request is ordinary, and it makes a legacy
image, or a new one after a lockfile bump upstream, unable to serve the
re-entry base. Two changes follow.

- **The check runs where a verification room is about to be built,** inside
  the two verification paths, not after the fetch. A task that already holds
  its verification evidence is not re-judged: the evidence was produced
  under a passing check, and the preparation clause follows the running
  binary, so re-judging it would refuse finished work after an upgrade.
- **A re-entry refusal stops that one cycle.** Chose the existing re-entry
  stop (a durable item carrying the pull request, task retired) over the
  lane error. This is the mechanism re-entry already uses for a condition no
  retry can clear, and it is where dependency drift on the base ended before
  this unit, through an unclean verification.

First publication keeps the plan's class. Composition admitted the same
image at the same base before the run started, so a refusal there means the
binary changed in between or durable state disagrees. Rejected for now: a
per-task blocked outcome. First-publication block reasons are a closed
contract shared with the API, so adding one is its own contract unit.

## Legacy Records Stay Exact-Commit

Chose "a record without an environment is admissible only at its build
commit" over backfilling. The daemon never observed those inputs, and a
backfill would attest to them after the fact. One rebuild produces a record
that can be reused.

## Re-Entry Checks Its Own Base

A re-entered readiness cycle keeps its producer's image and runs it on a
newer base. It used to compare the image with the producer's base and then
swap the base, so an older image already ran on a newer base with no check
beyond the preparation helper's exit 42. It now applies the same rule to the
re-entry base. A legacy image therefore stops a re-entry whose base
advanced; before, that cycle ran unchecked.

## Absent Versus Unreadable

`verify.ReadFileAtCommit` reports absence only when the tree holds nothing
at the path. An entry in a refused shape is an error. The plan proposed one
boolean, "present as a regular blob"; that would have read `.npmrc` as a
symlink or a directory as absent, and admitted a base the builder refuses.

## Refute-First Findings

One fresh-context reviewer tried to break the change before it was committed.
A second one took the reworked publication-lane handling before it was pushed
and found no reachable defect, only behaviors no test pinned.

- **Confirmed, fixed: a re-entry refusal stopped the whole lane.** See the
  section above.
- **Confirmed, fixed: the verdict follows the running binary but was judged
  on finished work.** The check moved to the verification paths.
- **Confirmed, fixed: the re-entry test asserted only an error.** It now
  asserts the stop item, the retired task, a clean later pass, and that no
  room was read.
- **Confirmed, fixed: nothing pinned "finished work is not judged again".**
  The engine now takes the binary's preparation digest as configuration, so
  a test stands for an upgraded daemon: a cycle that lost its process after
  verification finishes under it, and an unverified one is refused.
- **Confirmed, fixed: a manifest in a refused shape had no lane-level
  test.** A base whose `package.json` is a symbolic link now stops the cycle
  in a test.
- **Confirmed, fixed: the stop named one remedy for every cause.** A rebuild
  cannot serve a base that holds `.npmrc`; the item now says to rerun with
  an image that serves the base.
- **Allowed by decision: a first-publication refusal is still a lane
  error.** Reachable when an upgrade changes the preparation digest while a
  reused task awaits verification. Deferred to its own contract unit.
- **Allowed by decision: a base whose `.freeside` is a symlink reads as
  declaring no recipe.** Every at-commit recipe read sees that tree the
  same way, onboarding included, so Freeside never reads a recipe through
  it; the baked, approved recipe still runs.
- **Allowed by decision: a same-commit rebuild can collide with a legacy
  record.** See Revisit When.
- **Disproved by a check:** reading the wrong commit or `HEAD`; an absent
  read of a refused tree shape; a swallowed observation error; a v1/v2
  preimage collision or downgrade; a column and body that disagree; new git
  or network work on the exact-commit path; a room built before the check;
  a scratch checkout left behind at daemon start; an ambiguous digest
  encoding.
- **Not covered by a test:** the closure that binds the publication
  transport's fetch at daemon start. Its fetch, identity check, and cleanup
  are tested through a seam; the binding itself needs a live run.

## Revisit When

- **#1629 lands the policy-gated rebuild.** The rebuilt image's `CommitSHA`
  is the candidate's commit, not the base's, so its admissibility at the run
  base rests on this rule.
- **A rebuild reproduces a legacy record's exact image digest.** `image_ref`
  is unique, so recording the environment-bearing result conflicts with the
  legacy row and the build is refused. The image labels its build commit, so
  only a rebuild at the same commit with a fully warm build cache can do
  this; whether Apple's builder then reproduces the digest is not verified.
  The legacy record stays usable at that commit, and a build at any later
  base yields a reusable record.
- **An upgrade changes the preparation digest in production.** A reused
  task awaiting first-publication verification then fails the lane until
  the earlier binary drains it. That is the moment to give first publication
  a per-task blocked outcome.
- **A second dependency ecosystem arrives.** The input set is npm's. Another
  ecosystem needs its own manifests and unsupported-configuration list in
  the environment record, which is a new preimage version.
- **A repository needs the deleted-recipe case refused.** Record the build
  commit's in-tree recipe state, the alternative rejected above.
