---
title: Freeside Project Plan
revision: 30
status: active
phase: 1A
updated: 2026-08-11
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
7. Independent review and yield-driven remediation run within explicit
   emergency brakes.
8. The daemon publishes the verified, reviewed candidate under an audited
   GitHub trust profile.
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

## 4. The Attention Model

### Core records

**AttentionItem** contains:

`id`, `project_id`, `subject {subject_type: run | proposal_batch | project |
system, subject_id, run_id?}`, `type`, `priority`, `reason`,
`requested_decision`, `evidence_snapshot`, `agent_claims`, `artifact_digests`,
`pr_head_sha`, `pr_reference? {repo, number}`, `item_version`,
`interruption_class`, `conversation_id?`, derived timing aggregates,
`expires_when`, `review_recovery_binding?`,
`codex_reenrollment_recovery_binding?`, `review_configuration_recovery?`, and
`status`.

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

### Phase 1 Item Types and Actions

Approval is not a universal action.

| Item type | Available actions and behavior |
| --- | --- |
| `spec_approval` | Approve, request changes, discuss, or stop. Render the full specification. A revision shows the diff from the last reviewed version, prior comments, and claimed addressals. |
| `review_diminishing_returns` | Finish now; apply the current batch and finish; continue under specified policy; or turn a recurring preference into a project-policy proposal PR. It never mutates policy directly. |
| `review_dispute` | Adjudicate the finding, discuss, or stop. |
| `review_contradiction` | Recover only the exact persisted contradiction named by the card, or leave it parked. The card renders the bound run, invocation, round, base SHA, head SHA, and immutable failure-body digest; recovery preserves the original failure evidence. |
| `review_configuration` | Adopt the review configuration (`adopt_review_configuration`), discuss, or stop. The run is parked, not terminal: adoption authorizes an operator-approved, review-configuration-only profile supersession of exactly the parked failure named by the card's binding, resolved at decision time as the repository's currently activated revision and re-gated on every read; stop concludes the run as a configuration failure always did. The card renders the same bound coordinates as `review_contradiction` plus the superseded profile digest. |
| `execution_failure` | Retry; retry with a predefined policy-allowed capability manifest; discuss; or stop. |
| `agent_question` | Answer and retry, answer without retry, or stop. |
| `publish_blocked` | Rerun trust evaluation, choose an approved alternate publication profile, inspect the trust failure, or stop. |
| `ready_for_final_review` | View the PR (navigation, not resolution), return work to the agent with feedback, `mark_seen`, dismiss, or stop. It stays active until Freeside observes merge or close, work is returned, or the item is dismissed. |
| `run_proposal` | Start, **start with changes**, decline, or snooze. “Start with changes” creates a revised proposal artifact, supersedes the original item, creates a new item version, and starts the run from the exact revised digest. It never uses unversioned ad hoc parameters. Proposals are grouped under `proposal_batch_id` with per-candidate decisions. |
| `effect_proposal` | Approve, **approve with changes**, decline, or snooze a proposed effect from the Section 5.13 registry (added in 1B with the registry; first instance: follow-up issue filings in 1B.1, with proposed watches following once their schedule kind lands, Section 5.16). Approval binds to the proposal artifact digest; “approve with changes” creates a revised proposal artifact and supersedes the item, exactly as `run_proposal`'s start-with-changes. `run_proposal` remains its own type. |
| `system_health` | Acknowledge, run doctor, stop unattended operation, or, on the notice a stop raises, resume unattended operation. A revoked Codex identity marker additionally offers resolve re-enrollment (`resolve_reenrollment`) only after it carries the immutable binding for its exact latest verified re-enrollment operation; the command revalidates that operation and marker occurrence in the transaction that resolves the item. Acknowledge means seen, never resolved, and cannot clear revoked identity. Every item declares an immutable posture: `blocking` preserves the admission gate until the diagnostic clears, unattended operation is explicitly stopped, or a validated configuration supersedes it; `advisory` remains open and visible without blocking unrelated unattended admission. A stop is a durable operating transition: only the explicit resume reopens unattended admission, and a restart alone never does. |
| `blocked` | Consolidates external waits that exceed Section 5.12 thresholds. It is read-only. |

Section 9 governs each type's presentation: what its card leads with and what
layers below.

### Lifecycle Rules

- Approvals bind to artifact digests and the PR head SHA. Changed inputs
  invalidate them.
- Retries supersede failures.
- Resolutions are transactional and version-checked.
- A stale submission receives a conflict and the replacement item.
- Notifications are read-only hints, never authority.
- Fault-class capture is suggested, can be corrected with one tap, and may
  remain unknown.
- WIP caps apply to runs and initiatives. The all-work view is Freeside's
  deterministic initiative projection (Sections 5.18 and 11); GitHub Projects
  no longer serves that role (overturned, revision 25).

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
| **GitHub** | Owns source, issues, PRs, reviews, checks, and merge. Native Codex review, when observed, is recorded as best-effort extra evidence (Section 7). Freeside reconciles each active resource independently; there is no global cursor. |
| **Event inbox** | Accepts reconciled GitHub state, intake scans, cron events, and manual events idempotently. |
| **Workflow engine** | Runs code-defined state machines using policy from configuration. It records the resolved rein-policy digest and separate active, elapsed, and waiting clocks for each run. |
| **Scheduler** | Owns the closed union of durable schedule kinds, fire-time validation, and transactional redelivery (Section 5.16). |
| **signet** | Owns AttentionItems, deliveries, conversations, synchronization, device pairing, and ntfy integration. |
| **Research fetcher** | Retrieves immutable, digest-addressed research artifacts for agents. |
| **StageDriver** | Runs bounded local agent batch jobs: Claude in 1A, joined by Codex in 1B (Section 11). A permanent fake supports deterministic tests. |
| **ReviewSource** | Runs the Freeside-invoked review stage; the first production binding is a local Codex invocation (Section 7). A permanent fake supports deterministic tests. |
| **Finding classifier** | Adds versioned annotations to immutable raw review findings. |
| **ward** | Provides runner capability classes, workspace-handoff capabilities, per-stage egress, operating modes, and conformance checks. |
| **gauntlet** | Runs out of process. It normalizes export, treats import and evidence as hostile, builds a fresh checkout, and starts clean verification and evidence capture. |
| **Git/publish** | Owns all GitHub credentials, deterministic external identities, invocation reconciliation, and, in 1B, the EvidencePublisher. |
| **Store** | Uses SQLite with inbox/outbox and a content-addressed artifact store. Section 5.10 defines encrypted checkpointed backup. |
| **Sync API** | Serves atomic snapshots with revision, epoch, and invalidation semantics. |
| **Freeside app** | Provides the SwiftUI macOS and iOS inbox, decision detail, and run timeline using platform-protected caches. |

### 5.2 The Daemon and Its Supervisor

`freesided` is a single static Go binary. A supervisor keeps it running; the
daemon never supervises itself, and launchd/systemd knowledge never enters
`daemon/`: unit files and their registration live with the install tooling
and the operator app.

**Supervision modes** (decider: user; revision 27):

- **Mac-first single-operator (Phase 1):** a per-user launchd LaunchAgent in
  the operator's login session, registered by the Freeside Mac app through
  `SMAppService` from a plist shipped in the app bundle, with `KeepAlive`.
  The app is installer and trigger; launchd is the supervisor. This path has
  no privileged step: the daemon drives Apple `container`, per-user tooling,
  and the operator account is the isolation boundary (state and credentials
  stay `0700`/`0600` under it, so other accounts cannot access them). Cost,
  accepted for Phase 1: the daemon lives in the login session, so unattended
  operation assumes a logged-in operator, a bound already true of the
  terminal-launched process this replaces.
- **Hardened (multi-user or server hosts, deferred):** a dedicated-user
  LaunchDaemon or systemd unit installed through the Section 10 elevation
  helper, for boot-time start, logout survival, and operator isolation.
  Retained as the end state, not scheduled in Phase 1.

**The daemon never runs as root.** One-time privileged work, such as user
creation and LaunchDaemon installation on the hardened path, lives in a
narrow elevation helper. Privileged services bind only to loopback or
Tailscale.

**Exit discipline.** Every deliberate stop is durable and in-process; the
process exits only involuntarily or to be restarted. Classification of
today's fatal-channel writers and exit paths:

- **Durable stop** (close unattended admission through the Section 4
  transition, file `system_health`, keep serving reads; only explicit
  resume reopens admission, and a restart never does):
  - store I/O and correctness failures in any long-running loop (the
    workflow reconcile loop, a scheduler pass, active-resource enumeration
    or commit): an invariant on durable state recurs on restart, and a
    respawn loop would hide it;
  - local backup maintenance failure: persistent disk or encryption damage
    (Section 5.10);
  - a doctor or janitor pass failure with a local or definitively
    classified cause (an unreadable operational source, revoked or broken
    GitHub App authority): health can no longer be asserted, resolving the
    doctor source-error posture deferred by the operational-command
    packaging decision;
  - an externally caused pass or lane failure once persistence is
    established (a consecutive-failure threshold the implementation unit
    sets). A transient external failure alone never stops or exits: it
    retries on its cadence or backoff and is recorded.
- **Restart-safe exit:** a post-bind HTTP serve fault. Without the API
  surface the daemon cannot serve even read-only state, and a fresh bind
  plausibly clears the fault.
- **Process exit (involuntary):** panics and invariant violations, and
  startup failures from flag validation through migrations and the initial
  doctor pass. A pre-store startup failure cannot record a durable stop by
  construction. The supervisor restarts these under its throttle;
  crash-looping is visible as `started_at` churn on `/health` and, from
  1B.1, as the external probe's alarm.

After this contract the daemon's fatal channel carries only the two exit
classes; every durable-stop condition is consumed before reaching it.

**Restart policy.** Restart-always with the platform throttle. This is safe
only because every deliberate stop is in-process and durable: a restart can
resume only work the contract says is safe to resume, and Section 4's rule
stands (a restart never reopens unattended admission).

**Stop.** Supervisor stop is SIGTERM with an effectively unlimited exit
timeout, because credential-lease teardown is unbounded by design (decider:
user; the stop-wait fork closes on the unlimited side: any finite grace
recreates SIGKILL-mid-lease, and a bounded credential-safe teardown is
deferred hardening, not a tunable). SIGKILL and power loss remain
crash-equivalent, covered by kill-recovery.

**Liveness and address.**

- Unauthenticated `GET /health` returns exactly `{status, version,
  started_at}`: liveness, version-skew detection, and crash-loop evidence (a
  moving start time under a supervisor). Everything richer stays on the
  authenticated surfaces (Sections 4 and 5.14); the route widens what an
  unpaired caller learns by nothing else.
