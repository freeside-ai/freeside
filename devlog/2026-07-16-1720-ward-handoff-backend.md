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

Round 12 raised two P2s. One showed that round 11's exact-name correction was
still incomplete for ambiguous creates, so I ran a fresh-context refute pass
over the ownership state machine rather than patching the cited assignment:

- *P2: claiming the run namespace before a create could delete another run's
  objects.* An ordinary already-exists failure on the workspace, agent, or
  exporter set the shared `claimed` bit before ownership was proved. Deferred
  teardown could then delete the colliding object and any sibling object with
  the same RunID. Ownership is now tracked per object. A successful create
  owns that object's exact deterministic name; a failed create is ambiguous
  and authorizes deletion only when a fresh runtime listing finds both the
  exact name and this invocation's unpredictable 128-bit ownership label.
  The label is added to every object at creation, retained in list results,
  and is never inferred from the deterministic run label. The agent becomes
  proven absent only after delete plus a full-list absence check, so later
  teardown cannot reap a same-name replacement. Regression tests cover
  foreign collisions at all three creates, create-then-error for all three
  object types, cancellation after an ambiguous container create, and reuse
  after the agent's absence was proved.
- *P2: export verification allowed unmanifested directories.* The stray-file
  check rejected unmanifested files but skipped every directory, so hostile
  extra directory trees could be released beside the manifest. The verifier
  now accepts only the structural directories required by the contract
  (`.`, `blobs`, and `blobs/sha256`) and reports every other directory through
  the same redacted conformance failure as other unmanifested output.

The refute pass also found a pinned Apple container 1.1.0 limitation: volume
creation may make an on-disk directory before formatting and persistence, and
a formatter failure can leave that directory unlisted. The Runtime interface
cannot discover or safely delete an object absent from the runtime listing.
It contains no agent-written data because the writer never started, but it
can consume storage or poison a retry with the same name. Revisit when Apple
container exposes atomic creation or disk-level orphan cleanup, or when the
Runtime boundary gains a safe discovery primitive for unlisted artifacts.

Round 13 raised one P2 in the credential-leak class:

- *P2: a malformed bare environment entry was echoed in a refusal.* The
  semantic gate correctly rejected a host-inheriting entry, but formatted the
  would-be key in the conformance reason. For a bare entry, the whole string
  is untrusted caller input and may itself be a credential copied without a
  `KEY=` prefix. The refusal is now categorical and returns no part of the
  entry; regression cases cover both a secret-looking bare value and an
  empty-key value, including substring-only leak detection. A mechanical
  sweep confirmed the CLI phrasing boundary also uses a categorical refusal,
  while the exporter allowlist reports only the parsed variable name after an
  explicit `key=value` split. The focused fresh-context refute pass confirmed
  both refusal paths, then found that an external create failure could echo an
  accepted explicit `KEY=value` credential through CLI stderr. Container
  create now suppresses stderr while preserving the safe exit error; other
  runtime calls retain their bounded diagnostic stderr because their argv
  contains only gate-generated identifiers. A fixture CLI proves both the
  secret and its variable name remain absent from the returned error.

Round 14 raised one P2 at the runtime-list trust boundary:

- *P2: an omitted labels field looked like an observed empty label set.* An
  ambiguous create may be reaped only when the exact object also carries this
  invocation's ownership token. The JSON decoders previously collapsed an
  omitted `configuration.labels` field to an empty map, so teardown treated a
  shape-drifted owned object as foreign and silently skipped both deletion and
  the survivor proof. List summaries now retain label-field presence. For an
  exact object whose create was ambiguous, a missing field is a teardown
  failure; successful creates still own their exact name without needing the
  label, and unrelated unlabeled list entries do not cause a false global
  refusal (the pinned 1.1.0 container-list fixture legitimately omits labels
  for such entries). When a primary failure and teardown failure coincide,
  the returned error joins them: the original cause remains discoverable and
  the incomplete cleanup is no longer hidden. The fresh-context refute pass
  then found a contradictory-list variant: duplicate exact container IDs
  previously let the first entry decide ownership. Teardown now rejects them
  before using label evidence; a regression orders an empty foreign view
  before the owned token-bearing view and proves the ambiguity fails teardown
  without deleting either interpretation. The next reviewer
  pass caught the exact volume sibling our sweep missed: because deletion is
  by name, one labeled duplicate row cannot authorize deletion while another
  row presents the same name. Duplicate target volume names now fail with the
  same contradictory-order regression. A final fresh-context refute pass
  rejected decoder-wide duplicate failure: contradictory unrelated rows could
  otherwise make listing fail before teardown reaped this run's live object.
  Uniqueness is therefore enforced on the exact target identity in teardown,
  and a success-path regression proves unrelated duplicate summaries cannot
  suppress cleanup.

