# Reusable Project Images Hydrate Each Fresh Verification Workspace (#334)

## Decision

Chose a reusable daemon-internal builder that derives a temporary image context
from one canonical repository identity and exact commit, rather than restoring
the checked-in `images/project-gh-imgup/` directory removed by #335. The
builder copies only the selected commit's `package.json` and
`package-lock.json`, the exact trusted recipe bytes, and a fixed preparation
helper into a temporary context. A dependency update therefore produces a new
runtime artifact without importing another repository's lockfile churn into
Freeside source.

The first implementation supports npm lockfiles only. This is the selected
repository's real dependency shape and is enough to establish the reusable
seam. Rejected a detector framework for unobserved toolchains: #334 explicitly
excludes broad recipe-detection policy, and adding strategy abstractions before
a second shape exists would make onboarding policy implicit in plumbing.

## Fresh-Workspace Preparation

Chose an image-owned, fixed preparation command before each trusted recipe argv:

```text
/usr/local/bin/freeside-project-prepare
```

The helper runs `npm ci --ignore-scripts` in the verifier's fresh workspace,
using the baked npm cache and global npm configuration with networking
disabled. Before hydration
it compares the workspace's `package.json` and `package-lock.json` byte-for-byte
with the inputs baked into the image, rejects project `.npmrc` and
`npm-shrinkwrap.json` files that could override those inputs, and fixes npm's
user and global configuration paths. A dependency or install-policy change
therefore cannot reuse an older cache even when its selected packages happen
to be present. The
durable `ProjectImage` record carries this command with the repository, commit,
recipe digest, base image, and produced image so #237 can construct the
corresponding project-image-aware `verify.Room` after admission.

This prelude is required by two existing contracts that are both preserved:

- #232 selected the verbatim recipe `npm run lint`, `npm run typecheck`, and
  `npm test`; none installs `node_modules`.
- The verifier materializes a different pristine workspace for every recipe
  argv so candidate code in one check cannot rewrite what a later check reads.

Rejected sharing one workspace across recipe commands because that would weaken
the verifier's established isolation invariant. Rejected adding `npm ci` as an
earlier recipe command because its workspace would correctly be discarded
before lint. Rejected the old `npm ci && ...` shell chain because it is not the
selected recipe and the verifier deliberately refuses shell command strings.
Preparation is trusted room setup; after it completes, the declared argv
is passed directly to Apple `container run`, with the runtime setting
`/workspace` as the process working directory.

## Offline and Returned-Object Proof

Chose a two-direction proof before publication. Every recipe command gets a
fresh exact-commit workspace, preparation runs with networking disabled, and
the recipe argv must pass. A separate pristine workspace masks
`/opt/freeside/npm-cache`; preparation must then fail with a registry/network
error class. A generic nonzero result is insufficient because a container that
never started or an unrelated npm failure says nothing about whether the cache
was load-bearing.

The builder treats Apple `container` output as an external returned-object
boundary. It resolves the repository name and numeric ID through GitHub, clones
with a replacement environment rather than inheriting any ambient `GIT_*`
configuration, and materializes direct Git object bytes through the verifier's
exact-tree path. It verifies the operator's local base tag, copies it to an
unguessable private alias, verifies that alias against the approved digest, and
builds through the alias. It then proves the derived rootfs starts with that
exact private base's layers. It obtains and validates the built digest,
cross-checks every provenance label, saves the image as an OCI archive, verifies
the archive descriptor and every compressed layer digest on the host, and reads
both embedded provenance files from the effective layered filesystem without
running image code. Effective-filesystem reconstruction rejects direct,
ancestor, and opaque whiteouts before consulting an older layer. It seeds the
exact registry reference, re-inspects that reference for the same digest, and
runs the existing ward image-side allowlist against both the local build and
published digest. Only then does it construct and persist the content-addressed
result. Rejected trusting a pushed tag, a caller-supplied verification bit,
checkout-smudged files, image-owned hashing tools, or build stdout.

Every builder-owned registry and verification container uses the runtime's
generated UUID from a private `--cidfile` plus an independent ownership-token
label. Cleanup re-inspects that immutable runtime ID and label before deleting
that same ID, rather than checking and then reusing a caller-assigned name.
Verification commands use `--rm` as the ordinary instance-owned cleanup, with
explicit cleanup outside a canceled command context only as a fallback. A
failed local publication removes its
registry. A successful publication transfers cleanup ownership to the builder,
which retains the registry as managed runtime state only after the required
recorder durably accepts the result; a record failure removes it. A later build
using the same loopback port lists the runtime's containers, finds exactly one
running pinned registry image carrying both the builder ownership token and
port label by inspecting each safely shaped immutable ID from the deliberately
minimal Apple `container list` output, and only then requires its registry API
probe to pass. It reuses that retained service rather than trying to start a
conflicting second registry. A ready registry without that runtime identity is
rejected as foreign. Because the later invocation did not create the retained
service, it does not claim cleanup ownership over it.

