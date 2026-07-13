---
title: Freeside Project Plan
revision: 4
status: active
phase: 1A
updated: 2026-07-13
---

# Freeside

**Project charter and implementation spec.** The plan's identity of record is its default-branch commit digest (Section 5.8); `revision` is the human label, incremented on material change per Section 9's gating, with each increment recorded in Section 13's decisions log. Revision narrative lives in PR bodies and the devlog, not here.

---

## 1. What Freeside is

**Freeside is a local, durable workflow controller that turns a software work item into an evidence-backed pull request and interrupts me only when judgment is required.**

Category: **an agent control plane.** Harnesses (Claude Code, Codex) run the agent's inner loop; Freeside is the outer loop: it decides what work starts, inside what boundary, with which credentials withheld, what counts as done, when a human is interrupted, and what survives a crash. The self-brand register: *the harness runs the agent; the reins are yours.*

The system runs on a Mac Studio acting as a permanent runner (reference deployment; Linux is a supported deployment class, Section 3.3). A work item (a labeled issue discovered by an intake scanner, a scan-proposed candidate, or a hand-approved spec) flows through a staged workflow: elaboration to a spec; spec approval in my attention inbox; implementation in an isolated workspace holding no GitHub credentials; a hostile import boundary into a fresh checkout; deterministic verification under a trusted recipe in a clean workspace; daemon-side publication under an audited trust profile; yield-driven independent review with capped emergency brakes; a ready-for-final-review item on my phone with mechanical evidence. I review and merge on GitHub. The attention inbox is part of the control system: a daemon-owned domain model with lifecycle, staleness, synchronization, and concurrency rules. GitHub owns code and review; Freeside owns workflow execution and approvals.

Freeside is justified entirely as a personal-leverage tool; the metric is personal net leverage after maintenance. The usefulness of agent elaboration, implementation, and iterative review is established by my current manual workflow. **The open question is whether that workflow can become a safe, durable, low-attention system without moving the danger into provider credentials, CI, artifact import, stale approvals, or interruption creep.**

Success claims: (1) **attention reduction** against a passively logged baseline; (2) **decision quality preserved** (right moments, decidable evidence, no stale actions); (3) **safety invariants hold** (Section 12); (4) **autonomy preserved** (exceptional-interruption rate stays low and trends down, Section 3.2).

## 2. Goals and non-goals

### Goals

1. **Attention routing as a first-class system**: durable AttentionItems with lifecycle, type-specific actions, optimistic concurrency, cross-device synchronization, push notification, self-contained decision cards, on iPhone and Mac.
2. **Elaboration in the tested value proposition**, severable: entry from a raw issue or a hand-approved spec; elaborator weakness degrades the entry point, never the loop. (Decider: user.)
3. **Autonomous initiation**: initiators (manual, label, scan) map event sources to workflow entries under propose or auto_start modes, so Freeside drives loops, not only reacts to them. (Restored in revision 4; Section 5.12.)
4. **Yield-driven review remediation** matching the validated manual process; round counts are emergency brakes only.
5. **Bounded execution isolation**: capabilities fixed at spawn, no GitHub credentials in any workspace, declared per-run credential modes, hostile import boundary, trusted verification recipes.
6. **CI privilege containment**: agent-authored code never reaches secret-bearing or privileged CI without an audited trust profile or an explicit gate.
7. **Remote operation from iPhone**; human role is judgment at gates plus final review and merge on GitHub.
8. **Chat authors artifacts; the engine executes artifacts**; behavior lives in trusted versioned configuration.
9. **Verification defines completion**, deterministically, under a trusted recipe, in a clean workspace.
10. **Durability of decisions**: committed decisions survive restart; external effects converge (effectively-once); database and artifact store restore coherently; clients converge on daemon state.
11. **Instrumentation sufficient for agent-driven optimization**: traces with stable join keys, outcome attribution, attention telemetry, sampled shadow reviews (Section 8).
12. **Operational simplicity**: one-command setup, guided onboarding, self-diagnosis; targets in Section 10. (Decider: user.)

### Non-goals

1. Not an IDE; not a review surface (GitHub owns diffs and merge); no auto-merge, ever.
2. Not a product: no multi-tenancy, billing, or hypothetical users. (Portability and simplicity are personal requirements that incidentally keep the open-source option real.)
3. **Not a harness**: Freeside consumes vendor harnesses through their sanctioned batch interfaces and never owns a model loop.
4. Not runtime self-modification: agents extend Freeside via ordinary reviewed PRs; control-plane config is never hot-modified.
5. Not automatic provider fallback; not voice; not a pipeline DSL; not briefings or general project chat until earned.
6. Not a formal pre-build validation study; not a generic CI security auditor (Phase 2 at most; 1A needs one reviewed profile plus drift detection).
7. Not a general-purpose sync platform: server-authoritative snapshots suffice for one daemon and one user's devices.

## 3. Operating principles

### 3.1 Autonomy inside the envelope

Autonomy is the default; gates exist only at trust-boundary crossings and the two designed judgment points (spec approval, final review). Hardening must be invisible in the happy path: the importer, clean verifier, conformance tests, sync, and credential boundaries ask nothing of anyone. **Any gate that fires repeatedly for the same reason is a bug in policy, not a fact of life.**

