---
title: Freeside Project Plan
revision: 15
status: active
phase: 1A
updated: 2026-07-20
---

# Freeside

**Project charter and implementation specification.** This document defines what
Freeside is, how it must behave, and the order in which it will be built.

How to read it:

- Sections 1–4 define the product, its goals, and its human-attention model.
- Section 5 defines the architecture and its binding contracts.
- Sections 6–10 define verification, review, telemetry, comprehension, and
  operations.
- Sections 11–12 define the roadmap and exit criteria.
- Sections 13–15 record current decisions, risks, and naming.

The plan's identity of record is the default-branch commit digest (Section 5.8).
`revision` is only a human label. It changes when Section 9 classifies a change
as material. Section 13 records the current revision; the history it links
records every revision. PR bodies and decision notes carry the narrative.

---

## 1. What Freeside is

**Freeside is a local, durable workflow controller that grants agents the autonomy to turn work items into evidence-backed pull requests and interrupts me only when judgment is required.**

Category: **an agent control plane.** Harnesses such as Claude Code and Codex
run the agent's inner loop. Freeside runs the outer loop. It controls:

- what work starts;
- where it runs and what capabilities it receives;
- which credentials and network paths are withheld;
- what evidence is required before the work counts as done;
- when a human must decide; and
- what state survives a crash.

The self-brand register summarizes the relationship: *the harness runs the
agent; you hold the reins.*

The supported reference deployment is a Mac Studio. The daemon core remains
Linux-portable under Section 3.3.

### The end-to-end workflow

1. A manual submission, labeled issue, or scanner proposal creates a work item.
2. An elaborator turns it into a specification using research artifacts fetched
   by the daemon.
3. I approve the specification in the attention inbox.
4. An agent implements it in an isolated workspace with no GitHub credentials.
5. After the agent exits, a proven workspace handoff carries the result into an
   out-of-process hostile import boundary and then a fresh checkout.
6. A trusted recipe verifies the candidate and captures evidence in a clean
   environment.
7. The daemon publishes the verified candidate under an audited GitHub trust
   profile.
8. Independent review and yield-driven remediation continue within explicit
   emergency brakes.
9. A ready-for-final-review item appears on my phone with mechanical evidence.
10. I review and merge the pull request on GitHub.

The attention inbox is part of the control system, not a notification wrapper.
The daemon owns its lifecycle, staleness, synchronization, and concurrency
rules.

| Authority | Owns |
| --- | --- |
| GitHub | Source, issues, pull requests, reviews, checks, and merge |
| Freeside | Workflow execution, durable decisions, routing, and approvals |

Freeside exists as a personal-leverage tool. Its measure is a positive
return: useful, correct work worth more than the attention, maintenance,
money, and risk it costs. The manual workflow already shows that elaboration,
implementation, and iterative review are useful. The open question is whether
Freeside can make that workflow safe, durable, and low-attention without moving
the danger into provider credentials, CI, artifact import, stale approvals, or
interruption creep.

The project succeeds only if all four claims hold:

1. **Useful, correct work per unit of my attention rises** against a
   passively logged, normalized baseline.
2. **Decision quality is preserved.**
3. **Safety invariants hold** under Section 12, verified by conformance and
   adversarial tests, never read off telemetry.
4. **Autonomy is preserved:** exceptional interruptions remain rare and trend
   down under Section 3.2.

The four claims are necessary gates, not the goal: cost and maintenance still
decide whether passing them yields a positive return.

## 2. Goals and non-goals

### Goals

1. **Treat attention routing as a first-class system.** Durable AttentionItems
   have lifecycles, type-specific actions, optimistic concurrency, cross-device
   synchronization, honest per-delivery status, push notification, and
   self-contained decision cards on iPhone and Mac.
2. **Keep elaboration in the tested value proposition, but severable.** It uses
   daemon-fetched research artifacts (Section 5.4). (Decider: user.)
3. **Support autonomous initiation.** Manual, label, and scan initiators run in
   `propose` or `auto_start` mode.
4. **Use yield-driven review remediation.** Round counts are emergency brakes,
   not the normal stopping rule.
5. **Bound execution.** Capabilities are fixed at spawn; no workspace receives
   GitHub credentials; every run declares a credential mode; every stage uses a
   named egress profile with an honest risk class; post-agent handoff is proven;
   import is hostile and out of process; verification recipes are trusted.
6. **Contain CI privilege.** Agent-authored code never reaches secret-bearing or
   privileged CI and never changes automation-control paths through the ordinary
   workflow. Trust profiles attest effective PR-job authority.
7. **Operate remotely from iPhone.** The human judges at gates, then performs
   final review and merge on GitHub.
8. **Let chat author artifacts and the engine execute them.**
9. **Let verification define completion.** It is deterministic, recipe-trusted,
   and clean-room. The verifier captures visual evidence; the implementer does
   not claim it. Provenance is machine-enforced (Section 5.15).
10. **Make decisions durable.** Committed decisions survive restart; external
    effects converge; database and artifact state restore coherently from
    complete encrypted checkpoints; clients converge on daemon state.
11. **Record enough instrumentation for agent-driven optimization** (Section 8).
12. **Make operational simplicity a 1A exit criterion.** Automate setup only
    after interfaces survive real use. Privileged installation is a narrow
    elevation boundary, and the daemon never retains root. (Decider: user.)

### Non-goals

1. Freeside is not an IDE or review surface: code review and merging stay on
   GitHub; Freeside owns workflow decisions and approvals. Human merge is the
   current accountability checkpoint; whether narrow, risk-bounded classes of
   change ever earn automatic merge remains deliberately open.
2. It is not a product for hypothetical users: no multi-tenancy or billing.
3. It is **not a harness**. It uses sanctioned vendor batch interfaces and never
   owns a model loop.
4. It does not modify itself at runtime. Control-plane configuration is never
   hot-modified.
5. Automatic provider fallback, voice, a pipeline DSL, and briefings are out of
   scope until the recorded outcomes of later phases earn them.
6. It is neither a formal pre-build validation study nor a generic CI security
   auditor.
7. It is not a general-purpose synchronization platform. Server-authoritative
   snapshots are enough; there is no client-facing event log and no CRDT.

## 3. Operating principles

### 3.1 Autonomy inside the ward

Autonomy is the default. Gates exist only at trust-boundary crossings and the
two designed judgment points.

Repeated exceptional interruptions trigger a policy review. An eligible
repetition may produce a policy-change proposal; promotion to a standing grant
requires low risk, stable preconditions, and bounded downside, never
repetition alone. Safety invariants and non-waivable gates never auto-promote
and never offer a bypass.

The following classes are non-waivable:

- GitHub credential separation;
- CI trust-profile validity;
- candidate changes to automation-control or reviewer-instruction paths;
- control-plane modifications;
- stale-approval rejection;
- failed runner conformance, including the workspace-handoff gate;
- host reachability;
- artifact-integrity failure;
- secret detection; and
- capability escalation outside approved manifests.

### 3.2 The interruption budget

Every AttentionItem is tagged `planned_gate` or `exceptional`. The exceptional
rate is a health metric; a rising rate is a defect, subject to Section 3.1.

**Self-service rule:** recurring eligible classes must offer a way to resolve
the class through the control-plane proposal path.

**Rein is a convenience preset, not a security dial.** At run creation it
expands into explicit resolved policy, stored with a digest and per-key
provenance. Explicit keys visibly override preset defaults.

Accepted hot spot: work on Freeside itself often touches control-plane paths.

### 3.3 Portability

macOS is the supported reference deployment. The daemon core remains
Linux-portable and is built and tested on Linux from day one.

