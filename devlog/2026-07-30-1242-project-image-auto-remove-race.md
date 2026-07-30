# Close the Final Real-Run Integration Gaps

Work unit: #237 final acceptance. Scope: `daemon/cmd`,
`daemon/internal/exec/claude`, `daemon/internal/importer`,
`daemon/internal/projectimage`, `daemon/internal/ward`,
`images/agent-claude`, `devlog/`.

## Decision

Chose to accept an exact-ID `notFound` from the defensive temporary-container
delete after a successful ownership inspection. Apple `container run --rm`
removes a completed container asynchronously, so the runtime can report the
owned instance during inspection and remove it before the following delete.
Absence is the cleanup's intended result, and rejecting that result prevents
the already-built image from reaching its offline proof.

Rejected removing the defensive delete. Interrupted, canceled, or
runtime-inconsistent commands may still leave an owned temporary container,
and the ownership-checked delete is the cleanup for those paths. Rejected
accepting a generic delete failure or a bare “not found” substring. The
accepted shape requires the runtime's container-ID-specific `notFound`
diagnostic naming the inspected ID, after the existing label-token ownership
gate has authenticated the exact object.

## Refute-First Verification

The observed race was reproduced by the real #237 project-image build on Apple
container 1.1.0: inspection found the runtime-generated ID, then delete
reported that exact ID absent. Regression cases preserve rejection of a
foreign ownership label and of an unrelated delete failure whose text merely
contains “not found” or names a different ID. The fix changes no registry,
image, volume, or workspace deletion rule.

Revisit when Apple container makes `run --rm` removal synchronous, or exposes
an idempotent delete operation with a structured absence result.

## Keep the Installation Janitor Active in Production

Chose to start the always-on installation janitor while composing the
production Claude transport, wait until it publishes coverage for every
configured App registration, and retain it as a daemon-owned session. The
driver's conformance fetch is the first credentialed operation during
composition; constructing a janitor without running it made that fetch
deterministically fail with `ErrJanitorInactive`. A one-off `RunCycle` was
rejected because its contract deliberately does not activate the runtime
gate. Starting the loop later in the daemon was rejected because conformance
already needs its coverage.

The janitor now fails startup if it stops before coverage, stops the daemon if
it exits while the daemon is live, and is canceled and awaited after the
credential-bearing driver during shutdown. Tests pin the coverage-before-
return barrier and propagation of an early janitor failure.

Revisit when transport construction no longer requires live installation
coverage, or the janitor exposes a first-pass readiness primitive directly.

## Carry Ward's Egress Witnesses in the Agent Image

Chose to install Debian's recorded `busybox-static` package and expose its
`nslookup`, `nc`, and `wget` applets in the credential-bearing agent image.
The required conformance suite runs its benign writer in that image and uses
those exact BusyBox command contracts to distinguish a blocked network from a
missing or unusable diagnostic tool. The first production run stopped before
writing its run sentinel because the image carried none of them.

Rejected weakening or skipping the behavioral egress checks for production.
Rejected rewriting the generic ward probes around Node merely because this
agent currently carries Node: the established BusyBox contract is already
shared with the exporter and live reference suite, while Node is a
vendor-image implementation detail. The applets grant no route on their own;
ward still creates and inspects the network boundary, restricts the proxy to
declared provider authorities, and proves direct-IP and DNS failure before
credentials are admitted.

The build asserts the specific help shapes the suite interprets. The package
version joins the existing in-image dpkg manifest rather than acquiring a
fragile apt version pin.

Revisit if ward replaces shell-tool network witnesses with a runtime-native
probe whose implementation and diagnostics are pinned independently.

## Normalize Fresh Launch-State Filesystems Before Attestation

The first production launches reached the Claude state-volume preparation and
failed before credential observation or writer start. A credential-free probe
on Apple container 1.1.0 measured that each fresh volume contains one
`lost+found` directory with type directory, mode `0700`, uid/gid `0:0`, size
4096. Ward's state observer correctly rejected that undeclared entry, but the
state seeder had never normalized the filesystem root.

Chose to run the existing deterministic, credential-free launch-state seeder
once per volume before attestation. It removes `lost+found` only after proving
the exact measured directory type, non-symlink shape, owner, mode, and
emptiness, using `rmdir` rather than recursive deletion. Any other entry or
metadata still fails closed, and the observer continues to require the exact
config-root or empty manifest afterward. Reusing the one known seeder identity
keeps interrupted cleanup and recovery on the existing deterministic object
set.

Rejected teaching the observer to ignore `lost+found`: the writer would then
receive content the clean-state manifest did not cover. Rejected recursive
removal because a substituted or non-empty directory must stop the handoff,
not be erased.

Refute-first checks pin all three sequential mounts, every metadata predicate,
the non-recursive removal, and the final state proofs. The reference-runtime
acceptance reruns the same path against fresh Apple-container volumes.

Revisit when the runtime can provision a volume without filesystem-created
root entries, or exposes a structured filesystem-format contract ward can
attest without normalization.

## Attest a Traversable Read-Only Instruction Root

