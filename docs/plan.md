# Freeside

**Project charter, architecture, and roadmap.** Status: pre-implementation. This document is the starting point for the project and the reference against which changes should be argued. It encodes decisions made through analysis and research; the items marked as empirical remain open until Phase 0 closes them.

---

## 1. What Freeside is

Freeside is a personal system for running software development through autonomous, decomposed agent pipelines with human approval gates, on hardware I control, with my attention routed to the few moments that actually need it.

The one-sentence version: **a pipeline engine for agent work, with an inbox for my attention.**

The system runs on a Mac Studio acting as a permanent runner. Work items (bugs, features, enhancements) flow through staged pipelines: an agent elaborates the item into a spec, another implements it in an isolated container, deterministic verification runs, a different provider's model reviews the result, and a PR lands, with human gates at defined points and a final human review and merge on GitHub. A SwiftUI inbox app (macOS and iOS) surfaces the decisions waiting on me, each with enough context to decide from a phone. A comprehension layer (gated plan artifacts, devlogs, periodic briefings) keeps my mental model of each project current even though agents are doing most of the work.

Freeside is justified entirely as a personal-leverage tool. If the pipeline model works, it multiplies my throughput across every project I touch, indefinitely. A modest product or open-source release is a cheap option preserved by building as if others will read the code, exercised only on external signal, never a goal that shapes scope.

### Why it should exist despite a crowded field

The orchestration category consolidated rapidly in early 2026: Anthropic shipped Agent Teams and Claude Code on the web, OpenAI shipped Codex parallel sandboxes, and a wave of tools (Conductor, Nimbalyst, Vibe Kanban, Gas Town, Antfarm) now covers parallel-agent management. No existing tool provides the specific conjunction Freeside targets:

- Event-driven pipelines with **human approval gates** (Antfarm has pipelines but no gates, surface, or mobile story; Gas Town is gateless YOLO at industrial burn rates).
- **Cross-provider** stages using subscription plans, with the review stage deliberately run on a different provider's model than implementation.
- **Container-isolated** runners on my own hardware.
- An **approval inbox with resumable chat** on iOS.
- Provider-neutral **usage monitoring**.

The durable differentiator is the gates-and-attention layer over cross-provider pipelines on owned hardware. Everything else in the system should be expected to be commoditized by vendors and should be built accordingly (thin, replaceable, standard-shaped).

---

## 2. Goals and non-goals

### Goals

1. **Centralized attention routing.** One inbox, on desktop and phone, showing what needs me now: approvals, failures, completions. Not a workspace; a queue of decisions with context.
2. **First-class automation.** Push (webhooks) and poll (reconciler, cron) event sources spawning agents on events, so looping is owned by the system, not by me or by an agent burning context watching a PR.
3. **Decomposition across fresh-context stages.** Triage, spec, implement, verify, review, and cleanup as separate agent runs with compact artifact handoffs, chosen per stage for provider, model, and effort.
4. **Hard isolation.** Agents run with full tactical autonomy inside disposable containers scoped by repo, token, egress profile, and budget. Permissions are decided at spawn time by what the container holds, not by prompts.
5. **Mac Studio as runner; remote operation.** Full workflow drivable from an iPhone or laptop, with the human role reduced to taste, judgment, and gate decisions.
6. **Cross-provider on subscription plans**, with local usage accounting and manual routing rules.
7. **Chat as the authoring interface.** Conversations produce artifacts (issues, specs, plans, pipeline definitions); the engine executes only artifacts.
8. **Multi-project** from day one.
9. **Images both directions**: into prompts as files, out of stages as artifacts (screenshots from verification especially).
10. **Verification as a first-class pipeline concept.** Real test runs, screenshots for UI work, repro scripts as artifacts. Loop quality is bounded by verification quality.
11. **Agent-extensible.** Agents evolve the tool itself: pipeline definitions, stage prompts, and (with conventions) the daemon code.
12. **Comprehension preserved.** The system actively maintains my mental model (see Section 7) rather than optimizing me out of the loop and leaving me unable to handle exceptions.

### Non-goals