### 3.2 The interruption budget

Every AttentionItem is tagged **planned_gate** or **exceptional**. The exceptional rate per run is a tracked health metric; a rising rate is treated as a defect to fix (tune policy, extend an allowlist, promote a preference), never as the new normal. **Self-service rule:** every recurring item type must offer a path that resolves the class, not the instance (the convert-preference-to-policy action is the prototype; a gate satisfied the same way three times should offer to become policy, via the control-plane proposal path). The **rein posture** is the maturity dial: per-repo `rein: tight | loose` maps to propose-versus-auto initiation and gate strictness, loosening as trust accumulates, including eventually waiving spec approval for policy-defined low-risk change classes. Known hot spot, accepted: Freeside's own repo routinely touches control-plane paths, so a disproportionate share of its PRs carry the control-plane approval class; that is the correct cost where poisoning matters most.

### 3.3 Portability

macOS is the reference deployment; **Linux is a supported deployment class.** The daemon core takes no Apple-only dependencies; runner backends are per-platform implementations of the capability-classed interface (Apple container on macOS; Firecracker/Kata as VM-per-job class and Docker as the weaker class on Linux); launchd/systemd and dedicated-user setup are per-platform equivalents. **Enforcement: the daemon's own CI builds and tests on Linux from day one**, making portability continuously verified rather than aspirational. Cloud deployment notes: provider subscription credentials on a cloud host add exposure (host snapshots, operator access) recorded in the residual-risk documentation; vendor trusted-runner guidance for subscription auth on a single-tenant VM is a verify item. Nothing is built for cloud until wanted. (Decider: user.)

### 3.4 Simplicity

Setup, onboarding, and upkeep are designed features with targets (Section 10), not emergent properties. Defaults assume the common case; the strictest option must not gate first-run; the system diagnoses itself into the same inbox everything else uses. (Decider: user.)

## 4. The attention model

**AttentionItem**: id, project_id, run_id, type, priority, reason, requested_decision, evidence_snapshot (engine facts), agent_claims (labeled), artifact_digests, pr_head_sha, item_version, interruption_class (planned_gate | exceptional), conversation_id?, timing chain (created_at, notified_at, opened_at, decided_at), expires_when, status.

**Phase 1 types and actions** (approve is not universal):
- **spec_approval**: approve / request changes / discuss / stop. Full artifact rendering; a revised spec renders the complete current spec, a **diff against the last version reviewed**, and prior comments with claimed addressals.
- **review_diminishing_returns**: finish now / apply current batch then finish / continue under specified policy / convert recurring preference into project policy (**emits a policy-change proposal PR through the control-plane path; never mutates policy directly**).
- **review_dispute**: adjudicate finding / discuss / stop.
- **execution_failure**: retry / retry with a **predefined, policy-allowed capability manifest** (the phone never authors capabilities) / discuss / stop.
- **agent_question**: answer and retry / answer without retry / stop.
- **publish_blocked**: rerun trust evaluation / choose an approved alternate publication profile / inspect trust failure / stop.
- **ready_for_final_review**: open PR (**navigation, not resolution**; the item remains active until GitHub reports merge or close, work is returned to the agent, or it is explicitly dismissed; reconciliation auto-resolves on observed merge) / return to agent with feedback / acknowledge / stop.
- **run_proposal** (initiators, Section 5.12): start / start with changes / decline / snooze; batched, never one ping per finding.

**Lifecycle:** approvals bind to digests and head SHA; changed inputs invalidate; retries supersede failures; resolutions are transactional and version-checked; stale submissions receive a conflict plus the replacement item. **Notifications are read-only hints** (no sensitive payloads, no actions); the app fetches current state and submits version-bound decisions. **Fault-class capture:** adjudications and returned work record a fault class (bad_spec | bad_implementation | reviewer_preference | environment | other) at the moment of judgment. Capacity: WIP caps on runs and initiatives; GitHub Projects is the passive all-work view; the inbox is exclusively needs-me.

## 5. Architecture

### 5.1 Overview

```
GitHub (source, issues, PRs, reviews, checks, merge; Codex cloud review)
   │  per-resource reconciliation + intake scanners (no global cursor)
   ▼
freesided (Go daemon; launchd/systemd; dedicated user)
   ├─ event inbox: reconciled state, intake scanners, cron, manual (idempotent)
   ├─ workflow engine: code-defined state machines; policy from config
   ├─ signet (attention service): items, conversations, sync, devices, ntfy
   ├─ StageDriver: Claude batch execution (+ permanent fake driver)
   ├─ ReviewSource: CodexGitHubReview (+ permanent fake source)
   ├─ finding classifier: annotations over immutable raw findings
   ├─ envelope (runner layer): capability classes + fail-closed conformance
   ├─ gauntlet: hostile importer -> fresh checkout -> clean verifier (recipe)
   ├─ git/publish service: owns ALL GitHub credentials; deterministic identities
   ├─ store: SQLite (authoritative) + inbox/outbox + content-addressed
   │   artifact store; coherent backup invariant (5.10)
   └─ sync API: snapshots + revision/epoch + invalidations (Tailscale-private)
   ▲
   ▼
Freeside app (SwiftUI, macOS + iOS): inbox, decision detail, run timeline
```