Linux becomes supported only when one named distribution, architecture, and
`linux_vm` backend pass the complete setup, conformance, execution, recovery,
and upgrade suite. Running provider credentials on a cloud host adds exposure;
that exposure must appear in the residual-risk documentation. (Decider: user.)

### 3.4 Simplicity

Setup, onboarding, and upkeep are product features with committed targets
(Section 10). A permissive first run uses the honest `attended_dev` operating
mode (Section 5.7); it is never a bypass. Strict settings always gate
`unattended` operation.

### 3.5 Oversight

Oversight is part of my contribution, not pure overhead: it is how failures
are caught early, so it cannot be optional. The design goal is frictionless
oversight, because oversight that is a chore is oversight that gets skipped.
Sections 8 and 9 carry its designed instruments: honest attention telemetry
and sampled decision audits.

## 4. The attention model

### Core records

**AttentionItem** contains:

`id`, `project_id`, `subject {subject_type: run | proposal_batch | project |
system, subject_id, run_id?}`, `type`, `priority`, `reason`,
`requested_decision`, `evidence_snapshot`, `agent_claims`, `artifact_digests`,
`pr_head_sha`, `item_version`, `interruption_class`, `conversation_id?`, derived
timing aggregates, `expires_when`, and `status`.

`evidence_snapshot` contains engine facts and only verifier or daemon artifacts
produced under an approved recipe (Section 5.15). Agent claims are labeled.
Cards render image attachments directly from the artifact store by digest.

**AttentionDelivery** records one delivery attempt:

`item_id`, `device_id`, `channel`, `attempt`, `submitted_at`,
`channel_accepted_at`, `opened_at`, and `delivery_status`.

Provider acceptance is never called “delivered.” Stronger language requires a
real device receipt. Open-to-decision time is the headline attention-latency
metric; the Section 1 per-unit measure governs. Item timing fields are
aggregates derived from deliveries.

### Phase 1 item types and actions

Approval is not a universal action.

| Item type | Available actions and behavior |
| --- | --- |
| `spec_approval` | Approve, request changes, discuss, or stop. Render the full specification. A revision shows the diff from the last reviewed version, prior comments, and claimed addressals. |
| `review_diminishing_returns` | Finish now; apply the current batch and finish; continue under specified policy; or turn a recurring preference into a project-policy proposal PR. It never mutates policy directly. |
| `review_dispute` | Adjudicate the finding, discuss, or stop. |
| `execution_failure` | Retry; retry with a predefined policy-allowed capability manifest; discuss; or stop. |
| `agent_question` | Answer and retry, answer without retry, or stop. |
| `publish_blocked` | Rerun trust evaluation, choose an approved alternate publication profile, inspect the trust failure, or stop. |
| `ready_for_final_review` | Open the PR (navigation, not resolution), return work to the agent with feedback, `mark_seen`, dismiss, or stop. It stays active until Freeside observes merge or close, work is returned, or the item is dismissed. |
| `run_proposal` | Start, **start with changes**, decline, or snooze. “Start with changes” creates a revised proposal artifact, supersedes the original item, creates a new item version, and starts the run from the exact revised digest. It never uses unversioned ad hoc parameters. Proposals are grouped under `proposal_batch_id` with per-candidate decisions. |
| `system_health` | Acknowledge, run doctor, or stop unattended operation. Acknowledge means seen, never resolved. The item remains blocking until the diagnostic clears, unattended operation is explicitly stopped, or a validated configuration supersedes it. |
| `blocked` | Consolidates external waits that exceed Section 5.12 thresholds. It is read-only. |

Section 9 governs each type's presentation: what its card leads with and what
layers below.

### Lifecycle rules

- Approvals bind to artifact digests and the PR head SHA. Changed inputs
  invalidate them.
- Retries supersede failures.
- Resolutions are transactional and version-checked.
- A stale submission receives a conflict and the replacement item.
- Notifications are read-only hints, never authority.
- Fault-class capture is suggested, can be corrected with one tap, and may
  remain unknown.
- WIP caps apply to runs and initiatives. GitHub Projects remains the passive
  all-work view.

## 5. Architecture

### 5.1 Overview

```
GitHub  <── reconciliation and publication ──>  freesided
                                                    │
                               execution, import, verification, storage
                                                    │
                          Freeside app  <── sync ────┘
```

| Component | Responsibility |
| --- | --- |
| **GitHub** | Owns source, issues, PRs, reviews, checks, merge, and Codex cloud review. Freeside reconciles each active resource independently; there is no global cursor. |
| **Event inbox** | Accepts reconciled GitHub state, intake scans, cron events, and manual events idempotently. |
| **Workflow engine** | Runs code-defined state machines using policy from configuration. It records the resolved rein-policy digest and separate active, elapsed, and waiting clocks for each run. |
| **signet** | Owns AttentionItems, deliveries, conversations, synchronization, device pairing, and ntfy integration. |
| **Research fetcher** | Retrieves immutable, digest-addressed research artifacts for agents. |
| **StageDriver** | Runs bounded Claude batch jobs. A permanent fake supports deterministic tests. |
| **ReviewSource** | Integrates Codex GitHub review. A permanent fake supports deterministic tests. |
| **Finding classifier** | Adds versioned annotations to immutable raw review findings. |
| **ward** | Provides runner capability classes, workspace-handoff capabilities, per-stage egress, operating modes, and conformance checks. |
| **gauntlet** | Runs out of process. It normalizes export, treats import and evidence as hostile, builds a fresh checkout, and starts clean verification and evidence capture. |
| **Git/publish** | Owns all GitHub credentials, deterministic external identities, invocation reconciliation, and, in 1B, the EvidencePublisher. |
| **Store** | Uses SQLite with inbox/outbox and a content-addressed artifact store. Section 5.10 defines encrypted checkpointed backup. |
| **Sync API** | Serves atomic snapshots with revision, epoch, and invalidation semantics. |
| **Freeside app** | Provides the SwiftUI macOS and iOS inbox, decision detail, and run timeline using platform-protected caches. |

### 5.2 The daemon

`freesided` is a single static Go binary.

- A supervisor runs it under a dedicated user.
- Other accounts cannot access its state or credentials.
- Privileged services bind only to loopback or Tailscale.
- **The daemon never runs as root.** One-time privileged work, such as user
  creation and launchd installation, lives in a narrow elevation helper.
- SQLite runs with WAL, `synchronous=FULL`, `foreign_keys=ON`, and a configured
  `busy_timeout`.
- CI builds and tests on macOS and Linux; macOS jobs stay lean.

### 5.3 Execution: StageDriver and ReviewSource

Every stage is a bounded batch job. The daemon assigns an `invocation_id` to
every external start, then reconciles all later operations by that ID:

- execution: start, inspect, stream, cancel, collect;
- review: `request_review`, inspect, poll, verify.

**Execution guarantee:** one committed invocation intent produces at most one
accepted result. The workflow never advances twice.

Phase 1 uses:

- one local driver, **Claude**;
- one primary review source, **CodexGitHubReview**; and
- permanent fakes of both interfaces.

The 1B shadow arm runs a fresh-context Claude review against the same head.
Freeside records its findings but never routes them. It also serves as the dry
run for a local reviewer.

**Only the control plane may trigger review** (decider: user). Trigger failure
closes safely by creating an AttentionItem. Nested `AGENTS.md` guidance is
documented Codex behavior. Automatic re-review of remediation heads is a
standing 1B integration test. The Claude setup token's inference-only scope is
contract-tested against the pinned CLI.

