# Ward handoff backend: the fresh-VM read-only-volume gate

Work unit: #76 (ward lane head, gates 1A.2). Mandatory note:
credential-leak surface and a returned-object trust boundary (the
exporter's inspect report and exported archive are attacker-influenced
values the gate re-verifies), plus mechanism choices the spike and plan
leave to the implementation.

## The Runtime seam is a typed interface, not an argv command-runner

The gate's failure paths are semantic runtime states (an extra mount in
an inspect report, an ID lingering in `list --all`, a state that never
reaches stopped, a delete that leaves the object). A typed `Runtime`
interface lets the scripted fake produce exactly those states; an
argv→bytes seam would couple every conformance test to CLI string
formatting and duplicate JSON fixtures into each test. `os/exec` and
1.1.0 JSON decoding are confined to `runtime_cli.go` (the daemon's first
`os/exec` user; the export package's test asserts *it* never imports
`os/exec`, so this is a deliberate new idiom, not a drift). Rejected:
a lower-level command seam. Revisit when: a second backend (linux_vm,
§3.3) needs the same seam and the interface shape has to generalize.

## Pre-execution inspection uses create → inspect → start

Check 4 requires the exporter's mounts be verified *before* it executes.
The spike only exercised `container run` (create+start fused), leaving
open whether 1.1.0 can inspect a created-but-not-started container. It
can: verified on the reference host that `container create` +
`container inspect` reports the full mount configuration
(`type.volume.name`, `options: ["ro"]`, `ssh`, published sockets) before
any `start`. So the gate creates the exporter, inspects it against the
generated allowlist, and only then starts it. The documented fallback (a
gate-command payload released via `container exec` after inspection) was
not needed and is not scaffolded.

## Placeholder exporter payload, real digest verification

No pinned exporter image exists yet (`images/` is empty until its
phase); issue #76 allows landing checks 1-5, 7 against a placeholder.
`Config.ExporterImage`/`ExporterCommand` carry the image and argv, so the
real freeside-export image slots in later with no interface change. The
live test pins the spike's alpine:3.22 *by digest* and supplies an inline
payload that runs the check-5 probes, writes the proof, and emits a
minimal valid §5.6 manifest plus the one content blob — so check 7's
digest verification and the scanner hook run against genuinely exported
bytes, not a stub. The workspace-copy export cost (helper copies the
read-only workspace into the exporter rootfs; 1.1.0 has no direct
named-volume export) is the accepted cost recorded in the Wave 1
planning note; this unit builds no block-image workaround.

## §5.4 scanning is a fail-closed hook, not policy