## Trust Inputs and Policy Boundary

The manual primitive still takes the trusted recipe and approved base as
explicit operator inputs. It does not mark either approved and the
`ProjectImage` row carries no publish-eligibility bit. Rejected re-gating the row
against a current automation trust profile: #334 must create the real runtime
artifact before onboarding exists, while #238 owns profile creation and recipe
detection. The durable row records strict full digests and immutable
provenance; later selection/admission remains responsible for choosing a row
under current policy. For that reason this unit does not expose an execution
room from a merely self-consistent `ProjectImage`; #237 owns the admission gate
and the room constructed after it.

The builder does not execute a host allowlist script. It creates the stopped
probe directly through the configured Apple runtime, decodes the inspection
with the ward's native Go decoder, and compares the realized image-side fields
in Go. The configured runtime is therefore the only host executable involved;
ambient `PATH` entries cannot substitute a shell, JSON parser, or script helper
to forge success. `scripts/check-agent-image.sh` remains an operator-facing
manual diagnostic, not evidence the builder trusts.

An optional credential-free build-only HTTP proxy is passed through BuildKit's
predefined proxy arguments. It exists for hosts whose VPN makes container DNS
unusable, is not declared in the Containerfile or recorded in the image, and
never reaches verification: every positive command and the negative probe
still run with `--network none`.

## Refute-First Review

Confirmed and fixed:

- ambient `url.*.insteadOf` and clean/smudge filters could redirect or rewrite
  source while a checkout still appeared clean;
- inherited `GIT_OBJECT_DIRECTORY`, alternate-object, or counted-config
  variables could still redirect object reads after config-file scrubbing;
- suppressing tags during the bare clone omitted exact commits reachable only
  through a release tag after its branch moved;
- caller-assigned container names allowed collision and replacement races to
  arm cleanup against a foreign container;
- a mutable base tag could change between the initial inspection and the
  build, making a layer-prefix check prove a different base;
- a changed workspace manifest could reuse an older image when all newly
  selected packages happened to exist in its cache;
- a candidate `npm-shrinkwrap.json` or project `.npmrc` could override the
  compared lockfile or npm policy, while user-level configuration could vary
  hydration independently of the builder-owned global configuration;
- an approved lifecycle hook could invoke candidate-edited code during
  hydration and mutate the workspace after its manifest check;
- image-shape success alone did not prove the returned bytes held the recorded
  repository, commit, recipe, helper, or base;
- an image-owned `sha256sum` could lie about the embedded recipe and helper;
- a generic nonzero container CLI result blurred runtime failure with an inner
  command exit, cancellation could leave an anonymous container behind, and
  candidate lifecycle output could forge the negative failure class;
- deleting the local registry before persistence left its returned digest
  reference runnable only through incidental host cache state;
- releasing registry cleanup ownership before the store write could orphan an
  unrecorded service when persistence failed;
- checking an ownership label and then deleting by reusable name left a
  destructive TOCTOU window between those two runtime calls;
- a retained local registry made the next build on the documented port fail
  when publication tried to start a second service instead of reusing it;
- registry readiness alone could make a same-port rebuild publish to and
  durably reference a foreign loopback service;
- Apple `container list` omits the labels and image identity needed to select
  that retained service, so discovery has to inspect each listed runtime ID;
- the allowlist helper's bare `container` lookup could prove a different
  executable than the builder's resolved `-container` override;
- resolving the supposedly canonical checker relative to the caller's working
  directory could execute unrelated host code without a content binding;
- the legacy allowlist script still used a predictable container name and
  force-deleted that name without rechecking an ownership token;
- checker cleanup initially armed only after a successful `create`, missing a
  partially failed or interrupted create whose cidfile already held its UUID;
- checker cleanup silently preserved a probe and the original success status
  when its ownership re-inspection failed after verification;
- successful builds retained every uniquely tagged local project image even
  after publication, so repeated invocations accumulated runtime storage;
- an owned retained registry that stopped remained discoverable but blocked
  every later build on that loopback port until an operator deleted it;
- image-inspect labels and diff IDs were not cryptographically bound to the
  manifest descriptor whose digest the builder recorded;
- publication removed the unique build tag but retained its second local
  registry-tag alias after both success and later failures;
- resolving both provenance files early in one layer skipped a later tar entry
  that replaced an ancestor and removed the realized paths;