**Session durability contract:** transcripts and artifacts are durable.
Workflow recovery is guaranteed from stage inputs, workspace state, and
artifacts; provider session resume is best effort. Capabilities are fixed at
spawn. If they are insufficient, the stage emits a typed request and exits.

### 5.4 Credential modes, egress profiles, and concurrency

**No GitHub write credential ever enters any workspace.**

Every run declares and records one credential mode:

| Mode | Meaning |
| --- | --- |
| `subscription_contained` | Phase 1 default. The native vendor CLI runs in the agent VM. Its credential mount is read-only where permitted. The remaining exposure is an accepted, documented residual risk. |
| `api_key_isolated` | Supported in Phase 2. |
| `local_trusted` | Permitted only for explicitly trusted inputs. |

Secret scanning is intentionally described as **best effort**. It covers
supported text formats. Size, type, provenance, and publication controls govern
opaque artifacts. Universal detection across arbitrary encodings and images is
impossible; Section 5.15 records the image residual.

Every stage also receives an egress profile from control-plane policy. Profiles
sit above the credential-mode floor and represent different risk classes:

| Profile | Access and risk |
| --- | --- |
| `provider_only` | Default. Only the provider API is reachable. |
| `provider_web_read` | Materially wider credential-exfiltration exposure. Read-only HTTP can still exfiltrate through URLs, headers, bodies, redirects, and DNS while the provider credential shares the trust domain. It requires an explicit record of the wider exposure and a small trusted-domain allowlist. |
| Clean verification | No network access. |

The 1B elaborator does not receive general web access. It runs under
`provider_only` and emits typed fetch requests. The daemon fetches allowed URLs
and returns immutable, digest-addressed research artifacts, then reinvokes the
elaborator for a bounded number of iterations. This removes the broadest
credential-exfiltration surface from the injection-exposed stage and makes
research inputs provenance-bound, cacheable, and reproducible. Invocations bind
to artifact IDs, not live web state.

Provider concurrency has two independent controls:

`AuthIdentity {auth_store_mutation_lease, max_parallel_executions,
refresh_strategy, supports_read_only_auth_snapshot}`

1. Auth-store mutation, including refresh, login state, configuration writes,
   and store replacement, is serialized per identity.
2. Inference execution has a separate parallelism limit. 1B establishes that
   limit experimentally and exposes it to WIP scheduling.

If only one execution is safe, scheduling shows that constraint instead of
hiding it in a lock. API-key fallback is always available. Vendor tooling stays
native and unmodified.

### 5.5 The CI trust boundary

An agent branch can modify scripts that a privileged GitHub Actions job later
executes. Same-repository PRs do not receive the protections of fork PRs. A
job's implicit `GITHUB_TOKEN` and OIDC identity are authority even when the YAML
names no secret.

Every onboarded repository therefore has an **automation trust profile**:

```yaml
repository_security:
  pr_execution: audited_same_repo | fork_untrusted | local_only
  candidate_automation_changes: block        # .github/workflows/**,
                                             # .github/actions/**, action.y*ml,
                                             # CI entrypoints: publish-blocking
                                             # in the ordinary workflow; routed
                                             # through control-plane change
  pr_github_token_permissions: read_only
  allow_oidc: false
  allow_environment_secrets: false
  allow_secret_bearing_pr_jobs: false
  allow_self_hosted_ci: false
  allow_pull_request_target: false
  commit_plan: single_commit | plan_preferred
                                             # Section 5.6 agent-proposed
                                             # commit plan; conservative
                                             # default single_commit.
                                             # preferred falls back to one
                                             # commit with a surfaced
                                             # notice when a non-empty
                                             # import has an absent plan or
                                             # an enumerated agent-caused
                                             # structural/non-secret
                                             # screening rejection
                                             # (zero-change imports keep the
                                             # empty commit and surface a
                                             # present plan as not honored);
                                             # under plan_preferred, a
                                             # decoded secret
                                             # anywhere in the plan's text
                                             # blocks instead (Section 3.1)
  message_ruleset: github/1
                                             # built-in versioned commit-
                                             # message screening ruleset
                                             # (Section 5.6); the
                                             # identifier is validated
                                             # against the built-in
                                             # registry at profile review,
                                             # digest-bound
  workflow_audit_digest: sha256:...
  review: {mode: auto | framework_triggered, config_digest: sha256:...}
```

The audit attests the PR job's **effective authority**, including:

- effective `GITHUB_TOKEN` permissions;
- OIDC availability;
- environment and deployment credentials;
- reusable workflows and local composite actions;
- self-hosted runners;
- package-publishing permissions; and
- any workflow that consumes artifacts produced by untrusted PR code.

Phase 1A supports one repository with a machine-readable profile. A human
reviews it once; the daemon binds it by digest; drift fails closed.

**Standing prohibition:** the daemon host is never a self-hosted Actions runner
for a managed repository.

### 5.6 The gauntlet: workspace handoff, import, and clean verification

```
daemon-owned base repo ──exact base SHA──▶ agent workspace
agent exits ──▶ POST-AGENT WORKSPACE HANDOFF (5.7 gate): credential-bearing
   execution context terminated; workspace mounted READ-ONLY in a fresh
   credential-free context
export helper emits content blobs + normalized change manifest + optional
commit plan + evidence manifest ──▶ gauntlet worker (unprivileged, out of
   process) validates
gauntlet ──▶ fresh daemon-owned checkout; daemon re-authors clean commits
fresh checkout ──▶ clean verification workspace (no credentials, no network;
   trusted recipe runs checks and captures evidence)
verified candidate ──▶ git/publish ──▶ GitHub PR (under trust profile)
```

Exactly two channels leave the agent workspace, and they never mix:

1. The **repo-change channel** contains content blobs, a normalized manifest,
   and an optional agent-proposed commit plan: how the validated changes group
   into commits, in what order, with what messages, carried as plain untrusted
   data whose schema only the importer interprets and validates, never as git
   objects.
   It permits regular files only. Symlinks, submodules, special files, unusual
   modes, automation-control changes (Section 5.5), and reviewer-instruction
   changes (Section 5.8) are publish-blocking.
2. The **evidence channel** contains typed, provenance-bearing artifacts under
   Section 5.15.

The agent commits normally with git, but nothing of its `.git` is ever read
or imported by any trusted component: no objects, hooks, configuration, or
history as git state. What
may cross is a **commit plan** the agent writes as ordinary data at a
reserved workspace path, proposing how the final validated change set splits
into commits; it crosses as a declared member of the handoff output, so the
ward's stray rule admits it and the ward's whole-output secret scan covers
it like every other exported byte, in every mode. Under `plan_preferred`, the
daemon derives the
authoritative base-to-final change set
itself and accepts the plan only as an exact cover of it: every derived
change in exactly one ordered group, no unknown paths, every interpolated
intermediate tree structurally valid, every resolved non-empty group's
publishing message screened. For a non-empty import, it re-authors one
clean commit per resolved non-empty group when a plan is accepted, or one
daemon-authored commit under `single_commit` and the
enumerated `plan_preferred` fallback cases described below. A blocking
failure authors no candidate. Published tree content is confined to the
trusted base and the validated final snapshot by construction, so the
tree-content publication surface equals the single-commit import's, and the
screened messages are the one new published surface; intermediate commits
are unattested ancestry, and evidence and publication identities bind to the
single candidate head (Section 5.15). Agent commit SHAs, timestamps, and
identities never cross; publishing messages cross as validated, labeled claim
text screened as automation-control surface under the profile's
`message_ruleset`; under `plan_preferred`, an empty remainder's
non-publishing message skips those checks after the plan-wide secret scan. On
a non-empty import under `plan_preferred`, an absent plan or
one rejected for an enumerated agent-caused structural or non-secret
screening failure falls back to the single clean commit with a surfaced notice
naming the reason class. A zero-change import under `plan_preferred` takes the
deliberate empty-commit path after the tolerant scan and surfaces a present
plan as present-but-not-honored, while under `plan_preferred` a decoded secret
anywhere in the plan's text is publish-blocking until remediated
(Section 3.1 non-waivable). Under `single_commit` a plan is not decoded or
honored, its presence is surfaced as a notice, and escaped credentials retain
only the ward's literal best-effort coverage. Before either mode dispatches, a
trusted
base that tracks the reserved plan path or any descendant beneath it blocks
construction: the reserved name can be a Git tree even though the plan
channel itself is one regular file, and that entire namespace is excluded
from the derived change set. The walk exclusion and preflight use a
path-component boundary;
near-prefix names such as `.freeside-commit-plan.json.bak` remain ordinary
repository content.
The importer
never trusts the workspace's `.git`, hooks, configuration, or agent-written
manifests. It enforces the exact base SHA, canonical paths, allowlists, size
limits, control-plane restrictions, and Section 5.4 best-effort secret
scanning.