Check 7 splits cleanly: digest verification (manifest `Validate`,
per-blob re-hash against digest and size, unreferenced-blob rejection,
strict decode with unknown fields refused) is the ward's own trust
boundary and is implemented fully here. Actual secret-scanning policy is
gauntlet-worker territory (#74/#75); building it here would duplicate and
freeze rules the gauntlet owns and exceed the declared ward-only scope.
So the gate takes a required `OutputScanner` hook, runs it on every
verified export, and fails closed on any error. The live test injects a
marker-grep scanner to exercise the hook end to end.

## Containment is detachment, never deletion

The gate never deletes credential volumes: they are caller-owned. It
proves the marker absent from the exported archive while the credential
volume still holds it (the live test asserts the credential volume
survives teardown with its contents intact). Teardown reaps only what the
run created — the workspace volume and the two containers — and proves
absence by re-listing, not by trusting delete calls. A teardown failure
fails an otherwise-green gate; when a prior check already failed, that
check's error wins so teardown never masks the cause.

## The refuted same-VM fallback is absent by construction

The same-VM fallback class (terminate the agent process, detach
credentials, export in place) is refuted by execution in the spike, not
merely weaker: 1.1.0 exposes no host hot-detach, and a guest unmount
leaves the credential block device attached and remountable. It is not
implemented, declared, or scaffolded; `Capabilities()` never returns
`supports_credential_volume_detach` or `supports_workspace_snapshot`, and
the capability test enumerates `exec.AllCapabilities` so a new capability
must be explicitly placed as declared or refused.

## Refute-first verification pass

Two independent adversarial lenses ran against the diff before handoff:
one tasked with disproving the containment logic (can the gate pass a
handoff it should fail, or leak a credential?), one with disproving the
test suite (does a green test actually prove its claim?).

**Confirmed and fixed.**

- *Config did not enforce proof/handoff paths disjoint from the
  workspace.* `validate()` checked only absoluteness. Since the agent
  writes `/workspace` and it is mounted (read-only) in the exporter, a
  misconfigured `ProofPath` or `HandoffDir` nested under `WorkspaceTarget`
  would let agent-authored files shadow the exporter's own proof or
  manifest, forging check 5 or supplying a self-consistent manifest. Not
  reachable under the defaults, but the gate leaned on an unstated
  invariant; `validate()` now rejects any nesting among workspace,
  handoff, and proof paths (folded into the config commit).
- *Successful runs leaked an empty scratch parent, and a teardown failure
  after a good export orphaned the output dir.* The export path now uses
  two separate host-temp entities (an archive dir always reaped, an output
  dir returned only on full success and removed on any failure including a
  teardown failure that nils the result). Regression tests assert zero
  leftover handoff temp dirs after both a scanner-refused run and a
  teardown-failure-after-export run (folded into the handoff commit).
- *Three test-quality gaps let real regressions pass CI.* The
  cancellation test could not detect removal of `context.WithoutCancel`
  (the fake ignored its context), so an aborted run silently orphaning a
  live credential-bearing VM would stay green; the fake now fails its
  runtime calls on a cancelled context, and a mutation check confirms the
  test now fails if the detachment is removed. Teardown's container-survival
  sweep and the check-3/teardown list-error branches had no tests; added a
  lying container-delete hook, a list-volumes error hook, and cases for
  each (folded into the handoff commit).

**Rejected by verification (do not re-raise).** Writer termination
proceeding on scheduling intent (only observed `StateStopped` proceeds;
every other or unknown state, a runtime error, and a zero/negative or
sub-interval timeout all fail closed). Check 4 trusting a lying inspect
report (a bind, rw, multi-key, or unnamed-volume mount all decode to an
invalid type and are rejected; a rw mount cannot present as `ro`). Proof
forging under correct config (the proof lives on the exporter rootfs at a
path the read-only workspace cannot write). Tar extraction escaping
`destDir` (absolute, `..`, `handoff/../`, and symlink/hardlink/special
entries inside the output are all rejected; `O_EXCL` blocks following a
planted link; the byte cap treats exact-fill as over-budget). Manifest
smuggling (strict decode, digest and size re-verified, unreferenced files
rejected). Teardown masking a real failure (it only ever converts success
to failure, never overwrites a prior error) or reporting success while an
object survives (absence is proven by re-listing, not by the delete call).

**Accepted by decision.** The realized agent container's mounts are not
re-inspected (only the generated spec is validated), unlike the exporter;
the agent is the intentionally-credentialed VM and the containment
boundary is the downstream-inspected exporter plus the §5.4 scanner
(documented at `validateAgentSpec`). This holds only because the
spec-to-runtime phrasing cannot smuggle a topology the spec check never
saw: the Codex round below closed the two ways it could. A credential
*value* placed in an env entry remains the accepted, mechanically
undetectable residual §5.4 scanning covers. `waitStopped`'s first poll has no
initial sleep, so a workload that exits within one interval could be
observed stopped without having been seen running; this yields a no-op
export of an untouched workspace (nothing credentialed ran, nothing to
leak), a liveness concern, not a containment one, and the real workloads
this gate serves run far longer than a poll interval. The fake models no
volume-attachment relationship, so a teardown-ordering regression
(deleting the workspace volume before its containers) passes the fake
though it would fail on the real runtime; the failure direction is
fail-closed and the load-bearing orderings are pinned by call-index
assertions. The opt-in live test is happy-path only (issue #76 accepts
host-gating); the spike's negative probes as permanent real-runtime tests
are the next ward unit (#77).

## Codex review round (PR #129)

Round 1 confirmed the CLI-phrasing seam could realize a mount/env
topology the spec-level checks never approved, both a member of one class
(caller/config strings flowing into `container create` argument
construction where a delimiter or special token changes semantics):

- *P1: bare-key env inheritance.* `--env KEY` with no `=` tells the CLI to
  copy the host's value; a caller passing `GITHUB_TOKEN` would pull a host
  credential into the credential-bearing writer VM, defeating check 2 even
  though env *values* are (accepted) unscanned. Now rejected: an env entry
  must be `key=value` with a non-empty key.
- *P2: mount-field delimiter injection.* A comma in a caller-supplied
  credential volume/target or the configured workspace target would let
  the CLI parse the suffix as another `--mount` option, and since the
  agent is validated from its in-memory spec (not re-inspected), checks
  1-2 could approve a different realized topology. Now rejected: no comma
  or control character in any field phrased into a `--mount` value.

Fixed the class at every layer rather than the two cited lines: the
semantic gate (`validateAgentSpec` rejects both, as typed conformance
failures; `config.validate` rejects a delimiter in the workspace target)
and the CLI phrasing boundary (`createContainerArgs` refuses to phrase
either, so a direct `Runtime` caller cannot bypass the gate). Both fixes
folded into their home commits; the live lifecycle test still passes.
This tightens the accepted-by-decision item above: the agent is still not
re-inspected, but the phrasing can no longer diverge from the validated
spec, so that decision now rests on an enforced invariant rather than an
unstated one.

Round 2 raised two more, both real and both worthwhile:

- *P1: check 4 accepted a non-stopped exporter.* `verifyExporterAllowlist`
  approved the mount/env allowlist regardless of observed state, so a
  runtime that started the exporter during create (or future CLI
  behavior) could have run the trusted payload before the gate approved
  its configuration, defeating the inspect-before-execution boundary.
  Now the verifier fails unless `rep.State == StateStopped`; the reference
  runtime reports a created-not-started container as stopped, so the
  happy path is unaffected.
- *P2: unbounded detached teardown.* Teardown detaches from the caller's
  cancellation with `context.WithoutCancel`, which strips the deadline
  too, so a wedged `container stop/delete/list` could keep a cancelled
  `Handoff` from ever returning. Teardown now runs under its own
  `TeardownTimeout` (default 2m); a test blocks a teardown runtime call
  and proves `Handoff` still returns (as a teardown failure) rather than
  hanging.

Round 3 raised three, all real:

- *P1: premature liveness clear leaked a credential-bearing container.*
  `st.agentLive` was cleared right after the agent delete call returned,
  before `verifyContainerAbsent`. So when the delete reported success but
  the writer was still listed (the lying-runtime case check 3 exists to
  catch), teardown skipped the agent and left a restartable
  credential-mounted container behind. The flag is now cleared only after
  absence is proven; a test with a lying delete asserts teardown
  re-attempts to reap the agent.
- *P1: scanner error could leak the matched secret.* A leak scanner's
  error was interpolated into `ConformanceFailure.Reason`, which the
  contract says never carries credential material, and this fires exactly
  on a refused export containing a credential. The reason is now withheld
  (a scanner logs specifics to its own audited sink). This is the second
  member of the "no untrusted content in Reason" class; the sweep confirmed
  the others are safe: env values already redact to the key, and proof
  values, filenames, digests, and states are either the observed facts the
  contract sanctions or come from the credential-free exporter.
- *P2: manifest trailing bytes were ignored.* `json.Decoder.Decode` stops
  at the first value, so a `manifest.json` of a valid manifest followed by
  garbage passed and the full bytes were released for downstream to
  consume. The decode now requires EOF after the single value.

Round 4 raised two, one of them my own incomplete sweep:

- *P1: argv option injection past the image.* Round 1 swept the
  "caller/config strings reaching `container create` args" class but
  stopped at env and mount fields; the positional image and command were
  never option-terminated. A dash-prefixed image (e.g. `--mount`) is
  reparsed as a create option, letting the next word realize a host bind
  or SSH forwarding outside the validated spec. This is a recurrence of my
  own under-scoped fix, not new ground, so I closed the class at its
  boundary: `createContainerArgs` now emits `--` before the image, making
  the image and every command word positional. The sweep confirmed this is
  the one argv site fed caller-supplied positionals; every other CLI method
  takes only gate-generated identifiers (validated RunID-derived names,
  internal temp paths). Verified on the reference runtime that
  `container create` accepts the `--` terminator.
- *P2: teardown volume sweep missed an unlabeled survivor.* The sweep
  flagged only volumes still carrying the run label, so a workspace volume
  that survived with its label dropped would pass as reaped while holding
  agent-written data. It now also flags by the known workspace name.

Round 5 raised two, both fail-closed hardening:

- *P2: unidentified list entries decoded silently.* A `list --all` entry
  with no top-level `id` (or a volume with no name) decoded to an empty
  identifier, and the absence proofs compare identifiers to the run's
  names, so a nameless survivor would never match and the gate would treat
  it as absent. The list decoders now reject an entry with no id/name.
- *P2: negative duration config not range-checked.* Only `MaxExportBytes`
  was validated, so a negative `TeardownTimeout` (or any other duration)
  passed `New` and made teardown run under an already-expired context,
  failing its cleanup calls. Swept all the numeric/duration config fields:
  `validate` now rejects any negative duration.

Round 6 raised one P3 (test-only): the live test's failsafe cleanup swept
the agent and exporter but not the seed container, so a failure before its
explicit delete would leave it attached to the credential volume and block
that volume's deletion. The seed is now in the sweep.

Round 7 raised one P2, a second miss in round 5's own sweep: I made the
list decoders reject a missing identity but left `decodeInspect` trusting
a report whose `id` is missing or differs from the requested container, so
checks 3 and 4 could act on the wrong (or unidentified) object's state and
mounts while the requested name is started/deleted/exported.
`decodeInspect` now verifies the returned id matches the requested one.
That completes the "runtime object trusted without identity verification"
class across all three decoders (container list, volume list, inspect); a
further recurrence in this class should reopen the model rather than patch
a fourth leaf.

Round 8 raised one P2 in an adjacent class ("an absent decoded field reads
as a clean value"): `ssh`, `initProcess.environment`, `publishedPorts`, and
`publishedSockets` are non-pointer, so a drifted/partial inspect that omits
them decodes to false/nil, and `verifyExporterAllowlist` reads that as an
explicit clean report, approving check 4 without ever observing them. Rather
than patch these four leaves, I widened and enumerated the whole class: those
four fields are now presence-tracked pointers and `decodeInspect` fails
closed if any is absent; the remaining security-relevant inspect fields
already fail closed on absence (an empty state is not `stopped`, zero mounts
fails the exactly-one rule, an unnamed/typeless mount decodes to an invalid
type, a missing mount target/`ro` option fails the allowlist). A round-9
recurrence in this class should reopen the decoder model, not add another
field check.

Round 9 raised three, two of them recurrences of classes I had claimed
complete: the leaf-by-leaf sweeps had not been exhaustive, so this round I
closed both classes at the design level rather than patching another leaf.

- *P1: ambiguous create leaked a credential-bearing container (liveness
  class, round 3 again).* Round 3 fixed the flag being cleared too early
  after the agent delete, but the symmetric case at create remained: a
  `CreateContainer` that made the container and then returned an error (a
  cancellation, a post-create CLI failure) returned before the `agentLive`
  flag was set, so teardown skipped the credential-mounted writer. The fix
  removes the failure mode's root: teardown no longer trusts any in-memory
  "created" flag. It reaps by listing — discovering the run's containers and
  workspace volume in the runtime's own listing (matched by deterministic
  name and label), reaping what exists, and re-listing to prove absence. An
  object that exists is reaped whether or not any flag recorded its
  creation, so the whole created-but-flag-unset class is gone.
- *P2 + P2: identity checked at only one level (identity class, round 7
  again).* Round 7 verified the top-level inspect `id`, but the mounts,
  environment, SSH, and publications check 4 reads all come from
  `configuration`, whose id was unchecked; and the volume decoder trusted
  `configuration.name` without tying it to the volume's top-level `id`. The
  identity class is now swept exhaustively: `decodeInspect` requires both
  the top-level and configuration ids to match the requested name,
  `decodeContainerList` requires the two ids to agree, and
  `decodeVolumeList` requires the top-level id to equal the configuration
  name. Every identity a downstream match depends on is now bound.

Lesson recorded: three classes (identity, absent-field, liveness) each
recurred after a leaf fix that looked complete. The durable fixes were
structural (reap-by-listing removes the liveness flags; presence-tracking
pointers remove the absent-field ambiguity; both-id checks bind identity
everywhere), not another guarded line. A further recurrence in any of these
should reopen the corresponding model, not add a leaf.

Round 10 raised two, one a recurrence and one a regression from round 9:

- *P1: exported filenames leaked into failure reasons (the "no untrusted
  content in Reason" class, round 3 again).* Round 3 redacted the scanner
  error but I wrongly classified exported paths as contract-sanctioned
  observed facts. An exported filename is workspace content (the agent, which
  held credentials, could name a file after one), so a traversal /
  non-regular / unreferenced-stray failure that interpolated the path was an
  exfiltration channel into the returned error and logs. Swept the class in
  export verification: a `redactPath` token (a non-reversible sha256 prefix,
  still diagnosable by recomputation) replaces every archive-derived path,
  `extractFile` returns path-free category errors instead of wrapping os
  errors that embed the destination, and the manifest decode/validate errors
  (which quote entry paths and field names) are reported generically.
- *P2: reap-by-listing could reap another run (regression from round 9).*
  The round-9 teardown reaps by deterministic name, and its defer is armed
  before any create, so a failure before the first create (admission, spec
  validation) with a reused RunID could delete a different live run's
  objects. Added the ownership boundary the name-based reap needs: a
  `claimed` flag set immediately before the first create (so an ambiguous
  create is still reaped, preserving the round-9 fix) gates teardown, which
  reaps nothing until this invocation owns the names. The residual
  same-RunID-collision-between-two-claimed-runs case remains a caller
  contract violation (RunID uniqueness among live runs).

Round 11 raised one P2, another teardown-ownership regression from round 9:

- *P2: the run label could make a caller-owned credential volume look
  gate-owned.* Reap-by-listing treated either the deterministic workspace
  name or `freeside.handoff=<RunID>` as proof that the gate owned a volume.
  A provisioner may apply the run label uniformly, including to credential
  volumes the gate must never delete, so label-only matching could destroy a
  caller-owned credential volume on the success path. The ownership boundary
  is now exact: teardown deletes and proves absence only for the deterministic
  workspace volume name. Labels remain inspection metadata and never confer
  ownership. A regression test gives the caller-owned credential volume the
  same run label, then proves the handoff succeeds, the workspace is gone, and
  the credential volume survives without even receiving a delete attempt.
  This preserves round 4's unlabeled-survivor defense because the workspace's
  known name remains the teardown and proof key.
