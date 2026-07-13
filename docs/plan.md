---
title: Freeside Project Plan
revision: 7
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

The system runs on a Mac Studio (the supported reference deployment; the daemon core is Linux-portable, Section 3.3). A work item (a labeled issue discovered by an intake scanner, a scan-proposed candidate, or a hand-approved spec submitted through the manual initiator) flows through a staged workflow: elaboration to a spec, over daemon-fetched research artifacts; spec approval in my attention inbox; implementation in an isolated workspace holding no GitHub credentials; export across a proven post-agent workspace handoff and an out-of-process hostile import boundary into a fresh checkout; deterministic verification, including evidence capture, under a trusted recipe in a clean workspace; daemon-side publication under an audited trust profile; yield-driven independent review with emergency brakes; a ready-for-final-review item on my phone with mechanical evidence. I review and merge on GitHub. The attention inbox is part of the control system: a daemon-owned domain model with lifecycle, staleness, synchronization, and concurrency rules. GitHub owns code and review; Freeside owns workflow execution and approvals.

Freeside is justified entirely as a personal-leverage tool; the metric is personal net leverage after maintenance. The usefulness of agent elaboration, implementation, and iterative review is established by my current manual workflow. **The open question is whether that workflow can become a safe, durable, low-attention system without moving the danger into provider credentials, CI, artifact import, stale approvals, or interruption creep.**

Success claims: (1) **attention reduction** against a passively logged baseline; (2) **decision quality preserved**; (3) **safety invariants hold** (Section 12); (4) **autonomy preserved** (exceptional-interruption rate stays low and trends down, Section 3.2).

## 2. Goals and non-goals

### Goals

1. **Attention routing as a first-class system**: durable AttentionItems with lifecycle, type-specific actions, optimistic concurrency, cross-device synchronization, per-delivery tracking with honest status semantics, push notification, self-contained decision cards, on iPhone and Mac.
2. **Elaboration in the tested value proposition**, severable, over **daemon-fetched research artifacts** (Section 5.4). (Decider: user.)
3. **Autonomous initiation**: initiators (manual, label, scan) under propose or auto_start modes.
4. **Yield-driven review remediation**; round counts are emergency brakes only.
5. **Bounded execution isolation**: capabilities fixed at spawn, no GitHub credentials in any workspace, declared per-run credential modes, named per-stage egress profiles with honest risk classes, a **proven post-agent workspace handoff**, an out-of-process hostile import boundary, trusted verification recipes.
6. **CI privilege containment**: agent-authored code never reaches secret-bearing or privileged CI, **and never modifies automation-control paths through the ordinary workflow**; trust profiles attest effective PR-job authority.
7. **Remote operation from iPhone**; human role is judgment at gates plus final review and merge on GitHub.
8. **Chat authors artifacts; the engine executes artifacts.**
9. **Verification defines completion**, deterministically, under a trusted recipe, in a clean workspace; visual evidence is captured by the verifier, never claimed by the implementer, with **machine-enforced provenance** (Section 5.15).
10. **Durability of decisions**: committed decisions survive restart; external effects converge; database and artifact store restore coherently from complete, **encrypted** checkpoints; clients converge on daemon state.
11. **Instrumentation sufficient for agent-driven optimization** (Section 8).
12. **Operational simplicity as a 1A exit criterion**, automated only after the interfaces survive real use; privileged installation is a narrow elevation boundary, and the daemon never retains root. (Decider: user.)

### Non-goals

1. Not an IDE; not a review surface; no auto-merge, ever.
2. Not a product: no multi-tenancy, billing, or hypothetical users.
3. **Not a harness**: Freeside consumes vendor harnesses through their sanctioned batch interfaces and never owns a model loop.
4. Not runtime self-modification; control-plane config is never hot-modified.
5. Not automatic provider fallback; not voice; not a pipeline DSL; not briefings until earned.
6. Not a formal pre-build validation study; not a generic CI security auditor.
7. Not a general-purpose sync platform: server-authoritative snapshots suffice; no client-facing event log, no CRDTs.

## 3. Operating principles

### 3.1 Autonomy inside the ward

Autonomy is the default; gates exist only at trust-boundary crossings and the two designed judgment points. **Repeated exceptional interruptions trigger a policy review; eligible low-risk repetitions may produce a policy-change proposal; safety invariants and non-waivable gates never auto-promote or offer bypass.** Non-waivable classes: GitHub credential separation; CI trust-profile validity; candidate automation-control and reviewer-instruction changes; control-plane modifications; stale-approval rejection; failed runner conformance (including the workspace-handoff gate); host reachability; artifact integrity failures; secret detection; capability escalation outside approved manifests.

### 3.2 The interruption budget

Every AttentionItem is tagged planned_gate or exceptional; the exceptional rate is a tracked health metric and a rising rate is a defect to fix, subject to 3.1. **Self-service rule:** recurring eligible classes must offer a path resolving the class, always via the control-plane proposal path. **Rein is a convenience preset, never a security dial**: it expands into explicit resolved policy at run creation, stored with digest and per-key provenance; explicit keys override preset defaults visibly. Known hot spot, accepted: Freeside's own repo routinely touches control-plane paths.