Permanent tests include malicious manifests, commit plans, blobs, and
evidence. Trusted
verification recipes load only from approved control-plane configuration or the
trusted base commit. Freeside mechanically identifies, risk-flags, and gates
changes to verification-control files.

Named residual risk: candidate test code executes inside the warded verifier.

### 5.7 The ward: runners, handoff gate, and operating modes

Runner backends declare capabilities; policy declares minimums. Freeside never
silently downgrades. Named capabilities are:

- `supports_detachable_workspace`;
- `supports_post_exit_export`;
- `supports_read_only_remount`;
- `supports_credential_volume_detach`;
- `supports_workspace_snapshot`; and
- `supports_networkless_export`.

#### The first ward gate

The actual runtime must prove this sequence:

1. Write files in an agent workspace.
2. Terminate the credential-bearing execution context.
3. Mount the workspace read-only in a fresh, credential-free context.
4. Export it without exposing provider credentials, daemon state, or host
   credentials.

Candidate mechanisms include a detachable volume, host-controlled block image,
snapshot/export, or separate export VM.

The declared strong class for Apple container 1.1.0 is
`fresh_vm_read_only_volume_handoff`, conditional on the conformance checks
below; the name is the runner backend's declared identity.

The same-VM fallback (terminate the agent process, detach credentials, and
export from the same VM) is refuted on this runtime by execution, not merely
weaker: release 1.1.0 exposes no host hot-detach, and a guest unmount is not a
credential-device detach; the credential block device stays attached and
remountable. Freeside must not implement or declare that class.

#### Operating modes

| Mode | Requirements and limits |
| --- | --- |
| `attended_dev` | May use a weaker runner class. Disables `auto_start`, automatic publication, and unattended escalation. Reports its isolation class honestly. |
| `unattended` | Requires successful conformance including the handoff gate, a valid trust profile, an approved credential mode, all runner minimums including the proven `supports_networkless_export` exporter boundary, current backup health including encryption status, and no blocking `system_health` item. |

Run the full conformance suite at startup, after configuration changes, and on
the doctor's schedule. Run a lightweight probe before every unattended job.
Golden images pin CLI versions. Workspaces use VM-local disk.

Bootstrap exception: SwiftUI work is exempt until a macOS execution class
exists.

### 5.8 Control-plane trust

The following are control-plane content:

- workflow configuration;
- prompts and policy;
- egress and trust profiles;
- verification recipes;
- materiality rules; and
- vendor auto-loaded instructions.

Freeside loads them only from an approved default-branch commit. Every running
stage snapshots the trusted configuration digests. Copies inside an agent
workspace are untrusted data.

Vendor instructions use overlays from the trusted base. Agent-modified
instruction files remain candidate diff content and are always risk-flagged.

**Reviewer-instruction poisoning is publish-blocking.** In the ordinary
workflow, Freeside blocks every reviewer-instruction path, including
`AGENTS.md` at any depth, `AGENTS.override.md`, `.codex/**`, and peers. An
automatic review is not independent when its PR changes the instructions that
govern that review. The gauntlet detects these paths mechanically.

### 5.9 Durability: effectively-once

| System | Authority |
| --- | --- |
| GitHub | Source, issues, PRs, reviews, checks, and merge |
| SQLite | Workflow state, decisions, attempts, events, routing, conversations, and audit |
| Artifact store | Immutable inputs and outputs |
| Providers | Transient session state |
| Repository documentation | Promoted decisions |

Every external action uses inbox/outbox discipline. Committed workflow
decisions survive restart. Deterministic identities, reconciliation, and
bounded retry make external effects converge on one intended result. Anything
that cannot be safely retried waits for me.

Kill-before and kill-after tests are permanent.

### 5.10 Coherent backup: encrypted checkpoints

Local artifact commits follow this order:

`blob → verify digest → fsync → atomic rename → referencing database row`

A missing referenced blob fails closed. Orphans are garbage-collected according
to retention policy.

**Restore points are complete checkpoints:**

`BackupCheckpoint {checkpoint_id, sync_epoch, server_revision,
sqlite_snapshot_digest, artifact_manifest_digest, timestamps}`

- Write the completion marker last.
- Restore only from completed checkpoints.
- Verify every digest before unattended work resumes.
- Issue a new `sync_epoch` after rollback.

**Confidentiality is policy:**

`BackupPolicy {encryption_mode, key_id, destination,
retention_by_artifact_class, last_completed_checkpoint, last_restore_test}`

- Remote checkpoints are encrypted.
- Encryption keys live outside agent environments.
- Backup credentials are never mounted into workspaces.
- GitHub App keys and provider credentials are excluded unless a stronger
  recovery design encrypts them separately. Recovery may therefore require
  reauthentication.
- Raw transcripts have shorter retention than decisions, approved
  specifications, and audit events.
- `freesided doctor` checks checkpoint age, encryption state, artifact closure,
  and restore-test age.

Before unattended mode uses a private repository with remote replication,
encrypted backup is required. A local-only development checkpoint may come
first.

### 5.11 GitHub integration: reconciliation plus intake

Freeside reconciles each active GitHub resource independently with conditional
requests. Intake scanners discover new work using overlapping scans and
idempotent identities. Webhooks are deferred to Phase 2 and added only if
latency becomes a problem.

### 5.12 Workflow definition, initiators, and artifacts

The workflow is a Go state machine. YAML supplies policy only. Crash retry and
agent remediation are separate mechanisms. A pipeline DSL waits until Freeside
has three genuinely different workflow shapes.

Budgeting uses three clocks:

1. **Active compute:** `stage_active_time` applies to each stage attempt;
   `run_active_compute_time` applies to the whole run.
2. **Elapsed deadline:** ends an abandoned workflow.
3. **Waiting thresholds:** create one consolidated `blocked` item instead of
   terminating the run.

A run waiting overnight for a reviewer does not consume compute budget.
`review.hard_active_time` counts active review and remediation, not calendar
waiting.

