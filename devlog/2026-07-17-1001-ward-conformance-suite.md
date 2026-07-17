# Ward conformance suite: invocable §5.7 checks and the negative probes

Work unit: #77 (ward lane, gates 1A.2; after #76). Mandatory note:
safety-policy surface (the suite is the non-waivable §3.1 "failed runner
conformance including the handoff gate" class, and PreJob defines what a
pre-job gate does and does not verify), a durable owner choice (the
pre-job probe's scope is under-specified in the plan and spike), and the
credential-leak / returned-object-trust risk classes (the suite handles a
fake credential marker and trusts runtime-returned archive/inspect state).

## The suite is a synthetic handoff, not a per-check decomposition

#76 already implements the seven contract checks as verifiers wired into
the live `Handoff` gate. #77's job is to make them *invocable* at the
§5.7 cadence points without a real work item. `Suite.Full` runs one
synthetic handoff (a benign writer, a seeded fake credential) end to end,
so checks 1-5 and 7 are exercised together by the proven gate rather than
re-implemented as seven standalone probes. Rejected: a per-check
decomposition where each check has its own minimal runtime setup. It reads
closer to acceptance 1's "each check exists as an independent conformance
check", but checks 3 and 7 have no meaning outside a handoff sequence
(writer termination, export digest/scan), so the decomposition would have
rebuilt most of the gate to exercise a third of it, doubling the surface
that can drift from the real gate. The induced-failure half of acceptance
1 is satisfied by the gate's existing exhaustive per-check fake coverage
(conformance_test.go, handoff_test.go, export_verify_test.go); `Full`
adds one pass-through test proving it surfaces the gate's typed
`*ConformanceFailure` unchanged. Owner-confirmed before implementation.

## The pre-job probe verifies cheap preconditions only

Plan §5.7 names "lightweight probe before each unattended job" but never
says what it checks; the spike is silent too. This is a durable owner
choice, confirmed before implementation: `PreJob` verifies only the
capability declaration is intact, the images are digest-pinned, the
runtime is reachable, and a create→inspect→delete liveness round-trips. It
boots no VM, copies no workspace, and exports nothing, so it is
meaningfully cheaper than `Full` (acceptance 4). It deliberately does
**not** re-verify realized isolation: credential separation holding in a
started writer, the read-only remount, export digest/scan containment, or
the negative probes. Those stay with `Full` at startup, configuration
change, and the doctor schedule. Documented at point of use (doc.go and
the `PreJob` godoc) so the gate's honesty is legible: a green PreJob means
"plausibly still operable", not "conformant". Rejected: a stripped
one-VM mini-handoff that re-checks containment per job — materially slower
and duplicating what `Full` already proves at the coarser cadence.

## The same-VM refutation probe drives the CLI directly, by necessity

Two of the three negative probes (read-write-attach exclusion,
credential-marker containment) compose from the existing `Runtime`
interface and are `Full` members. The third — guest `umount` is not a
credential-device detach — needs a `CAP_SYS_ADMIN` guest process. The
gate's `ContainerSpec` vocabulary cannot express a capability add, and
that is load-bearing: checks 1-2's isolation argument is precisely that
the spec cannot say SSH forwarding, published sockets, privileged caps, or
host binds, so the runtime is never asked for them. Widening
`ContainerSpec` to run this probe would weaken the very gate the suite
protects. So the probe is a permanent host-gated test that drives the
container CLI directly (`TestLiveConformanceSameVMRefutation`), replicating
the spike's reproduction, and is documented as reference-runtime-only. It
demonstrates the refutation; it never implements the refuted class. This
also honors the #76 constraint that the same-VM fallback stays absent by
construction — no capability, spec field, or backend path scaffolds it.

## Probe identifiers reuse ConformanceFailure, register apart from AllChecks

The negative probes and the pre-job probe get typed identifiers
(`writer_volume_exclusion`, `credential_containment`, `same_vm_refutation`,
`pre_job_probe`) that share the `Check` type and `ConformanceFailure` so
every suite result is uniformly typed and fail-closed (acceptance 3). They
register in `AllProbeChecks` / `CheckPreJobProbe`, not `AllChecks`, so the
spike's "check N" numbering keeps its meaning; `valid()` and the
exhaustiveness test cover all identifiers. No shared-package edit: the
suite lives entirely in `daemon/internal/ward`, and no probe needed a new
`Runtime` primitive.

## Reference-runtime verification

Apple container 1.1.0 is available on the dev host, so the whole suite was
exercised for real, not just against the fake.

- **`TestLiveConformanceSuite` passes**: `Suite.Full` (checks 1-5, 7 plus
  the read-write-attach exclusion and credential-containment probes) and
  `Suite.PreJob` run green end to end on real VMs.
- **`TestLiveConformanceSameVMRefutation` passes**: a CAP_SYS_ADMIN guest
  `umount` of the credential mount leaves the block device attached, and a
  remount rereads the marker. The same-VM class stays refuted by live
  execution, permanently.

The image half of checks 2/4 was initially a blocker: container 1.1.0
reports `image.reference` as `name@digest` and `image.descriptor.digest`
as a *different* resolved platform-index digest, which the #76 gate did
not expect (it broke `TestLiveHandoffLifecycle` too). That was a returned-
object-trust defect in #76's gate, out of this unit's scope; it was filed
as #143, fixed in #146, and inherited here by rebasing #144 onto the
updated main. The fake still covers the orchestration, every check/probe
failure mode, and the fail-closed results deterministically for CI, where
Apple container is unavailable.

## Refute-first verification pass

Credential-leak and returned-object-trust lenses (two of the finish
line's refute passes were interrupted by a platform outage; the
equivalent adversarial review was carried out directly and is recorded
here):

**Confirmed and fixed.**

- *The exclusion probe passed on any second-container failure.* The first
  cut of `probeWriterVolumeExclusion` returned success on any create or
  start error from the second container, so an unrelated failure (a
  transient start error, an OOM) would have green-lit a runtime whose
  read-write-attach exclusion does not actually hold, defeating the
  probe's purpose. Hardened: the probe now confirms the writer is observed
  running, treats a create failure as inconclusive (fail closed), requires
  the refusal at start, and confirms via `isAttachmentExclusion` that the
  error is the storage-device attachment refusal (VZErrorDomain / the
  storage-attachment wording) rather than any error; a non-matching start
  failure fails closed.
- *The credential marker was embedded unquoted in a shell command.* The
  seed and audit containers build a shell `-c` string containing the
  marker, so a marker with shell metacharacters could break or inject it.
  `SuiteFixture.validate` now requires the marker to match
  `^[A-Za-z0-9_]{1,128}$` (an inert token, as the spike's marker is).

**Rejected by verification (do not re-raise).** No marker or credential
value reaches a `ConformanceFailure.Reason`: the two containment reasons
(export half, detached-volume half) are static strings, and every other
`failf`/`Errorf` interpolates only gate-generated names or Runtime errors
that reference names, never the marker; the sole argv carrying the marker
value is the seed command, whose Runtime errors are operational
(`fmt.Errorf`) and whose `container create` suppresses stderr and argv
(#76 round 13). `markerScanWriter` catches a marker split across chunk
boundaries (it retains `len(marker)-1` tail bytes; verified for 1-byte
writes) and never false-positives (empty-marker guard). `Suite.Full` fails
closed on every path including a deferred reap failure (which turns a
success into a failure, never the reverse); the deferred cleanup closures
observe the named return `err`, not a shadowed inner one. Every object the
suite creates has a reap path on every exit, containers before their
volumes. `PreJob` fails closed on each precondition and reaps its liveness
container.

**Codex review (PR #144).** Two P-findings, both real and both fixed by
folding into their commits:

- *P1: the exclusion probe accepted any single substring.*
  `isAttachmentExclusion` OR-matched "vzerrordomain", "attachment", or
  "storage device" independently, so an unrelated VZError or a message
  merely mentioning an attachment would pass a probe `Full` relies on to
  prove the exclusion actually holds. Tightened to require "attachment"
  together with the storage-device or VZ-domain context; a unit table and a
  fake-driven Full case lock that an unrelated start failure fails closed.
- *P2: suite cleanup reused the caller's cancellation.* The deferred reaps
  ran under the caller `ctx`, so a mid-run cancellation would leave the
  suite's own credential volume (and other objects) behind, since
  `CLIRuntime` deletes via `exec.CommandContext`. Added `cleanupContext`
  (`context.WithoutCancel` + the teardown timeout, as the handoff teardown
  does) and routed every deferred reap through it.

Round 2 raised four, all real and all fixed:

- *P1 (recurrence of the round-1 class from my own incomplete fix): the VZ
  branch still passed on any attachment error.* The tightened matcher still
  accepted "attachment" + any "vzerrordomain", so a different VZ attachment
  error would pass. Swept the class properly: require the storage-device
  wording, or the exact VZErrorDomain Code=2 the spike observed; a unit
  table pins that a bare VZ attachment and an unrelated VZ code both fail
  closed.
- *P2: cleanup registered after the create leaked an ambiguous create.* A
  CreateVolume/CreateContainer that made the object then errored returned
  before its reap defer was registered. Every reap is now registered before
  its create (suite object names are unique per run, so name-addressed
  best-effort reap only ever hits this run's objects; the handoff's
  ownership machinery is not needed here). Added `reapVolume` and a
  regression proving an ambiguous credential-volume create is reaped.
- *P2: the seed and audit reaps deleted without stopping.* A probe container
  that started but never reached stopped would survive a delete-only reap
  (the runtime deletes only stopped containers) and keep the credential
  volume attached. Both now use the exclusion probe's `reapRunning`
  (stop then delete).
- *P2: default shell payloads embedded unquoted paths.* A non-default
  `WorkspaceTarget`/`CredentialTarget` with spaces or metacharacters (which
  `cleanAbs`/`cliSafe` still allow) would be parsed as shell syntax. Added
  `shellQuote` (POSIX single-quote escaping) around every generated path in
  the agent, seed, and audit commands.

Round 3 raised two P2s, both real (the round-2 register-before-create fix
traded away the fail-closed-on-cleanup-failure property; this restores it):

- *P2: best-effort reaps let a leak return nil.* Making the reaps
  best-effort (to reap ambiguous creates) meant a genuine cleanup failure
  left an object while `Full` returned nil, breaking the fail-closed
  contract that gates unattended operation. Added `verifyReaped`: after the
  reaps, list the runtime and fail closed if any object with this run's
  name prefix survives, or if the listing itself fails (the handoff gate
  proves absence by listing the same way). Reaps stay best-effort so an
  ambiguous create is still attempted; the listing is the fail-closed gate.
- *P2: probe start calls were unbounded.* A runtime that wedged inside a
  probe `StartContainer` (the failure mode `HandoffTimeout` guards) would
  hang the suite forever under a long-lived caller context
  (`context.Background()` in the live test, daemon startup/doctor
  contexts). `Full` now wraps its work in `suiteBudget` (the handoff budget
  plus the probe waits) and `PreJob` in the teardown timeout; the reaps and
  absence proof detach from the budget so they still run after it fires.

Round 4 raised one P2 (the absence-proof class extended to PreJob):

- *P2: PreJob trusted its liveness delete.* PreJob's create→inspect→delete
  liveness reported success on a lying delete that left the container
  behind, which would then collide with a later probe. It now runs the same
  `verifyReaped` listing proof after the delete and fails closed on a
  survivor; a regression drives a delete that reports success but skips
  removal.

Round 5 raised one P2 (a false-positive in round 3's own absence proof):

- *P2: the absence proof matched by prefix.* `verifyReaped` tested the run
  prefix `...<RunID>-`, but run IDs may contain hyphens, so run `conf-1`'s
  proof also matched a stale or concurrent `conf-1-a` object and would mark
  a conformant backend nonconformant. It now matches the exact generated
  object names (seed/audit/prejob/excl-writer/excl-second and
  cred/excl-ws); a regression proves a hyphen-extended foreign run's
  objects are not flagged.

Round 6 raised one P2 (a suppression I had considered and wrongly left):

- *P2: a probe error hid a concurrent cleanup failure.* Full's absence-proof
  guard set the return only when no error existed yet, so a probe failure
  plus a leaked object surfaced only the probe error, hiding that suite
  objects remained. It now joins the leak with the primary error
  (`errors.Join`, as the handoff teardown does); a regression proves a
  failed containment probe with a leaked credential volume returns both
  signals.

Round 7 raised one P2 (the last asymmetry between PreJob and Full's
cleanup): PreJob's absence proof was inline, so it ran only after a clean
delete. PreJob now uses the same deferred `verifyReaped`/join as Full, so a
PreJob ambiguous create or an inspect/delete failure whose reap also fails
still fails closed on the survivor. `verifyReaped` failures are now typed
`CheckTeardown` (the gate's existing cleanup-failure vocabulary) so every
cleanup leak is a proper fail-closed conformance result in both entry
points. This makes Full and PreJob symmetric: bounded context, reaps
registered before creates, and one deferred absence proof over every suite
object on every post-create exit.

Round 8 raised one P2 in a new class (host-resource bound, not cleanup):

- *P2: the audit export was unbounded.* The credential-containment probe
  streamed the audit container's rootfs into `markerScanWriter` with no byte
  cap, so a runtime that cannot enforce `maxBytes` itself (the current
  CLIRuntime) could make Full drain an oversized audit export up to the
  suite budget. The scan now sits behind the handoff's `archiveCapWriter`
  at `MaxArchiveBytes` (as `materializeRootFS` caps its archive); an
  over-cap stream fails the probe closed. A regression floods the audit
  export past a lowered cap and asserts containment fails.

Round 9 raised two P2s (each strengthening a prior fix's coverage):

- *P2: the exclusion probe did not verify the writer's realized mount.* It
  confirmed the writer was running but not that the runtime actually gave it
  the read-write workspace mount, so a dropped/changed/read-only mount would
  let the probe trust a refusal it never exercised. It now compares the
  inspected mounts to the writer spec (`sameMounts`) before the second
  attach.
- *P2: the audit cap trusted the runtime to propagate its error.* The cap
  only failed the probe if `ExportRootFS` returned the writer error; a
  runtime that swallowed it and returned nil left `overflow` set but passed.
  The probe now checks `capped.overflow` first (as `materializeRootFS`
  does), failing closed even on a nil return. Regressions cover the
  unrealized mount and the swallowed-overflow paths.

Round 10 raised one P2 (completing the second-attach proof):

- *P2: the second attach trusted the error string alone.* A runtime that
  returned a storage-looking error yet actually started the second VM (or
  realized it without the requested mount) would pass the exclusion. The
  probe now inspects the second container's realized mount before start
  (`sameMounts`, like the writer) and, after a storage-attachment error,
  re-inspects and fails closed if the second is observed running. A
  regression makes the second start report the error while leaving the
  container running and asserts the probe fails. This completes the
  exclusion proof: writer live with a realized rw mount, second with a
  realized rw mount, a storage-attachment refusal, and the second proven
  not running.

Round 11 raised two P2s (making two proofs non-vacuous):

- *P2: containment could pass over an empty export.* If the runtime
  reported the credential mount but did not realize it, the default
  `set -eu` agent aborted before writing the workspace, so with a permissive
  exporter "marker absent" proved nothing. Full now fails closed when the
  handoff manifest is empty; a non-empty manifest proves the writer ran to
  completion over a realized credential (it reads the credential before
  writing under `set -eu`). Regression exports an empty manifest.
- *P2: the second-attach post-error inspect ignored its own error.* An
  inspect that failed transiently was treated as "not running", hiding a
  second that was actually running. It now fails closed on an inspect error
  or an identity mismatch. Regression makes the post-error inspect fail.

Round 12 raised two P2s (fixture-validation gaps at construction):

- *P2: `NewSuite` accepted invalid credential targets.* `validate` checked
  only `cleanAbs`, so a target carrying a CLI mount-option delimiter, or one
  equal to or nested under the workspace, passed fixture validation and only
  failed downstream in the handoff's own agent-spec check — mis-reporting a
  malformed fixture as a runtime conformance failure. `NewSuite` now rejects
  it up front as `ErrInvalidConfig` (`cliSafe` plus `disjointPaths` against
  the workspace target). Regression covers the delimiter, nested, and equal
  cases.
- *P2: the suite stored the caller-owned `AgentCommand` without cloning.* A
  caller mutating its slice after `NewSuite` returned could change the
  synthetic writer after validation. `NewSuite` now `slices.Clone`s the
  command, matching the freezing in `New`/`Handoff`. Regression mutates the
  caller slice and asserts the stored command is unchanged.

Round 13 raised one P1 and one P2, each a recurrence of an earlier fix's
class from my own incomplete first pass:

- *P1 (recurrence of the round-1/2 VZ-code class): the `Code=2` match was a
  substring.* `strings.Contains(msg, "code=2")` also matched `Code=20` and
  `Code=21`, so an unrelated `VZErrorDomain Code=20` attachment error could
  pass as the storage-attachment exclusion and green-light a backend whose
  read-write refusal was never proven. Anchored with a `code=2($|[^0-9])`
  regexp. Regression adds `code=20`/`code=21` → false.
- *P2 (recurrence of the round-11 post-error-inspect class): the branch
  accepted any non-running state.* After the attach refusal it required only
  "not running", so `starting`, the zero/unknown value, or a future state
  passed even though runtime.go treats exactly `StateStopped` as proof the VM
  is gone (a created-but-never-started container reports stopped too). It now
  requires `StateStopped` and fails closed otherwise. Regression forces the
  post-error state to `starting`.

Round 14 raised one P2 (the no-VM pre-job contract was assumed, not proven):

- *P2: PreJob's liveness check could self-mask an accidental create-time
  start.* The create→inspect→delete round-trip asserted the container's
  identity but not its state, and its `true` payload could exit before the
  inspect — so a runtime that accidentally started the container at create
  (booting a VM, violating the documented no-VM contract) would be observed
  as an ID-matching stopped container and pass. The payload is now a
  long-lived inert `sleep`, and PreJob asserts `StateStopped` after create;
  an accidental start keeps the payload running, so a non-stopped state fails
  closed. The correct create-realizes-metadata-only path reports stopped (a
  created-but-never-started container reports stopped), so the assertion does
  not tighten the normal path. Regression forces the post-create state to
  running.

Round 15 raised two P2s, both further instances of the containment
non-vacuousness class round 11 opened (the empty-export instance), so rather
than patch each I closed the class in one push: the containment proof must
show *this run's writer actually ran with the real seeded credential*, or
"marker absent" proves nothing.

- *P2: the writer proved only that some `token` file was readable, not that it
  was the seeded marker.* A runtime that realized a different volume or path
  carrying a `token` file would still let the default writer run and pass
  containment though the writer never saw `CredentialMarker`. The default
  writer now runs `test "$(cat token)" = MARKER` first, so a realized-but-wrong
  credential (or an unrealized mount) fails the test and aborts the writer
  under `set -eu`. The marker is an inert token, embedded as in the seed's own
  use.
- *P2: containment accepted any non-empty manifest, not this run's output.* A
  runtime returning stale or prepopulated workspace files while the writer
  aborted could still make the manifest non-empty and pass. The default writer
  now emits a run-unique sentinel (`freeside-ward-writer-<run-id>`) only after
  the marker check, and Full requires that sentinel in the export; stale
  content carries a different run's sentinel or none. A caller-supplied command
  opts out of the sentinel proof (the suite cannot know a custom payload's
  output), keeping only the non-empty-manifest floor — a `defaultWriter` bit
  gates which proof applies. Regressions: a non-empty export without the
  sentinel fails closed; a custom command passes on the floor alone; the
  default command gates on the marker and emits the sentinel.

Round 16 raised one P2, the audit-half sibling of round 15's class that I
should have swept in the same push (the export half and the detached-volume
audit share the vacuous-pass shape, and round 15 only closed the export half):

- *P2: the detached-volume audit scanned the whole rootfs for the bare
  marker.* The audit copied the credential `token` into its rootfs with `cat
  token > markerfile` (no `set -e`) and scanned the entire rootfs export for
  `CredentialMarker`. With a short valid marker (say `bin`), a coincidental
  occurrence in a base-image path or file set `scan.found` even if the audit
  `cat` failed (unmounted or deleted volume) and the shell still exited through
  `sync`, passing containment without proving the detached credential was
  readable. The audit now runs `set -eu; test "$(cat token)" = MARKER` (so an
  unreadable or wrong token aborts) and writes a run-unique audit sentinel
  (`freeside-ward-audit-<run-id>`) that the scan looks for instead of the bare
  marker; a coincidental base-image marker no longer matches. Regression: an
  audit export carrying the marker but not the sentinel fails closed. Lesson:
  when a class recurs across a shared shape (export half, audit half), sweep
  every instance in the first push, not the cited one.

Round 17 raised one P2 refining round 15's own sentinel check (my fix was
incomplete, not a new class):

- *P2: the sentinel check scanned the whole export tree, so a filename could
  satisfy it.* Round 15 required the writer sentinel via
  `dirContainsMarker(res.ExportDir, sentinel)`, which walks every extracted
  file including `manifest.json`. A stale or prepopulated workspace with a file
  merely *named* `freeside-ward-writer-<run-id>` puts that string into the
  manifest's path list, satisfying the scan though the writer never produced
  the sentinel *content*. Now the default writer writes exactly the sentinel
  line to `result.txt`, and Full matches that path's manifest **digest**
  (`sha256(sentinel+"\n")`) — which verifyExport already confirmed against the
  blob — instead of scanning the tree, so the check binds to the exact content
  at the exact path and a filename echo cannot satisfy it. Regression: a decoy
  file named like the sentinel with unrelated `result.txt` content fails
  closed. The breach fixture became two entries (clean `result.txt` sentinel
  plus a separate leaked-marker file), which is also the more faithful breach
  model.

Round 18 raised two P2s, both closing residual vacuous-pass paths in the
containment proof. The first was surfaced independently by an adversarial
refute pass (a fresh-context lens tasked to disprove the containment claim)
*and* by Codex in the same round — the refute pass was armed precisely because
the class had recurred across rounds 15-17, and it paid for itself here:

- *P2: the sentinel digest check trusted an unverified digest for
  `blob_omitted` entries.* `verifyManifest` skips re-hashing omitted regular
  entries (`export_verify.go`), and `validateRegular` permits `blob_omitted` on
  a regular entry, so a runtime could return a `result.txt` entry with
  `blob_omitted: true` carrying the publicly derivable sentinel digest and no
  blob — `manifestHasContent` matched it though no content existed, so the
  "writer ran and read the credential" proof was vacuous. Fixed: the digest
  match now requires `Kind == EntryRegular && !BlobOmitted`, i.e. only an entry
  `verifyManifest` actually blob-verified. Regression: a blob_omitted sentinel
  entry with no blob fails closed.
- *P2: the audit trusted its token without inspecting the realized mount.* A
  runtime that mounted a different volume carrying the same marker (a stale
  same-marker fixture volume) at the credential target would pass the audit's
  `test "$(cat token)" = MARKER` while proving nothing about `credVolume`'s own
  survival. The audit now inspects the container after create and requires
  `sameMounts(realized, spec)` (source and read-only included) before starting,
  mirroring the exclusion probe's mount-realization check. Regression: an audit
  whose realized mount is a substituted volume fails closed.

The refute pass also noted the sentinels are deterministic functions of the
public `RunID` rather than a CSPRNG nonce (unlike the handoff's ownership
token). Rejected by decision: the sentinel is embedded in the writer/audit
shell command the runtime itself executes, so the runtime always observes it;
a CSPRNG nonce would add no protection against a byte-fabricating runtime here
(unlike the ownership token, which defends against *foreign* objects that never
see it). The threat this suite addresses is a non-conformant runtime, and the
export/digest chain — not sentinel secrecy — is what binds the proof.

Round 19 raised one P2 — the first false-*negative* of the series (spuriously
failing a conformant backend, not passing a bad one) — and following it to its
root closed a deeper unsoundness in the same scan:

- *P2: a marker colliding with a generated suite string failed a conformant
  backend.* With marker `conf` and run ID `conf-run`, the default writer writes
  `freeside-ward-writer-conf-run` into result.txt, and the export marker scan
  then reports the suite's own sentinel as a credential leak. Fixed at
  construction: for the default writer, `NewSuite` rejects a marker that is a
  substring of the run's writer sentinel or the fixed state payload
  (`durable-workspace`). A caller-supplied command owns its output, so the
  collision cannot arise there and the guard is skipped.
- *Root cause, fixed with it: the marker scan read gate metadata, not just
  agent content.* The scan walked the whole `ExportDir`, including
  `manifest.json`. Beyond the sentinel collision, `manifest.json` carries
  content-derived hex digests and fixed vocabulary (`result.txt`, the manifest
  version, `sha256`); a short marker could coincidentally match a **digest**,
  which no construction-time guard can prevent (the digest is computed from
  content after the fact) — a latent, non-deterministic spurious failure. The
  scan now targets only the extracted blobs (`blobsContainMarker` over
  `ExportDir/blobs`, the agent-authored files), never the manifest. This is the
  marker-side analogue of round 17's sentinel fix (bind to agent content, not
  the whole tree). Regressions: markers colliding with the sentinel, a
  run-specific substring, and the state payload all reject at construction.

Round 20 raised two P2s, each a trust-boundary sibling of a recent fix:

- *P2: the probe specs shared their Mounts slice across the Runtime seam and
  the comparison baseline.* The audit and exclusion writer/second specs were
  passed to `CreateContainer` and then reused as the expected allowlist in
  `sameMounts(realized, spec.Mounts)`. A runtime that normalizes or mutates the
  spec it receives could rewrite `spec.Mounts` in place, then satisfy the
  comparison against its own mutation — substituting or dropping the credential
  mount while `Full` still passed. `Handoff` already clones at this seam
  (`cloneContainerSpec`, whose doc names exactly this hazard); the suite's
  probes did not. Fixed: every suite `CreateContainer` now passes
  `cloneContainerSpec(spec)`, so the local spec stays the immutable baseline.
  Regression: a runtime that mutates the audit spec at create and realizes the
  mutated mount still fails closed.
- *P2: the marker scan couldn't see an omitted blob (the absence-check sibling
  of round 18's presence-check guard).* After round 19 narrowed the marker scan
  to `ExportDir/blobs`, a `blob_omitted` regular entry has no bytes there, so a
  runtime that omitted the very file carrying the leaked marker would scan
  clean and pass containment. Round 18 guarded the sentinel *presence* check
  against blob_omitted; the marker *absence* check needed the mirror. Fixed:
  Full fails closed if any regular manifest entry is `blob_omitted` before the
  marker scan — absence is only provable over content the export actually
  carries. Regression: an export with a present sentinel plus an omitted
  workspace blob fails closed. (Both blob_omitted findings trace to one gap:
  verifyManifest does not re-hash omitted entries, so any digest- or
  scan-based proof must reject omitted regular entries first.)

Round 21 raised one P2, a third refinement of the VZ-code matcher (after the
round-1 wording match and the round-13 `Code=20` boundary):

- *P2: `Code=2` was matched anywhere in the message, not against the
  VZErrorDomain's own code.* `vzCode2Pattern` searched the whole lowercased
  error for `code=2`, and the domain check was a separate substring test, so a
  differently-coded top-level VZError (say `Code=7`) carrying a nested
  `Code=2` in an underlying NSError's UserInfo satisfied both and passed the
  exclusion probe though the top-level error was not the storage refusal. The
  pattern is now anchored to `vzerrordomain\s+code=2(...)`, matching the code
  only immediately after the domain, which also subsumes the separate domain
  substring test. Regression: a `VZErrorDomain Code=7` with a nested `Code=2`
  returns false.

Round 22 raised two P2s:

- *P2: the exclusion probe never re-checked the writer after the refusal.* It
  proved the second container was refused and stopped, but the claim is that a
  *live* writer holding the rw mount excludes the second attach. A runtime that
  resolved the conflict by evicting the holder (stopping/replacing the writer,
  then failing the second start for that reason) satisfied every check without
  demonstrating exclusion. The probe now re-inspects the writer after the
  refusal and requires it still running with the same rw mount. Regression: a
  writer observed no longer running after the refusal fails closed. (The fake
  reports a started container running for only its first inspect, so the
  happy-path exclusion tests now mark the live writer as running across both
  inspects.)
- *P2: the round-21 VZ-code anchor still matched a non-first VZErrorDomain.*
  Anchoring `vzerrordomain\s+code=2` fixed a nested NSError Code=2 but still
  matched a *second* `VZErrorDomain Code=2` inside an underlying error when the
  top-level was, e.g., `Code=7` — my round-21 fix was incomplete. Now the code
  is parsed: `vzerrordomain\s+code=(\d+)` captures the first (leftmost)
  domain's code and requires it be exactly `2`. Regression: a top-level
  `Code=7` with a nested `VZErrorDomain Code=2` returns false. Lesson repeated:
  "anchor to the domain" needed to be "the *first* domain"; parse, don't widen
  the pattern.

Round 23 raised one P2, a false-negative completing the credential-target
path-collision class (round 12 covered the workspace; this covers the audit
marker path):

- *P2: a credential target equal to the audit marker path shadowed the
  sentinel write.* A caller passing `CredentialTarget = "/" +
  conformanceName("audit-marker")` (clean, CLI-safe, workspace-disjoint, so
  previously accepted) makes the audit mount the credential volume over the
  exact rootfs path the audit writes its sentinel to, so the export cannot
  carry the sentinel and `Full` fails a conformant backend. `NewSuite` now
  rejects a credential target that equals or nests with `auditMarkerPath(RunID)`
  (extracted alongside a free `conformanceObjectName` so the path is derivable
  before a Suite exists). Regression: a credential target set to the audit
  marker path rejects at construction.

Round 24 closed two vacuous-pass paths: one Codex raised, one a second
adversarial refute pass surfaced (re-armed because the collision class had
recurred across rounds 12/19/23; it found only the one hole below, so the
surface is near exhaustion):

- *P2 (Codex): Full never ran the no-eager-start proof PreJob has.* Full enters
  the synthetic handoff directly. The gate inspects the agent's config before
  starting it but does not assert it is stopped, so a runtime whose
  `CreateContainer` executes the container would run the writer over the mounted
  credential before checks 1-2 look, and Full would still pass. PreJob's
  liveness (create→inspect→require StateStopped, long-lived payload) is now a
  shared `proveNoEagerStart` helper Full runs as a preamble (a runtime-wide
  eager-start shows up on the throwaway probe). Regression: a liveness container
  observed running after create fails Full closed. *Follow-up:* the gate itself
  should assert stopped-before-start for the real agent (defense-in-depth, #76
  scope); filed as a deferral, since the suite now detects the runtime so no
  admitted backend reaches the gate with that behavior.
- *P3 (refute pass): the content-only marker scan missed a metadata leak.*
  Round 19 narrowed the marker scan to blob content (to dodge manifest hex-digest
  false positives), which opened a gap: a runtime could exfiltrate the marker as
  a filename (a manifest `path`) or a symlink `target` — neither becomes a
  scanned blob, and the §5.4 scanner is blind to the inert marker. For the
  default writer, Full now asserts the manifest carries only `result.txt` and
  `nested/state.txt` as regular files, rejecting any smuggled name or non-regular
  entry. Not a false-failure risk (the benign writer produces exactly those).
  Regression: a file named after the marker fails closed.

Round 25 raised two P2s, both siblings of the immediately preceding fixes (my
misses to sweep, not convergence):

- *P2: the round-24 eager-start probe was unmounted, but the protected path is
  mounted.* A runtime that eager-starts only containers with mounts would pass
  the unmounted `sleep 300` probe, then run the mounted handoff writer to
  completion during its `CreateContainer` and be observed stopped before the
  gate inspects it. `proveNoEagerStart` now takes a mount set; Full runs it
  after creating the credential volume and mounts the writer's own topology (a
  fresh workspace volume read-write and the credential volume read-only, still
  empty — the payload never reads it), so a mount-conditional eager-start is
  caught. PreJob stays unmounted (its scope is cheap preconditions). Regression
  asserts the probe carries the workspace+credential mounts.
- *P2: the storage-device wording bypassed the VZ-code parser.* The early
  `Contains("storage device")` branch returned true regardless of the code, so
  `VZErrorDomain Code=20: storage device attachment failed` passed as the
  exclusion. `isAttachmentExclusion` now treats a present VZErrorDomain code as
  authoritative (first/top-level code must be exactly 2, wording notwithstanding)
  and falls back to the storage-device wording only when no VZ code is present
  at all. Regression: a Code=20 storage-device error returns false, while a
  code-less reworded storage-device message still matches.

Round 26 raised one P2, a lossy-export gap completing the default-writer
export pinning:

- *P2: the proof pinned result.txt but not the nested fixture.* The default
  writer writes both `result.txt` (sentinel) and `nested/state.txt`
  (`durable-workspace`, the deterministic directory-tree coverage), but Full
  required only the result sentinel and merely rejected *unexpected* entries —
  so a lossy exporter that dropped `nested/state.txt` while preserving
  `result.txt` passed. Full now also requires `nested/state.txt`'s exact
  content (via the manifest digest verifyExport confirmed), completing "the
  export is exactly this run's writer output". Placed after the marker scan so
  the marker-leak and omitted-blob probes keep their own failure reasons.
  Regression: an export with result.txt but no nested fixture fails closed.

Round 27 raised one P2, the custom-command sibling of round 24's metadata-leak
fix (my miss: round 24 gated the shape check to the default writer):

- *P2: the custom-command path had no metadata-leak guard.* A caller-supplied
  `AgentCommand` skips the exact-shape check, and the marker scan reads only
  blob content, so a custom payload or exporter could leak the marker as a
  filename or symlink target with clean blob bytes and pass. Full now scans the
  manifest's structured path and symlink-target metadata for the marker on the
  custom path (`manifestLeaksMarker`, never the content-derived digests whose
  hex could coincidentally match a short marker; path_hex decoded for non-UTF8
  names). The default writer needs none — its exact-shape check already forbids
  any entry but the two expected files. Regression: a custom-command export
  leaking the marker as a filename fails closed. Metadata-leak protection now
  covers both paths.

Round 28 raised three P2s, all sweeps of my own preceding rounds (a marker
collision, a liveness sibling, and a live-test regression I introduced):

- *P2: a marker could collide with the default writer's output paths.* Round
  27 scanned custom-command manifest metadata for the marker but skipped the
  default path (its exact-shape check makes a metadata scan collide with the
  suite's own `result.txt`/`nested/state.txt` names). A marker like `result`
  or `nested` therefore passed `NewSuite` yet sat in the released manifest
  metadata unflagged. The construction-time collision guard now also rejects a
  marker that is a substring of `writerResultPath` or `workspaceStateFile`
  (joining the sentinel and state-payload it already covered).
- *P2: the round-26 nested-fixture requirement broke the live suite.* Requiring
  `nested/state.txt` in the export meant the reference-runtime suite would fail
  `CheckCredentialContainment` on a *conformant* backend, because its exporter
  payload (shared `liveExporterPayload`, which the handoff live test pins to a
  single entry) manifested only `result.txt`. Added a dedicated
  `liveSuiteExporterPayload` that copies and manifests both default-writer
  files. (Host-gated; not run this session — recorded as a `Not run` gap.)
- *P2: the mounted liveness probe never verified its realized spec.* Round 25
  mounted the probe but only checked StateStopped; a runtime that dropped the
  mounts or changed the command could report stopped without exercising the
  mounted create path. `proveNoEagerStart` now confirms the realized image,
  command, and mounts match the spec (via `sameImage`/`slices.Equal`/
  `sameMounts`) before trusting the state. Regression: a liveness container with
  dropped mounts fails closed.

**Accepted by decision.** The marker value appears in the seed/audit
container argv; this is safe because the marker is an inert fake credential
by contract (the whole point of a *fake* marker is to test containment
without a real secret), and the value never reaches a
`ConformanceFailure.Reason`. `waitStopped` relies on the configured
context-aware `Sleep` for its deadline; a caller injecting a
context-ignoring sleep and a never-stopping container could spin, but the
production `Sleep` respects the context and the fake always transitions, so
this is a test-fixture concern, not a runtime one.

Revisit when: #143 lands (then the suite's reference-runtime full run
moves from recorded gap to green); doctor scheduling wiring lands (the
operations unit consuming this library); or Apple container exposes a
mechanism that lets the pre-job probe cheaply re-verify a realized
isolation property, which would reopen the PreJob scope choice.