### 3.3 Portability

macOS is the supported reference deployment; the daemon core is Linux-portable and continuously built and tested on Linux from day one; Linux becomes supported only after one named distribution, architecture, and `linux_vm` backend pass the complete setup, conformance, execution, recovery, and upgrade suite. Cloud notes: provider credentials on a cloud host add exposure recorded in the residual-risk documentation. (Decider: user.)

### 3.4 Simplicity

Setup, onboarding, and upkeep are designed features with committed targets (Section 10). Permissive first-run is the **attended_dev** operating mode (Section 5.7), honest, never a bypass. Strict settings gate **unattended** operation, always.

## 4. The attention model

**AttentionItem**: id, project_id, subject {subject_type: run | proposal_batch | project | system, subject_id, run_id?}, type, priority, reason, requested_decision, evidence_snapshot (engine facts; only verifier/daemon artifacts under an approved recipe, Section 5.15), agent_claims (labeled), artifact_digests, pr_head_sha, item_version, interruption_class, conversation_id?, derived timing aggregates, expires_when, status. Cards render image attachments directly from the artifact store by digest.

**AttentionDelivery** (per attempt): item_id, device_id, channel, attempt, **submitted_at, channel_accepted_at, opened_at**, delivery_status. A channel provider's acceptance is never called "delivered"; only a real device-level receipt earns stronger language. The product metric is open-to-decision time. Item timing fields are aggregates derived from deliveries.

**Phase 1 types**: spec_approval, execution_failure, agent_question, review_diminishing_returns, review_dispute, ready_for_final_review, publish_blocked, run_proposal, system_health, plus a consolidated **blocked** item for external-wait thresholds (Section 5.12).

**Actions** (approve is not universal):
- **spec_approval**: approve / request changes / discuss / stop. Full rendering; revisions show the diff against the last version reviewed and prior comments with claimed addressals.
- **review_diminishing_returns**: finish now / apply current batch then finish / continue under specified policy / convert recurring preference into project policy (proposal PR; never mutates policy).
- **review_dispute**: adjudicate finding / discuss / stop.
- **execution_failure**: retry / retry with a predefined, policy-allowed capability manifest / discuss / stop.
- **agent_question**: answer and retry / answer without retry / stop.
- **publish_blocked**: rerun trust evaluation / choose an approved alternate publication profile / inspect trust failure / stop.
- **ready_for_final_review**: open PR (navigation, not resolution) / return to agent with feedback / mark_seen / dismiss / stop. Active until merge/close is observed, work is returned, or dismissal.
- **run_proposal**: start / **start with changes** (user changes produce a revised proposal artifact; the original item is superseded; a new item version is created; the run starts from the exact revised digest, never from unversioned ad hoc parameters) / decline / snooze; batched under a proposal_batch_id with per-candidate decisions.
- **system_health**: acknowledge (**means seen, never resolved**) / run doctor / stop unattended operation. A system_health item remains blocking until the underlying diagnostic clears, unattended operation is explicitly stopped, or a new validated configuration supersedes the condition.

**Lifecycle:** approvals bind to digests and head SHA; changed inputs invalidate; retries supersede failures; resolutions are transactional and version-checked; stale submissions receive a conflict plus the replacement item. Notifications are read-only hints. Fault-class capture is suggested, one-tap corrected, unknown permitted. WIP caps on runs and initiatives; GitHub Projects is the passive all-work view.

## 5. Architecture

### 5.1 Overview

```
GitHub (source, issues, PRs, reviews, checks, merge; Codex cloud review)
   │  per-resource reconciliation + intake scanners (no global cursor)
   ▼
freesided (Go daemon; launchd/systemd; dedicated user; never root)
   ├─ event inbox: reconciled state, intake scanners, cron, manual (idempotent)
   ├─ workflow engine: code-defined state machines; policy from config;
   │   resolved rein policy digested per run; active/elapsed/waiting clocks
   ├─ signet: items, deliveries, conversations, sync, device pairing, ntfy
   ├─ research fetcher: daemon-fetched, digest-addressed research artifacts
   ├─ StageDriver: Claude batch execution (+ permanent fake driver)
   ├─ ReviewSource: CodexGitHubReview (+ permanent fake source)
   ├─ finding classifier: annotations over immutable raw findings
   ├─ ward: capability classes incl. workspace-handoff capabilities;
   │   per-stage egress profiles; attended_dev|unattended; conformance
   ├─ gauntlet: OUT-OF-PROCESS worker (export normalization, hostile import,
   │   evidence validation) -> fresh checkout -> clean verifier (recipe,
   │   evidence capture)
   ├─ git/publish: owns ALL GitHub credentials; deterministic identities;
   │   invocation-ID reconciliation; EvidencePublisher (1B)
   ├─ store: SQLite (WAL, synchronous=FULL) + inbox/outbox + content-
   │   addressed artifact store; encrypted checkpointed backups (5.10)
   └─ sync API: atomic snapshots + revision/epoch + invalidations
   ▲
   ▼
Freeside app (SwiftUI, macOS + iOS): inbox, decision detail, run timeline;
   platform-protected caches (5.14)
```