Round 15 raised one P1 in the credential-leak class:

- *P1: malformed proof content was echoed before scanning.* The proof parser
  formatted archive-derived lines, unknown keys, and observed values into
  conformance reasons. Although the trusted exporter writes fixed markers,
  the returned rootfs is an external Runtime result and cannot be trusted to
  preserve them; a hostile or drifted archive could therefore return or log a
  credential before the output scanner ran. Every proof-parser failure is now
  categorical: no observed line, key, value, or scanner detail is emitted.
  Regression cases place the same secret-shaped substring in a malformed
  line, an unknown key, and a required key's wrong value and prove the reason
  contains none of it. The class sweep also removed caller-built agent mount
  fields and Runtime-observed exporter state, mount, and environment strings
  from conformance reasons. The fresh-context refute pass found no remaining
  string leak across malformed, unknown, duplicate, wrong-value, scanner, or
  missing-key branches. Exporter socket and port cardinalities remain by
  decision: they cannot reproduce input bytes or a secret substring, while
  retaining useful topology diagnostics.

Round 16 raised two P1s at the returned-object and cleanup ownership trust
boundaries:

- *P1: exporter approval did not bind the inspected helper payload.* Check 4
  verified the observed state, mounts, environment, SSH, and publications,
  but not the observed image or init argv. A Runtime could therefore realize
  a different executable behind an otherwise conforming container and have it
  started under the pinned exporter's approval. The inspect seam now retains
  the exact container identity, normalized image reference, descriptor digest,
  and init executable plus arguments. Check 4 compares all four to the
  deterministic exporter identity and digest-pinned configured payload before
  start; absent fields, a substituted digest/reference, or any argv drift fail
  categorically without echoing observed values. The stopped-state poll also
  verifies the returned identity before trusting state.
- *P1: ambiguous container cleanup depended on labels the pinned list shape
  omits.* The full container list can identify an exact candidate but omit its
  labels, so teardown previously refused to reap a create-then-error object
  even though container inspect exposes the invocation token. Ambiguous
  cleanup now uses the list row when labels are present and otherwise inspects
  that exact candidate under the detached teardown deadline. It authorizes
  cleanup only when inspect identifies the same object and explicitly exposes
  the unpredictable ownership label; a wrong identity or labels omitted from
  both views remains a teardown failure.

The required fresh-context refute pass confirmed one coupling defect in the
first implementation: the shared inspect decoder required every exporter-only
allowlist field before returning any report, so a partially created container
could expose a valid identity and ownership token yet remain unreaped because
an unrelated image, environment, SSH, or publication field was missing.
Presence is now retained as evidence on the report and enforced only by check
4. Teardown consumes only exact identity and observed labels. Regression tests
prove that missing exporter-only fields still refuse exporter start, while the
same omission cannot suppress cleanup when the ownership evidence is complete.
The refute pass also exercised wrong identities and labels missing from both
views; those unsafe or unknown cases remain fail-closed, and it found no path
that deletes a foreign object.

Round 17 raised two P2s at the host-resource boundary:

- *P2: the full stopped-rootfs archive was materialized at an unbounded
  gate-owned path before extraction applied its byte cap.* The Runtime seam
  now streams into a caller-owned writer, and the backend writes at most
  `MaxArchiveBytes` to its archive file before failing categorically. This is
  distinct from `MaxExportBytes`, which still bounds only the extracted
  handoff tree. The required refute pass rejected the first CLIRuntime
  implementation: an `RLIMIT_FSIZE` on the `container export` child does not
  constrain Apple Container 1.1.0's hidden temp archive. Inspection of the
  pinned source established that the CLI chooses a UUID temp path and asks
  the already-running Container API XPC service to write it with
  `EXT4Reader.export`, then copies that completed file to stdout. The false
  limit and its same-process fixture were removed. The gate therefore hard
  caps only bytes crossing the Runtime boundary and its own archive; the
  stock runtime's private pre-stream materialization remains inside the
  trusted runtime implementation because its public CLI exposes neither
  direct stopped-rootfs streaming nor a quota for that temp file. Revisit
  when Apple exposes either mechanism; do not claim a child-process limit
  constrains the XPC service.
- *P2: a header-count cap did not bound filesystem objects when `MkdirAll`
  could create arbitrarily many implicit parents for one deep file.* The
  extractor now accepts a nested entry only after its parent appeared as an
  earlier directory header, creates directories one level at a time, and
  counts every object before creation. A separate archive-path length cap
  rejects pathological names before filesystem calls. Regression cases prove
  both a zero-byte directory flood and a single deep implicit-parent entry
  fail before the refused object reaches the host filesystem.