### 5.2 The daemon

Go, single static binary, supervised (launchd/systemd), dedicated user, state and credentials inaccessible to other accounts, privileged services bound to loopback/Tailscale only. Rationale: appliance fit (static deployment, stdlib strength, subprocess support, explicit concurrency, supply-chain restraint, compatibility culture). Reliability from process boundaries around drivers, worker-boundary recovery, supervision restarts, durable state, idempotent external operations. Agent-maintenance compensations: small packages, table-driven tests, golden-file JSON tests, AGENTS.md conventions. CI builds and tests on macOS and Linux.

### 5.3 Execution: StageDriver and ReviewSource

Stages are bounded batch jobs (immutable inputs, workspace, typed outputs, exit).

```
StageDriver:  start(StageRequest) -> run_id;  stream(run_id) -> events
              cancel(run_id);  collect(run_id) -> StageResult;  capabilities()
ReviewSource: request_review(pr, head_sha) -> review_request_id
              poll(review_request_id) -> raw_findings
              verify(raw_finding) -> bool   # actor, head SHA (commit_id
                                            # binding), request id, freshness
```

Phase 1: one local driver (**Claude**, non-interactive / Agent SDK worker, auth mode as config); one review source (**CodexGitHubReview**, the trusted cloud reviewer from the manual workflow; keeps Codex auth out of local workspaces; local reviewer is the Phase 2 hedge). **Permanent fake implementations of both are first-class test fixtures**, reproducing crashes, delays, duplicate outputs, stale heads, malformed artifacts, and pathological review patterns without provider spend. ACP is not an execution interface; it returns in Phase 3 for interactive attachment.

**Review triggering is exclusively control-plane** (decider: user): the ReviewSource is the only component with a code path to review requests; no implementation-side path may request, suppress, or re-trigger review. Preference: repo-configured auto-review (declarative, digest-audited in the trust profile); framework-issued re-requests after remediation pushes if auto-review does not cover new heads (minimal templated text, never agent-authored content: a submitter-composed trigger is an injection channel). Fail-closed: unconfirmed review becomes an attention item; review is never silently skipped. 1B verify items: auto-review coverage of remediation heads; Codex's exact instruction-file discovery behavior (policy in 5.8 lands regardless).

**Session durability contract:** transcripts and artifacts durably recorded; workflow recovery guaranteed from stage inputs, workspace state, and artifacts; provider resume best-effort; a dead unattended process means clean retry, snapshot resume, or escalation. **Capabilities fixed at spawn**; insufficiency means a typed request and exit; interactive sessions (Phase 3) never grant capabilities.

### 5.4 Credential modes

**No GitHub write credential ever enters any workspace.** The git/publish service owns the App key and installation tokens and performs every mutation after gauntlet validation. Provider credentials run in a declared, recorded, per-run mode; project policy states acceptable modes:

- **subscription_contained** (Phase 1 default): native vendor CLI in the agent VM; provider-only egress; no registries at runtime (dependencies baked); read-only credential mount where permitted; secret scanning on every exported byte; **exposure accepted as documented residual risk.** (Setup-token scope claims: verify during 1A.)
- **api_key_isolated** (Phase 2, supported): key in a trusted broker; mediated access; usage billing.
- **local_trusted**: dedicated-account tooling for explicitly trusted inputs; never represented as isolated.

Native unmodified vendor tooling; terms confirmed during 1A; **provider-level concurrency leases** serialize each auth identity; API-key fallback always available.

### 5.5 The CI trust boundary

Tokens out of workspaces does not protect CI: pushed agent branches can execute agent-modified scripts inside secret-bearing Actions jobs, and same-repo branch PRs lack fork-style restrictions. Every onboarded repository carries an **automation trust profile** (pr_execution: audited_same_repo | fork_untrusted | local_only; allow_self_hosted_ci; allow_secret_bearing_pr_jobs; allow_pull_request_target; workflow_audit_digest; review: {mode, config_digest}). Default for owned repos: **audited_same_repo**. **1A scope: one repository's machine-readable profile, reviewed manually once, with deterministic drift detection failing closed**; the generalized auditor is Phase 2 material, and static inspection can never fully prove workflow safety, so drift-detection-plus-fail-closed is the durable mechanism. Fail-closed: unaudited repo means local implementation and verification complete, publish gated behind publish_blocked. **Standing prohibition:** the daemon host is never a self-hosted Actions runner for any managed repo.

### 5.6 The gauntlet: hostile import and clean verification

```
daemon-owned base repo ──exact base SHA──▶ agent workspace
agent workspace ──patch / constrained bundle + typed manifest──▶ importer
importer ──validated──▶ fresh daemon-owned checkout (hooks disabled)
fresh checkout ──▶ clean verification workspace (no credentials, no network)
verified candidate ──▶ git/publish ──▶ GitHub PR (under trust profile)
```