```yaml
project:        {repository: freeside-ai/<first-repo>, rein: tight}
initiators:
  - {type: manual}                      # freesided submit --spec
  - {type: label, label: "freeside",
     mode: auto_start}                  # explicit, recorded preset override
  - {type: scan, query: stale_prs, schedule: daily, mode: propose}   # Phase 2
elaboration:    {driver: claude, enabled: true, egress: provider_only,
                 research: daemon_fetched}
implementation: {driver: claude, failed_execution_retries: 2,
                 egress: provider_only}
review:
  source: codex_github
  continue_while: new_material_findings
  pattern_sweep_after: 2
  low_value_streak_before_attention: 2
  hard_active_time: 8h                  # active review/remediation clock
  hard_round_limit: 25                  # emergency brakes only
verification:   {recipe: trusted, commands: [go test ./..., go vet ./...],
                 capture: none}
gates:          {spec_approval: true, before_final_review: true}
budgets:        {stage_active_time: 45m,
                 run_active_compute_time: 4h,
                 run_elapsed_deadline: 7d,
                 max_diff_files: 40}    # cumulative vs base
waiting:        {checks_attention_after: 2h, review_attention_after: 4h}
security:       {credential_mode: subscription_contained}
telemetry:      {shadow_review_rate: 0.2}
```

Additional rules:

- `rein` resolves into digested per-run policy with per-key provenance.
- **Manual initiation uses `freesided submit`.** It registers the approved
  specification as a digest-addressed artifact and creates the run.
- `auto_start` is bounded by WIP caps. The conservative default is `propose`.
- Raw findings are immutable. Classification is a versioned annotation.
- Low-confidence materiality defaults to continued remediation or human
  attention.
- The classifier cannot declare a finding fixed.
- Artifacts are typed, immutable, and digest-addressed. Approvals bind to their
  digests.

### 5.13 Deterministic components

The engine, not an agent, runs deterministic policy jobs:

- verification;
- evidence capture;
- research fetching;
- card facts;
- evidence publication; and
- cleanup.

Agents appear where judgment is the work: elaborator, implementer, remediator,
diagnostic, finding classifier, shadow reviewer, and, later, briefer.

### 5.14 Client synchronization and conversations

#### Authority and consistency

The daemon is the sole authority. Client databases are disposable read caches.
The synchronization contract guarantees:

- transactional consistency in the daemon;
- optimistic concurrency;
- eventual convergence;
- read-your-write after acknowledgment;
- a cached, read-only view with a freshness banner while the daemon is
  unreachable; and
- no consequential action until the client validates current state.

#### Revision, epoch, and cache semantics

`ServerState {sync_epoch, revision}`

- Every client-visible transaction increments `revision`.
- A restore creates a new `sync_epoch`, which forces clients to discard caches.
- **A partial fetch never advances the whole cache.** Clients track
  `last_full_snapshot_revision` separately from
  `highest_observed_server_revision`.
- Every `ResourceSnapshot` carries `as_of_revision` and `entity_version`.
- `/sync/bootstrap` returns one canonical snapshot from one read transaction.
- A revision gap triggers a full bootstrap or a refetch of every potentially
  affected resource.
- A periodic revision heartbeat detects lost invalidations.
- Push and WebSocket improve latency only; correctness does not depend on them.

#### Devices, commands, and caches

Pairing uses a short-lived code shown or printed on the daemon host; no display
is assumed. The daemon stores only a credential hash or device public key, never
reusable plaintext. Devices can be revoked.

Every judgment-bearing mutation is:

`ClientCommand {command_id, device_id, expected_entity_version,
expected_bindings, payload}`

A retry returns the original result.

Monotonic telemetry, the credential-control surface, and attachment upload sit
outside `ClientCommand`:

- A delivery-opened receipt is an idempotent `PUT` identified by `(item,
  channel, attempt)`.
- The device identity comes only from the credential.
- The receipt records a fact and carries no judgment. It has no version
  precondition because the delivery may advance from `submitted` to
  `channel_accepted` before the receipt arrives.
- Attachments upload through a digest-addressed endpoint. Retrying converges on
  one artifact by digest (test 10).

Phase 1 has no offline approvals.

Client caches are part of the threat model:

- metadata uses platform-protected storage;
- only device credentials use Keychain;
- high-sensitivity attachments are not cached long-term by default;
- epoch changes evict caches; and
- revocation prevents future access but cannot erase content already cached.
  Freeside must not imply remote wipe.

#### Conversations and discuss

Conversations are Freeside domain objects:

- `Conversation`;
- `Message`, with a daemon-assigned sequence; and
- `AgentInvocation`, bound to explicit input IDs.

Messages are immutable; a correction is a new message. Phase 1 synchronizes one
whole conversation snapshot at a time. Text lives in SQLite. Attachments live
in the artifact store by digest.

Discuss commits this transaction:

`append message → record item version and bindings → supersede or transition →
write AgentInvocationRequested outbox intent with invocation_id → record
command result → increment revision`

Recovery accepts exactly one invocation result and never advances the workflow
twice.

On agent completion, Freeside finalizes and fsyncs blobs, then commits the
message, transition, and replacement item in one SQLite transaction. A failed
transaction leaves only harmless orphan blobs. Live streaming and mid-turn
steering are deferred to Phase 3.

#### Permanent Phase 1A sync and device tests

1. Resolving on one device produces a conflict on a second device.
2. An offline device submitting against a superseded version is rejected and
   receives the replacement item.
3. If every notification is missed, foreground refresh reconstructs the inbox.
4. Retrying a `command_id` after losing the HTTP response returns the committed
   result.
5. If discuss commits and the daemon dies before invocation, recovery produces
   exactly one accepted invocation result and never advances twice.
6. An agent response that arrives while both clients are closed is later
   retrieved by both as the same ordered thread.
7. Two concurrent discuss commands against one item version produce one winner
   and no second accepted result.
8. After daemon restore, a new epoch makes clients discard newer cursors and
   bootstrap.
9. A late notification for a resolved item deep-links to canonical state and
   exposes no stale action.
10. Retrying an attachment or message produces one artifact and one message.
11. A partial entity refetch does not mark the whole cache current.
12. A conversation-status change reaches a client that has already fetched
    beyond that conversation sequence.
13. An expired or consumed pairing code cannot create a device.
14. Simultaneous pairing attempts with one code create one device.
15. A revoked device cannot submit a prepared but uncommitted command.
16. Retrying a previously recorded command after revocation may return its
    recorded result but causes no new side effect.

### 5.15 Evidence and images

Four machine-enforced rules govern evidence:

1. **Capture belongs to the verifier.** The trusted recipe defines capture.
   Credential-free, network-free rooms capture “before” at the base SHA and
   “after” at the candidate. Agent workspaces do not load capture skills.
   Clean-room capture is the pixel-side secret mitigation. Text scanning cannot
   inspect pixels; OCR is deferred to Phase 2.
2. **Every artifact carries provenance:**

   `Provenance {producer_class: verifier | agent | daemon,
   producer_invocation_id, source_head_sha, verification_recipe_digest?,
   sensitivity_class, publish_eligible}`

   Only verifier or daemon artifacts produced under an approved recipe may
   enter `evidence_snapshot`. Agent images appear only as labeled claims.
   Agent-generated opaque files are never uploaded to GitHub automatically.
   Trusted policy computes `publish_eligible`; the agent never supplies it. A
   remediation head invalidates evidence from an earlier head unless the
   artifact is explicitly head-independent. The publisher verifies head binding
   before publication.
3. **The daemon treats images as opaque blobs.** It validates magic bytes, type,
   and size only. Server code never decodes an image; clients and GitHub render
   it.
4. **EvidencePublisher owns publication.** It lives in git/publish and follows
   effectively-once discipline through digest-derived names,
   check-before-create, and deterministic PR-section markers. It is deferred to
   1B because the first repository is deliberately non-UI (Section 11). Phase
   1A ships the artifact schema, provenance enforcement, and client rendering;
   1B adds external publication with the first evidence-bearing workflow.

## 6. Verification