After launch-state normalization, the credentialed writer reached the pinned
CLI and exited before inference. A one-run diagnostic exported the transcript
only after writer teardown through the credential-free exporter and the normal
secret scanner. It reported `EACCES` opening `/root/.claude/CLAUDE.md`.
The seeder had made the file world-readable, but left the fresh volume root
non-traversable to the dropped UID. The earlier shell probe proved only file
mode plus `/root` traversal and therefore missed the mounted directory between
them.

Chose to make the instruction-volume root explicitly root-owned `0755`, then
extend the independent observer's existing clean/dirty verdict to require
exactly that metadata. The mount remains read-only in the writer, contains
only the composed instruction file, and grants traversal rather than mutation.
Rejected moving the instruction into a writable state volume or restoring a
root writer because either would weaken the reviewed read-only instruction and
privilege-drop boundaries.

The same production launcher now also writes the canonical evidence-source
descriptor for its JSONL transcript before dropping privilege. Driver tests
already treated that transcript as the run audit artifact, but without the
descriptor the exporter deliberately discarded the reserved subtree. The
descriptor is root-owned beside the protected outcome control and identifies
the agent invocation as a head-independent, sensitive claim.

The importer now admits `application/jsonl` evidence only after validating
every non-empty line as a complete JSON value. The closing run proved that
transcript export otherwise reached the importer and failed because its
image-only allow-set had no transcript representation. Treating arbitrary text
as evidence was rejected: JSONL gets a bounded structural check against the
already size-capped verified snapshot, while malformed, blank-record, empty,
or mislabeled content still fails closed.

Revisit if the pinned CLI gains a file-only instruction mount contract that
does not require a traversable vendor directory.

## Remove Runtime Dependencies Before Export

Two independent #81 attempts completed the writer but were rejected by the
post-writer scanner because one returned blob carried a PEM private-key shape.
A read-only scan of the exact admitted gh-imgup base found no such shape. The
same blob digest then recurred in an unrelated #76 test refactor, even after
the work specification prohibited credential-shaped test data. That changed
the diagnosis: the common input was the project image's required `npm ci`,
which materializes `node_modules` inside the workspace. The exporter
intentionally walks the entire workspace and therefore scanned that
runtime-only dependency tree before import policy could reject its
out-of-scope paths.

Chose to have the root launcher remove the fixed `/workspace/node_modules`
directory after the unprivileged CLI returns and before writing the outcome
marker. The dependency tree is reproducible project-image state, not repository
output. Removing it before the success marker makes cleanup part of writer
success and leaves the whole returned tree subject to the unchanged scanner.

Ignoring `.gitignore`, relaxing the PEM rule, special-casing tests, redacting
returned bytes, or filtering scanner input was rejected because each could hide
agent-controlled returned bytes. The cleanup uses one literal path; `rm -rf`
does not follow a substituted top-level symlink, and any cleanup failure
prevents a success marker and fails closed. The rejected runs remain durable
loss outcomes and do not count toward final acceptance.

A refute-first shell test replaces `node_modules` with a symlink to an outside
directory, runs the exact fixed-path cleanup primitive, and proves the link is
removed while an outside sentinel survives. The command-order test separately
pins cleanup after the unprivileged writer and before its outcome marker.

After dependency cleanup removed the credential-shaped blob, the next #76 run
reached import and reported 19 publish-blocking findings. Reproducing the
trusted recipe locally showed that `npm test` leaves 18 compiled files under
`dist/`; that generated directory is outside the admitted `src/**` scope.
Final acceptance specifications therefore require the repository's existing
`npm run clean` after verification. Generated project output remains visible
to the unchanged importer if an agent omits cleanup, while a compliant work
item returns only its intended source changes.

## Final Acceptance Evidence

All accepted runs used repository `freeasinbird/gh-imgup` (GitHub repository
ID `1278475858`), exact base
`6ab4e3dff2be53f74bde9b8b3150290775152f9f`, the admitted `src/**` scope,
the same trusted policy, the identity-bound setup-token volume, the rebuilt
digest-pinned project image, and the digest-pinned exporter.

- The small documentation work item
  `run-957b63f7036e4c5d9fc337a20de2b74d8c58f4725b03927173e349f4cb194c01`
  produced verified head `d7873f06ef3e1a036f8c8d7260e250762ec076ce`.
- gh-imgup #76
  `run-b20476c3923721c39a51e11e439bc787d1f3d45144c402b564f71f553da85e5f`
  produced verified head `1fd75175c57904a9831822bf6a56e36f7150d0d6`.
- gh-imgup #79
  `run-792f3a04369a300242f1fe7d88ba2054d9397c5d87dfbe912e75213cbbe853b9`
  produced verified head `83acc47bd670a726788a9485d3d165029129c55a`.
- The post-self-review no-change validation
  `run-4b61b39cee0d0eb383626e23a650219d8a4cdb11b481d5b25ece27dc97da6738`
  bound the final runner code to verified head
  `547942e363bbea508906a9f58475dede8385235e`.

Each run passed the production runner's conformance preflight, authenticated
CLI launch, writer teardown, post-run credential observation, whole-output
credential scan, transcript evidence import, path policy, clean candidate
construction, and durable `ExecutionExport` check. Earlier diagnostic or
rejected attempts remain recorded as failed/loss outcomes and are not counted.