### 5.2 The daemon

Go, single static binary, supervised, dedicated user, state and credentials inaccessible to other accounts, privileged services bound to loopback/Tailscale only. **The daemon never runs as root; one-time privileged installation steps (user creation, launchd installation) live in a narrow elevation helper.** SQLite: WAL, synchronous=FULL, foreign_keys=ON, configured busy_timeout. CI builds and tests on macOS and Linux; macOS jobs stay lean.

### 5.3 Execution: StageDriver and ReviewSource

Stages are bounded batch jobs. Every external start takes a daemon-generated invocation_id and is reconcilable by it (start/inspect/stream/cancel/collect; request_review/inspect/poll/verify). **Guarantee: one committed invocation intent and at most one accepted result; the workflow never advances twice.**

Phase 1: one local driver (**Claude**); one primary review source (**CodexGitHubReview**); permanent fakes of both. The 1B shadow-review arm is a Claude-driver review stage (fresh context, same head; findings recorded, never routed), doubling as the local-reviewer dry run. **Review triggering is exclusively control-plane** (decider: user); fail-closed to an attention item. Factual grounding: nested AGENTS.md reviewer guidance is documented Codex behavior; auto-re-review of remediation heads is a standing 1B integration test; the Claude setup token's inference-only scope is contract-tested against the pinned CLI.

**Session durability contract:** transcripts and artifacts durably recorded; workflow recovery guaranteed from stage inputs, workspace state, and artifacts; provider resume best-effort. Capabilities fixed at spawn; insufficiency means a typed request and exit.

### 5.4 Credential modes, egress profiles, and concurrency

**No GitHub write credential ever enters any workspace.**

**Credential modes** (declared, recorded, per run): **subscription_contained** (Phase 1 default; native vendor CLI in the agent VM; read-only credential mount where permitted; exposure accepted as documented residual risk), **api_key_isolated** (Phase 2, supported), **local_trusted** (explicitly trusted inputs only). **Export scanning is stated honestly: best-effort secret scanning of supported textual formats, with size, type, provenance, and publication controls for opaque artifacts** (universal detection across arbitrary encodings and images is impossible; Section 5.15 carries the image residual).

**Egress profiles** are per-stage control-plane policy over the credential-mode floor, and their risk classes are distinct, not interchangeable:
- `provider_only` (default): provider-API reachability, nothing more.
- `provider_web_read`: **a materially wider credential-exfiltration surface, not the same residual as provider_only** (read-only HTTP still exfiltrates via URLs, headers, bodies, redirects, DNS while the provider credential shares the trust domain). Permitted only when explicitly recorded as the wider exposure, against a small trusted-domain allowlist.
- Clean verification workspaces have no network.

**The 1B elaborator default is neither: the daemon fetches research.** The elaborator runs provider_only; it emits typed fetch requests; the daemon's research fetcher retrieves URLs through its own allowlist and returns **immutable, digest-addressed research artifacts**; the elaborator is re-invoked with them (bounded iterations). This removes the credential-exfiltration surface from the injection-exposed stage and gives research inputs provenance, caching, and reproducibility (invocations bind to artifact IDs, not live web state).

**Provider concurrency is two controls, not one** (AuthIdentity {auth_store_mutation_lease, max_parallel_executions, refresh_strategy, supports_read_only_auth_snapshot}): auth-store mutation (refresh, login-state, config writes, store replacement) is serialized per identity; inference execution parallelism is a separate limit **established experimentally in 1B and exposed to WIP scheduling**. If the safe answer is one concurrent execution, that is a visible scheduling constraint, not a hidden lock. API-key fallback always available; native unmodified vendor tooling.

### 5.5 The CI trust boundary

Pushed agent branches can execute agent-modified scripts inside privileged Actions jobs; same-repo branch PRs lack fork-style restrictions; **a job's implicit GITHUB_TOKEN and OIDC identity are authority even when no secret is named in YAML.** Every onboarded repository carries an **automation trust profile**:

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
  workflow_audit_digest: sha256:...
  review: {mode: auto | framework_triggered, config_digest: sha256:...}
```

The audit attests **effective PR-job authority**: effective GITHUB_TOKEN permissions, OIDC availability, environment and deployment credentials, reusable workflows, local composite actions, self-hosted runners, package-publishing permissions, and any workflow consuming artifacts produced from untrusted PR code. 1A scope: one repository's machine-readable profile, reviewed manually once, digest-bound, drift fails closed. **Standing prohibition:** the daemon host is never a self-hosted Actions runner for any managed repo.

### 5.6 The gauntlet: workspace handoff, import, and clean verification

```
daemon-owned base repo ──exact base SHA──▶ agent workspace
agent exits ──▶ POST-AGENT WORKSPACE HANDOFF (5.7 gate): credential-bearing
   execution context terminated; workspace mounted READ-ONLY in a fresh
   credential-free context