Verification defines “done.” It is deterministic, engine-run, clean-room, and
controlled by a trusted recipe. It includes evidence capture. Its outputs are
run-bound artifacts cited by AttentionItems. False-ready tracking under Section
12 starts on day one.

## 7. Review policy

Independent error detection is the goal. Provider diversity is one way to
achieve it.

Routing comparisons use accumulated traces, including the 1B Claude shadow
arm. Shadow findings are recorded but never routed, and comparisons are
adjudicated blind where practical.

The classifier is never the sole safety gate:

- A raw shadow finding that claims critical or high severity and receives a
  low-confidence classifier score cannot disappear silently.
- It receives a second adjudication, deterministic or from a distinct agent, or
  becomes an AttentionItem.
- A credible critical or high shadow finding blocks ready status.

Some contamination is accepted. Freeside does not pass or fail based on routing
results.

## 8. Observability and optimization telemetry

Telemetry uses typed relational rows with stable join keys. Transcripts are
drill-down pointers, not the primary data model.

Each run records:

- stage and all governing digests;
- per-key rein preset or override provenance;
- driver, credential mode, egress profile, and operating mode;
- artifacts and their provenance;
- tokens and cost;
- active and elapsed clocks;
- attempts, review rounds, and yield;
- classifier samples and shadow results; and
- outcome and human decisions.

Defect issues reference their producing runs and may carry suggested fault
classes, closing the attribution loop.

Attention telemetry uses `AttentionDelivery` rows with honest status fields.
Open-to-decision time is the headline attention-latency metric; the Section 1
per-unit measure governs. Interruption-class rates are
tracked. Card drill-down opens are recorded per item and device, and sampled
decision audits record comprehension defects; both feed the Section 9
measurements. Passive baseline logging runs alongside Phase 1A. Usage is observed
telemetry, never asserted quota state.

Routing policy sits above the harness and is informed by task class, quality,
latency, usage, and cost, all drawn from these records. The provider
balancing I do by hand today, including usage-limit-driven switching, is
attention work: until routing absorbs it, it counts in the attention
accounting.

## 9. Comprehension

Comprehension is a first-class attention concern. Agents produce more text
than anyone reads, and unread text pushes the human out of the loop, so a card
is judged by whether it enables a fast, informed decision. The unit of
presentation is the decision card; presentation order is normative, not a
rendering preference.

### Layering

A card presents at most four layers, in this order; the per-item-type table
below governs which layers each type carries:

1. **The ask and the facts**: `requested_decision` plus deterministic card
   facts (verdicts, diff stats, counts, digests, timing). Daemon-produced
   only (Section 5.13).
2. **The summary**: what happened, why, and what remains open, with
   uncertainty preserved; absorbable in seconds. A labeled agent claim,
   present only where the card concerns agent work: a purely mechanical card
   (`system_health`, `blocked`) carries daemon facts alone.
3. **Evidence**: the `evidence_snapshot` packet (Section 5.15). Evidence
   precedes any long-form agent text.
4. **Drill-down**: full artifacts, full specifications and diffs, and
   transcript pointers (Section 8).

Three digests are required wherever their content appears:

- **Change summaries** for candidate diffs: what changed and why, rendered
  before the diff itself.
- **Plan altitude**: summaries and key questions high, detail lower. Altitude
  becomes enforced structure once plans become structured artifacts; until
  then it is prompt-level convention.
- **Digested review feedback**: findings grouped by disposition, dissent
  preserved. A digest never silently drops an unresolved or
  low-confidence-classified finding (Section 7).

### Presentation per Item Type

Actions and lifecycle live in Section 4; presentation is specified here.

| Item type | Leads with | Below |
| --- | --- | --- |
| `spec_approval` | The ask and a plan-altitude summary: intent, then key questions and decisions. A revision leads with the diff-from-last-reviewed summary and claimed addressals mapped to prior comments. | Full specification and full diff. |
| `review_diminishing_returns` | Daemon facts: rounds, finding-rate trend, cost so far. Agent claim: what remains. | Per-finding list. |
| `review_dispute` | The disputed finding with both positions side by side. Dissent is the content; it is never summarized away. | Code context and the full thread. |
| `execution_failure` | Daemon facts: failure class and failing step. Labeled diagnostic claim: probable cause. | Log excerpt and transcript pointer. |
| `agent_question` | The question as a labeled agent claim, self-contained: what is blocked and any enumerated options. Answering never requires the transcript. | The agent's supporting context. |
| `publish_blocked` | The trust rule that failed (daemon fact) and the approved alternate profiles. | The failing artifact or scan detail. |
| `ready_for_final_review` | The ask, a labeled change summary, and daemon verification verdicts with diff stats. | Digested review history, then the evidence packet, and the PR link last (navigation, not resolution). |
| `run_proposal` | One line per candidate: intent plus expected cost and scope facts. | Full proposal artifact; “start with changes” shows the revised-digest diff. |
| `system_health` | The diagnostic fact and the unattended capability it impairs. | Doctor output. |
| `blocked` | What is waited on and since when. Daemon facts only; no agent prose. | The waiting run's context. |

### Summary Provenance

Two content classes, two producers:

1. Every objective assertion in the ask-and-facts layer is a daemon-produced
   card fact (Section 5.13). A false or stale card fact is a mechanical
   false-ready (Section 12).
2. Every judgment summary (the summary layer, change summaries, plan
   altitude, digested feedback) is `producer_class: agent`, carried in
   `agent_claims`, and rendered as a labeled claim. It never enters
   `evidence_snapshot` (Section 5.15).