1. **Not an IDE, not a CLI.** Code is touched in other tools when necessary; the goal is not touching code.
2. **Not a review surface.** GitHub remains the system of record for code, PRs, issues, and the merge gate. Freeside does not rebuild diff review.
3. **Not a venture product.** No multi-tenancy, billing, onboarding, or design for hypothetical users. The moment scope is being added for imagined users, stop.
4. **Not automatic fallback routing** between providers (deferred until vendors expose quota state; v1 is accounting plus manual rules).
5. **Not voice** (deferred; revisit after the core loop runs daily).
6. **Not a general agent harness.** Freeside orchestrates vendor agents (Claude Code, Codex, and anything ACP-speaking); it does not own a model loop.

---

## 3. The human role

Two distinct jobs, served by two distinct surfaces:

**Keep agents flowing** (the inbox). Approve or reject gate items, unblock stuck runs, answer agent questions. Optimized for low latency and lock-screen decidability.

**Understand the whole** (the comprehension layer). Maintain a living model of each project's plan, decisions, and trajectory, so gate decisions stay good and exceptions can be handled. Served by gated plan artifacts, briefings, altitude context on every inbox item, and a queryable chat interface over project state. GitHub Projects boards, self-maintained via built-in workflows, serve as a passive all-work status view (distinct from the inbox's needs-me view).

Capacity limits are enforced on both: WIP caps on concurrent pipeline runs (review capacity) and on concurrent major initiatives (comprehension capacity, honestly two or three for one person). Exceeding either is a visible choice, not a drift.

---

## 4. Architecture

### 4.1 Overview

```
GitHub (system of record: code, issues, PRs, reviews, merge gate)
   │  webhooks + API (GitHub App, short-lived scoped tokens)
   ▼
freesided (Go daemon on Mac Studio)
   ├─ event bus: webhooks, cron, reconciler backstop
   ├─ pipeline engine: persisted state machines over stages
   ├─ session layer: ACP client, spawns adapter processes
   ├─ runner layer: Apple `container` (Docker fallback), per-stage micro-VMs
   ├─ token broker: GitHub App installation tokens, per-stage scopes
   ├─ egress proxy: allowlist profiles per stage
   ├─ store: SQLite (WAL) + Litestream replication
   └─ API: OpenAPI-defined HTTP + WebSocket event stream (Tailscale)
   ▲
   │  generated Swift client (swift-openapi-generator)
   ▼
Freeside app (SwiftUI, multiplatform macOS + iOS) + APNs (ntfy interim)
```

### 4.2 The daemon (`freesided`)

Go, single static binary under launchd. Owns all state and all agent processes; clients are thin. Rationale for Go over TypeScript (a revised decision, originally TS): the daemon is a security-sensitive unattended appliance (credential broker, years-long runtime), where Go wins on supply-chain surface, rot resistance (1.x compatibility promise), single-binary deployment, and per-goroutine fault isolation in the session multiplexer. TS's advantages (agent fluency, JSON ergonomics, ACP reference libraries) were neutralized by the SwiftUI client decision and by ACP adapters being language-independent subprocesses. Compensations for Go's weaker agent fluency: small single-responsibility packages, table-driven tests, golden-file tests on all JSON boundaries, and an AGENTS.md stating conventions.

**State.** SQLite in WAL mode as the coordination store; Litestream replicating to object storage for the irreplaceable slice (run history, traces, usage accounting). Durability comes from where authority lives, not the engine: GitHub holds code and work items; pipeline definitions and prompts are versioned files in repos; transcripts are JSONL on disk. Losing the daemon DB must mean losing analytics and in-flight runs, never work. A reconciler can cold-start the daemon from external state, and doubles as the backstop for GitHub's at-least-once (sometimes never) webhook delivery. Every stage transition is written before acted on; recovery replays state; all event handling is idempotent.

### 4.3 Sessions: ACP as the agent interface

The daemon is an ACP **client**. Each stage spawns an adapter subprocess (Zed's claude-code-acp for Claude, the Codex adapter, or any other ACP agent) speaking JSON-RPC over stdio, via a pinned Go ACP SDK (currently coder/acp-go-sdk, wrapped behind Freeside's own session interface so it can be regenerated or vendored if it lags the spec).