Round 18 raised one P1 at the cleanup availability boundary:

- *P1: a full container-list failure suppressed cleanup of exact names whose
  create had already succeeded.* An unrelated malformed row could make the
  decoder reject the full listing, after which teardown skipped even the
  known-owned credential-mounted agent. On list failure, teardown now falls
  back to inspecting and reaping only claims established by a successful
  create; ambiguous creates remain excluded because they still require fresh
  per-invocation label evidence. The full-list error remains a teardown
  failure, and the ordinary second list still proves absence.

The required refute pass found two cleanup-effect gaps in the first fallback:
an unknown inspect state skipped stop as though it proved stopped, and a stop
error prevented deletion even when the stop side effect had succeeded. The
shared reap primitive now attempts stop for every state not affirmatively
`stopped`, attempts exact-name deletion regardless of the stop result, and
joins both errors. Agent and exporter regressions cover unknown state; a third
case makes stop take effect and return an error, then proves deletion still
runs. Wrong inspect identity still prevents action, and no ambiguous or
foreign-object deletion path was found.

Round 19 raised one P1 and two P2s across pre-execution approval and cleanup:

- *P1: the credential-bearing writer was started from generated intent
  without inspecting the runtime-realized topology.* The writer now follows
  the exporter's create-inspect-approve-start sequence. Before start, the
  exact identity, digest-pinned image, argv, fixed working directory, full
  environment, mount multiset, stopped state, SSH setting, and publications
  must match the approved writer spec. Runtime-added binds, credential mounts,
  environment, forwarding, publications, or payload drift fail categorically
  before credentials can execute.
- *P2: accepting any PATH-only exporter environment left relative argv
  resolution dependent on runtime state.* The observed exporter environment
  is now exactly one fixed system PATH with no workspace entry. The refute
  pass found PATH alone did not cover slash-containing relative execution
  (`./helper`) if the runtime changed `initProcess.workingDirectory` to the
  workspace. Working directory is therefore required inspect evidence and is
  fixed to `/` for both writer and exporter. Omission or drift fails before
  start.
- *P2: a full volume-list failure suppressed deletion of a successfully
  created workspace.* Teardown now attempts exact-name deletion on that path
  only when `workspaceOwned` was established by successful create. An
  ambiguous create remains list-and-token gated, the list error remains a
  teardown failure, and the second list remains the absence proof.

The refute pass also found that tag-only agent images made the new payload
comparison non-exact: a stable reference with different resolved bytes passed
because no expected digest existed. `HandoffSpec` now requires the agent image
to be digest-pinned, and pre-start inspection binds both reference and digest.
The reference-runtime live fixture was already pinned. The cleanup fallback
had no remaining sibling finding: it cannot act on an ambiguous workspace and
still performs the absence re-list.

Round 20 raised two P2s at the hostile-manifest verification boundary, both
resource-exhaustion vectors the archive and per-file extraction caps miss:

- *P2: the manifest was read whole into the daemon heap.* `verifyManifest`
  used `os.ReadFile`, but a single `manifest.json` can fill the per-file
  extraction budget (`MaxExportBytes`, 2 GiB) and blobless entries (symlinks,
  submodules) evade `MaxExportEntries`, so nothing bounded the read before JSON
  validation could reject it, an OOM DoS. Added a configurable
  `MaxManifestBytes` (default 64 MiB), read through `io.LimitReader` with the
  same +1/at-cap discipline as the proof read; over-cap fails closed as an
  export_verification conformance failure.
- *P2: each blob was re-hashed once per manifest entry.* Distinct paths may
  legally share a digest (identical files), so a small archive (one large blob,
  many entries citing it) forced thousands of full-file re-hashes. Blob
  verification now dedupes. Key choice is load-bearing: the dedup key is
  `(digest, size)`, not digest alone, because a per-digest skip would leave a
  second entry lying about the shared blob's size unverified; `Validate` orders
  entries but never cross-checks that two entries citing one digest agree on
  size. A wrong size fails `verifyBlob` on the first read, so only entries that
  agree on `(digest, size)` collapse to a single hash, bounding a hostile
  same-digest fan-out to at most two reads before failure. The stray-check map
  stays separate (it needs every referenced blob, dedup or not).