- concurrent builds shared the caller's publication tag, allowing one build to
  overwrite or delete the alias while another still needed it;
- concurrent builds on one loopback port could reuse a newly started registry
  before its owner durably recorded a result, then lose their own recorded
  reference when that owner failed and exercised its cleanup lease;
- parsing an exit marker from shared recipe stdout let a candidate background
  writer append a forged success after the real command failed;
- checking only a provenance file's own whiteout missed later-layer whiteouts
  of its ancestor directories and the root opaque marker;
- a structurally valid `ProjectImage` could have reached an execution room
  without a current trust-profile admission decision;
- the manual CLI let a later flag replace the canonical image checker;
- executing a content-pinned checker through `#!/usr/bin/env bash` still let an
  ambient `PATH` substitute the interpreter or `jq` and forge allowlist success;
- the native replacement initially ignored failure removing its private
  runtime-identity file after an otherwise successful proof;
- cleanup ownership inspection initially accepted duplicate JSON keys, letting
  ambiguous identity or label evidence reach the destructive delete decision.

The fixed runner launches the exact recipe argv as the container process and
takes its exit status only from the host process result, never from
candidate-controlled output. It preserves output and truncation, maps signal
status to the room's signal result, and treats failures without a
runtime-assigned cidfile as runtime errors. The negative probe runs the
builder-owned `npm ci --ignore-scripts` command so repository lifecycle code
cannot supply its network-error evidence. Once a build succeeds, cleanup of
its unique local image reference remains armed across every later proof,
publication, and persistence return path; only the published digest reference
is durable. Registry discovery re-inspects a stopped service's immutable ID,
ownership token, port label, and pinned image before restarting that exact
container and its existing storage; unknown or changing states still fail
closed. Host-side OCI verification hashes the manifest's config blob and uses
only its labels and diff IDs for the provenance decision, for both the approved
base and project image. It also hashes every uncompressed layer against the
config's corresponding diff ID, so a malformed archive cannot make the bound
config describe different rootfs bytes. The runtime's unbound inspect
projection supplies no trusted provenance fields.
The temporary publication tag is cleanup-armed immediately after creation and
removed before the registry cleanup lease can transfer to the durable result.
Effective-filesystem reconstruction continues checking same-layer whiteouts,
opaque directories, and ancestor replacements after capturing a target; only
targets resolved by a newer layer are exempt while scanning an older layer.
Each build adds a cryptographically random suffix to its bounded one-shot
publication-tag prefix; only the digest reference is durable. Probe cleanup
continues refusing deletion unless the runtime UUID and ownership token can be
re-inspected, but any inspection, deletion, or identity-file cleanup failure
now turns an otherwise successful standalone-checker result into a failure.
The builder's native allowlist probe uses the same UUID and ownership-token
cleanup discipline without invoking that script. Its identity-file removal is
also fail-closed. Every JSON trust boundary in the project-image package now
reuses the ward's duplicate-key rejection before typed decoding, so ambiguous
runtime, OCI, or repository identity evidence cannot reach a proof or cleanup
decision.

Local-registry discovery, creation, publication, cleanup, and durable recording
are serialized per loopback port with a cross-process advisory lease in the
owner's cache directory. The publication transfers that lease to the builder:
recording success releases it after the row is durable, while recording failure
deletes the still-owned registry before releasing it. A following build can
therefore observe either the retained post-record registry or the cleaned
failure state, never the in-progress interval. External registries do not take
this host-local lease. Rejected a process-local mutex because separate CLI
invocations would bypass it, and rejected an immutable container-label state
because it cannot transition atomically with the database record.

Rejected by verification: recipe argv rewriting (the runtime receives the
supplied tokens directly), tag substitution after publication (the exact
digest is pulled and re-inspected), structural store tampering (ID
recomputation plus extracted-column cross-checks fail closed), checked-in
per-project definitions, and overlap with #330 (`daemon/internal/ward/**` and
`images/**` remain untouched).

Accepted by decision: #334's manual proof resolves and clones the selected
public repository without credentials. GitHub App installation resolution,
repository-scoped token minting, and the credential injection needed for
private repositories remain in #238's `freesided onboard <repo>` scope. Adding
an ambient token flag or credential-bearing Git configuration to this manual
primitive would preempt that trust boundary and violate #334's onboarding UX
non-goal.

## Manual Proof

The primitive was exercised against the selected repository on 2026-07-27:

- repository: `freeasinbird/gh-imgup` (`1278475858`);
- commit: `6ab4e3dff2be53f74bde9b8b3150290775152f9f`;
- recipe digest:
  `sha256:6d9aa0bfe897a64ee5a6af4e2e31c2bb1d5530fecf09644bf33a4c4df7152371`;