The governing model: **the session is the transcript on disk; the process is an ephemeral executor.** All session events append to a persisted log; clients subscribe over WebSocket with replay-from-cursor, so phone and desktop can attach mid-flight and daemon restarts lose nothing. A user message routes by liveness: live process gets the message (queued for next turn; mid-turn steering is not first-class in ACP and is a per-adapter behavior to verify), dead session gets resumed (session/load) into a new process. The UI treats both identically. Resume gets a verification step with transcript-seeding as fallback, since native resume has documented reliability gaps. Session state dirs (~/.claude, ~/.codex) are volume-mounted to durable storage; long transcripts age out in favor of fresh agents reading stage artifacts, by explicit policy.

**Per-adapter contract tests are load-bearing**: permission-request fidelity varies across ACP agents (Copilot CLI documented auto-approving without ever sending session/request_permission), loadSession is a capability flag defaulting to false, and parts of the schema are marked unstable. Pin adapter and SDK versions; upgrade deliberately against the contract tests.

### 4.4 Gates

Two kinds, only one of which is ACP:

**Between-stage gates** (the primary kind: approve spec before implementation, approve PR-ready work before review spend). Owned by the daemon's state machine. The stage completes, the process exits, the container dies, artifacts persist, the gate emits an inbox item, and the next stage spawns on approval. Nothing blocks waiting on a human.

**In-session gates** (a live agent pauses mid-context, e.g. plan approval during elaboration). Native ACP session/request_permission with custom options and rich markdown content; structured approval-card fields ride in `_meta`. The daemon persists the request, emits an inbox item, and answers the blocked call when I decide. Used only where interactive latency is expected.

**The approval card is a data contract, not a UX problem.** Every gate emission must include: a one-line requested action; a 3-to-5 line synthesized summary; a plan-altitude line ("implements step 3 of 6 of milestone Y; plan last changed <date>"); mechanical risk flags (test status, diff size, files touched, budget consumed); links to PR, run, and plan; and the decision set (approve / reject / discuss). A stage that cannot populate the card is a defective stage.

### 4.5 Runners: containers

Ephemeral `container run` micro-VMs via Apple's container 1.0 on the Mac Studio (sub-second start, one VM per container, OCI images), behind a runner interface that keeps Docker interchangeable. Note: Apple's "container machine" mode mounts the host home directory and is explicitly **not** used for agents; it defeats isolation.

Provisioning is three one-time investments, after which per-spawn cost is a template:

**Golden images.** `agent-claude` and `agent-codex` bases (pinned CLI + adapter versions, git, gh, common tooling), rebuilt on a schedule; per-project images extend a base with the project toolchain, devcontainer-spec shaped. Agents clone onto VM-local disk rather than working over virtiofs bind mounts (large performance gap, and better isolation); artifacts ship out at stage end.

**Token broker.** The daemon runs as a GitHub App and mints short-lived installation tokens per stage, scoped to the stage's needs (contents:read for review; contents:write + pull-requests:write for implement). No long-lived personal tokens anywhere; no SSH agent forwarding into autonomous containers; git credential env hardening in images. Vendor OAuth state (the crown jewels: subscription-granting tokens) lives in named volumes per provider, protected primarily by egress control.

**Egress profiles.** Enforcement lives outside the guest: an allowlist proxy on the host, with agent containers having no route except the proxy. In-guest firewalls are treated as soft guards only (documented cases of agents actively attacking their own firewall). Named profiles referenced by stage definitions: `vendor-only` (review), `vendor+github+registries` (implement), `vendor+github+web` (triage/research). Anthropic documents Claude Code's required domains; profiles are known lists, not guesswork.

### 4.6 Security model

The threat model is prompt injection into autonomous agents: issue bodies, PR comments, CI logs, and web content are instruction channels into processes with shell access. Everything observed by an agent is data, not command. Mitigations are architectural: container isolation, per-stage scoped credentials, egress allowlists, human gates positioned **before** irreversible side effects, per-run budgets (tokens, wall time, iterations) that halt rather than degrade, max-iteration counts and kill switches on every loop (two automated loops ping-ponging is a designed-against failure mode), and full audit traces. The containers protect the machine; the token scopes and egress profiles protect the accounts and repos; the gates and verification protect the codebase. The residual channel no boundary closes is legitimate output reviewed too fast, which is what WIP limits and the comprehension layer defend.