The refute pass over both fixes confirmed: the manifest cap admits an exactly-
at-cap file and rejects `cap+1`; the valid decode/trailing-bytes path is
unchanged (decode still runs on the buffered bytes); missing-manifest still maps
to export_verification. For the dedup, a same-digest/different-size manifest
fails closed regardless of entry order, `*Size`/`*Digest` derefs stay guarded by
`EntryRegular && !BlobOmitted` (which `validateRegular` proves non-nil), and the
`verified` set is bounded by the now-capped manifest. Regression tests cover the
manifest cap, the identical-files dedup, and the size-liar rejection.

Round 21 raised one P2 in the "approve exactly one, reject duplicates" class
(rounds 4 and 14):

- *P2: a duplicate proof header silently overwrote the first.* The proof is the
  only extracted artifact kept in memory rather than written through `O_EXCL`
  (output files) or `Mkdir` (dirs), so a second `handoff-proof.txt` header
  overwrote the earlier `proof` bytes and the last one decided check 5. A
  malformed archive could pair a contradictory proof with a valid duplicate and
  pass. `extractHandoff` now tracks `proofSeen` and fails closed on a second
  proof header, approving exactly one observed proof. The class sweep confirmed
  the proof is the sole memory-kept artifact: the manifest and every output file
  extract through `O_EXCL`, and output directories through `Mkdir`, so a
  duplicate of any of those already fails closed. Regression: a
  "duplicate proof entry" violation case.

Round 22 raised one P2 at the returned-object (inspect) trust boundary:

- *P2: a mount claiming both `ro` and `rw` read as read-only.* `toMount`
  collapsed the options to `ReadOnly=true` on the mere presence of `ro`, so a
  contradictory report satisfied the read-only checks (writer credential mounts,
  exporter workspace mount) without proving read-only access. `ReadOnly` is now
  `ro && !rw`, and a new decode-time `Mount.AccessConflict` records `ro && rw`.
  The conflict fails closed in both allowlist paths: the writer's `sameMounts`
  equality can no longer match a conflicting realized mount against the
  conflict-free generated spec, and `verifyExporterAllowlist` rejects it with an
  explicit case. The field is unserialized (`json:"-"`), so specs and their
  goldens are untouched, and it participates in `Mount` equality as a plain
  comparable field. The class sweep confirmed `toMount` is the only place
  read-only is inferred from option presence (the generation side sets
  `ReadOnly` explicitly and never conflicts). Regressions: a `toMount` access
  table (ro / rw / neither / both, order-independent), plus contradictory-access
  cases in the agent and exporter allowlist violation tables.

Round 23 raised one P2 spanning a config-validation gap and a latent
functional break:

- *P2: unclean workspace/handoff/proof paths were accepted.* `validate` checked
  only a leading slash, so a common operator variant like `HandoffDir:
  "/handoff/"` passed, yet `extractHandoff` compares the unnormalized
  `handoffRel` (`"handoff/"`) against `path.Clean`ed tar names, so no exporter
  entry ever matched and every handoff failed export verification. The same
  laxity let `.`/`..` segments slip past the string-prefix `disjointPaths`
  nesting check that guards proof/manifest shadowing. All three paths are now
  required to be clean absolute non-root paths via the existing `cleanAbs`
  predicate (`path.Clean(p) == p`, absolute, non-root), which rejects trailing
  slashes, `.`/`..`, double slashes, and root; `disjointPaths` consequently only
  ever sees normalized paths, so its prefix comparison is sound. Regressions:
  trailing-slash, dotdot, double-slash, and root cases across the three fields
  in the config-validate table.

Round 24, P1 (the "no untrusted content in Reason" credential-leak class,
rounds 3 / 10 / 13 / 15): a blob-verification failure formatted the manifest
digest into the conformance reason. A regular entry's digest is 64 hex chars of
archive-derived input, so a hostile export could encode a credential there,
force a blob mismatch or omission, and have the token returned or logged despite
the reason contract. `verifyBlob` now returns value-free category errors (its
`os.Open` error would otherwise embed the digest-bearing blob path, and the
wanted digest/size are manifest-derived), and the caller redacts the blob
identifier with `redactPath`. Regression: a refused export with a
credential-shaped digest and no matching blob, asserting the reason omits the
digest.

Round 24, P2 (host-resource boundary, adjacent to round 17): the container CLI
buffered stderr into an unbounded `bytes.Buffer`, truncating to 512 bytes only
after the child exited, and a redacted create failure buffered it before
discarding. A noisy or wedged runtime/XPC service could exhaust daemon memory
before the call failed closed. Stderr is now captured through a `capWriter` that
buffers at most the reported cap and drops the rest, and a redacted call drains
stderr to `io.Discard` rather than buffering bytes it throws away. Regressions: a
`capWriter` bound/truncation unit test; the existing redacted-stderr credential
test still passes.