- approved Node 24 base:
  `127.0.0.1:5006/freeside-agent-claude@sha256:717d3c3260b4a7fcc0cd8631526328bcb2969e2d738e1a17c047e1908b99d3f7`;
- complete pre-final-review publication:
  `127.0.0.1:5106/freeside-project-freeasinbird-gh-imgup@sha256:7e42a1767a385333d7cf5ca2697617670b5efbcde801979b586ded8dfcb15522`;
- durable project-image ID:
  `sha256:0c8d38e053548a9ff646c4fb620610f8cadbbb3bca250cada8535a0cb9df199a`.

That local and published image passed the ward image-side allowlist. Each
declared command (`npm run lint`, `npm run typecheck`, `npm test`) passed after
fixed preparation in its own exact-commit workspace with networking disabled.
With the baked cache masked, script-disabled `npm ci` failed through registry
resolution. The exact digest was pulled, re-inspected, saved as OCI and
host-verified, and recorded in SQLite. A direct registry request returned that
same digest after the build command exited. Its managed loopback registry
backed the reference until it was explicitly removed to retry the
final-hardened image; execution containers and the build proxy were absent.

The final review hardening changed preparation to suppress all lifecycle
scripts and produced local digest
`sha256:8a3ebfcf6e0ce12b55253d94a27bc642ca336011bee07449c22290469454cbf7`.
That image independently passed provenance, allowlist, all three fresh
network-disabled recipe workspaces, and the cache-masked negative probe on
every rerun. Final publication could not be repeated after Apple `container`'s
host-port forwarding began resetting or refusing every new loopback registry;
the builder removed each failed registry, including a forced recorder-failure
path covered by unit tests. No final-hardened row was recorded. This is a
manual verification gap, not evidence that the final publish path passed.
The runtime-assigned UUID lifecycle was separately exercised against the final
local image; `--rm` removed that exact instance after a network-disabled run.
The later-hardened allowlist checker was also exercised against a retained
project image through `/usr/local/bin/container`: the runtime assigned probe ID
`1c79c53a-04ca-4372-9256-d9d18f32cccf`, the allowlist passed, and the
ownership-checked cleanup removed that exact ID.

The selected repository supports Node 24 even though #232 described Node 22 as
the pinned project input. The manual proof records the actual Claude base's
Node 24 leg; changing the agent base is outside this unit.

Revisit when a second dependency toolchain is selected, Apple `container` can
resolve local digest references directly, or the project-image-aware room is
packaged behind `freesided onboard` in #238.

## Refute-First Pass After the Final Review Round

A fresh-context adversarial sweep over the branch (five lenses: decoded-object
trust, hostile candidate inputs, tooling substitution, cleanup ownership, OCI
provenance completeness) ran alongside the reviewer's final round. Dispositions:

Confirmed and fixed in this unit:

- The provenance proof was bound to its own re-inspection of the mutable local
  tag, not to the digest the builder publishes and records; `provenanceSpec`
  now carries the built digest and `CheckProvenance` fails unless the tag
  still resolves to it.
- OCI `Config.User` was discarded by the typed config decoder (the reviewer's
  final P1); both digest-bound configs now reject any non-root user, since
  plan §5.7 records that runtime inspection cannot observe the launch user.
- An interrupted or identity-losing `create` orphaned the probe, execution, or
  registry container; a token-scoped recovery lists containers, re-gates
  ownership through inspection, and deletes only the owned instance.
- The shell checker's ownership token was predictable (`$$`/`RANDOM`/time);
  it now draws 16 bytes from `/dev/urandom`, mirroring the daemon's
  `crypto/rand` token.

Deferred with tracker issues (hygiene, not trust violations: persisted rows
are re-gated on reconstruction, so unreferenced residue confers no trust):

- Publication residue on failure and across builds (local seeded digest image,
  pushed one-shot tags in retained registries, proof-failed manifests in
  reused registries): #352.
- Duplicate-key JSON defense for `check-agent-image.sh`'s jq parsing: #353.

Rejected by verification:

- "Cancellation is misreported as a proof failure": the signal-style `-1`
  mapping is a deliberate, test-pinned decision
  (`TestAppleRunReportsContextKillAsSignalExit`); no verdict is persisted on
  any failed build, so the cost is an error string, not recorded state.
- "Candidate lockfiles can direct build-time egress past the proxy": build
  egress is by design unconstrained (the build requires the network); the
  enforced boundary is the network-disabled offline proof, which a
  git+ssh/tarball dependency still has to survive with scripts disabled.