### 4.7 Pipelines

YAML files in `.pipelines/` per repo, versioned, chat-authored, engine-executed. Draft schema shape (Phase 0 refines against Antfarm source reading):

```yaml
name: bug-to-pr
triggers:
  - github: {event: issues, action: labeled, label: "agent:go"}
  - manual: true
stages:
  - name: elaborate
    role: elaborator          # role = prompt file + provider + model + effort
    egress_profile: vendor+github+web
    budgets: {tokens: 500k, wall: 30m, iterations: 3}
    outputs: [spec, devlog, approval_card]
    gate: approve             # none | notify | approve
    on_failure: {retry: 1, then: escalate}
  - name: implement
    role: implementer
    egress_profile: vendor+github+registries
    inputs: [spec]
    outputs: [pr, devlog, verification_report, approval_card]
    gate: notify
  - name: review
    role: reviewer            # cross-provider by policy
    egress_profile: vendor-only
    inputs: [pr, spec]
    gate: none
```

Roles are data referencing versioned prompt files, never code. Initial role set: **elaborator** (triage + repro + spec merged: repro context improves the spec and a handoff wastes it), **implementer**, **verifier** (separate from implementer on principle; mostly deterministic scripts with a cheap model wrapping them), **reviewer** (cross-provider frontier model), **briefer** (cheap model), **janitor** (a scheduled pipeline for branches, worktrees, containers, stale runs). Initial model mapping is a placeholder corrected by trace data.

### 4.8 Clients and API boundary

SwiftUI, one multiplatform codebase for macOS and iOS. The daemon's OpenAPI spec is a first-class artifact: swift-openapi-generator for the client, oapi-codegen for server stubs, event payloads defined as named versioned schemas in the same spec (the WebSocket channel is where drift will try to hide). Any spec change is treated as a migration. Notifications via ntfy initially, native APNs when the app is real. If a web-rendered view ever becomes necessary (diff rendering being the likely temptation), it is daemon-served HTML in a WKWebView; no JS toolchain enters the client. Remote access via Tailscale.