export helper emits content blobs + normalized change manifest + evidence
manifest ──▶ gauntlet worker (unprivileged, out of process) validates
gauntlet ──▶ fresh daemon-owned checkout; daemon authors a clean commit
fresh checkout ──▶ clean verification workspace (no credentials, no network;
   trusted recipe runs checks and captures evidence)
verified candidate ──▶ git/publish ──▶ GitHub PR (under trust profile)
```

Two channels leave the workspace and never mix: the **repo-change manifest** (regular files only; symlink, submodule, special-file, unusual-mode, **automation-control (5.5), and reviewer-instruction (5.8) changes are publish-blocking**) and the **evidence channel** (typed artifacts with provenance, 5.15). The daemon authors its own clean commit; the importer never trusts workspace .git, hooks, config, or agent-written manifests; enforces base SHA, canonical paths, allowlists, size limits, control-plane restrictions, best-effort secret scanning per 5.4. Malicious manifest, blob, and evidence fixtures are permanent suite members. **Trusted verification recipes** load only from approved control-plane config or the trusted base commit; verification-control file changes are mechanically identified, risk-flagged, and gated. Named residual: candidate test code executes inside the warded verifier.

### 5.7 The ward: runners, handoff gate, and operating modes

Backends declare capabilities; policy states minimums; no silent downgrade. **New named capabilities: supports_detachable_workspace, supports_post_exit_export, supports_read_only_remount, supports_credential_volume_detach, supports_workspace_snapshot.** **The first ward implementation gate:** write files in an agent workspace, terminate the credential-bearing execution context, mount the workspace read-only in a fresh credential-free context, and export it without exposing provider credentials, daemon state, or host credentials, proven against the actual runtime (candidate mechanisms: detachable volume, host-controlled block image, snapshot/export, separate export VM). **The honest fallback** (terminate the agent process, detach credentials, export in the same VM) **is a different, weaker isolation class, declared as such, never described as fresh-context handoff.**

**Operating modes:** **attended_dev** (weaker runner class permitted; auto_start, automatic publication, and unattended escalation disabled; honest isolation claims) and **unattended** (conformance success including the handoff gate, valid trust profile, approved credential mode, runner minimums, current backup health including encryption status, no blocking system_health item). Conformance cadence: full suite at startup, after configuration changes, and on doctor's schedule; lightweight probe before each unattended job. Golden images pin CLIs; workspaces on VM-local disk. Bootstrap exception: SwiftUI work is exempt until a macOS execution class exists.

### 5.8 Control-plane trust

Workflow config, prompts, policy, egress profiles, trust profiles, verification recipes, and materiality rules load only from an approved default-branch commit; running stages snapshot trusted-config digests; workspace copies are data. Vendor auto-loaded instructions are control-plane: trusted-base overlays; agent-modified instruction files are diff content, always risk-flagged. **Reviewer-instruction poisoning (blocking):** any reviewer-instruction path (AGENTS.md at any depth, AGENTS.override.md, .codex/**, peers) is publish-blocking in the ordinary workflow; **auto-review is not independent review for a PR that modifies the instructions governing it.** Detection is mechanical in the gauntlet.

### 5.9 Durability: effectively-once

Authority: GitHub owns source/issues/PRs/reviews/checks/merge; SQLite owns workflow state, decisions, attempts, events, routing, conversations, audit; the artifact store owns immutable inputs/outputs; providers hold transient session state; repo docs hold promoted decisions. Inbox/outbox on all external actions. **Committed workflow decisions survive restart; external effects converge to one intended result through deterministic identities, reconciliation, and bounded retry.** Kill-before/kill-after tests are permanent.

### 5.10 Coherent backup: encrypted checkpoints

Local ordering: blob → digest verify → fsync → atomic rename → referencing row. Missing referenced blobs fail closed; orphans GC per retention. **Restore points are complete checkpoints** (BackupCheckpoint {checkpoint_id, sync_epoch, server_revision, sqlite_snapshot_digest, artifact_manifest_digest, timestamps}; completion marker last; restore only from completed checkpoints; verify all digests before unattended work; new sync_epoch after rollback). **Confidentiality is policy** (BackupPolicy {encryption_mode, key_id, destination, retention_by_artifact_class, last_completed_checkpoint, last_restore_test}): remote checkpoints are encrypted; the key lives outside agent environments; backup credentials are never mounted into workspaces; **GitHub App keys and provider credentials are excluded from checkpoints** unless separately encrypted under a stronger recovery design, so a recovery may require reauthentication; raw transcripts have shorter retention than decisions, approved specs, and audit events; doctor checks checkpoint age, encryption status, artifact closure, and restore-test age. Encrypted backup is required before unattended mode touches private repositories with remote replication; a local-only development checkpoint may precede it.

### 5.11 GitHub integration: reconciliation plus intake

Per-active-resource state reconciliation with conditional requests; intake scanners discover new work (overlap plus idempotent identity). Webhooks are Phase 2, only if latency hurts.

### 5.12 Workflow definition, initiators, and artifacts

Go state machine; YAML is policy only; crash retries separate from remediation. **Budget clocks are three, not one: active-compute budgets (stage_active_time: per stage attempt; run_active_compute_time: whole run), an elapsed deadline for abandoned workflows, and waiting thresholds that raise a consolidated blocked item rather than terminating** (a run waiting overnight on a reviewer must not burn compute budget). review.hard_active_time counts active review/remediation time, not calendar waiting.

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

`rein` resolves into digested per-run policy with per-key provenance. **Manual initiation is `freesided submit`**, registering the approved spec as a digest-addressed artifact and creating the run. auto_start is bounded by WIP caps; conservative default is propose. **Findings:** raw findings immutable; classification is a versioned annotation; low-confidence materiality defaults to continuation or human attention; the classifier cannot declare a finding fixed. Artifacts are typed, immutable, digest-addressed; approvals bind to digests. A DSL waits for three genuinely different shapes.

### 5.13 Deterministic components

The engine runs verification, evidence capture, research fetching, card facts, evidence publication, and cleanup as policy jobs. Agents appear where judgment is the work: elaborator, implementer, remediator, diagnostic, finding classifier, shadow reviewer, later a briefer.

### 5.14 Client synchronization and conversations

**Authority and consistency.** The daemon is sole authority. Client databases are disposable read caches. Guarantees: transactional consistency in the daemon; optimistic concurrency; eventual convergence; read-your-write after acknowledgment; cached read-only view with a freshness banner while unreachable; consequential actions disabled until current state validates.

**Revision, epoch, and cache semantics.** ServerState {sync_epoch, revision}; every client-visible transaction increments revision; restores issue a new epoch forcing cache discard. **A partial fetch never advances the whole cache**: clients track last_full_snapshot_revision and highest_observed_server_revision separately; each ResourceSnapshot carries as_of_revision and entity_version; /sync/bootstrap is one canonical snapshot in a single read transaction; a revision gap triggers full bootstrap or refetch of all potentially affected resources; a periodic revision heartbeat catches lost invalidations. Push and WebSocket improve latency only.

**Devices and commands.** Pairing via a short-lived code displayed or printed to the terminal on the daemon host (no display assumed); the daemon stores a credential hash or device public key, never reusable plaintext; revocation supported. Every mutation is a ClientCommand {command_id, device_id, expected_entity_version, expected_bindings, payload}; retries return the original result. Attachments upload through a digest-addressed endpoint; a retried upload converges on one artifact by digest (test 10). No offline approvals in Phase 1. **Client caches are part of the threat model:** cached metadata in platform-protected storage; Keychain only for device credentials; no long-term caching of high-sensitivity attachments by default; cache eviction on epoch change; **revocation stops future access but cannot delete already-cached content, and the plan says so** rather than implying remote wipe.

**Conversations** are Freeside domain objects (Conversation, Message with daemon-assigned sequence, AgentInvocation binding explicit input IDs). Messages immutable; corrections are new messages. **Phase 1 conversation sync is whole-snapshot per conversation.** Text in SQLite; attachments in the artifact store by digest.

**Discuss semantics.** One transaction: append message → record item version and bindings → supersede/transition → write AgentInvocationRequested outbox intent with invocation_id → record command result → increment revision. **Recovery: exactly one accepted invocation result; the workflow never advances twice.** Agent completion: finalize and fsync blobs; then one SQLite transaction (message, transition, replacement item); failed transactions leave harmless orphans. Live streaming and mid-turn steering are Phase 3.

**Sync and device tests (permanent, in 1A), sixteen:** (1) cross-device resolve with second-device conflict; (2) an offline device's submission against a superseded version is rejected with the replacement item; (3) all notifications missed, foreground refresh reconstructs the inbox; (4) a lost HTTP response retried by command_id returns the committed result; (5) discuss commits and the daemon dies pre-invocation: recovery produces exactly one accepted invocation result and the workflow does not advance twice; (6) an agent response arriving with both clients closed is later retrieved by both as the same ordered thread; (7) concurrent discuss on one item version: one wins, no second accepted result; (8) a restored daemon issues a new epoch and clients discard newer cursors and bootstrap; (9) a late notification for a resolved item deep-links to canonical state and exposes no stale action; (10) a retried attachment or message yields one artifact and one message; (11) a partial entity refetch does not mark the whole cache current (revision-gap test); (12) a conversation status change reaches a client that had already fetched past that sequence; (13) an expired or consumed pairing code cannot create a device; (14) simultaneous pairing attempts with one code yield one device; (15) a revoked device cannot submit a previously prepared, uncommitted command; (16) a command retry after revocation may return its recorded result but produces no new side effect.

### 5.15 Evidence and images

Four rules, now machine-enforced through provenance:

1. **Capture belongs to the verifier**, per the recipe's capture block, in credential-free, network-free rooms (before at base SHA, after at candidate). Capture skills are not loaded in agent workspaces. Clean-room capture is the pixel-side secret mitigation; **pixels are invisible to text scanning** (OCR is a recorded Phase 2 deferral).
2. **Every artifact carries provenance**: {producer_class: verifier | agent | daemon, producer_invocation_id, source_head_sha, verification_recipe_digest?, sensitivity_class, publish_eligible}. **Only verifier or daemon artifacts produced under an approved recipe may enter evidence_snapshot; agent-generated images appear only in labeled claims; agent-generated opaque files are never automatically uploaded to GitHub; publish_eligible is computed by trusted policy, never supplied by the agent.** A new remediation head invalidates prior-head evidence unless explicitly head-independent; the publisher verifies head binding before publication.
3. **Images are opaque blobs to the daemon**: magic-byte, type, and size validation only; nothing server-side decodes an image; rendering happens only in clients and on GitHub.
4. **Publication is the EvidencePublisher** (git/publish), under effectively-once discipline (digest-derived names, check-before-create, deterministic PR-section markers). **Deferred to 1B**: the first repository is deliberately non-UI (Section 11), so 1A ships the artifact schema, provenance enforcement, and client rendering; the external publisher lands with the first evidence-bearing workflow.

## 6. Verification

Deterministic, engine-run, clean-room, recipe-trusted, including evidence capture; outputs are run-bound artifacts cited by cards. "Done" is verification. False-ready tracking per Section 12 from day one.

## 7. Review policy

Independent error detection is the goal; provider diversity is one mechanism. Routing comparisons run over accumulated traces including the 1B Claude-driver shadow arm (findings recorded, never routed), adjudicated blind where practical. **Safety override, with the classifier never sole gatekeeper: a raw shadow finding claiming critical or high severity that the classifier scores low-confidence does not silently drop; it receives a second adjudication (deterministic or a distinct agent) or becomes an attention item. A credible critical/high shadow finding blocks ready status.** Contamination accepted. Freeside does not pass or fail on routing results.

## 8. Observability and optimization telemetry

Typed relational rows with stable join keys; transcripts as drill-down pointers. Per run: stage, all governing digests with per-key preset/override provenance, driver, credential mode, egress profile, operating mode, artifacts with provenance, tokens and cost, active and elapsed clocks, attempts, review rounds and yields, classifier samples, shadow results, outcome, decisions. Closed-loop attribution (defect issues reference producing runs; suggested fault classes). Attention telemetry: AttentionDelivery rows with honest status fields; open-to-decision time as the product metric; interruption-class rates. Baseline logging passive and concurrent with 1A. Usage is observed telemetry, never claimed quota state.

## 9. Comprehension

Evidence packets first; altitude lines once plans are structured artifacts. Plan changes gate by materiality; wording changes are recorded, not interrupted for; materiality rules are control-plane policy. Devlogs split by cadence; shared promotion channel. Briefings and querying are Phase 3, only if demanded.

## 10. Operations and onboarding

The binary is the installer, built after interfaces survive real use: `freesided setup` (privileged steps in a narrow elevation helper; the daemon never retains root), `freesided onboard <repo>` (trust profile with effective-authority attestation for one-time manual review, recipe detection, project image), `freesided doctor` (conformance, handoff gate, backup health including encryption and restore-test age; scheduled; files system_health items), `freesided submit`. GitHub App via the manifest flow, key landing directly in protected storage. Defaults: hosted ntfy; embedded SQLite; single config directory; attended_dev with honest class reporting. **Targets, as 1A exit criteria, verified against a clean VM or spare machine:** fresh machine to first run under one hour; repo onboarding under thirty minutes with exactly one manual step.

## 11. Roadmap, build order, and coordination

### The first repository is deliberately boring

**Not Freeside.** Freeside constantly touches control-plane paths and is the hardest possible test case; it becomes the bootstrap *test* once the path works, not the initial obstacle. The first repository: a small Go service or library; read-only PR tokens; no OIDC, environment secrets, deployments, or self-hosted runners; no UI screenshot requirement; dependencies baked into the image; direct `go test` / `go vet` verification; infrequent workflow or instruction-file changes.

### Phase 1A: the secure publish path, in three internal exits

**1A.0: control plane with fakes.** Fake run → attention item → resolve on iPhone → second device converges → conversation feedback → fake invocation → workflow transition. Exit: all sixteen sync/device tests pass; command retries idempotent; kill-before/kill-after with fakes; no container, Claude, publication, or backup complexity.

**1A.1: secure publication with a fake candidate.** Fake candidate → workspace handoff → gauntlet → clean verification → daemon GitHub publication → ready item. Exit: malicious fixtures contained; candidate automation-control and reviewer-instruction paths blocked; verification binds to exact recipe and head; PR creation converges effectively-once; checkpoint restore succeeds (local-only acceptable); attended_dev sufficient.

**1A.2: real unattended execution.** Claude → proven credential mode → **proven workspace handoff (the ward gate)** → gauntlet → clean verifier → audited publication → iPhone, via `freesided submit`, under manually configured unattended preconditions. Exit: runner conformance green including the handoff gate; no undeclared credential in any workspace; several real items complete without terminal intervention; then setup/onboard/doctor package the proven manual operations and meet the Section 10 targets.

Build order within the exits: (1) domain, sync, devices, fakes; (2) clients and the sixteen tests; (3) export/gauntlet/verifier with fake candidates, artifact store with checkpoint and provenance rules; (4) publication and reconciliation, kill tests; (5) ward with the handoff gate, then the Claude driver; (6) real items; (7) setup/onboard/doctor; (8) exit. **The workspace-handoff gate is investigated early and in parallel** (it is the largest runtime unknown) but blocks only 1A.2, never 1A.0/1A.1.

### Phase 1B: the useful workflow

labeled issue (intake scanner) → elaboration over daemon-fetched research artifacts → spec_approval → implementation → gauntlet → PR under trust profile → checks → Codex review, control-plane triggered → yield-driven remediation with pattern sweeps → diminishing-returns / dispute items → ready_for_final_review with yield history → merge on GitHub. Adds: elaborator with the research fetcher; intake scanner; ReviewSource with freshness verification and the auto-re-review test; finding classifier with sampled accuracy and the second-adjudication rule; convergence policy; shadow arm; **EvidencePublisher with provenance-gated publication**; experimental determination of max_parallel_executions per auth identity, exposed to scheduling; run timeline screen. Real backlog immediately. **1B exit:** no agent-window patrol; no manual polling; productive review rounds run unasked; consolidated low-value items; phone-decidable approvals; attention materially below baseline; exceptional rate low; false-ready per Section 12.

### Implementation coordination (building Freeside with agents)

Contracts and fakes are the coordination mechanism; CI keeps lanes honest. Wave 0 (serial): module, dual-platform CI, domain package, schema and migrations, outbox, interfaces, fakes, provisional API schema; domain/migration PRs exclusive; shared-interface changes are `kind:contract`. Wave 1 (parallel lanes): signet, gauntlet, publish, ward, saddle pair. Wave 2 (convergent): workflow engine, real driver, end-to-end fakes, real work; the **spine** role owns integration and contract adjudication. Width bounded by review bandwidth. **Each wave's exit includes a fresh-context adversarial review by an agent given only the repository and documents, never this design history.** The issue protocol lives in AGENTS.md's Coordination section; the 1A backlog doubles as elaborator fixtures.

### Phase 2: breadth and hardening

Second repository and workflow shape; scan initiators and chaining; local Codex driver if useful; api_key_isolated; full failure-injection campaign; restore drills; generalized (bounded) CI audit tooling; richer classification; webhooks if latency hurts; APNs; registry-capable egress profiles; provider_web_read where explicitly accepted; OCR-based image scanning if warranted; risk-classified cards; the Linux deployment matrix if wanted.

### Phase 3: comprehension and interaction

ACP interactive attachment; best-effort resume; material plan-change gates; briefings; usage display and evidence-informed routing; WIP and initiative views; auto_start maturation.

### Phase 4: generalization

Pipeline DSL (after three shapes); more agents and skills; macOS runner class; App Intents, widgets, Live Activities; voice; open-source packaging decision.

## 12. Exit criteria definitions

**Mechanical false-ready** (zero tolerated): the card asserted something objectively stale or false. **Substantive false-ready** (zero critical/high; lesser misses recorded): a material in-scope failure the automation should reasonably have caught. **Safety failure** (any blocks unattended use): workspace obtains a GitHub write credential; agent reaches a privileged host service; output escapes gauntlet constraints (either channel); untrusted PR code receives privileged CI authority (secrets, writable tokens, OIDC) without an explicit gate; **candidate automation-control changes reach publication through the ordinary workflow**; a stale mobile decision takes effect; a crash produces uncontrolled duplicates or advances a workflow twice; provider auth corrupted by concurrency; control-plane content from an implementation head influences later execution; reviewer instructions from a candidate branch govern that candidate's review; a known credible critical/high shadow finding is disregarded; **an unencrypted checkpoint replicates off-host once encryption is required**. **Kill criterion:** stop if agents perform acceptably manually but Freeside does not materially reduce attention burden; elaborator weakness alone is not a kill.

## 13. Decisions log

Material changes are recorded here per revision, with deciders in parentheses. This section holds only the current revision's items: when a new revision lands, the outgoing items move to docs/history/decisions.md, which holds the complete log of every revision, including revisions superseded before commit. The history file is appended in the same PR as any plan revision, so the two cannot drift. A decision promotes to a docs/decisions/ ADR on first re-litigation, citing its revision entry in the history.

Revision 7:
1. **Candidate-automation policy**: automation-control paths publish-blocking in the ordinary workflow; trust profiles attest effective PR-job authority (implicit token, OIDC, environments, reusable/composite actions, artifact-consuming privileged jobs); new safety-failure entries. (Review 6.)
2. **Post-agent workspace handoff is a named capability set and the first ward implementation gate**, proven against the actual runtime; the same-VM fallback is a declared weaker class, never described as fresh-context. (Review 6.)
3. **provider_web_read reclassified as a materially wider credential-exfiltration mode**; the 1B elaborator default is **daemon-fetched, digest-addressed research artifacts** via typed fetch requests, chosen from the review's options for security plus provenance and reproducibility. (Review 6; option selection this revision.)
4. **Secret-scanning language corrected to best-effort supported-format scanning** with provenance and publication controls for opaque artifacts. (Review 6.)
5. **Auth-store mutation leases separated from inference concurrency**; max_parallel_executions established experimentally in 1B and exposed to scheduling. (Review 6.)
6. **Three budget clocks**: active-compute budgets, elapsed deadlines, and waiting thresholds raising consolidated blocked items; review ceilings count active time. (Review 6.)
7. **Artifact provenance with trusted publish_eligible**; evidence_snapshot restricted to verifier/daemon artifacts under approved recipes; head-binding invalidation; agent opaque files never auto-uploaded. (Review 6.)
8. **EvidencePublisher deferred to 1B**; schema, provenance enforcement, and client rendering remain 1A. (Review 6.)
9. **Backup confidentiality policy**: encrypted checkpoints, keys outside agent environments, credential exclusion with recovery-may-reauth, per-class retention, doctor checks; required before unattended remote replication of private repos. (Review 6.)
10. **Four pairing/revocation tests added; suite is sixteen.** (Review 6.)
11. **system_health is condition-driven** (acknowledge means seen); **start-with-changes is versioned through a revised proposal artifact**; **notification statuses are submitted/channel-accepted/opened**, never claimed delivery; **client caches protected, with revocation-versus-deletion stated honestly**; **setup elevation is a narrow helper and the daemon never retains root**. (Review 6.)
12. **1A formalized into internal exits 1A.0/1A.1/1A.2**, with the handoff gate investigated early but blocking only 1A.2. (Review 6.)
13. **The first repository is deliberately boring, not Freeside**; Freeside becomes the bootstrap test after the path works. (Review 6; reverses this conversation's earlier self-hosting instinct.)
14. **The classifier is never sole gatekeeper of the shadow safety override**: raw critical/high claims get a second adjudication or an attention item regardless of classifier confidence. (This revision, sharpening review 6.)
15. **Naming amendment: the runner subsystem is the ward** (formerly envelope, which broke register with signet/gauntlet/daemon and greps poorly against message/HTTP-envelope vocabulary); the generative naming rule is restated as the binding-and-summoning register with mundane surface readings, replacing the riding-tack line, which no longer described practice. The flight-envelope concept survives as explanatory prose only. (User.)

## 14. Risks

Provider-credential exposure in subscription_contained (documented; egress floors; daemon-fetched research removing the widest surface from the most exposed stage; api_key_isolated as the escape); CI privilege crossing (effective-authority attestation, candidate-automation blocking, drift fail-closed, runner prohibition); reviewer-instruction poisoning; **workspace-handoff uncertainty** (the largest runtime unknown; gated, investigated early, honest fallback class); Codex cloud review as load-bearing dependency (shadow arm dry-runs the hedge); classifier mislabeling (immutable raw findings, second-adjudication rule, ceilings); subscription-terms drift; Apple container immaturity; vendor CLI churn; review saturation; interruption creep; setup and upkeep burden; sync complexity creep; image handling (provenance, opaque-blob rule, OCR deferral); backup confidentiality (encryption policy, credential exclusion); 1A scope: large but ordered into three internal exits; reviewer monoculture (per-wave fresh-context reviews); prompt injection as the organizing threat (no write credentials in workspaces, proven handoff, out-of-process gauntlet with two validated channels, control-plane overlays, publish-blocking automation and instruction paths, egress floors, daemon-fetched research, gates before irreversible actions, budgets, brakes).

## 15. Naming and references

**Freeside** (freeside.ai, github.com/freeside-ai); *free as in bird*; category line "an agent control plane"; self-brand "the harness runs the agent; the reins are yours." Subsystems: the **ward** (runner, handoff, and safety boundary), the **signet** (attention and approval service), the **gauntlet** (export, import, and verification path, including the evidence channel); daemon **freesided**; subsystem names come from the binding-and-summoning register: rare single-metaphor tokens with mundane surface readings (ward, signet, gauntlet, daemon); code takes functional names; rein appears only in brand and policy vocabulary. Reference shelf: Anthropic devcontainer/Agent SDK/credential docs; OpenAI Codex SDK, sandbox design, cloud review docs; GitHub Actions security hardening docs (token permissions, OIDC, pull_request_target); Apple container docs and issue tracker; SQLite online backup and WAL durability docs; Litestream; Antfarm, Nimbalyst, Conductor, Gas Town/Beads (cautionary); agentclientprotocol.com (Phase 3).