The importer never trusts the workspace's .git, hooks, git config, symlinks/hardlinks, device files, archives, submodules, modes, or agent-written manifests; enforces base SHA, ancestry and count bounds, canonical paths, type/mode allowlists, size limits, control-plane path restrictions (5.8), secret scanning, no shell interpolation, restricted git protocols, daemon-generated manifests. **Malicious artifact fixtures are permanent suite members.**

**Trusted verification recipe** (clean rooms are insufficient alone, since a candidate that weakens the Makefile makes the clean VM faithfully execute hollow verification): VerificationRecipe {config_digest, runner_image_digest, commands, trusted_wrapper_digest, network_policy, timeout, expected_outputs, sensitive_control_paths} loads from approved control-plane config or the trusted base commit, never the implementation head. First repo prefers direct tool invocations (go test ./..., go vet ./...) over mutable project wrappers. Changes to verification-control files are mechanically identified, risk-flagged, invalidate prior-recipe assumptions, and gate when they can weaken or redirect verification; they are reviewed as part of the candidate, never trusted as its judge. Evidence records exact command, image digest, environment digest, exit status, immutable log. **Named residual:** candidate-authored test code still executes in the verifier VM; the recipe protects verification's judgment, while the verifier's credential-free, network-free envelope contains its execution. Both halves are required.

### 5.7 The envelope: runners

Backends declare capabilities (isolation class, VM-per-job, private networking, snapshots, read-only root, resource limits, egress-proxy support); policy states minimums; **no silent downgrade**, but first-run may accept a weaker declared class with the conformance suite reporting exactly what class is running (Section 10). Primary backends: Apple container (macOS), Firecracker/Kata (Linux VM-class), Docker (weaker class, both platforms). **Fail-closed conformance at startup and before unattended runs**: internet unreachable except via the intended proxy; daemon API, Tailscale services, host gateways, databases, artifact storage, and control sockets unreachable from the VM; DNS and IPv6 cannot bypass policy; refuse to run on failure. VM boundary treated as filesystem/kernel isolation, not proven network isolation; host hardening applies regardless. Golden images pin CLIs and toolchains; workspaces on VM-local disk; artifacts ship out at stage end. **Bootstrap exception:** the Go daemon builds itself through Freeside; SwiftUI work is exempt until a macOS execution class exists (deferred, possibly forever).

### 5.8 Control-plane trust

