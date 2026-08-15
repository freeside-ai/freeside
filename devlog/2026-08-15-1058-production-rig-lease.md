# Lease the Production Acceptance Rig

Work unit: #796. This is a safety-policy change on a destructive path, so the
decision note is mandatory.

Chose one harness-held lease across the production rig over a PID-file or
timestamp lease. A UID-stable private coordination directory holds a global
production-campaign flock and hashes each canonical state root, seed root,
database, and resolved listener into an additional resource flock, while
same-named local root flocks preserve inspectability and make swapped
state/seed roles collide. Global exclusion is deliberate: deterministic
handoff-container names can collide even when every pre-submission resource
is disjoint, and the separate bind process cannot safely transfer new flocks
back into the long-lived holder. The directory is independent of
`HOME`/`TMPDIR`, lives under the OS account's durable home, and is rejected
unless owned and private. Kernel flocks are the
only liveness authority; the recorded PID is descriptive, an old timestamp
never expires authority, and an unclean release leaves a manifest that only
the explicit `freesided rig recover -confirm` path can clear. The global
coordination domain also carries an atomically published global manifest as a
durable stale gate: a crash keeps global exclusion in force after the flock
drops, while an intentional clean release removes it before removing the
per-root manifests. Publishing the global gate only after the authoritative
state manifest keeps every acquisition failure recoverable. The fixed
state-root lock also keeps the acquisition identity and a checksummed,
monotonically sequenced append-only log of every token-authorized runtime
resource amendment. Each record carries a domain-separated Ed25519 signature;
the random holder token is the private-key seed, while the pinned public key is
part of the fixed identity. That last complete log record is the sole cleanup
authority and is appended before any mirror publication; the global, state,
and seed manifests may lag it after a crash but may never add names. Every
decoded manifest must match the fixed identity, and the recorded database must
still resolve to its originally canonical path, before live authorization or
stale recovery can act.

Chose a state-root manifest plus a seed-root mirror over a state-only marker.
This makes two campaigns with different databases but one seed root collide,
while keeping refusal metadata available at either lock. They are inspectable
mirrors, not cleanup authority: initial acquisition publishes the state copy
first, clean release removes the seed copy first, and stale recovery acquires
every recorded resource lock before reconstructing both mirrors from durable
amendment authority. It never unions decoded resource arrays, because syntax
proves namespace shape but cannot prove ownership.

Chose exact recorded deterministic names over pattern cleanup. A mechanical
sweep of every non-test runtime `CreateContainer`, `CreateVolume`, and
`CreateNetwork` site, plus the direct Apple-container command sites in the
project-image and verification rooms, found four deterministic production
namespaces. The direct command sites use runtime-assigned IDs captured through
private cidfiles and create no named persistent volume or network, so they do
not expose a deterministic cross-campaign collision. The four deterministic
namespaces are all now derived from ward's naming registries and signed before
their first runtime call:

- Claude handoff: 13 containers (`seeder`, `observer`, `ins-seed`,
  `ins-check`, `cfg-seed`, `cfg-check`, `projects-check`, `sess-check`,
  `writer-check`, `cred-pre`, `cred-post`, `agent`, `exporter`), five volumes
  (`ws`, `ins`, `cfg`, `projects`, `session-env`), and the `egress` network;
- invocation pre-job: the deterministic
  `freeside-ward-conf-<digest>-prejob` container; and
- Codex review: workspace `seeder` and `observer` plus `ws-obs`, `agents-init`,
  `agents-obs`, `snap-init`, `snap-obs`, and `codex` containers; `ws`,
  `agents`, and `snap` volumes; and the `egress` network. Recovery derives
  this set from the authenticated durable intent, including the historical
  `workspace-observer` and `agents-observer` spellings for legacy records; and