Screens for v1 (roughly eight): inbox; approval card detail; run detail (CI-run-page idiom: stage timeline with artifacts, not a chat log); session chat (attach to any run's session, live or resumed); project list; project status/briefings; usage dashboard; settings. Mobile diff review stays on GitHub's app.

### 4.9 Usage monitoring

v1 is local accounting: per-turn token usage from session logs (ACP usage_update notifications where adapters emit them; session JSONL otherwise), aggregated per stage, provider, project, and day. Periodic probes of CLI status output for rate-limit flags. Manual thresholds and manual coarse routing rules ("Claude weekly flag set: route reviewer stages to OpenAI"). Automatic fallback is explicitly out of scope until vendors expose quota state. Constraint honored throughout: subscription plans are driven only through the vendors' own CLIs/adapters as black boxes; nothing skirts ToS by driving subscription auth from a custom harness.

---

## 5. Verification (first-class, not a stage detail)

Autonomous loops converge on plausible-looking garbage exactly as fast as their verification is weak. Each project defines a verification environment as part of onboarding to Freeside: real test invocation, linting, build, screenshot capture for UI work, repro-script execution for bug pipelines. Verification outputs are artifacts attached to the run and referenced by the approval card; devlog claims of outcomes must point at them. A stage's "done" is defined by its verification, not its self-report.

---

## 6. Observability and the improvement loop

Every run leaves a durable trace: stage, prompt version, provider/model, transcript pointer, artifacts, tokens, wall time, outcome, gate decisions. Prompts and pipeline definitions are versioned files so a trace identifies exactly which version produced which outcome. This is simultaneously the usage-monitoring substrate and the tuning dataset: which stages fail, which prompts cause rework, and, critically, whether cross-provider review catches findings same-provider review misses (the load-bearing moat claim, testable from these logs). Findings from review stages are tagged true/false positive and same/cross provider from the start.

---

## 7. The comprehension layer

Defends against the out-of-the-loop problem: the better the pipeline gets at not needing me, the worse I get at the moments it does. Mechanisms, in order of load-bearing-ness:

**Plans are gated artifacts.** Since agents drive decomposition and plan evolution, plan documents, milestone structures, and specs are versioned files, and **changes to them flow through approval gates exactly like code**. An agent restructuring an approach opens a diff against the plan; that diff is an inbox item. No implementation is ever approved under a plan whose evolution I have not seen.

**Devlogs as output contracts.** Implementation and review stages must emit a devlog entry: one file per run in `devlogs/`, front-matter (run ID, stage, date, linked issue/PR), headed sections, length-disciplined. Required content is decisions and trade-offs; outcome claims only with pointers to verification evidence (devlogs are claims, not evidence). End-of-session norms: durable decisions get promoted to the plan or an ADR with the devlog as citation; deferred work becomes issues, with the devlog recording why.

**Briefings.** A scheduled briefer stage per project synthesizes traces + devlogs + GitHub state into a digest: what happened, what changed in the plan, what is in flight, what is coming to me. Pull counterpart: chat queries against daemon state and plan artifacts ("brief me on X", "what did I approve last week and what came of it").

**Altitude on every inbox item** (the approval card's plan-context line), so routine gate decisions passively refresh the model.

**Initiative WIP limits**, enforced and visible.

---

## 8. Roadmap

### Phase 0: validation (≈2 weeks, mostly agent-delegable)

The purpose is to answer the questions that determine whether and what to build, before building.

- **The trial.** 10 to 15 real work items across two projects run through a staged flow using existing tools (Antfarm-style loops and/or hand-driven staging). Measures: merge rate without rework, my minutes per item, tokens per item, review findings tagged true/false positive and same/cross provider. **Pass criteria set in advance:** ≈60% of items reach mergeable state with only gate-level input; cross-provider review surfaces at least a few true findings same-provider review missed. If elaboration fails but implementation passes, proceed with me keeping the specing role. **If implementation-stage results fail, stop the project.**
- **Source reading** (delegable to agents): Antfarm (pipeline schema and loop mechanics), Nimbalyst (harness adapters, iOS relay), claude-code-acp and the Codex adapter (loadSession, permission fidelity, mid-turn message behavior, static pass), coder/acp-go-sdk (client-side coverage for the multiplexer), Gas City (adopt-or-ignore for state layer).
- **Spikes** (mostly delegable to static verification, minutes of authenticated confirmation on my machine): usage/status probe yield from both CLIs; image round-trip headless in both directions; ACP adapter contract behavior (authenticated).
- **Design artifacts** (delegable): pipeline schema v0 with two worked examples; OpenAPI skeleton including the approval-card schema and event payloads.

**Exit criteria:** trial numbers in hand; adapter contracts characterized; schema and API drafts reviewed. These four are the named revision triggers for everything below.

### Phase 1: the headless loop (the 80%-of-value milestone)

freesided running real work daily with no Freeside UI: event bus (webhooks + reconciler + cron), pipeline engine with persisted state machines, ACP session layer behind contract tests, container runners with golden images, token broker, egress proxy, between-stage gates delivering approval cards via ntfy, devlog and verification contracts enforced, traces recorded. GitHub notifications + terminal are the interim surface; GitHub Projects auto-workflows as the passive status board. **The ugly-bootstrap rule: the earliest Freeside development work itself flows through the pipeline.** If the daemon cannot carry my real workload before the UI exists, the UI will not save it.

### Phase 2: the attention surface

OpenAPI boundary frozen (v1), SwiftUI app: inbox, approval cards, run detail, session chat with live-attach and resume, APNs. In-session ACP gates wired end to end. Success measure: a full working day driven from the phone.

### Phase 3: comprehension and tuning

Briefer stages and chat queries over project state; plan-diff gating enforced across projects; usage dashboard and manual routing rules; first prompt-tuning pass driven by trace data, including the cross-provider review analysis; WIP limits surfaced in UI.

### Phase 4: expansion (each item justified by daily annoyance, not roadmap momentum)

App Intents over Freeside entities, widgets, Live Activities; skills/tool exposure to stages; additional ACP agents (Gemini CLI is nearly free); voice; container-machine-based interactive environments; open-sourcing decision.

---

## 9. Key decisions log (with the reasoning that would need to be overturned)

1. **Daemon owns all state; clients are thin.** Enables second clients for free; state in an app was the named failure mode.
2. **GitHub is the system of record and review surface.** Freeside never rebuilds review; "final review without reading diffs" was rejected as vibes.
3. **Chat authors artifacts; the engine executes artifacts.** Workflows as conversation-resident state was rejected as rebuilding manual looping with extra steps.
4. **Elaboration lives in vendor apps for now; artifacts flow in.** Halves early UI scope.
5. **ACP is the session interface**; adapters are external subprocesses; per-adapter contract tests required. Codex's app-server may warrant a custom adapter if its ACP adapter underdelivers on steering.
6. **Gates split: between-stage (daemon-native) and in-session (ACP request_permission).** Blocking a live container for hours on a human was rejected.
7. **Go daemon, SwiftUI clients, OpenAPI boundary.** Conditional on the SwiftUI choice; a web-stack client would have flipped this to TS. Revisit only if daemon-side agent extensibility proves painful in practice.
8. **Apple container runtime, ephemeral mode only, Docker-swappable.** Container machines excluded for agents (home-dir mount).
9. **Egress enforced outside the guest.** In-guest firewalls demonstrated subvertible by agents.
10. **Subscription usage only via vendor CLIs as black boxes.** ToS constraint; also kills clean automatic fallback, hence manual routing.
11. **SQLite + Litestream; durability by authority placement, not engine choice.**
12. **Elaborator role merges triage + repro + spec.** Repro context improves specs; handoffs waste it.
13. **Verification defines "done"; devlogs are claims requiring evidence pointers.**
14. **Plan changes are gated like code.** The core defense against comprehension drift.
15. **Personal-tool scope; product option preserved but never load-bearing.**

## 10. Open questions and risks

**Open (empirical, Phase 0):** stage-level payoff numbers, especially elaboration; cross-provider review value; adapter contract results (permission fidelity, loadSession, mid-turn steering); usage-probe yield; whatever the Antfarm/Nimbalyst reading overturns in the schema draft.

**Risks and mitigations:**
- *Vendor CLI/adapter churn:* pinned versions everywhere, contract tests, deliberate upgrades. Ongoing tax; budgeted.
- *First-party absorption:* expected for everything except the cross-provider + gates + owned-hardware core; do not over-polish features vendors will ship natively.
- *Anthropic/OpenAI policy shifts on subscription automation:* black-box CLI usage is the tolerated pattern today; a hard policy change against headless subscription use is the existential external risk. Mitigation is architectural neutrality (API-key mode works identically, at worse economics) and staying visibly inside each vendor's own tooling.
- *Apple container 1.0 immaturity:* four weeks past 1.0; Docker fallback interface maintained.
- *Review saturation:* WIP caps, fewer-higher-confidence tuning, prioritized inbox.
- *Solo-builder scope:* Phase 1 is the go/no-go artifact; the ugly bootstrap keeps the project honest.
- *Prompt injection:* Section 4.6; treated as the primary threat, mitigations architectural, never retrofitted.

## 11. Naming

Project: **Freeside** (freeside.ai, github.com/freeside-ai). The Gibson reference is apt: autonomous intelligences operating under hard constraints, humans holding the gates. Brand line: *free as in bird*. Components take boring functional names (freesided, Freeside for iOS); the Gibson well is umbrella, not convention.

## 12. Reference shelf

Study-for-parts, not adopt: Antfarm (staged loops, YAML+cron+SQLite), Nimbalyst (cross-harness adapters, iOS companion, open source), Conductor (Mac orchestration UX baseline), Gas Town/Gas City/Beads (persistent work units, merge queues, and the cautionary tale on gateless scale), Omnara (mobile relay), Claude Code web/iOS and the Codex app (vendor baselines to beat on the specific loop, not on polish), Anthropic's reference devcontainer and Trail of Bits' sandbox (container hardening patterns), GitHub Agentic Workflows (typed event-driven schemas). Protocol: agentclientprotocol.com, coder/acp-go-sdk, Zed's adapters. Runtime: apple/container docs.
