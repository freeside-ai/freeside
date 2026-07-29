---
run: manual
stage: wave2-1a2
date: 2026-07-28
branch: feat/vendor-instruction-input
---

# Host Vendor Instructions as an Admitted Control-Plane Input

Work unit #375 (mandatory note: material control-plane and trust-policy
change).

## Decisions

- **Mirror the host mechanism through a per-run snapshot.** Chose the
  configured `~/.claude/CLAUDE.md` path as operator-owned authority, resolved
  once at admission into exact bytes or explicit absence, over repository
  storage or a second approval system because the objective is behavioral
  parity with agents on the operator host. The trade is deliberate: instruction
  content has no Freeside-managed approval history, but every admitted run
  carries an immutable digest that makes cross-run drift observable.
- **Keep vendor instructions a distinct content role.** Chose a versioned
  stage-input role and content-addressed blob over concatenating the file into
  the implementer prompt because native user instructions and stage purpose
  have different provenance, lifecycle, audit meaning, and future vendor
  targets. The v1 stage-input encoding remains reconstructible; every new
  admission uses v2 with either a digest or explicit absence.
- **Expose one file, not a host configuration directory.** Chose a
  gate-authored, run-owned volume containing exactly `CLAUDE.md`, independently
  observed and mounted read-only at the container's native user-instruction
  directory, over mounting the live host `.claude/` tree or baking content into
  the image. An empty volume for admitted absence also masks an instruction
  file a nonconforming image might carry at that path. The agent and exporter
  allowlists keep this volume outside the workspace and candidate export.
- **Make repository authority an exhaustive invocation contract.** Chose a
  ward contract that names the exact trusted base as the only valid repository
  instruction source and enumerates startup, recovery, resume, and child
  process entry over relying on one initial process's working directory. The
  writable workspace is deliberately not a representable valid authority.
  #237 must realize this contract in the production Claude driver and prove the
  authenticated CLI's loading behavior; #375 supplies the digest,
  materialization, topology, and boundary vocabulary it consumes.
- **Fail closed on every ambiguous source state.** Chose clean absence only
  for a missing configured path; an existing path that is dangling, unreadable,
  non-regular, oversized, unstable while read, missing from the artifact store,
  or digest-mismatched is an admission or materialization failure. Allowing a
  final symlink preserves the operator's free-prompts layout without exposing
  later retargeting to an admitted run.

## Verification Findings

- The deterministic runtime fixture observes a networkless, credential-free
  seeder copying exactly one file, a separate read-only observer proving its
  digest and topology, the writer mounting the resulting volume read-only at
  `/root/.claude`, and the exporter mounting only the candidate workspace.
- The backup closure already walks admitted stage-input roles; adding the
  vendor digest there is sufficient and needs no schema migration.
- The stage-input record itself is the durable audit metadata: its explicit
  prompt-package and vendor-instruction fields survive persistence, replay,
  backup, and driver handoff without a second drift-prone metadata record.

## Refute-First Ledger

Confirmed and fixed:

- A same-size in-place host edit could evade a metadata-only stability check
  while admission was reading it. Admission now reads the already-open regular
  file twice and requires identical bytes as well as stable identity, size, and
  modification time before hashing the snapshot.
- A one-megabyte volume had no room beyond the maximum one-megabyte instruction
  body. The run-owned overlay is two megabytes, while admission and ward still
  enforce the one-megabyte file ceiling.
- Adding the instruction seeder and observer shifted failure-injection tests
  that deliberately target later list boundaries. The updated tests address
  the intended role explicitly and the full lifecycle suite proves both new
  roles are reaped on success, refusal, and recovery.
- The first review found that exact-target validation did not reject a
  workspace or credential mount above or below `/root/.claude`. The
  pre-resource allowlist now rejects both overlap directions for both
  caller-controlled mount classes, with an adversarial four-case regression
  matrix.
- The first review also found that adding exported `AgentSpec` fields changed
  the unversioned journal digest and made open pre-upgrade handoffs
  unrecoverable. Recovery now checks an exact historical field projection
  after the current digest, admits only explicit vendor-instruction absence
  through that compatibility path, and proves both teardown-to-loss and
  completed-writer adoption. Present unbound instructions still fail closed.

Rejected by verification:

- A foreign same-name instruction volume cannot be deleted after an ambiguous
  create: it travels through the same fresh ownership-label and creation-
  fingerprint classification as the workspace, and teardown leaves foreign or
  unprovable objects untouched.
- A baked image instruction cannot govern an admitted-absence run: the agent
  always receives the run-owned instruction volume at the native directory,
  and the observer proves that volume empty before the agent starts.

Revisit when another vendor driver is initialized, when the operator-host
instruction source needs managed history or approval, or when the production
driver exposes a new local process-entry shape beyond the four enumerated
boundaries.