- Under supervision the listen address is explicit fixed loopback
  configuration in the unit file, never the ephemeral default; bare
  foreground runs keep `127.0.0.1:0`.
- The daemon durably publishes readiness (`{api_url, pairing_code}`, today's
  one-shot stdout line) to a `0600` runtime file in the state directory on
  every start: under a supervisor no terminal exists to read stdout, and
  same-user file readability is the same trust boundary as today's
  terminal. The stdout line remains for foreground runs.
- The away-from-host liveness probe stays outside the process (Section 5.16
  keeps process heartbeats as plain tickers): an external probe polls
  `/health` and notifies over ntfy on unreachability or crash-loop, landing
  in 1B.1. The local surface is the Mac app's menu bar presence (Section
  10).

Storage and CI invariants:

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

- one local driver, **Claude**, in 1A; a second local driver, **Codex**,
  joins in 1B as an execution capacity hedge (Section 11), blocked on its
  pre-adoption gates (#401);
- one production review source, a **Freeside-invoked local Codex review**
  binding (Section 7); GitHub-native Codex review is best-effort extra
  evidence and never satisfies the review requirement; and
- permanent fakes of both interfaces.

The 1B shadow arm runs a fresh-context Claude review against the same head.
Freeside records its findings but never routes them. It is the dry run for
promoting a selectable Claude ReviewSource (#397).

**Freeside invokes review directly** (decider: user; revision 25, replacing
"one primary review source, CodexGitHubReview" and the former
control-plane-triggered review step). The 2026-07-31 live-run falsification
(#427) showed GitHub-native
Codex review has no App-visible trigger path: automatic review never starts
for App-authored PRs; an App-authored `@codex review` request fails at
account resolution; reviews are head-bound, so every remediation push needs
another valid trigger; and a human-PAT trigger binds unattended operation to
one person's account linkage, token lifecycle, quota, and attribution,
rejected as a production dependency. Each review pass is therefore a
control-plane invocation reconciled by `invocation_id` like any other stage.
Invocation failure closes safely under Section 7's classification. Nested
`AGENTS.md` guidance is documented Codex behavior. Automatic re-review of
remediation heads is a standing 1B integration test. The Claude setup token's
inference-only scope is contract-tested against the pinned CLI.

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

**Credential delivery under `subscription_contained` (Claude).** The setup
token lives as a single read-only file on a per-identity credential volume
authored or replaced by a daemon-owned enrollment transaction while no
execution can use that identity. Phase 1A mounts no per-identity writable
Claude state. Ward instead supplies a read-only clean
`CLAUDE_CONFIG_DIR`, a narrow per-invocation continuity mount, and
per-launch scratch state (§5.8); none is credential state or reusable by a
different invocation. Serializing writers on one shared directory would not
isolate a later invocation from settings or hooks an earlier writer
persisted.

The daemon-supplied launcher argv reads the token file into the vendor's
environment variable at exec; the writer's spec environment carries no
credential and the driver's fixed environment rides the launcher, not the
spec. The token value never appears in argv text, inspect reports, ward
journals, or driver state; the credential mount path and the launcher text
are the only durable traces. The credential remains ambient in the writer
process tree (children inherit it; the mounted file is readable at agent
privilege): that is the documented residual this mode accepts, backstopped
by `provider_only` egress and export secret scanning. Vendor behaviors this
path depends on are pinned-CLI empirical contracts, re-proved on every CLI
version bump, not vendor-documented guarantees; the work unit's decision
note enumerates them.

Secret scanning is intentionally described as **best effort**. It covers
supported text formats. Size, type, provenance, and publication controls govern
opaque artifacts. Universal detection across arbitrary encodings and images is
impossible; Section 5.15 records the image residual.

Every stage also receives an egress profile from control-plane policy. Profiles
sit above the credential-mode floor and represent different risk classes:

| Profile | Access and risk |
| --- | --- |
| `provider_only` | Default. The writer has one host-only network: direct external and guest-DNS paths are absent, and the provider API is reachable only through the daemon's allowlisting proxy. The host gateway remains a network neighbor. The production API is isolated by its loopback-or-Tailscale-owned listener gate; every other host service needs its own declared binding policy, and the ward proxy is the intentional agent-reachable exception. |
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

`AuthIdentity {auth_store_mutation_lease, auth_store_volume,
max_parallel_executions, refresh_strategy, supports_read_only_auth_snapshot}`

1. Auth-store mutation, including refresh, login state, configuration writes,
   and store replacement, is serialized per identity.
2. Inference execution has a separate parallelism limit. 1B establishes that
   limit experimentally and exposes it to WIP scheduling.

If only one execution is safe, scheduling shows that constraint instead of
hiding it in a lock. API-key fallback is always available. Vendor tooling stays
native and unmodified.

### 5.5 The CI Trust Boundary

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
  allow_reusable_workflows: false
  allow_package_publishing: false
  allow_artifact_consumers: false
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
  review: {mode: freeside_invoked, config_digest: sha256:...}
                                             # Freeside-owned production
                                             # review stage (Section 7);
                                             # historical auto and
                                             # framework_triggered profiles
                                             # require owner re-approval
                                             # under the versioned digest;
                                             # native review is observation-
                                             # only evidence
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

Publication authenticates as a per-user agent principal under Section 10's
GitHub App identity model. Every trusted registration is bound by numeric App
ID to that principal; App names and slugs are display metadata, never trust
inputs. Installation-token minting fails closed unless the target repository
is onboarded and trusted and the specific installation is recorded as known
for that repository under a known registration bound to the principal,
regardless of whether that registration uses the public default or the private
work-account posture. Every worker-bound publication mint request supplies
`repository_ids` containing only the target repository's canonical numeric ID
and narrows `permissions` to the profile-approved operation. The response is
untrusted until Freeside verifies that it names exactly that repository, grants
no permission beyond the approved effective set, includes every permission the
operation requires, and has the expected bounded expiry. A missing or
mismatched field discards the token before any worker can receive it and fails
closed.

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
verified candidate ──▶ required review pass (Section 7; pre-publication,
   findings drive remediation and reverification)
reviewed candidate ──▶ git/publish ──▶ GitHub PR (under trust profile)
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
- `supports_workspace_snapshot`;
- `supports_networkless_export`; and
- `supports_enforced_provider_egress` — the proven writer-egress boundary:
  the agent workspace reaches only the declared provider authorities through
  the daemon's CONNECT proxy on a host-only network, with DNS and direct
  connections refuted by live in-writer probes. It attests the enforcement
  mechanism, distinct from the *requested* egress profile (Section 5.4).

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

#### Writer Outcome Authority

Apple container 1.1.0 exposes no process exit status: the runtime models
only running and stopped, so a stopped writer is indistinguishable from a
crashed one at the inspection surface. The exit status's value is
agent-controlled under every delivery mechanism (an agent process chooses
its own exit code), so exit status is crash and refusal detection, never
adversarial proof; acceptance authority stays with output verification and
the export gates. What the gate trusts is freshness and delivery: it
authors a per-invocation nonce, journals it before start, and passes it in
the launcher argv; the launcher's final act writes the nonce and the CLI's
exit status to a fixed evidence path and exits with that status.

The write-once `ExecutionOutcome` is canonical terminal authority for a
failed, canceled, or lost invocation; `ExecutionExport` is canonical
completed authority. The ward journal is the crash bridge and cleanup
authority, not a competing execution result. In addition to the nonce and
`WriterComplete`, its open record can carry a durable
`CancellationIntent {reason, recovery_capture_required}`, an optional
validated nonzero `WriterFailureStatus`, and an optional
`RecoveryCaptureDigest`; its terminal outcomes include `completed`,
`failed`, `canceled`, and `loss`. After restart as well as in the live path,
the driver idempotently maps `completed` to `ExecutionExport` and every
other closed outcome to the corresponding `ExecutionOutcome`.

`WriterComplete` is the successful release predicate: the writer is stopped
or proven absent, the marker is present with the journalled nonce and status
zero, and the live daemon observed the proxy healthy throughout the writer's
life. Only that live daemon may set the bit after all four facts hold.
Recovery never reconstructs the lost proxy-health observation from a zero
marker.

A daemon-commanded cancellation durably records `CancellationIntent` before
issuing stop, and that intent takes precedence over marker classification.
After proving quiescence and satisfying any capture requirement, ward
completes teardown and closes `canceled`; the driver then converges
`ExecutionOutcomeCanceled`. Cancellation never makes the partial workspace
a publication candidate or clean-verifier input. For graceful portable
handoff, the intent sets `recovery_capture_required`; after quiescence,
§5.10's normalized encrypted workspace capture completes and its verified
digest is durable in the ward journal before cleanup can erase its source or
the ward can close `canceled`. Restore exposes that recovery object only as
untrusted input to a new attempt.

For an uncommanded stop, a matching nonzero marker is terminal failure.
Ward validates the nonce and status, persists `WriterFailureStatus` before
any cleanup can erase the marker-bearing workspace, completes teardown,
and closes `failed`; export is refused even when partial edits exist.
Recovery dispatches durable amendments before inspecting marker state:
`CancellationIntent` takes first precedence, then an existing
`WriterFailureStatus` remains the failure classification while recovery
finishes teardown and closes `failed`, even when cleanup already erased the
marker. Marker classification runs only when neither amendment exists. After
stopped or absence proof, a missing, malformed, or mismatched marker
classifies loss; ward completes teardown before closing `loss`. A matching
zero marker permits recovery adoption only when `WriterComplete` was already
durable: recovery revalidates the surviving marker and absence facts but
never synthesizes the bit. Zero without that bit classifies loss and follows
the same teardown-before-close ordering; nonzero closes `failed` even if a
stale or legacy completion bit exists. If any required amendment, capture,
teardown, or close fails, the journal remains open for recovery to retry.
The writer's transcript is evidence, never an outcome signal: the pinned
CLI's terminal stream event can report success alongside an authentication
error, and only the exit status distinguishes them.

#### Golden Agent and Project Images

Golden images split into reusable agent bases, which carry a pinned vendor CLI,
and project images, which add one managed repository's verification
dependencies. A project image is still an agent image and must pass the same
ward gate.

The ward owns the realized launch shape; the contract is not the blanket
absence of every OCI metadata key. No agent base or project image may contribute
or inherit `ENV`, `WORKDIR`, `ENTRYPOINT`, `CMD`, `USER`, or `VOLUME` metadata
that changes the required shape. Apple `container` 1.1.0 merges image
environment and working-directory metadata into the created container and
honors the other launch metadata. Before use, the image-side probe creates a
container with a supplied command and no environment or mounts, then requires
exactly the fixed `PATH`, `/` as the working directory, that supplied command,
no mounts, no SSH forwarding, and no publications. Ward repeats the
runtime-exposed checks against the full daemon specification before credentials
enter or the container starts. The current inspect report exposes no user
field, so source and build validation must also require the runtime-default root
user for the root-owned workspace.

Inherited base metadata such as the fixed `PATH`, or a default `CMD` that the
daemon-supplied command replaces, is acceptable only when the probe proves the
required realized shape. A derived project image is checked again; a compliant
base does not make the extension trusted.

A reusable builder consumes the canonical repository identity, an exact commit,
and the trusted verification recipe. It derives a project image from the
approved agent base, bakes the dependency closure and tool configuration as
files, records the repository, commit, recipe, and base-image provenance, and
returns a digest-pinned image reference. Per-project image definitions and
copied dependency manifests do not live in the Freeside control-plane source.
A changed dependency manifest therefore rebuilds the runtime artifact without a
Freeside source change.

The declared verification recipe runs verbatim with networking disabled. The
builder proves both that this clean run passes and that the baked dependency
material is load-bearing: a negative probe masks that material and must fail by
attempting the registry or network access the positive run did not need. A
candidate that changes the dependency closure beyond the baked inputs fails
loudly and requires a new reviewed project image; verification never fetches a
missing dependency.

Every runnable agent-base and project-image reference is
registry-resolvable `name@sha256:<digest>` and is admitted by digest, never by
tag. A local content-store digest without a registry identity is not a runnable
reference on Apple `container` 1.1.0. Where that runtime also cannot use a
locally built `name@digest` as a build base, the builder may use a tag only for
that build-time hop after verifying its digest, and must record the exact base
digest in the derived image. The image supplied to ward remains a
registry-resolved digest reference.

#### Operating modes

| Mode | Requirements and limits |
| --- | --- |
| `attended_dev` | May use a weaker runner class. Disables `auto_start`, automatic publication, and unattended escalation. Reports its isolation class honestly. |
| `unattended` | Requires successful conformance including the handoff gate, a valid trust profile, an approved credential mode, all runner minimums including the proven `supports_networkless_export` exporter boundary and the proven `supports_enforced_provider_egress` writer egress boundary, current backup health including encryption status, and no blocking `system_health` item. |

Run the full conformance suite at startup, after configuration changes, and on
the doctor's schedule. Run a lightweight probe before every unattended job.
Golden images pin CLI versions. Workspaces use VM-local disk.

Each completed, generation-current full pass durably records the backend's
proven class and capabilities with a monotonic proof generation and time; a
beginning recheck first durably supersedes the previous declaration, so it
cannot admit while the recheck is pending; a
failed pass records the failure and invalidates the declaration, and an
unpersisted proof is not a proof (the pass fails and the capabilities are
never declared: publication follows the durable append). A recorded declaration can never exceed its class's registered
provable ceiling, and an unattended admission is refused at the write
boundary unless its capability snapshot sits within the named backend's
current durable conformance record; a conformance lapse closes new admission
without making recorded history unreadable. The declaration a new unattended
admission is gated against is therefore reconstructed from persisted
conformance evidence, never from transient process state.

Phase 1A.2 exception (owner decision, 2026-07-26): unattended admission may
waive the encryption-state dimension of backup health, and only that
dimension — checkpoint currency, artifact closure, and restore-test age
still gate admission, evaluated against the local owner-only checkpoint
(§5.10) — only while all of the following hold, checked mechanically at
admission: an explicit operator-set `backup_encryption_waiver` naming the
exact trusted numeric repository ID it covers is present in the daemon
configuration; the run targets exactly that repository, verified against its
trusted binding rather than a positional notion like "the first onboarded";
and
the daemon does not yet carry the encrypted, digest-bound `BackupCheckpoint`
(a build that carries it rejects the waiver as invalid configuration,
retiring the exception). Admission without the waiver fails closed as
before, and every admission under it records the waiver in the run's audit
record and surfaces the degraded posture as a `system_health` item whose
blocking state the validated waiver configuration supersedes (the §4
supersession rule), keeping it visible without blocking the subsequent
admissions the waiver exists to permit. The encrypted checkpoint must land
before the Phase 1A exit; the doctor (§10) packages its encryption check.

Bootstrap exception: SwiftUI work is exempt until a macOS execution class
exists.

### 5.8 Control-Plane Trust

The following are control-plane content:

- workflow configuration;
- prompts and policy;
- egress and trust profiles;
- verification recipes;
- materiality rules; and
- vendor auto-loaded instructions.

Freeside loads every class except host vendor instructions only from an
approved default-branch commit. Every running stage snapshots the trusted
configuration digests. Copies inside an agent workspace are untrusted data.

Host vendor instructions are the narrow second trust class. At admission,
Freeside snapshots the exact regular-file bytes reached through the configured
operator-host path (the final path may be a symlink), records their content
digest as a role distinct from the stage prompt, and stores those bytes in the
artifact closure. A genuinely missing path records explicit absence; a
dangling, unreadable, non-regular, unstable, or oversized source fails
admission. The live host path is never mounted. Materialization re-verifies the
recorded digest, then ward places only the admitted file, or an empty overlay
for admitted absence, read-only at a fixed staging mount outside the
workspace.

The pinned Claude CLI co-locates instructions, executable configuration, and
session data under `CLAUDE_CONFIG_DIR`. Phase 1A therefore never mounts a
shared identity directory there. For each gate-mediated launch, ward creates
a fresh clean config-root volume with exactly two pre-created empty mountpoint
directories, `projects/` and `session-env/`. Before the credential enters or
a writer process exists, a networkless, credential-free observer verifies the
complete root manifest, including ownership, modes, entry types, and absence
of unknown entries, links, or special files, then records its digest and
binding in the open ward journal. Observation failure refuses launch. The
config root is mounted read-only, including against root in the writer.

Only two nested paths are writable. A `projects/` continuity volume is created
for one invocation, mounted at `$CLAUDE_CONFIG_DIR/projects`, and never reused
by another invocation. A fresh per-launch scratch volume is mounted at
`$CLAUDE_CONFIG_DIR/session-env` and never carried to a later launch. No other
config path is writable. Both surfaces are untrusted activity: the continuity
volume is retained only because the provider transcript is needed for an
exact same-invocation resume, while the scratch volume carries shell
initialization needed by that process.

Ward creates every state volume under a non-reusable opaque identity and
refuses a pre-existing or ambiguous object. A credential-free observer proves
the continuity volume empty before its invocation's first launch and each
scratch volume empty before its sole launch, then journals their runtime
fingerprints, lifecycle bindings, exact mount targets, and expected options.
Immediately before every writer start, runtime inspection must match the
journalled root, continuity, and scratch fingerprints; exact source objects,
targets, and read-only/read-write options; and absence of any extra mount.
Resume permits the bound continuity volume's now-untrusted contents but
re-verifies that it is the same invocation object. Pre-existence,
substitution, unexpected initial scratch or continuity contents, an
uninspectable object, or any mismatch fails closed before credential delivery.

Every gate-mediated launch uses the pinned CLI's `--safe-mode`, which disables
user and project instructions, hooks, plugins, MCP configuration, skills,
commands, agents, styles, workflows, themes, and keybindings while retaining
the image-owned administrator policy at
`/etc/claude-code/managed-settings.json`. Ward separately mounts a
digest-bound instruction bundle read-only and passes it explicitly with
`--append-system-prompt-file`. That bundle deterministically composes the
admitted host instruction (including explicit absence) with the repository
vendor instructions resolved from the exact trusted base, preserving their
path scopes and precedence. Its source digests, composition version, and
result digest are journalled before launch. Agent-modified instruction files
remain candidate diff content and are always risk-flagged, never launch
authority.

An initial launch uses a daemon-generated UUID supplied with `--session-id`
and journalled before process creation. Provider resume is a separate
ward-owned launch generation in the same invocation: ward proves predecessor
absence while retaining the credential lease and fence, supplies a fresh
verified config root, fresh `session-env` scratch, and freshly materialized
instruction bundle, remounts only that invocation's `projects/` continuity,
and starts `--fork-session --resume <exact-predecessor-id> --session-id
<journalled-successor-id>`. Ambient `--continue`, a non-forking resume, an
unjournalled session ID, cross-invocation continuity, and a second process
while the predecessor may exist are forbidden. Forking is load-bearing: the
pinned CLI retained the predecessor's system prompt on an ordinary resume,
whereas a fork accepted the fresh explicit bundle while preserving
conversation continuity.

A replay of an already-journalled launch adopts or reaps that exact process;
it never substitutes a resume or starts a duplicate. A resume generation
whose predecessor-absence proof, bindings, or prepared volumes cannot be
reconciled fails closed. After each launch is absent, ward deletes its clean
root and scratch volumes. After terminal invocation capture, it also deletes
the continuity volume before close; cleanup failure leaves the journal open
for recovery. A CLI process the agent itself spawns inside the writer is
untrusted agent activity, not a gate-mediated launch, and the export gates
bound its effects.

This topology is a pinned-CLI empirical contract. The exact image must pass
the minimal-writable-state, workspace/config poison, exact-resume,
fresh-invocation isolation, crash-matrix, and live-race probes before a CLI
version change may enter the image.

**Reviewer-instruction poisoning is publish-blocking.** In the ordinary
workflow, Freeside blocks every reviewer-instruction path, including
`AGENTS.md` at any depth, `AGENTS.override.md`, `.codex/**`, and peers. An
automatic review is not independent when its PR changes the instructions that
govern that review. The gauntlet detects these paths mechanically.

### 5.9 Durability: Effectively Once

| System | Authority |
| --- | --- |
| GitHub | Source, issues, PRs, reviews, checks, and merge |
| SQLite | Workflow state, decisions, attempts, events, routing, conversations, and audit |
| Artifact store | Immutable inputs and outputs |
| Replica store (`portable`) | Encrypted recovery frontier, atomic remote head, and active-epoch fencing |
| Providers | Transient session state |
| Repository documentation | Promoted decisions |

Every external action uses inbox/outbox discipline. Committed workflow
decisions survive restart. Deterministic identities, reconciliation, and
bounded retry make external effects converge on one intended result. Anything
that cannot be safely retried waits for me.

One logical control plane has a stable `control_plane_id`, one or more enrolled
hosts with distinct host identities, and exactly one active host: a single
global execution seat. GitHub App private keys remain per-machine credentials;
the logical identity does not turn them into shared secrets. A standby may
verify replica and takeover readiness but serves no authoritative work. It does
not process inbox or restored outbox work, run agents, mutate workflow state,
or execute external effects.

Freeside has two operating modes:

- **`standalone` is the default, zero-configuration mode.** Local SQLite and
  artifacts are the durable frontier, the active epoch is implicit, and the
  operator contract permits one machine only. Running copied standalone state
  as the same principal on two machines is out of contract, like copying a
  GitHub App PEM. If that machine and its backup are lost, forge
  reconstruction with human re-adjudication is the disaster floor.
- **`portable` is required before a second enrolled host may activate.** A
  conforming remote store holds the durability frontier in one remote head
  whose conditional writes also carry the active host identity and epoch.
  Portable-mode fencing applies only after the activation ceremony below
  completes. Standalone does not pretend to fence a second copy it cannot
  observe.

Portable mode is enabled only by a completed ceremony:

1. provision independently revocable store credentials for each enrolled
   host;
2. wrap the control-plane data key separately for every enrolled standby and
   create the offline recovery wrap required by Section 5.10;
3. create a complete seed checkpoint and begin the local append-only journal at
   the same transaction boundary, then upload and verify that checkpoint and
   all referenced blobs while standalone work continues;
4. quiesce authoritative local mutations, flush and verify the journal delta
   and its blob closure, then conditionally create the remote head in
   `activating` state with the complete frontier, initial active host, and
   initial active epoch; and
5. pass `freesided doctor` takeover-readiness checks from every enrolled
   standby, then conditionally change that same head to `portable` and resume
   authoritative work.

Before the final cutover, the control plane remains fully functional in
`standalone`. The cutover pauses acknowledged mutations until step 5 succeeds
or the candidate activation is conditionally marked abandoned. Failure resumes
standalone from its intact local frontier; it never leaves a partly fenced
portable state, and no standby may activate from an `activating` or abandoned
head.

In portable mode, lease expiry is never authority. A host becomes active only
by conditionally rewriting the observed remote head to advance its active
epoch and name its own enrolled host identity. Every external effect requires
that head's current host identity and epoch plus a remotely durable intent whose
referenced artifacts have reached the head's durability frontier. If the store
cannot acknowledge that frontier or validate the host and epoch, portable
external effects stop. A stale host that returns becomes passive before it may
inspect or process restored outbox work. Starting an agent invocation counts as
an external effect: after takeover, the successor does not start a replacement
while the prior invocation may still run. It first cancels or proves that
invocation ended, then records its adoption disposition.

Epoch fencing and credential fencing solve different problems. Ordinary
failover uses the active epoch; it does not rotate GitHub credentials. Deleting
a lost or compromised host's App key prevents new App authentication by that
key. When immediate installation-wide fencing is required, Freeside suspends
the installation. Exclusion becomes terminal only after every outstanding
installation token expires or is explicitly revoked. Revocation cannot undo an
effect already caused with copied credentials.

The active host is the only writer, so registration bindings and pending
installation intents need no principal-wide mutation lease or binding-set
version. A pending envelope instead binds to `active_epoch` and a monotonically
increasing `durable_intent_revision`. The active host serializes changes in its
local transaction, publishes the resulting intent to the portable frontier
before redirecting or producing any external effect, and rejects an envelope
from another epoch or superseded revision.

Kill-before and kill-after tests are permanent.

### 5.10 Coherent Backup: Encrypted Checkpoints

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

Portable replication adds four object classes around that checkpoint:

- periodic complete encrypted checkpoints;
- an encrypted append-only journal that records every committed transaction
  after the selected checkpoint;
- encrypted content-addressed blobs for every referenced artifact and
  workspace capture; and
- one conditional-write remote head naming the checkpoint, journal frontier,
  active host, active epoch, and complete content-addressed blob closure.

`RemoteHead {control_plane_id, mode, active_host_id, active_epoch,
checkpoint_id, journal_frontier, blob_closure_digest}`

Only `mode: portable` grants portable authority. `activating` and abandoned
heads are recovery evidence, not fencing or activation authority.

The head advances atomically only after every referenced object is durably
acknowledged. A conversation message, decision, workflow transition, or other
result presented as committed or completed must therefore be recoverable by
another enrolled host. An external effect in portable mode follows the same
rule: its intent and every referenced artifact reach the head's durability
frontier before execution. This extends, rather than bypasses, the Section 5.9
outbox discipline.

The replica store contract is capability-based:

- strong read-after-write and overwrite consistency for control objects;
- conditional destination writes sufficient for remote-head compare-and-swap;
- immutable content-addressed objects;
- persisted-write acknowledgment;
- independently revocable per-host credentials with bounded, observable
  revocation; the conformance suite proves a revoked credential is rejected by
  both control-object and data-object operations before recovery resumes;
- declared, bounded object and metadata sizes that accommodate Freeside's
  objects; and
- no caching or sync layer in front of mutable control objects.

Every portable backend passes the same multi-client conformance suite.
Cloudflare R2 through its direct S3 API is the first reference backend because
it offers the required consistency and conditional `PutObject`; neither R2 nor
S3 compatibility is an architectural assumption. A filesystem target is
always valid for standalone backup and testing. It is portable only after the
full suite passes for the exact filesystem and mount configuration. Consumer
sync folders such as iCloud Drive and Dropbox are categorically ineligible.
The availability trade is deliberate: portable external effects stop while
the replica store is unavailable rather than risk an unfenced effect.

Takeover restores a complete frontier; there is no partial mode:

- **Graceful handoff:** the active host stops new work, cancels or waits for
  every in-flight workspace writer and proves each one ended, flushes the
  journal, performs one normalized workspace capture, and uploads and verifies
  the resulting frontier. One conditional head write then both names that
  frontier and names the successor host while advancing the active epoch,
  transferring the seat atomically. The successor restores the resulting head
  and records explicit adoption events for in-flight attempts. An attempt whose
  writer committed a terminal result reconciles that result. In particular, a
  canceled invocation remains terminal and is never restarted or resumed;
  continuation requires a new attempt seeded from the recovered workspace as
  untrusted input.
- **Crash takeover:** the successor conditionally rewrites the remote head to
  name itself and advance the active epoch while retaining the last complete
  frontier, restores that frontier, records the same adoption events, proves or
  waits for any prior agent invocation to end, and reconciles from there. The
  workspace recovery point is the last successful daemon-side push. Because
  workers hold no GitHub write credential and crash mode performs no ad hoc
  capture, every unexported change from an in-flight invocation may be lost.
  Periodic or per-turn workspace capture is not yet in contract. Loss of the
  replica store itself falls back to forge reconstruction and human
  re-adjudication, not a partial database or artifact restore.

Workspace capture uses one mechanism: a normalized, content-addressed export
that reuses the gauntlet handoff machinery, excludes credentials and trusted
`.git` state, and restores only as untrusted workspace input. Tier 1, one
capture during graceful handoff, is contractual. Periodic and per-turn capture
tiers remain an evolution of trigger policy over the same mechanism. Revisit
them only after real handoffs measure capture cost and show that the loss
window since the last successful daemon-side push is unacceptable.

**Confidentiality is policy:**

`BackupPolicy {encryption_mode, key_id, destination,
retention_by_artifact_class, last_completed_checkpoint, last_restore_test}`

- Remote checkpoints are encrypted.
- Journals, artifact blobs, and workspace captures are encrypted with the
  control-plane data key before remote upload. Content addresses continue to
  identify verified plaintext; the remote objects contain only ciphertext.
- Encryption keys live outside agent environments.
- Backup credentials are never mounted into workspaces.
- Each enrolled host receives its own host-specific wrap of the control-plane
  data key. An operator-held recovery wrap remains offline and outside every
  daemon host, so retiring the last healthy host does not destroy the only
  recovery path.
- Retiring, losing, or compromising a host first revokes that host's replica
  credential. Portable effects remain stopped until the store rejects that
  credential on both control and data paths, a remaining host selects and
  verifies the trusted frontier, and one head compare-and-swap establishes the
  new epoch. The control-plane data key and remaining host wraps then rotate.
  Revocation prevents future access; it cannot erase ciphertext or keys a
  compromised host already copied.
- GitHub App private keys are per-machine credentials under Section 10 and are
  excluded, as are provider credentials, unless a stronger recovery design
  encrypts them separately. Recovery may therefore require reauthentication;
  copying a key from another machine is not a recovery mechanism.
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

### 5.12 Workflow Definition, Initiators, and Artifacts

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
project:        {repository: freeasinbird/gh-imgup, rein: tight}
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
  source: codex_local                   # Freeside-invoked (Section 7)
  continue_while: new_material_findings
  pattern_sweep_after: 2
  low_value_streak_before_attention: 2
  hard_active_time: 8h                  # active review/remediation clock
  hard_round_limit: 25                  # emergency brakes only
verification:   {recipe: trusted,
                 commands: [[npm, ci], [npm, run, lint],
                            [npm, run, typecheck], [npm, test]],
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
- **Manual initiation uses `freesided submit`.** It registers the source work
  item as a digest-addressed artifact, creates the elaboration run, and
  reserves the deterministic implementation identity. The implementation run
  starts only after elaboration accepts a specification and, when configured,
  the operator approves that specification's digest.
- `auto_start` is bounded by WIP caps. The conservative default is `propose`.
- Raw findings are immutable. Classification is a versioned annotation.
- Low-confidence materiality defaults to continued remediation or human
  attention.
- The classifier cannot declare a finding fixed.
- Artifacts are typed, immutable, and digest-addressed. Approvals bind to their
  digests.
- The stall heartbeat (1B.1) is ward- or daemon-observed and may only
  accelerate a stall notice: it never resets or extends any hard budget and
  cannot be influenced by agent output.

### 5.13 Deterministic Components, Judgment Calls, and the Effect Registry

The engine, not an agent, runs deterministic policy jobs:

- verification;
- evidence capture;
- research fetching;
- card facts;
- evidence publication; and
- cleanup.

Agents appear where judgment is the work: elaborator, implementer, remediator,
diagnostic, finding classifier, reviewer, shadow reviewer, and, later,
briefer.

#### Daemon Judgment Calls

The daemon may call a model for judgment where judgment genuinely helps, but
an answer can never do anything by itself. Terminal authority modes are
exhaustive: **annotate**, **propose**, **explain**, and **choose**. Composed
inference inherits its eventual sink; repetition, starvation, attention
creation, and telemetry reuse count as sinks. Advisory output — all explain
sites and audit telemetry — lives in an advisory store structurally
unreachable by policy evaluation, segregated from Section 8 policy-input
telemetry.

Every call site carries exactly one per-site authority contract:

1. **Ceiling-bounded annotation** (type case: the finding classifier). It
   declares its behavioral lattice and deterministic fallback; which outputs
   reduce work; raw-severity ceilings; second-adjudication rules; cumulative
   bounds on attention, compute, and starvation; and tests for extreme
   outputs and repeated calls. Existing classifier ceilings are retained
   verbatim. Monotone-conservative annotation is a stricter subtype.
2. **Advisory-only**: human and advisory-store consumers only.
3. **Proposal** into the closed effect registry below.
4. **Bounded choice** among daemon-authored options whose worst-case effects
   were independently bounded before the call; cross-vendor driver selection
   is not choosable (standing owner decision).

Cumulative bounds compose globally: per-site budgets aggregate across sites
and runs under project-level and global windows, attributed to root lineage.
Bound resets require gate-waiver-class authority, never the calling site.

Hard rules: outputs are schema-validated and producer-labeled; nothing flows
into trust computation, transition legality, or `publish_eligible`; every
site declares a fail-safe default; "operable with inference down" means the
control plane stays available and fails safe — inference-dependent steps
pause or degrade per declared defaults, never promised to complete; every
site is budgeted; untrusted-input sites carry sampled-audit telemetry; every
site has a deterministic fake. Proposal cards separate registers: "the
proposal requests X" is a daemon fact from the artifact, while agent cost,
safety, and scope assertions are labeled claims. Section 3.1's "designed
judgment points" means human judgment points.

Daemon-side inference is its own contract, not a reuse of `provider_only`:
driver binding, credential handling, outbound field selection (an explicit
allowlist per site), input sensitivity classification, redaction, provider
identity, retention, size limits, and input digests recorded per call. No
tools, no workspace, no ward container.

#### The Closed Effect Registry

Agent-requested real-world effects — anything a run, a client proposal
surface, or daemon-side inference asks the daemon to make happen — exist only
as typed, digest-addressed proposal artifacts targeting a closed registry of
effect kinds; each kind has a fixed Go type, trusted constructor, and gate.
Effects the trusted workflow performs itself (publication, notifications,
installation maintenance) remain engine-run under Section 5.9 and the
deterministic-jobs list above; they are not proposal-gated. Proposals supply bounded parameters, never
event bodies, target identities, or authority. Targets are daemon-selected
context or a selection among daemon-enumerated opaque subject handles
("watch PR 42" parses as picking from daemon-enumerated subjects).

Admission allocates and persists a daemon-generated proposal-instance ID
atomically under a stable admission idempotency key: the canonical upstream
event ID, the client submission-command ID, or, for proposals emitted from
within a run, the accepted invocation or export identity plus an emission
ordinal. A deliberate repeat gets a new command ID; retrying the same
occurrence preserves it. Semantic content never defines occurrence identity.
The instance ID is the effect identity for idempotence, ledgering, and crash
reconciliation; content digests bind approvals. Instances: `run_proposal`
(existing), follow-up issue filings (Section 5.17, 1B.1), and proposed
watches (a planned extension landing with its schedule kind and consumer,
Section 5.16). Gates read resolved policy; rein is not a security dial.

### 5.14 Client Synchronization and Conversations

#### Authority and Consistency

The active daemon is the sole authority. In portable mode, activation restores
conversations, decisions, workflow state, and artifact references from one
remote durability frontier before the successor serves clients. Client
databases are disposable read caches. The synchronization contract guarantees:

- transactional consistency in the daemon;
- optimistic concurrency;
- eventual convergence;
- read-your-write after acknowledgment;
- a cached, read-only view with a freshness banner while the daemon is
  unreachable; and
- no consequential action until the client validates current state.

#### Revision, Epoch, and Cache Semantics

`ServerState {sync_epoch, revision}`

- Every client-visible transaction increments `revision`.
- A restore or portable-host takeover creates a new `sync_epoch`, which forces
  clients to discard caches.
- **A partial fetch never advances the whole cache.** Clients track
  `last_full_snapshot_revision` separately from
  `highest_observed_server_revision`.
- Every `ResourceSnapshot` carries `as_of_revision` and `entity_version`.
- `/sync/bootstrap` returns one canonical snapshot from one read transaction.
- A revision gap triggers a full bootstrap or a refetch of every potentially
  affected resource.
- A periodic revision heartbeat detects lost invalidations.
- Push and WebSocket improve latency only; correctness does not depend on them.

#### Devices, Commands, and Caches

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

#### Conversations and Discuss

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

#### Permanent Phase 1A Sync and Device Tests

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
8. After daemon restore or portable-host takeover, a new epoch makes clients
   discard newer cursors and bootstrap.
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

### 5.16 The Durable Scheduler

One scheduler owns every durable deferred check — PR watches, deadlines,
subject-bound polls — as a closed union of schedule kinds with fixed Go types
and trusted event constructors. 1B implements only the kinds with 1B consumers: the PR-checks
deadline, the review-wait threshold, the base-advance staleness watch
(consumer: the base-freshness fact on `ready_for_final_review` items, which
stay live until merge or close), and the installation poll, plus permanent
trusted-config jobs (doctor, janitor; not proposable, no expiry requirement).
The doctor, the janitor, and the onboarding pending-install-or-expansion
poll already run pre-1B on plain tickers under their Section 10 obligations;
the scheduler adopting them is a 1B migration that preserves those
obligations, never a precondition for them.
The proposed-watch, scan-sweep, and grant-expiry kinds are planned
extensions added with their consumers (proposed watches are deferred past
the four 1B timer kinds; an approved watch proposal is representable only
once its kind and consumer land); scan activation stays Phase 2. Stateless process heartbeats stay
plain tickers. Active-resource reconciliation (Section 5.11) also stays
outside the kind union: the per-resource conditional-request polling that
observes PR state, checks, merge and close, and native review activity is a
continuous process cadence on a plain ticker, and `ready_for_final_review`
items observe merge or close through it. Schedule kinds carry durable,
subject-bound deadlines and watches, never the reconciler's cadence.

Proposed watches (Section 5.13's effect registry) require expiry and are
bounded by minimum cadence; per-subject, per-project, and global active-watch
caps; maximum occurrences or explicit renewal; and proposal, card, and
notification coalescing.

Occurrence identity is (`schedule_id`, `generation`, `nominal_fire_at`);
missed fires coalesce to the latest nominal occurrence with a recorded gap.
Fire-time validation — project binding, resolved policy, expiry, activation
state (Section 5.9), operating-mode eligibility (Section 5.7; kind-specific:
permanent trusted-config jobs run in every operating mode, so the doctor and
janitor keep their Section 10 obligations in `attended_dev`, while workload
kinds require the operating mode their consumer demands), and subject
existence — precedes event construction; the event carries the expected
schedule generation and subject version, rechecked by the consuming handler.
A stale event is never silently discarded: the handler recomputes and either
re-arms (new generation, corrected binding) or records proof the condition no
longer applies. Consumption and its outcome commit in one transaction;
otherwise the occurrence stays durably pending and is redelivered. One-shot
deadlines always terminate fired-and-handled or explicitly resolved. Firing
never extends or preserves authority. Schedule state is durable, queryable,
and synced.

### 5.17 Follow-Up Issue Filing

Filing a GitHub issue is a fan-out effect: it can start workflows, notify
people, and wake integrations, and an issue those systems create in response
could re-enter intake as new work. Filing is introduced human-gated (1B.1):
every filing takes explicit per-proposal human approval regardless of profile
state. The policy-approved path below is the later autonomous path; a valid
authority profile is an additional precondition for it, never a replacement
for the 1B.1 human gate.

The policy-approved path requires a digest-bound, freshness-limited
issue-event authority profile covering a complete enumerated authority
surface (unknown or uninspectable surfaces render the repository ineligible),
revalidated immediately before each creation, drift failing closed, as a
filing precondition only. The eligibility predicate: every known transitive
issue-creation or labeling path must be (a) ledgered before intake, (b)
proven unable to become intake-eligible, or (c) structurally forced to
propose — quantified over every path in the complete enumerated authority
surface, not merely known paths; otherwise filing stays human-gated.

Intake eligibility requires event-level authority proof: ledgered
proposal-instance lineage (a daemon-authored ledger mapping to the canonical
numeric issue ID under the canonical repository ID; markers in issue content
carry zero authority) or explicit human admission, checked at the final
intake transition. An event without proof is forced to propose even when
current configuration validates: current-state revalidation cannot
authenticate a historical event (drift-create-revert is assumed reachable).

Repository, filing identity, labels, and milestone derive from trusted policy
and run lineage, never proposal text. Every agent-controlled textual field is
screened under a versioned ruleset on the Section 5.5 commit-message-screening
pattern. Effect identity is the proposal-instance ID (Section 5.13):
idempotent check-before-create and crash-after-create reconciliation key off
it; origin and canonical issue ID live in the immutable daemon ledger;
discovered candidates are validated (repository, App authorship, expected
ledger bindings; markers in issue text are rendering hints, never matching
keys) before adoption. Creation is fenced for recovery: a durable creation
intent precedes the API call, and unledgered creations serialize per
repository — the candidate collision domain, since filing identities within
a repository share App authorship and candidate-visible fields — so recovery
holds at most one outstanding intent per repository to bind; it adopts the
single validating App-authored candidate
created in the intent window or proves absence before any retry, and
residual ambiguity fails closed to a durable attention item, never a blind
retry. Rate, depth, and cost caps come from resolved policy.

Freeside-origin issues enter intake as propose, never `auto_start`, enforced
at every intake observation including after relabeling. In any repository
where Freeside has ever filed and no current valid issue-event authority
profile exists, all label intake demotes to propose: automation-created
descendants cannot be attributed there, so no unattributed labeled issue in a
Freeside-seeded repository is trusted for `auto_start`. A current valid
profile restores `auto_start` eligibility only for non-Freeside-origin issues
it admits that pass the intake proof check; Freeside-origin issues stay
propose-only regardless of profile state.

### 5.18 The World Model: Post-Merge Recompute and Frontier Projection

After a merge, the daemon recomputes its map of the project: what completed,
what is now unblocked, what could run in parallel. Capture hooks — work-unit
bindings, completion criteria, dependency and scope facts — record from
1B.0; projection computation and its UI land in 1B.2 (Section 11).

A merge marks a unit done only through an exact daemon-recorded work-unit
binding and completion criterion (for example, the bound issue closed by the
merged PR); partial, stacked, or related merges do not complete units. The
frontier projection derives from explicit declarations only — dependency
edges, declared path scopes, contract serialization, merge state — binds to
a per-resource freshness vector (reconciliation is per-resource; there is no
global cursor to wait on), and renders per-resource staleness and incomplete
coverage explicitly. "No declared mechanical conflict detected" is the
strongest daemon fact; inferred parallelism is a labeled planner claim;
unknown scope serializes. The planner judgment call is deferred past 1B.

### 5.19 Deferred Subsystems: Provisional Contracts

The contracts below are design constraints for deferred subsystems, recorded
now and re-reviewed at implementation. None is scheduled inside 1B.

**Scoped consent grants (deferred past 1B).** A standing permission binds:
canonical repository ID, effect kind, an effect-specific authority identity
union (GitHub App identity, provider auth identity, or none), trust, policy,
and profile digests, operating mode, cost, use, and concurrency limits, a
validity interval, and the effect constructor/schema version (a constructor
change invalidates the match). The daemon selects matching grants; agents and
runs never nominate one. Issuing, renewing, widening, or extending requires
version-bound human approval or a trusted-configuration change; inference and
runs may only propose. Grants are immutable: renewal or changed bindings
create a new grant. Direct operator revocation is always available. Before
the first irreversible request, the executor atomically matches every binding
against current state, reserves use, cost, and concurrency capacity, and
marks the attempt started under the exact grant and constructor version. The
durable EffectAuthorized intent is the linearization point: it binds grant
ID, constructor version, payload digest, active epoch, and fencing token.
Revocation committed before the intent prevents it. Revocation after the
intent does not prevent reconciliation or adoption of an effect that may
already have occurred, under least authority, with anything wider raising
attention; but if reconciliation proves the irreversible request was never
sent or the effect is absent, no new request may be made under the revoked
grant — that requires a current grant, lease, epoch, and new intent.
Reservations confer no authority and are invalidated by revocation; use and
cost reservations (accounting) are distinct from fenced concurrency leases
(correctness). In portable mode, grant changes and authorized intents
acknowledge only after reaching the remote frontier; after takeover, a stale
fencing token permits reconciliation only, and creating an absent effect
requires a new current grant, lease, epoch, and intent. Fencing is enforced
by the daemon-owned effect executor. Grants pre-answer a risk acknowledgement
only; digest- and head-bound approvals and non-waivable gates are untouched.
Until built, per-run authorization continues (accepted cost; revisit at the
1B exit).

**External findings ingestion (deferred).** Externally produced reviews are
quarantined at entry: quota-bound advisory proposals (a future effect kind
added to the Section 5.13 registry with this subsystem) with
`external_untrusted` provenance, a raw-source digest, and a reconstructed —
never asserted — project and head binding. The authenticated ingestion actor
is recorded separately from the claimed producer; the operator-selected
ingestion target separately from the artifact's own source binding (exact /
claimed / unknown; promotion to any blocking role requires exact).
Quarantined findings cannot block readiness, trigger remediation, or consume
remediation budgets. Automatic blocking or remediation requires
source-specific admission or explicit human promotion, deduplication, and a
declared authority-site contract (Section 5.13). External findings never
satisfy ReviewSource freshness, independence, or review-completeness.

**Pre-publication adversarial pass (deferred).** An optional adversarial
self-review before a PR opens, so the external reviewer starts from a higher
floor. It reviews the daemon-constructed publication candidate after hostile
import, never the raw workspace. Each pass binds the exact candidate head and
invocation inputs; stopping is bounded by resolved policy; the pass holds no
direct remediation or publication authority. Each remediation repeats the
gauntlet, verification, and the adversarial pass itself. Distinct from the
Section 7 review requirement, which itself anchors pre-publication
(revision 28): the Section 7 pass is required; this pass is optional and
deferred.

**Readiness registry (deferred).** When built, a projection over current
typed proofs recomputed on read, never a stored ready bit; the Section 10
doctor consumes it.

## 6. Verification

Verification defines “done.” It is deterministic, engine-run, clean-room, and
controlled by a trusted recipe. It includes evidence capture. Its outputs are
run-bound artifacts cited by AttentionItems. False-ready tracking under Section
12 starts on day one.

### The Verification State Algebra

Every check's result is recorded honestly: passed with proof it ran against
exactly this code and policy, failed, or not run; nothing passes by omission,
and one check's proof cannot stand in for another's.

Every requirement resolution binds requirement identity, the requirement-set
digest, the daemon-floor/registry generation, resolved policy, and any
durable sampling decision, shared across both branches so a proof cannot
structurally occupy another requirement's state. A pass carries proof: the
candidate head; the base or prospective-merge identity for any requirement
whose evidence depends on one (integration checks, Section 7 review passes);
the recipe digest; and the resolution digest. The re-gate binds the current
base alongside the requirement-set digest, so a base advance structurally
mismatches base-dependent proofs and forces reruns; it never restores
readiness from evidence bound to a prior base.

Waivers exist only inside Failed and NotRun, only for required checks of
registered waiver-eligible classes, and name the waived dimension and
granting authority. Granting authorities are a closed set — explicit human
approval or daemon-owned trusted configuration; resolved policy alone cannot
mint a waiver, and inference may only propose one. Daemon-owned applicability
and requiredness floors may be tightened by policy, never weakened;
non-waivable classes have no waiver representation.

Readiness evaluation starts from the complete current requirement set; an
absent record evaluates as required plus NotRun, blocking. The aggregate
verdict distinguishes Blocked, ReadyClean (every applicable check passed),
and ReadyDegraded (any progress-permitting non-clean outcome: waived required
checks and/or optional Failed/NotRun as advisory outcomes, at least one
nonempty). Rendering exposes both classes; no downstream consumer receives a
flattened boolean. The re-gate runs at the publication/admission effect
boundary and binds the exact current requirement-set digest, floor/registry
generation, policy, sampling decisions, and waiver lifecycle state;
historical proofs match only on exact binding match. Historical
applicability keeps its run-time digests.

Illustrative annex (non-binding):

    CheckState {
      resolution: RequirementResolution {
        requirement_key, requirement_set_digest,
        floor_registry_generation, resolved_policy_digest,
        sampling_decision? },
      state:
          NotApplicable { resolution_proof }
        | Applicable {
            requirement: required | optional,
            outcome:
                Passed  { proof: CheckProof }
              | Failed  { waiver?: ValidatedDegradedWaiver }
              | NotRun  { waiver?: ValidatedDegradedWaiver } } }

    ReadinessVerdict =
        Blocked       { reasons }
      | ReadyClean    { evaluation_set_digest }
      | ReadyDegraded { evaluation_set_digest, waiver_ids,
                        advisory_outcomes: [{ requirement_resolution_digest,
                                              outcome: Failed | NotRun }] }

    evaluation_set_digest commits to the complete canonical current
    CheckState set: resolutions, outcomes, proofs, waivers, advisories.
    CheckProof carries the base or prospective-merge identity wherever the
    requirement's evidence depends on one, so the digest shifts on a base
    advance.
    waiver? is representable only when requirement = required and the check
    class is registered waiver-eligible; the real types enforce this
    structurally (the annex elides it for brevity).

## 7. Review Policy

Independent error detection is the goal. Provider diversity is one way to
achieve it.

**Review is a durable, Freeside-invoked and Freeside-orchestrated stage of
the run workflow** (decider: user; revision 25): request, acknowledge, ingest
normalized findings, drive remediation, reverify, re-review the new head,
escalate a stalled or exhausted loop to durable attention. The first
production ReviewSource is a Freeside-invoked local Codex invocation.
GitHub-native Codex review, when observed, is recorded as best-effort extra
evidence; it never satisfies the review requirement. The trigger
falsification behind this is recorded in Sections 5.3 and 13 and on #427.

Each review pass binds the exact base and candidate head SHAs, and a change
to either — a new candidate head or an advanced base — invalidates the pass
and requires re-review. Integration evidence follows the same rule: a base
advance also invalidates verification and check evidence bound to the prior
base, and readiness recomputes under the Section 6 re-gate before any ready
state is restored. The pass runs with fresh context independent of the
implementing invocation, a read-only workspace, and no publication
credentials; it receives repository instructions and verification evidence,
never the implementer's reasoning history; it returns normalized findings
with severity, location, explanation, and stable identity; and it records
provider, model configuration, invocation, cost owner, and completion
evidence. The findings → remediation → reverify → re-review loop is bounded
by resolved policy; exhaustion or ambiguity produces a durable AttentionItem,
never a silent stall. Failure classification matches the publication
boundary: transient failures retry with backoff; configuration or quota
failures create attention; durable contradictions fail loudly.

**Resolved fork (decider: user, 2026-08-05; revision 28): the review anchor
is pre-publication.** Implement → verify → review → clean: publish; the PR
opens already reviewed, and forge checks still gate merge. The stage as
landed (#427, PR #490) reviews the published PR under the then-open fork;
the implementation re-anchor is tracked in #527. The internal loop
is the agent's pre-push work; the PR is the collaboration surface. The PR
list stays a decision queue, not a work queue; post-publication state is the
expensive place to be correct (the #496/#514 ready-identity class); PR
comments are mutable, so the authoritative ReviewRecord lives in the store
under either anchor, and PR-anchoring would mean building both surfaces; and
the owner's own drill-down usage — progress pulse, forensic drill-down on
agent/forge disagreement, disposition reconstruction — is served by computed
readiness, the run timeline, and structured dispositions, not comment
threads. The owner's condition on this resolution pins the
EvidencePublisher's first slice (Section 5.15; #525, wave 5 per Section
11): at publication the PR carries the disposition history — review
rounds, final dispositions including declined and deferred with reasons,
and the readiness derivation — so the merged PR is forensically
self-explanatory on the forge. The condition is not an immediate
publication precondition. Until #525 lands the forge carries no
disposition history; the store carries the durable review state —
ReviewRecords (round outcomes, finding identity), raw findings, and
classifications. Per-finding dispositions with reasons and the readiness
derivation are not yet persisted anywhere; persisting them is a
prerequisite both of #525's rendering and of any pre-#525 publication
that treats the store as the authoritative disposition record. The
trigger falsification forced
neither anchor; a Freeside-invoked reviewer can review either surface. The
PR-anchored shape remains the recorded, considered-and-rejected alternative
(revision 25's fork text, docs/history/decisions.md); revisit when real
usage shows the owner cannot trust review they did not watch — it is the
fallback.

Review activity arriving on a published PR from outside the control plane —
human maintainers, GitHub-native Codex when it fires, other bots — is the
deferred external review response capability (Section 11; #524); it never
satisfies this section's requirement, which stays Freeside-invoked and
pre-publication.

Sequencing preserves independence (spine-confirmed on #427): the #427
implementation unit, then production runs with Claude implementing and Codex
reviewing, then the #397 Claude ReviewSource promotion, then #408 Codex
execution routing — so Codex-implements plus Codex-reviews never becomes the
default pairing. The first production Codex review pass is additionally
gated on the applicable subset of the Codex pre-adoption probes (#401:
credential handling, vendor-instruction binding, auth refresh,
child-environment credential exposure), verified at #427 scheduling; a
read-only workspace and withheld publication credentials do not by
themselves close those provider-credential and instruction surfaces.

Routing comparisons use accumulated traces, including the 1B Claude shadow
arm. Shadow findings are recorded but never routed, and comparisons are
adjudicated blind where practical.

Scheduled Codex execution (Section 11) will make Codex-executes plus
Codex-reviews a same-vendor pairing, weakening the independence this section
targets and raising the later value of a selectable Claude ReviewSource. The
sequencing above and the deferred #397 promotion keep that pairing from
becoming the default; shadow findings remain recorded and never routed.

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
| `effect_proposal` | The requested effect as a daemon fact from the validated artifact (Section 5.13): kind, daemon-resolved target, and bounded parameters. Agent cost, safety, and scope assertions are labeled claims, never merged into the fact line. | Full proposal artifact; “approve with changes” shows the revised-digest diff. |
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
- Maintenance accounting: time and CI spend consumed operating and
  maintaining Freeside itself are recorded and netted against the return;
  per-run cost telemetry remains Section 8.

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

## 10. Operations and Onboarding

Build the installer only after the underlying interfaces survive real use. The
`freesided` binary provides:

| Command | Function |
| --- | --- |
| `freesided setup` | Performs installation. On the Mac-first path the operator app registers the daemon LaunchAgent (Section 5.2) and no step is privileged; when a hardened deployment needs privileged steps (user creation, LaunchDaemon installation), they run through a narrow elevation helper; the daemon never retains root. |
| `freesided onboard <repo>` | Resolves the selected GitHub App installation, creates the trust profile, attests effective authority for one-time human review, detects the verification recipe, and invokes the proven reusable project-image builder. If installation, organization approval, or repository selection is missing, onboarding records a bounded pending-install-or-expansion intent before routing the operator into GitHub's native flow, then polls; a callback or `--resume` reopens the same review after approval. |
| `freesided doctor` | Checks conformance, the workspace-handoff gate, checkpoint encryption, backup age, artifact closure, restore-test age, and, from 1B.1, stored-credential integrity (a truncation and corruption probe). It runs on a schedule and files `system_health` items. |
| `freesided submit` | Registers a manually initiated source work item, starts elaboration, and reserves its future implementation run. |

The project-image builder is an internal primitive, not an onboarding-only
implementation. Phase 1A manually proves that primitive against the selected
repository before the first real run, then `freesided onboard` packages the
same primitive after the run path has survived real use. Onboarding must not
carry a second image builder or a second copy of recipe semantics.

The operator client installs Mac-first: a locally built, personal-team-signed
FreesideMac with a direct install-and-update script, an icon from the Section
15 visual identity, and real device pairing land with 1B's first wave
(Section 11). The iOS client
follows mid-1B under free provisioning; the paid Apple Developer Program is
deferred until APNs arrives in Phase 2, because client correctness never
depends on push (Section 5.14).

From revision 27, the Mac app on the daemon host also owns the local
daemon's lifecycle (Section 5.2): it registers the LaunchAgent, reads the
daemon's published readiness file, and carries a menu bar presence showing
liveness and version with start and stop through launchd. The menu bar is
the out-of-band surface for the one failure the attention system cannot
report, a dead daemon; richer operational state on that surface (doctor
results, the 1B.1 signals) layers on later waves.

### GitHub App Agent Identity

Each Freeside operator is a distinct agent principal and holds their own
GitHub App registration or registrations. Registrations and private keys are
never shared across operators. A principal may map to more than one numeric
App ID under the work-account posture below, but every App ID is explicitly
bound to exactly one trusted principal.

Registration topology is owner policy, not an architectural distinction:

- **Default: one public App per operator.** The registration is owned by the
  operator's personal account and installed separately on the operator's
  personal account and each repository-owning organization through GitHub's
  native install or approval flow, with repository-scoped selection. This
  default is permitted only while the Section 5.5 mint gate applies
  unconditionally, GitHub inputs can cause AttentionItems only through a
  trusted principal and recorded installation-to-repository binding, there is
  no unauthenticated GitHub write path into the attention system, and the
  always-on installation-grant janitor below is active.
- **Opt-in: one private App per repository-owning account.** An operator may
  choose this work-account posture when an organization must own and terminate
  the credential. Each such registration still represents that operator and
  is never shared with another operator. The same mint, attention, and
  installation-grant reconciliation gates apply; private visibility is not
  treated as a substitute for them.

Before redirecting to create or approve an installation or to add a repository
to an existing selected-repository installation, `freesided onboard`
records exactly one single-use pending-install-or-expansion intent containing
its `active_epoch`, `durable_intent_revision`, registration, expected numeric
owner, installation ID when known, current trusted repository IDs, exact
expected post-change repository IDs, required `repository_selection:
selected`, callback nonce, and expiry. In portable mode, the redirect waits
until the intent and its referenced state reach the remote durability
frontier. The intent grants no authority beyond existing trusted bindings: in
particular, the added repository cannot mint, publish, or reach attention. The
janitor may leave exactly one remote installation matching that owner,
installation ID when known, selected-repository mode, active epoch, durable
intent revision, and exact post-change repository set untouched only until
expiry. A same-session callback that matches the nonce is an acceleration, not
the authority path. The active daemon also polls App-authenticated installation
state for exact pending matches, and
`freesided onboard <repo> --resume` reopens that state after another browser,
session, or daemon restart.
Either path moves the intent only to ready-for-review. Promotion still requires
canonical owner, installation, selected mode, and post-change IDs plus
acceptance of the local one-time trust review; that acceptance atomically
creates the installation-to-repository binding. A missing, ambiguous,
over-broad, replayed, stale-epoch, superseded-revision, or expired match fails
closed, invalidates the intent, and lets ordinary reconciliation resume.

Every registration, public or private, requires the always-on
installation-grant janitor. The daemon refuses to operate a registration
without it. The janitor enumerates every installation and its granted
repositories against the recorded principal, owner, installation, and
repository bindings. Before minting any installation token, it uses the
App-authenticated installation record to require either a trusted binding (and,
for a public registration, a trusted owner) or an unexpired pending envelope
whose registration, expected owner, installation ID when known, active epoch,
and durable intent revision match current state. This pre-token gate does not
claim the pending repository set matches. It deletes and audit-logs every other
installation from that metadata alone, without enumerating its repositories.
For a candidate that passes this gate, the janitor first requires the canonical
installation response's `repository_selection` to be `selected`; a missing or
different mode is drift even when the current repository IDs happen to match.
To make complete repository enumeration possible, the janitor alone may mint
an installation token without `repository_ids`, narrowed to the
minimum permission set accepted by GitHub's list-repositories endpoint. This
credential remains in daemon memory, may call only that paginated read
endpoint, is never logged, persisted, or exposed to a worker, and is revoked as
soon as enumeration completes or fails. The returned repository pages are
untrusted until pagination completes and their canonical IDs form the compared
set. Only then does a pending envelope become an exact pending match by
matching its expected post-change repository set. For that exact,
unexpired match only, the expected owner and post-change
repository set are temporarily exempt from the untrusted-owner and
unrecorded-grant branches; the exception grants no authority and disappears on
promotion or expiry. Outside that exception, any unrecorded repository grant,
including `all repositories`, immediately suspends and audit-logs the whole
affected installation. That suspension is
terminal quarantine: Freeside records the observed grant set, deletes the
installation with App authentication, invalidates its binding, and requires a
fresh native installation through the pending-intent flow. It never
automatically unsuspends a drifted installation or mints a token against it.
Unsolicited installations and repository grants never authorize Freeside
minting or reach the attention system.

Registration uses the manifest flow, and the initial key lands directly in
protected storage. Each additional machine receives a distinct private key
within the registration. Freeside computes and records for each machine the
same SHA-256 public-key fingerprint GitHub displays in the App settings, and
uses that fingerprint to identify the exact key the operator must delete when a
machine is lost, retired, or compromised. A single key can therefore be revoked
without replacing its siblings. Copying PEM files between machines is outside
the contract.

GitHub App names are globally unique and limited to 34 characters while
GitHub user and organization names can reach 39. Freeside generates a
suggested name by truncating the account-derived portion while retaining a
legible username, reserves room for a numeric collision suffix, and retries
with increasing suffixes. The requested name is only a suggestion: the
manifest conversion response supplies the canonical numeric App ID, name, and
slug that Freeside stores. App names are thereafter effectively immutable
policy because a rename churns the visible `[bot]` login; trust decisions
continue to use only the numeric App ID.

Daemon-authored agent commits use the selected GitHub App bot as their Git
author and committer, with name `<app-slug>[bot]` and no-reply address
`<bot-user-id>+<app-slug>[bot]@users.noreply.github.com`. This makes GitHub
associate the commit with the same visible App principal and avatar that
publishes it. Freeside resolves and records the bot account's numeric user ID
from the canonical App slug and requires durable per-run attribution to match
the registration selected by the repository-scoped installation token before
execution or import. The bot user ID and slug are attribution metadata, not
trust inputs. This human-readable provenance complements credential-level
attribution and never substitutes for the App-ID, installation, repository,
or token-mint checks.

Defaults are hosted ntfy, embedded SQLite, one configuration directory, and
`attended_dev` with honest isolation-class reporting.

Phase 1A exit targets, verified on a clean VM or spare machine:

- fresh machine to first run in under one hour; and
- repository onboarding in under thirty minutes with exactly one Freeside
  manual review when the selected App installation and any organization
  approval are already complete. GitHub's native installation or approval is
  an account-onboarding prerequisite measured separately; after it completes,
  `freesided onboard` resumes the same onboarding transaction.

## 11. Roadmap, Build Order, and Coordination

### The First Repository Is Deliberately Boring

The first managed repository is **not Freeside**. Freeside often changes
control-plane paths, so it is the hardest possible starting case. It becomes the
bootstrap test after the path works. The selected first target is
`freeasinbird/gh-imgup`, a small TypeScript CLI. Language and toolchain are not
selection criteria; the generic recipe and project-image model must verify the
repository honestly.

The selected repository must:

- have automation authority the current machine-readable `WorkflowAudit` and
  `AutomationTrustProfile` can represent without repository-specific logic;
- support deterministic, networkless clean verification through a trusted
  recipe, with its toolchain and dependency closure baked into the project
  image;
- contain representative ordinary changes that can traverse the gauntlet
  without inherently touching publish-blocking control-plane paths;
- have enough genuine work for the several real items required by the 1A.2
  exit; and
- permit installation of the Freeside GitHub App and one-time human review of
  the generated trust profile.

Prefer, without requiring, a small code and dependency surface, fast direct
verification commands, low PR-reachable automation authority, no UI evidence
requirement, and infrequent workflow or instruction-file changes. Ordinary
repository features such as tag-only release automation belong in the audited,
digest-bound trust profile; they are not prose exceptions.

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
- the reusable project-image builder manually proven against the selected
  repository at an exact commit and recipe, with its digest-pinned result
  available to an admitted run;
- several real work items completed without terminal intervention; and
- `setup`, `onboard`, and `doctor` packaging the proven manual operations,
  including that same project-image builder, and meeting the Section 10
  targets.

#### Phase 1A Build Order

1. Domain, synchronization, devices, and fakes.
2. Clients and the sixteen permanent tests.
3. Export, gauntlet, and verifier with fake candidates; artifact store with
   checkpoint and provenance rules.
4. Publication, reconciliation, and kill tests.
5. ward and its handoff gate, then the Claude agent base.
6. The reusable project-image builder (#334), manually proven against the
   selected repository.
7. The Claude driver and real work items (#237), consuming that project image.
8. `setup`, `onboard`, and `doctor` (#238), packaging the same builder.
9. Phase exit.

Investigate the workspace-handoff gate early and in parallel because it is the
largest runtime unknown. It blocks only 1A.2, never 1A.0 or 1A.1.

### Phase 1B: The Useful Workflow, in Three Internal Exits

Phase 1B turns the secure path into the useful daily workflow:

`labeled issue → daemon-fetched research → elaboration → spec approval →
implementation → gauntlet → Freeside-invoked review (Section 7,
pre-publication) → yield-driven remediation and pattern sweeps →
diminishing-returns or dispute item → clean: PR under a trust profile,
carrying the Section 7 disposition history (#525) → checks →
ready-for-final-review with yield history → human GitHub merge`

External review activity on the published PR re-enters the workflow
asynchronously when it arrives (deferred; #524, sharing the
re-entry-after-terminal-state trigger shape with #502); it is not a stage
of the chain and never a readiness prerequisite.

Phase 1B adds:

- the elaborator and research fetcher;
- label-initiator intake (scan-query initiators stay Phase 2);
- the Freeside-invoked review stage (Section 7; implementation #427;
  pre-publication re-anchor #527), with ReviewSource freshness verification
  and automatic re-review testing;
- finding classification with sampled accuracy and second adjudication;
- convergence policy and the shadow arm;
- provenance-gated EvidencePublisher;
- experimental `max_parallel_executions` per auth identity, visible to
  scheduling;
- the Codex execution driver, an execution capacity hedge against
  single-provider stalls (Section 14): the `agent-codex` agent base, the
  project images the reusable builder derives from it (Section 5.7),
  ward's second vendor topology, the `codex` driver binding, and the
  driver-selection contract land as separate follow-on units, sequenced
  after the 1A.2 exit and blocked on the #401 pre-adoption gates; driver
  selection stays explicit, with no automatic fallback; and
- the run timeline screen.

Precondition: the verified 1A exit. 1B proceeds in three internal exits.

#### 1B.0: The Useful Loop

The workload above, with the review step rebased onto the Freeside-invoked
binding; the Section 5.13 judgment-call contracts, with the finding
classifier as the first ceiling-bounded annotation site and the diagnostic as
the first advisory-only site; the Section 6 verification state algebra; the
Section 5.16 scheduler with the four consumer-backed timer kinds; the runs
list (project-filterable, showing attached watches and deadlines) with the
run timeline drill-down; and Section 5.18 capture hooks recording from the
start.

Contract sequencing inside 1B.0: the scheduler and the Section 7
review-stage chain (#427 and its substrate) gate first real-backlog use
(revision 26, amending revision 25's scheduler-only statement); the state
algebra and the effect-registry retrofit of `run_proposal` land within 1B.0
behind them, serialized per contract discipline but off the loop's critical
path. Real-backlog use begins during 1B.0 as soon as the minimal loop
stands, at the close of wave 4 (this section's coordination table).

#### 1B.1: Operational Closure

Human-gated follow-up issue filing (Section 5.17); the doctor
credential-integrity probe (Section 10); the stall heartbeat (Section 5.12).

#### 1B.2: The Initiative View

The Section 5.18 frontier projection and the deterministic initiative view,
shipped minimal as a deterministic projection (owner decision), under Section
5.18's rendering and coverage discipline. This placement materially overturns
two standing statements — Section 4's GitHub-Projects-as-all-work-view and
this section's former Phase 3 initiative-view placement — recorded in
Section 13.

Client-side settings editing does not exist as a mechanism: today every
configuration change lands as a control-plane change under Section 5.8's
gating — an operator-authored control-plane PR, or, for recurring
preferences, the Section 4 policy-proposal path — and when the
deferred settings surfaces ship, a configuration-change proposal kind joins
the Section 5.13 registry with them as its consumer — approval cards, never
edit forms, stay the client surface.
Deferred past 1B, with provisional contracts where Section 5.19 records one:
the planner judgment call, scoped consent grants, external findings
ingestion, the pre-publication adversarial pass, the readiness registry, the
project detail screen, past-work history, the system/schedules page,
consent-grant UI, and plain-English scheduling (CLI-first and sequenced
before any conversational surface; owner decision). Open question carried:
daemon construction of meaningful multi-commit history without guessing
intent (current fallback: a single clean re-authored commit).

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
| **3 (1B.0): loop foundations** | Parallel lanes | Spine, serialized: the Section 5.16 scheduler (four timer kinds, trusted-job ticker migration), then Section 5.18 capture-hook recording. Ward: #401 gates 1/2/4/5 as parallel probes, then the #404 base image pinned per gate 2's outcome. App: Mac-first operator access (Section 10). |
| **4 (1B.0): the review stage** | Serial | The spine rescopes #406/#407 into review cores and execution remainders, then lands the review-selection contract core, the review ward-topology slice, #405 only if review needs a project-derived image, and #427 — landed PR-anchored under the then-open Section 7 fork (resolved pre-publication in revision 28; the implementation re-anchor is #527, unscheduled). Its close stands the minimal loop; real-backlog use begins. |
| **5 (1B.0): loop depth** | Parallel lanes | Elaborator and daemon research fetching with the spec-approval gate; label-initiator intake; the Section 5.13 classifier and diagnostic sites; the provenance-gated EvidencePublisher (first slice: the Section 7 disposition history at publication, #525); the runs list and run timeline; the `max_parallel_executions` experiment. The contract track drains the Section 6 state algebra, then the effect-registry retrofit of `run_proposal`. The supervision core consumes the revision-27 Section 5.2 contract, pulled forward by owner fiat: #454's daemon side and the app-side LaunchAgent and menu-bar unit. |
| **6 (1B.0): convergence and yield** | Integrated | Convergence policy; the Claude shadow arm with second adjudication and sampled classification accuracy; automatic re-review of remediation heads as a standing integration test; yield history on ready-for-final-review; the full chain on the real backlog. iOS on-device install (Section 10). 1B.0 exit. |
| **7 (1B.1): operational closure** | Parallel lanes | Human-gated follow-up filing with the `effect_proposal` card; the doctor credential-integrity probe; the stall heartbeat; the external daemon-liveness probe (Section 5.2); the deferral drain (sweep-eligible open deferrals enumerated at this wave's planning; dormant contract units excluded unless the spine assigns chain positions). The execution tail closes in order: #401 gate 3, the #406/#407 execution remainders, #405 if outstanding, #397 by explicit owner decision on shadow evidence, then #408. |
| **8 (1B.2): the initiative view** | Integrated | The Section 5.18 frontier projection and the deterministic initiative view. 1B exit evaluation. |

Review bandwidth limits parallel width. Every wave ends with a fresh-context
adversarial review by an agent given only the repository and its documents,
never this design history. `AGENTS.md` defines the issue protocol; each
wave's unit list lives in its pinned tracking issue, while this table
records shape and sequencing. The 1A backlog also serves as elaborator
fixtures.

### Phase 2: breadth and hardening

Expand beyond the first constrained path:

- a second repository and workflow shape;
- scan initiators and chaining;
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

### Phase 3: Comprehension and Interaction

Add ACP interactive attachment, best-effort resume, material plan-change gates,
briefings, usage display, evidence-informed routing, WIP views, and mature
`auto_start` behavior. The initiative view moved to 1B.2 (revision 25).

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
- a portable checkpoint, journal, artifact blob, or workspace capture
  replicates off-host without its required encryption.

**Kill criterion:** stop if agents work acceptably in the manual workflow but
Freeside does not materially raise useful, correct work per unit of attention.
Elaborator weakness alone is not a kill criterion.

## 13. Decisions Log

Record material changes here by revision, with the decider in parentheses.

- This section contains only the current revision.
- When a new revision lands, move the outgoing items to
  `docs/history/decisions.md`.
- The history contains every revision, including revisions superseded before
  commit.
- Update the history in the same PR as the plan revision so they cannot drift.
- On first re-litigation, promote the decision to a `docs/decisions/` ADR that
  cites its history entry.

Revision 30 ("Verified Codex re-enrollment recovery"):

1. **Revoked Codex identity recovery is command-backed by exact verified
   evidence** (Section 4): the marker remains a `system_health` item and gains
   `resolve_reenrollment` only after its exact latest re-enrollment operation
   is verified and immutably bound to that marker occurrence. The resolving
   command revalidates both records atomically; `acknowledge` remains seen-only,
   and no human assertion can clear the identity gate. Revisit when a provider
   requires additional durable recovery evidence beyond the current digest and
   access-token expiry.
   (User; devlog 2026-08-11-1025-codex-reenrollment-recovery.md; #684.)

## 14. Risks

| Risk | Current response |
| --- | --- |
| Provider credentials in `subscription_contained` | Document the residual; enforce egress floors; let the daemon fetch research for the most exposed stage; provide `api_key_isolated` as the escape. |
| CI privilege crossing | Attest effective authority; block candidate automation changes; fail closed on drift; prohibit the daemon host as a runner. |
| Reviewer-instruction poisoning | Treat instruction paths as control-plane content and block candidate changes in the ordinary publication path. |
| **Workspace-handoff uncertainty** | Resolved by the workspace-handoff spike: the strong class is declared and conformance-gated (Section 5.7); the same-VM fallback is refuted by execution, never implemented or declared. |
| **Codex cloud review as a load-bearing dependency** | Realized 2026-07-31: the live-run trigger falsification (#427) showed no App-visible trigger path. The dependency is removed: review is Freeside-invoked (Section 7), and native review is best-effort extra evidence. |
| Single-provider execution capacity | Claude usage limits can stall real work. Schedule the 1B Codex execution driver as a hedge (Section 11); keep driver selection explicit with no automatic fallback; usage remains observed telemetry (Section 8). |
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
`docs/coordination.md` owns the canonical lane glossary.

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