- startup and scheduled full conformance: the synthetic handoff's exact
  `seeder`, `observer`, `ins-seed`, `ins-check`, `agent`, and `exporter`
  containers, `ws` and `ins` volumes, and `egress` network under
  `conf-<16hex>`, plus probe
  containers `liveness`, `seed`, `audit`, `excl-writer`, `excl-second`,
  `net-live`, `net`, `inx-live`, and `inx`, and probe volumes `cred`,
  `liveness-ws`, `excl-ws`, `net-live-ws`, and `inx-ws`, all under the exact
  `freeside-ward-conf-conf-<16hex>-<role>` namespace.

The daemon's per-invocation boundaries authenticate the live holder with a
random token before any member can be created. This covers research
continuations, every request-changes iteration, Codex review launch and
relaunch, startup recovery, and every full conformance pass. The cleanup path
mechanically rejects any manifest name outside those namespaces. Runtime listing is decoded through
ward's existing fail-closed Apple-container boundary before a recorded
container can be stopped or deleted. A clean release or stale recovery refuses
to clear global exclusion while any recorded volume or network remains, so the
owning ward journal must reconcile intentionally preserved persistence first.
Recovery also refuses while any same-runtime CLI is active, then re-lists all
recorded object classes; an orphaned creation either remains active at that
check or becomes visible in the following snapshot before the gate can clear.
No PID, argv, environment, name glob, or timestamp grants cleanup authority.

Rejected making every daemon startup require a rig lease. The database lock
already owns ordinary daemon exclusion, while supervised and development
operation are valid outside production acceptance. Acquisition probes that
database lock, the resolved fixed nonzero listener, and the known LaunchAgent
before publishing success. Cleanup and recovery re-check the relevant daemon
authorities while holding the database lock. The harness continuously
authenticates its live holder and requests a clean release only after exact
resource cleanup; interruption abandons the lease for explicit recovery.

The refute-first passes confirmed and fixed root-role swaps, disjoint-root
database/listener sharing, cross-campaign container-name reuse, decoded
database/listener authority substitution, listener alias re-resolution,
runtime-resource authority substitution, bind/cleanup/release races, recovery
crossing a clean release, unrecorded pre-job probe persistence, PID-reuse
signaling, subprocess-group escape from bounded cleanup, environment-split or
temporary coordination roots, ephemeral listeners,
losing-contender root mutation, and ambiguous or non-durable manifest
publication/removal. The later Codex review finding corrected an incomplete
agent class sweep, not an owner decision: journal ownership authenticates
destructive recovery but does not prevent another campaign from reusing the
same deterministic runtime names after the global gate clears. The complete
review topology therefore joins the same signed per-invocation authority as
Claude handoff resources. The writer also rejects an amended manifest that
would exceed its decoder's 64 KiB limit before appending signed authority, so a
large campaign cannot strand the gate behind an unreadable mirror. A daemon
that starts after the successful acquisition preflight remains a same-user
local-authority race; the acceptance criterion is to reject a daemon already
running before launch, and later harness checks still fail the campaign rather
than granting cleanup authority from process metadata.

The remaining Apple-container limitation is the same one recorded for ward:
stop and delete are name-addressed and have no compare-and-delete primitive.
The rig prevents cooperating campaigns from reusing its recorded namespace;
same-user runtime mutation outside the harness remains host authority and is
not made safe by pretending a bare name is an immutable runtime identity.
The same boundary applies to a same-user adversary who deliberately rolls back
the signed log and every independent mirror together: preventing coordinated
rollback requires a high-water anchor outside that user's writable authority,
not another checksum or signature in the same files.
The harness bounds exact cleanup by first cancelling `freesided`, allowing its
context to terminate separately grouped runtime subprocesses, and only then
escalating against a supervisor-reserved process group.

Revisit when Apple container exposes immutable object IDs or conditional
deletion, when daemon startup can inherit the rig's held database exclusion
and eliminate the post-preflight race, or when production acceptance owns
another runtime namespace that must join the lease manifest.