The same labeling covers any agent prose a card leads with (an agent's
question, a proposal's intent line): it renders as a labeled claim, never as
unlabeled authoritative text.

In Phase 1 the summarizer is the stage agent whose work the card concerns,
labeled with its `producer_invocation_id`. An independent invocation would
still be `producer_class: agent`, so independence buys no trust-class
upgrade, only resistance to self-serving framing; that risk is bounded by
composition instead: a summary may not assert a verifiable fact except by
citing the daemon fact or linking the artifact digest it compresses. Every
summary is itself a trust surface: producer identified, uncertainty and
dissent preserved, evidence linked.

A labeled summary contradicted by its cited evidence is a **comprehension
defect**: found by sampled decision audits and recorded in Section 8
telemetry. It is not a Section 12 false-ready (claims are claims), but
recurring contradictions promote summarization to an independent briefer
invocation (Section 5.13) blind to the implementer's rationale. The claim
contract currently carries labeled artifact references, not inline prose, so
the summary layer requires a renderable text carrier on the claim path; that
carrier is an explicit contract change that precedes the implementing work,
never an ad hoc rendering choice.

### Measurement

- Open-to-decision time per item type (median), against the passive Phase 1A
  baseline. It must not degrade as evidence volume grows.
- Reversal rate: decisions later reversed or work returned after approval.
- Drill-down rate: the fraction of decisions made without opening the
  drill-down layer. A health signal, never a target; it is trivially gamed by
  hiding detail.
- Comprehension-defect count from sampled audits: the target is zero;
  occurrences are recorded; the tolerance is not zero.
- Normalization by volume and risk: rates are compared against the period's
  workload, never as raw counts.
- Maintenance accounting: time spent operating and maintaining Freeside
  itself is recorded and netted against the return.

Speed counts only alongside correctness: an open-to-decision improvement is
claimed only with the reversal rate, the comprehension-defect count, and
Section 12 substantive false-ready held level or better.

### Document Change Discipline

Plan changes are gated by materiality:

- Wording and clarification changes are recorded but do not interrupt work.
- Material changes require the plan-change gate.
- The materiality rules are themselves control-plane policy.

Decision notes are selective and mandatory only for the classes listed in
`AGENTS.md`. The issue tracker, not decision notes, owns active work state.
Briefings and querying are deferred to Phase 3 and added only if demanded.

## 10. Operations and onboarding

Build the installer only after the underlying interfaces survive real use. The
`freesided` binary provides:

| Command | Function |
| --- | --- |
| `freesided setup` | Performs installation. Privileged steps run through a narrow elevation helper; the daemon never retains root. |
| `freesided onboard <repo>` | Creates the trust profile, attests effective authority for one-time human review, detects the verification recipe, and builds the project image. |
| `freesided doctor` | Checks conformance, the workspace-handoff gate, checkpoint encryption, backup age, artifact closure, and restore-test age. It runs on a schedule and files `system_health` items. |
| `freesided submit` | Starts a manually approved work item. |

The GitHub App uses the manifest flow, and its key lands directly in protected
storage.

Defaults are hosted ntfy, embedded SQLite, one configuration directory, and
`attended_dev` with honest isolation-class reporting.

Phase 1A exit targets, verified on a clean VM or spare machine:

- fresh machine to first run in under one hour; and
- repository onboarding in under thirty minutes with exactly one manual step.

## 11. Roadmap, build order, and coordination

### The first repository is deliberately boring

The first managed repository is **not Freeside**. Freeside often changes
control-plane paths, so it is the hardest possible starting case. It becomes the
bootstrap test after the path works.

Choose a small Go service or library with:

- read-only PR tokens;
- no OIDC, environment secrets, deployments, or self-hosted runners;
- no UI screenshot requirement;
- dependencies baked into the image;
- direct `go test` and `go vet` verification; and
- infrequent workflow or instruction-file changes.

### Phase 1A: the secure publish path, in three internal exits

Phase 1A proves the secure path from controlled input to published PR.

#### Open-source publication, accelerated

The entire monorepo, including owned prior revisions, is licensed under
AGPL-3.0-or-later and will become public after the licensing change lands. This
moves only the packaging and visibility decision forward from Phase 4 so the
project can use public-repository CI capacity. It does not advance Phase 4
features or create new support commitments. See
[`docs/decisions/0001-license-freeside-under-agpl.md`](decisions/0001-license-freeside-under-agpl.md).

#### 1A.0: control plane with fakes

Flow:

`fake run → AttentionItem → iPhone decision → second-device convergence →
conversation feedback → fake invocation → workflow transition`

Exit requires:

- all sixteen sync and device tests;
- idempotent command retry;
- kill-before and kill-after recovery with fakes; and
- no dependency on containers, Claude, publication, or backup complexity.

#### 1A.1: secure publication with a fake candidate

Flow:

`fake candidate → workspace handoff → gauntlet → clean verification → daemon
GitHub publication → ready item`

Exit requires:

- containment of malicious fixtures;
- blocking candidate automation-control and reviewer-instruction paths;
- verification bound to the exact recipe and head;
- effectively-once PR creation;
- successful checkpoint restore, with local-only acceptable; and
- completion in `attended_dev`; unattended operation is not required.

#### 1A.2: real unattended execution

Flow:

`Claude → proven credential mode → proven ward handoff → gauntlet → clean
verifier → audited publication → iPhone`

The run starts through `freesided submit` under manually configured unattended
preconditions.

Exit requires:

- green runner conformance, including the workspace-handoff gate;
- no undeclared credential in any workspace;
- several real work items completed without terminal intervention; and
- `setup`, `onboard`, and `doctor` packaging the proven manual operations and
  meeting the Section 10 targets.

#### Phase 1A build order

1. Domain, synchronization, devices, and fakes.
2. Clients and the sixteen permanent tests.
3. Export, gauntlet, and verifier with fake candidates; artifact store with
   checkpoint and provenance rules.
4. Publication, reconciliation, and kill tests.
5. ward and its handoff gate, then the Claude driver.
6. Real work items.
7. `setup`, `onboard`, and `doctor`.
8. Phase exit.

Investigate the workspace-handoff gate early and in parallel because it is the
largest runtime unknown. It blocks only 1A.2, never 1A.0 or 1A.1.

### Phase 1B: the useful workflow

Phase 1B turns the secure path into the useful daily workflow:

`labeled issue → daemon-fetched research → elaboration → spec approval →
implementation → gauntlet → PR under a trust profile → checks →
control-plane-triggered Codex
review → yield-driven remediation and pattern sweeps → diminishing-returns or
dispute item → ready-for-final-review with yield history → human GitHub merge`

Phase 1B adds:

- the elaborator and research fetcher;
- intake scanning;
- ReviewSource freshness verification and automatic re-review testing;
- finding classification with sampled accuracy and second adjudication;
- convergence policy and the shadow arm;
- provenance-gated EvidencePublisher;
- experimental `max_parallel_executions` per auth identity, visible to
  scheduling; and
- the run timeline screen.

Use the real backlog immediately.

Exit requires:

- no patrol of agent windows;
- no manual polling;
- productive review rounds that run without prompting;
- consolidated low-value interruptions;
- approvals decidable from the phone;
- useful, correct work per unit of attention materially above baseline;
- a low exceptional-interruption rate; and
- false-ready performance within Section 12.

### Implementation coordination (building Freeside with agents)

Contracts and fakes coordinate implementation. CI keeps lanes honest.

| Wave | Shape | Work |
| --- | --- | --- |
| **0: foundations** | Serial | Module, dual-platform CI, domain package, schema and migrations, outbox, interfaces, fakes, and provisional API schema. Domain and migration PRs are exclusive. Shared-interface work is `kind:contract`. |
| **1: subsystems** | Parallel lanes | signet, gauntlet, publish, ward, and the saddle pair. |
| **2: convergence** | Integrated | Workflow engine, real driver, end-to-end fakes, and real work. The **spine** owns integration and contract adjudication. |

Review bandwidth limits parallel width. Every wave ends with a fresh-context
adversarial review by an agent given only the repository and its documents,
never this design history. `AGENTS.md` defines the issue protocol. The 1A
backlog also serves as elaborator fixtures.

### Phase 2: breadth and hardening

Expand beyond the first constrained path:

- a second repository and workflow shape;
- scan initiators and chaining;
- a local Codex driver, if useful;
- `api_key_isolated`;
- full failure-injection and restore drills;
- generalized but bounded CI-audit tooling;
- richer classification and risk-classified cards;
- webhooks if latency hurts;
- APNs;
- registry-capable egress profiles;
- `provider_web_read` where explicitly accepted;
- OCR image scanning if warranted; and
- the Linux deployment matrix if wanted.

### Phase 3: comprehension and interaction

Add ACP interactive attachment, best-effort resume, material plan-change gates,
briefings, usage display, evidence-informed routing, WIP and initiative views,
and mature `auto_start` behavior.

### Phase 4: generalization

After three real workflow shapes, consider a pipeline DSL. Add more agents and
skills, a macOS runner class, App Intents, widgets, Live Activities, and voice.

## 12. Exit criteria definitions

| Criterion | Definition | Tolerance |
| --- | --- | --- |
| **Mechanical false-ready** | A card asserted an objectively stale or false fact. | Zero. |
| **Substantive false-ready** | Automation missed a material in-scope failure it should reasonably have caught. | Zero critical or high misses; record lesser misses. |
| **Safety failure** | Any invariant below fails. | Any occurrence blocks unattended use. |

Safety failures are:

- a workspace obtains a GitHub write credential;
- an agent reaches a privileged host service;
- output escapes either gauntlet channel;
- untrusted PR code receives privileged CI authority, including secrets,
  writable tokens, or OIDC, without an explicit gate;
- candidate automation-control changes reach publication through the ordinary
  workflow;
- a stale mobile decision takes effect;
- a crash produces uncontrolled duplicates or advances a workflow twice;
- concurrent work corrupts provider authentication;
- control-plane content from an implementation head influences later
  execution;
- reviewer instructions from a candidate branch govern that candidate's
  review;
- Freeside disregards a known credible critical or high shadow finding; or
- an unencrypted checkpoint replicates off-host after encryption becomes
  required.

**Kill criterion:** stop if agents work acceptably in the manual workflow but
Freeside does not materially raise useful, correct work per unit of attention.
Elaborator weakness alone is not a kill criterion.

## 13. Decisions log

Record material changes here by revision, with the decider in parentheses.

- This section contains only the current revision.
- When a new revision lands, move the outgoing items to
  `docs/history/decisions.md`.
- The history contains every revision, including revisions superseded before
  commit.
- Update the history in the same PR as the plan revision so they cannot drift.
- On first re-litigation, promote the decision to a `docs/decisions/` ADR that
  cites its history entry.

Revision 15:

1. **The canonical thesis grants autonomy.** §1, README, and AGENTS.md now
   read "grants agents the autonomy to turn work items into evidence-backed
   pull requests", superseding "turns a software work item into an
   evidence-backed pull request". (User; devlog
   2026-07-20-2331-plan-alignment-harvest.md; #192, #208.)
2. **The objective is a positive return, and the success claims are
   necessary gates.** §1's measure is restated as useful, correct work worth
   more than the attention, maintenance, money, and risk it costs; claim 1
   gains its numerator (work per unit of attention rising against a
   passively logged, normalized baseline); claim 3 is verified by
   conformance and adversarial tests, never read off telemetry; passing all
   four is named necessary, not sufficient. §9 Measurement adds
   normalization by volume and risk and maintenance accounting; the §11
   exit criterion and §12 kill criterion align to the same per-unit
   measure, and §4/§8 subordinate open-to-decision time to it as the
   headline attention-latency metric. (User; devlog
   2026-07-20-2331-plan-alignment-harvest.md; #208.)
3. **The auto-merge door stays deliberately open.** §2 non-goal 1 drops
   "never auto-merges": code review and merging stay on GitHub, human merge
   is the current accountability checkpoint, and whether narrow,
   risk-bounded classes of change ever earn automatic merge remains an open
   question, adopting the owner decision recorded on PR #192. (User; devlog
   2026-07-20-2331-plan-alignment-harvest.md; #192, #208.)
4. **Oversight and standing-grant promotion become stated principles.** New
   §3.5 states oversight as non-optional and deliberately frictionless;
   §3.1 gains the promotion criteria: low risk, stable preconditions, and
   bounded downside, never repetition alone. (User; devlog
   2026-07-20-2331-plan-alignment-harvest.md; #208.)
5. **Durability names its fallback.** §5.9: anything that cannot be safely
   retried waits for me. (User; devlog
   2026-07-20-2331-plan-alignment-harvest.md; #208.)
6. **Routing inputs are named and manual balancing is accounted.** §8
   states routing policy is informed by task class, quality, latency,
   usage, and cost, and counts today's manual provider balancing in the
   attention accounting; §2 non-goal 5's deferrals open on recorded
   outcomes. The intro's "stops opening pull requests" drift claim was
   verified against §5.5 and needed no edit. (User; devlog
   2026-07-20-2331-plan-alignment-harvest.md; #208.)

## 14. Risks

| Risk | Current response |
| --- | --- |
| Provider credentials in `subscription_contained` | Document the residual; enforce egress floors; let the daemon fetch research for the most exposed stage; provide `api_key_isolated` as the escape. |
| CI privilege crossing | Attest effective authority; block candidate automation changes; fail closed on drift; prohibit the daemon host as a runner. |
| Reviewer-instruction poisoning | Treat instruction paths as control-plane content and block candidate changes in the ordinary publication path. |
| **Workspace-handoff uncertainty** | Resolved by the workspace-handoff spike: the strong class is declared and conformance-gated (Section 5.7); the same-VM fallback is refuted by execution, never implemented or declared. |
| Codex cloud review as a load-bearing dependency | Use the shadow arm to dry-run the hedge. |
| Classifier mislabeling | Preserve immutable raw findings; require second adjudication for the safety case; enforce ceilings. |
| Subscription-terms drift | Keep it as an explicit operating risk. |
| Apple container immaturity | Prove actual runner capabilities and retain honest fallback classes. |
| Vendor CLI churn | Pin tooling in golden images and verify its contracts. |
| Review saturation | Bound work by review bandwidth and use yield policy. |
| Interruption creep | Measure exceptional interruptions and treat a rising rate as a defect. |
| Setup and upkeep burden | Make operational simplicity a Phase 1A exit criterion. |
| Synchronization complexity creep | Keep the daemon authoritative and clients disposable; test the sixteen permanent cases. |
| Image handling | Enforce provenance and opaque-blob handling; defer OCR to Phase 2. |
| Backup confidentiality | Require encryption policy and exclude credentials by default. |
| Large Phase 1A scope | Order it into three internal exits. |
| Reviewer monoculture | Require a fresh-context adversarial review at every implementation wave exit. |
| Prompt injection, the organizing threat | Keep write credentials out of workspaces; prove handoff; import through the out-of-process two-channel gauntlet; use trusted overlays; block automation and instruction paths; enforce egress floors; fetch research through the daemon; gate irreversible actions; use budgets and brakes. |

## 15. Naming and references

### Product and subsystem names

| Name | Meaning |
| --- | --- |
| **Freeside** | Proper noun at `freeside.ai` and `github.com/freeside-ai`. Capitalize it wherever prose permits. Lowercase only where required by the medium, such as URLs and the daemon name. |
| **Free as in Bird** | The organization. |
| **an agent control plane** | Category line. |
| **the harness runs the agent; you hold the reins** | Tagline. |
| **ward** | Runner, handoff, and safety boundary. |
| **signet** | Attention and approval service. |
| **gauntlet** | Export, hostile import, clean verification, and evidence path. |
| **freesided** | Daemon name. |
| **rein** | Brand and policy vocabulary only. |

Subsystem names follow the binding-and-summoning register: rare,
single-metaphor words with ordinary surface meanings. Code uses functional
names.

### Visual identity

- Light surfaces are **Freeside**: vellum ground and bronze accent.
- Dark surfaces are **Straylight**: umber ground and tawny accent.
- Appearance follows the viewer's system setting. The distinction assigns
  meaning, not audience.
- Semantic colors never borrow the accent. Green remains success and go.
- The mark is **the signet box**, a plain chambered box whose inlaid dividers
  suggest the maker's initial.
- Identity assets never depict the agent.

The full identity system and rejected alternatives are in
`devlog/2026-07-17-0050-brand-register.md`.

### Coordination names

Coordination vocabulary sits outside the subsystem register. A lane takes a
subsystem name where one exists. The client lane is informally the **saddle**.
The integration role is the **spine**, a role rather than a territory.
`AGENTS.md` owns the canonical lane glossary.

### Reference shelf

- Anthropic devcontainer, Agent SDK, and credential documentation;
- OpenAI Codex SDK, sandbox design, and cloud-review documentation;
- GitHub Actions security-hardening documentation, including token
  permissions, OIDC, and `pull_request_target`;
- Apple container documentation and issue tracker;
- SQLite online-backup and WAL-durability documentation;
- Litestream;
- Antfarm, Nimbalyst, Conductor, and Gas Town/Beads as cautionary references;
  and
- `agentclientprotocol.com` for Phase 3.