Workflow config, prompts, policy, trust profiles, verification recipes, and materiality rules load **only from an approved default-branch commit**; running stages snapshot trusted-config digests; workspace copies are data. **Vendor auto-loaded instructions are control-plane**: local agents get trusted-base overlays of all agent configuration (CLAUDE.md, .claude/*, .mcp.json, .codex/*, hooks, plugins; project MCP/hooks/plugins/skills disabled unless allowlisted); agent-modified instruction files are ordinary diff content, always risk-flagged. **Reviewer-instruction poisoning (blocking):** cloud reviewers read instruction files from the candidate branch (nested AGENTS.md included), so for the ordinary workflow, **adding or modifying any reviewer-instruction path (AGENTS.md at any depth, AGENTS.override.md, .codex/**, and peers) is publish-blocking**; such changes route through the control-plane change workflow with human approval before publication or before relying on cloud review; **auto-review is not independent review for a PR that modifies the instructions governing it.** Detection covers nested paths mechanically in the importer.

### 5.9 Durability: effectively-once

Authority: GitHub owns source/issues/PRs/reviews/checks/merge; **SQLite owns workflow state, gate decisions, attempts, event processing, routing, conversations, audit**; the artifact store owns immutable stage inputs/outputs; providers hold transient session state; repo docs hold promoted decisions. External actions use an **inbox/outbox** (state + intent + idempotency key in one transaction; worker acts and records). Claim: **committed workflow decisions survive restart, and external effects converge to one intended result through deterministic identities, reconciliation, and bounded retry** (run-derived branch names, hidden PR run markers, check-before-create, marker-updated comments, expected-state records, reconciliation after ambiguous timeouts). Kill-before/kill-after tests are permanent.

### 5.10 Coherent backup invariant

Blob writes: temp path → digest verify → fsync (file and directory) → atomic rename into the content-addressed store → **only then** the referencing SQLite row. Disaster recovery tracks ArtifactBackupState {replicated_through_revision, verified_at}; **a backup is restorable only when every artifact reachable from database state at that revision exists in replicated storage**; restore verifies every referenced digest before unattended work resumes. Rows referencing missing blobs: integrity failure, fail closed. Orphan blobs: safe, garbage-collected **subject to retention policy**. Small irreplaceables (conversation text, decisions) live in SQLite; large attachments, screenshots, patches, raw transcripts live in the artifact store. Any rollback restore issues a new client sync_epoch.

### 5.11 GitHub integration: reconciliation plus intake

Per-active-resource state reconciliation (issues, PRs, check runs, reviews) with conditional requests, modest intervals, refresh on app open. **Intake scanners discover new work** (reconciliation only tracks known resources): IssueIntakeScanner {repository, required_label, updated_since_with_overlap, issue_identity, last_completed_scan}, using overlap plus idempotent identity rather than trusting timestamps as perfect cursors. The scanner is the template all scan-class initiators follow. Webhooks are Phase 2, only if latency hurts, feeding the same event inbox; reconciliation remains mandatory.

### 5.12 Workflow definition, initiators, and artifacts

The Phase 1 workflow is a Go state machine; YAML is policy only; crash retries are separated from review remediation:

```yaml
project:        {repository: freeside-ai/<first-repo>, rein: tight}
initiators:
  - {type: manual}
  - {type: label, label: "freeside", mode: auto_start}
  - {type: scan, query: stale_prs, schedule: daily, mode: propose}   # Phase 2
elaboration:    {driver: claude, enabled: true}
implementation: {driver: claude, failed_execution_retries: 2}
review:
  source: codex_github
  continue_while: new_material_findings
  pattern_sweep_after: 2
  low_value_streak_before_attention: 2
  hard_wall_time: 8h
  hard_round_limit: 25            # emergency brakes only
verification:   {recipe: trusted, commands: [go test ./..., go vet ./...]}
gates:          {spec_approval: true, before_final_review: true}
budgets:        {wall_time: 45m, max_diff_files: 40}
security:       {credential_mode: subscription_contained}
telemetry:      {shadow_review_rate: 0.2}
```

**Initiators** map event sources to workflow entries with mode auto_start or propose (propose emits batched run_proposal items). Deterministic queries where possible; agent triage only where judgment is required; initiators are control-plane config; auto_start is bounded by WIP caps and budgets; the conservative default is propose. 1B ships manual and label; scan initiators arrive early Phase 2; **workflow chaining** (one workflow's output initiating another, e.g. bug-report triage feeding implementation) is Phase 2/4 horizon.

**Findings and classification:** raw reviewer findings are **immutable**; classification (finding_id, head_sha, severity, category, pattern, materiality, scope, confidence, status) is a separate versioned annotation; classifier prompt/model changes are digest-traceable; **low-confidence materiality defaults to continuing or human attention, never dismissal; the classifier cannot declare a finding fixed.** Yield, not round count, ends review; classification accuracy is sampled telemetry; ceilings guard against label-driven misbehavior.

Artifacts are typed, immutable, digest-addressed (type, schema version, digest, producing run/attempt, base/head SHAs, validator, retention, sensitivity); inputs reference artifact IDs; approvals bind to digests. Budgets are enforceable controls; token budgets are advisory telemetry. A DSL waits for three genuinely different shapes.

### 5.13 Deterministic components

The engine runs verification, computes card facts, and performs cleanup as policy jobs. Agents appear where judgment is the work: elaborator (widest egress, zero write credentials), implementer, remediator, diagnostic, finding classifier, later a briefer.

### 5.14 Client synchronization and conversations

**Authority and consistency.** The daemon is sole authority for items, workflow state, conversations, messages, decisions. Client databases are disposable read caches; both platforms use the same API. Guarantees: transactional consistency in the daemon; optimistic concurrency for commands; eventual convergence of connected clients; read-your-write after acknowledgment; no availability while the daemon is unreachable, **but the cached read-only view remains visible with a freshness banner** (a blank app during a network hiccup would reinstate agent-window patrol); consequential actions are disabled until current state validates.

**Revision and epoch.** ServerState {sync_epoch UUID, revision uint64}. Every client-visible transaction increments revision, returned by all mutations and state endpoints. sync_epoch persists across ordinary restarts; an explicit restore or state rewind issues a new epoch; a client seeing an unknown epoch discards its cache and bootstraps (prevents a device ahead of a restored daemon from concluding it is current). item_version detects per-entity conflicts; revision detects overall staleness.

**Snapshots and invalidations.** Small canonical snapshots (GET /sync/bootstrap, /attention, /runs/{id}, /conversations/{id}/messages?after={sequence}) carry epoch and revision. An active WebSocket sends lightweight StateChanged {revision, entity_type, entity_id} invalidations; the client refetches; missed revisions or reconnects trigger snapshot refresh (the active inbox is small; replacing it beats a premature delta protocol). Refresh on launch, foreground, network restoration, reconnect, notification tap, manual pull, and version conflict. **Push and WebSocket improve latency only; correctness never depends on either.**

**Devices and commands.** Each device is explicitly paired with a revocable credential in the Keychain (Device {id, display_name, credential, created_at, last_seen_at, revoked_at}); Tailscale is reachability, not authorization. Every mutation is a ClientCommand {command_id UUID, device_id, expected_entity_version, expected_bindings, payload}; the daemon records command and result transactionally; retries return the original result. A resolution transaction: authorize device → check status/version/bindings → record decision → advance workflow → write outbox intents → record command result → increment revision; only then does the client show success. No offline approvals in Phase 1; drafts may persist locally but never present as accepted without live acknowledgment.

**Conversations** are Freeside domain objects, distinct from provider transcripts: Conversation {id, project_id, work_item_id, run_id, status, version, timestamps}; Message {id (client UUID for human messages), conversation_id, sequence (daemon-assigned, monotonic), author_type human|agent|system, author_device_id, body, attachment_digests, reply_to, created_at server/client, stage_attempt_id, status committed|superseded|redacted}; AgentInvocation {id, conversation_id, trigger_message_id, stage_attempt_id, input_message_ids, input_artifact_digests, status, result_message_id, transcript_artifact_digest}. Daemon sequence orders messages, never device clocks; messages are immutable (corrections are new messages); text in SQLite, attachments in the artifact store by digest. Items reference conversation_id; resolution never deletes the conversation.

**Discuss semantics.** One transaction: append the human message → record the exact item version and bindings addressed → supersede/transition the item → write an AgentInvocationRequested outbox intent → record the command result → increment revision. Daemon death after commit but before stage start yields exactly one invocation on recovery. **Invocations bind to explicit message and artifact IDs, never "the conversation so far"**: fresh-context stages preserved, every invocation reproducible. Agent completion commits the normalized response message, artifacts, workflow transition, and any replacement item together; the raw transcript remains an audit artifact. Phase 1 conversations are asynchronous; live streaming and mid-turn steering are Phase 3.

**Sync tests (permanent, in 1A):** (1) cross-device resolve with second-device conflict; (2) offline device submits against superseded version, rejected with replacement; (3) all notifications missed, foreground refresh reconstructs the inbox; (4) lost HTTP response, command_id retry returns the committed result; (5) discuss commits, daemon dies pre-invocation, recovery starts exactly one; (6) agent response with both clients closed, both later retrieve the same ordered thread; (7) concurrent discuss on one version, one wins, no second invocation; (8) restored daemon issues new epoch, clients discard newer cursors and bootstrap; (9) late notification for a resolved item deep-links to canonical state with no stale action; (10) retried attachment/message yields one artifact and one message.

## 6. Verification

Deterministic, engine-run, clean-room, recipe-trusted (5.6), per-project configured; outputs are run-bound artifacts cited by cards. "Done" is verification. False-ready tracking per Section 12 from day one.

## 7. Review policy

Goal: independent error detection; provider diversity is one mechanism. 1B's configuration (Claude implements, Codex cloud reviews) is component fit, not doctrine. Routing comparisons run over accumulated traces **including sampled shadow reviews, which are the comparison's only valid data source**: at shadow_review_rate, a second reviewer runs against the identical head, findings recorded but never fed to remediation, deduplicated and adjudicated blind where practical. Freeside does not pass or fail on routing results.

## 8. Observability and optimization telemetry

Traces land as **typed relational rows in SQLite with stable join keys** (run_id, item_id, finding_id, head SHA, digests); transcripts are drill-down pointers, never the primary substrate; an optimizing agent writes SQL, not archaeology. Per run: stage, prompt/config/trust/recipe digests, driver, credential mode, transcript pointer, artifacts, tokens and cost (recorded now, displayed Phase 3), wall time, attempts, review rounds and yields, classifier samples, shadow-review results, outcome, decisions. **Closed-loop attribution:** defect issues reference the producing run (PR run markers make blame-to-run mechanical); fault classes captured at adjudication (Section 4). **Attention telemetry:** item timing chains, decisions taken, conflict/staleness events, interruption-class rates (the Section 3.2 health metric); long open-to-decide gaps and discuss detours flag cards whose evidence is not self-contained. **Baseline logging is passive and concurrent with 1A**: a lightweight time log of the manual workflow anchoring the usefulness comparison; not a study, not a gate. Usage is observed telemetry, never claimed quota state.

## 9. Comprehension

Evidence packets on every decision first; altitude lines once plans are structured artifacts. Plan changes gate by materiality (scope, acceptance criteria, milestones, sequencing affecting active work, architecture, risk posture, commitments); wording changes are recorded, not interrupted for; materiality rules are control-plane policy. Devlogs split by cadence: repo protocol for human sessions; artifact-store summaries for autonomous runs; shared promotion channel (decisions to ADRs/plan, deferrals to issues). Briefings and open-ended querying are Phase 3, only if repeated real questions demand them.

## 10. Operations and onboarding

**The binary is the installer.** `freesided setup` creates the dedicated user, supervision config, config skeleton, and replication config. `freesided onboard <repo>` generates the machine-readable trust profile for one-time manual review, detects a verification recipe from the repo's toolchain, and builds the project image. `freesided doctor` repackages the conformance suite and restore verification as diagnostics, runs on a schedule, and files failures as attention items: the system reports its own degradation into the same inbox. (This is admin tooling; the "not a CLI" non-goal governs the product experience.) **GitHub App creation uses the manifest flow** (one click and a redirect, not a webform and key juggling). **Defaults:** hosted ntfy; embedded SQLite; single config directory; Docker accepted as the initial runner class where the strict backend is absent, with the conformance report stating the running class and its meaning; strictest settings never gate first-run. **Targets, committed:** fresh machine to first successful run under one hour; repo onboarding under thirty minutes with exactly one manual step (reviewing the generated trust profile, manual on principle). Supervision is installed by setup; upgrades are deliberate (pinned CLI and adapter versions, driver contract tests).

## 11. Roadmap and build order

### Phase 1A: the secure publish path

From a **hand-approved spec**: implementation (subscription_contained, provider-only egress) → gauntlet (import, clean recipe verification) → daemon publication under the reviewed trust profile → ready_for_final_review on iPhone. One repository, dependencies baked. Components: workflow state; signet (items with 1A types, conversations, sync revision/epoch, device pairing, ClientCommands); **fake StageDriver and fake ReviewSource**; minimal macOS/iOS clients; artifact store with the coherent backup invariant; hostile importer with malicious fixtures; clean verifier with trusted recipes and a fake candidate producer; git/publish with deterministic identities and reconciliation; envelope with conformance suite; Claude StageDriver; trust profile (manually reviewed) with drift detection; ntfy; kill-before/kill-after, stale-approval, and the ten sync tests; `freesided setup`/`onboard`/`doctor`.

**Build order:** (1) workflow state, AttentionItem, conversations, sync revision, device commands, fake StageDriver; (2) minimal clients plus stale/concurrent-device tests; (3) artifact store and backup rules; (4) importer and clean verifier against a fake candidate producer; (5) git publication and reconciliation; (6) envelope and conformance; (7) Claude StageDriver; (8) real 1A work items; (9) 1B.

**1A exit (safety and durability only):** conformance green; malicious fixtures contained; kill, stale-approval, and sync tests pass; restore drill verifies the backup invariant; several real hand-specced changes publish with no duplicate side effects, **no undeclared credential in any workspace** (the sole workspace credential is the provider credential the recorded subscription_contained mode explicitly permits), and no terminal intervention after approval.

### Phase 1B: the useful workflow

labeled issue (intake scanner) → elaboration → spec_approval (full rendering; revision diffs) → implementation → gauntlet → PR under trust profile → checks (reconciliation) → Codex review, control-plane triggered → yield-driven remediation with pattern sweeps → diminishing-returns / dispute items as needed → ready_for_final_review with yield history → human merges on GitHub. Alternate entry: hand-approved spec. Adds: elaborator; intake scanner; ReviewSource with freshness verification and the auto-review-coverage question resolved; finding classifier with sampled accuracy; convergence policy; shadow-review sampling; run timeline screen. Real backlog immediately; its telemetry is the trial. **1B exit (usefulness):** no agent-window patrol; no manual polling; productive review rounds run unasked; low-value streaks yield one consolidated item; spec approval and handoff decidable from the phone; active attention materially below baseline; exceptional-interruption rate low; false-ready per Section 12.

### Phase 2: breadth and hardening

Second repository and workflow shape; scan initiators (propose mode) and workflow chaining; local Codex driver if useful; api_key_isolated; full failure-injection campaign; restore drills; generalized (still bounded) CI audit tooling; richer classification; webhooks if latency hurts; APNs; egress proxy for registry-needing projects; risk-classified cards; Linux deployment exercised in anger if wanted.

### Phase 3: comprehension and interaction

ACP interactive attachment; best-effort resume; material plan-change gates end to end; briefings; usage display and evidence-informed routing (shadow-review analysis); WIP and initiative views; auto_start maturation via rein posture.

### Phase 4: generalization

Pipeline DSL (after three shapes); more agents and skills; macOS runner class; App Intents, widgets, Live Activities; voice; open-source packaging decision.

## 12. Exit criteria definitions

**Mechanical false-ready** (zero tolerated): the card asserted something objectively stale or false (checks on a previous head; review bound to an earlier commit; verification not run; spec digest changed post-approval; artifact links not matching reviewed code). **Substantive false-ready** (zero critical/high; lesser misses recorded): final review finds a material in-scope correctness, security, data-integrity, or acceptance-criteria failure the automation should reasonably have caught; a requested design improvement is not false-ready. **Safety failure** (any blocks unattended use): workspace obtains a GitHub write credential; agent reaches a privileged host service; output escapes importer constraints; untrusted PR code receives privileged CI credentials without an explicit gate; a stale mobile decision takes effect; a crash produces uncontrolled duplicates; provider auth corrupted by concurrency; control-plane content from an implementation head influences later execution; **reviewer instructions from a candidate branch govern that candidate's review**. **Kill criterion:** stop if agents perform acceptably manually but Freeside does not materially reduce attention burden after a reasonable integration pass; elaborator weakness alone is not a kill.

## 13. Decisions log

Each revision's material changes are recorded here with deciders. Revision 4 folded in five delta sets: optimization telemetry closure; workflow initiators (restoring autonomous initiation that revision 3 had narrowed unintentionally); a fourth external review (client synchronization and conversations, reviewer instruction poisoning, cross-store durability, trusted verification recipes, plus corrections); three standing user constraints (portability, autonomy preservation, operational simplicity); and the naming stack.

Held from revision 3 (abbreviated): daemon owns workflow state, clients thin; GitHub owns source/review/merge; chat authors artifacts; gates daemon-native; Go + SwiftUI; monorepo; verification defines completion; personal scope; capabilities at spawn; control-plane from approved commits; digest-bound artifacts and approvals; SQLite + inbox/outbox; polling-first; capability-classed runners, no silent downgrade; SwiftUI bootstrap exemption; native vendor tooling with leases and API-key fallback; cross-provider review as routing hypothesis; attention inbox as control system; elaboration in scope, severable (user); devlog cadence split; materiality-gated plans; provisional API; Phase 0 deleted into 1A gates; yield-driven review with separated crash retries; finding classifier named; ReviewSource with CodexGitHubReview; control-plane-only review triggering (user); CI trust profiles, audited_same_repo default, no self-hosted runner; hostile import and clean verification; credential modes; effectively-once; per-resource reconciliation; type-specific version-bound attention actions; mechanical/substantive/safety false-ready; 1A/1B exit split.

New in revision 4 (decider in parentheses):
1. **Client sync and conversation model** (Section 5.14): revision/epoch, snapshots plus invalidations, device pairing, command idempotency, conversation/message/invocation domain, discuss transactions, ten permanent sync tests. (Review 4; cached read-only view with freshness banner added, this revision.)
2. **Reviewer-instruction poisoning closed**: nested instruction paths publish-blocking in the ordinary workflow; control-plane change path required; auto-review is not independent review for PRs modifying its instructions. (Review 4; punctured revision 3's "reads through GitHub" parenthetical.)
3. **Coherent backup invariant**: blob-before-row ordering, restorable-only-with-closure, digest-verified restore, new epoch after rollback; orphan GC respects retention. (Review 4; retention amendment this revision.)
4. **Trusted verification recipes**, with the named residual that candidate test code still executes inside the contained envelope. (Review 4; residual named this revision.)
5. **Corrections batch**: 1A credential-criterion wording; agent_question and publish_blocked actions; capability retries from predefined manifests; preference-to-policy via proposal PR; open-PR-as-navigation with reconciliation-driven resolution; spec revision diff cards; immutable raw findings with annotation-only classification and no-dismissal defaults; intake scanner; 1A CI audit scoped to one reviewed profile plus drift detection; fake driver and review source as permanent fixtures; build order adopted. (Review 4.)
6. **Initiators restored**: manual/label/scan with propose|auto_start, run_proposal items, chaining on the Phase 2/4 horizon; revision 3's narrowing recorded as unintentional drift. (This revision; user prompt.)
7. **Optimization telemetry closed**: relational trace store, timing chains, fault-class capture, defect back-links, shadow-review sampling as the routing comparison's data source. (This revision; user prompt.)
8. **Portability as principle**: Linux a supported class, no Apple-only daemon dependencies, Linux CI from day one, cloud exposure documented. (User.)
9. **Autonomy preservation as principle**: interruption budget with planned/exceptional tagging, exceptional rate as tracked health metric, self-service rule, rein posture as maturity dial. (User.)
10. **Operational simplicity as principle with committed targets**: setup/onboard/doctor, manifest-flow App creation, permissive first-run defaults with honest class reporting. (User.)
11. **Naming stack**: Freeside; category "an agent control plane"; register "the harness runs the agent; the reins are yours"; subsystems envelope, signet, gauntlet; `rein:` as the gate-posture policy key. (User.)

## 14. Risks

Provider-credential exposure in subscription_contained (documented, egress-bounded, secret-scanned; api_key_isolated is the escape; elevated on cloud hosts); CI privilege crossing (trust profiles, drift detection, fail-closed publish, runner prohibition); reviewer-instruction poisoning (publish-blocking paths, control-plane routing); Codex cloud review as load-bearing dependency (local reviewer hedge; fail-closed review status); classifier mislabeling (immutable raw findings, sampled QA, no-dismissal defaults, ceilings, dispute items); subscription-terms drift (native tooling, leases, API-key fallback); Apple container immaturity and network-boundary uncertainty (fail-closed conformance, host hardening, capability classes, Linux class as pressure valve); vendor CLI churn (pins, contract tests, deliberate upgrades); review saturation and shallow approval (WIP caps, digest binding, false-ready tracking, revision-diff cards); interruption creep (budget metric, self-service rule); setup and upkeep burden (Section 10 targets, doctor); sync complexity creep (snapshot model is deliberately minimal; delta protocols deferred); solo scope (1A small, production-shaped, go/no-go); prompt injection as the organizing threat (no write credentials in workspaces, gauntlet, control-plane overlays, publish-blocking instruction paths, egress profiles, gates before irreversible actions, budgets, brakes).

## 15. Naming and references

**Freeside** (freeside.ai, github.com/freeside-ai); *free as in bird*; category line "an agent control plane"; self-brand "the harness runs the agent; the reins are yours." Subsystems: the **envelope** (runner and safety boundary), the **signet** (attention and approval service), the **gauntlet** (import and verification path); daemon **freesided**; clients take functional names. Reference shelf: Anthropic devcontainer/Agent SDK/credential docs; OpenAI Codex SDK, sandbox design, cloud review docs; Apple container docs and issue tracker; Firecracker/Kata; Litestream; Antfarm, Nimbalyst, Conductor, Gas Town/Beads (cautionary); agentclientprotocol.com (Phase 3).
