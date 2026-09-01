---
title: Freeside Project Plan
revision: 44
status: active
updated: 2026-09-01
---

# Freeside

**Project charter and implementation specification.** This plan defines
Freeside, its required behavior, and the order we will build it in.

Read it in this order:

- Sections 1–4 define the product, its goals, and its human-attention model.
- Section 5 defines the architecture and its binding contracts.
- Sections 6–10 define verification, review, telemetry, comprehension, and
  operations.
- Sections 11–12 define the roadmap and exit criteria.
- Sections 13–15 record current decisions, risks, and naming.

The default-branch commit digest identifies the plan of record (Section 5.8).
`revision` is only a human label. It changes when Section 9 classifies a change
as material. Section 13 records the current revision, and its linked history
records every revision. PR bodies and decision notes explain what changed and
why.

## Contents

- [1. What Freeside is](#1-what-freeside-is)
  - [The end-to-end workflow](#the-end-to-end-workflow)
- [2. Goals and non-goals](#2-goals-and-non-goals)
  - [Goals](#goals)
  - [Non-goals](#non-goals)
- [3. Operating principles](#3-operating-principles)
  - [3.1 Autonomy inside the ward](#31-autonomy-inside-the-ward)
  - [3.2 The interruption budget](#32-the-interruption-budget)
  - [3.3 Portability](#33-portability)
  - [3.4 Simplicity](#34-simplicity)
  - [3.5 Oversight](#35-oversight)
- [4. The Attention Model](#4-the-attention-model)
  - [Core records](#core-records)
  - [Phase 1 Item Types and Actions](#phase-1-item-types-and-actions)
  - [Lifecycle Rules](#lifecycle-rules)
- [5. Architecture](#5-architecture)
  - [5.1 Overview](#51-overview)
  - [5.2 The Daemon and Its Supervisor](#52-the-daemon-and-its-supervisor)
  - [5.3 Execution: StageDriver and ReviewSource](#53-execution-stagedriver-and-reviewsource)
  - [5.4 Credential modes, egress profiles, and concurrency](#54-credential-modes-egress-profiles-and-concurrency)
  - [5.5 The CI Trust Boundary](#55-the-ci-trust-boundary)
  - [5.6 The gauntlet: workspace handoff, import, and clean verification](#56-the-gauntlet-workspace-handoff-import-and-clean-verification)
  - [5.7 The ward: runners, handoff gate, and operating modes](#57-the-ward-runners-handoff-gate-and-operating-modes)
  - [5.8 Control-Plane Trust](#58-control-plane-trust)
  - [5.9 Durability: Effectively Once](#59-durability-effectively-once)
  - [5.10 Coherent Backup: Encrypted Checkpoints](#510-coherent-backup-encrypted-checkpoints)
  - [5.11 GitHub integration: reconciliation plus intake](#511-github-integration-reconciliation-plus-intake)
  - [5.12 Workflow Definition, Initiators, and Artifacts](#512-workflow-definition-initiators-and-artifacts)
  - [5.13 Deterministic Components, Judgment Calls, and the Effect Registry](#513-deterministic-components-judgment-calls-and-the-effect-registry)
  - [5.14 Client Synchronization and Conversations](#514-client-synchronization-and-conversations)
  - [5.15 Evidence and images](#515-evidence-and-images)
  - [5.16 The Durable Scheduler](#516-the-durable-scheduler)
  - [5.17 Follow-Up Issue Filing](#517-follow-up-issue-filing)
  - [5.18 The World Model: Post-Merge Recompute and Frontier Projection](#518-the-world-model-post-merge-recompute-and-frontier-projection)
  - [5.19 Deferred Subsystems: Provisional Contracts](#519-deferred-subsystems-provisional-contracts)
- [6. Verification](#6-verification)
  - [The Verification State Algebra](#the-verification-state-algebra)
- [7. Review Policy](#7-review-policy)
  - [Finding Adjudication](#finding-adjudication)
- [8. Observability and optimization telemetry](#8-observability-and-optimization-telemetry)
- [9. Comprehension](#9-comprehension)
  - [Layering](#layering)
  - [Presentation per Item Type](#presentation-per-item-type)
  - [Summary Provenance](#summary-provenance)
  - [Measurement](#measurement)
  - [Document Change Discipline](#document-change-discipline)
- [10. Operations and Onboarding](#10-operations-and-onboarding)
  - [GitHub App Agent Identity](#github-app-agent-identity)
- [11. Roadmap, Build Order, and Coordination](#11-roadmap-build-order-and-coordination)
  - [The First Repository Is Deliberately Boring](#the-first-repository-is-deliberately-boring)
  - [Phase 1A: the secure publish path, in three internal exits](#phase-1a-the-secure-publish-path-in-three-internal-exits)
  - [Phase 1B: The Useful Workflow, in Three Internal Exits](#phase-1b-the-useful-workflow-in-three-internal-exits)
  - [Implementation Coordination (Building Freeside with Agents)](#implementation-coordination-building-freeside-with-agents)
  - [Phase 2: breadth and hardening](#phase-2-breadth-and-hardening)
  - [Phase 3: Comprehension and Interaction](#phase-3-comprehension-and-interaction)
  - [Phase 4: generalization](#phase-4-generalization)
- [12. Exit criteria definitions](#12-exit-criteria-definitions)
- [13. Decisions Log](#13-decisions-log)
- [14. Risks](#14-risks)
- [15. Naming and references](#15-naming-and-references)
  - [Product and subsystem names](#product-and-subsystem-names)
  - [Visual identity](#visual-identity)
  - [Coordination names](#coordination-names)
  - [Reference shelf](#reference-shelf)

---

## 1. What Freeside is

**Freeside is a local, durable workflow controller that grants agents the autonomy to turn work items into evidence-backed pull requests and interrupts me only when judgment is required.**

**Freeside is an agent control plane.** Harnesses such as Claude Code and Codex
run the agent's inner loop. Freeside runs the outer loop. It controls:

- what work starts;
- where it runs and what capabilities it receives;
- which credentials and network paths are withheld;
- what evidence is required before the work counts as done;
- when a human must decide; and
- what state survives a crash.

The brand register's tagline puts it plainly: *the harness runs the agent;
you hold the reins.*

The supported reference deployment is a Mac Studio. The daemon core remains
Linux-portable under Section 3.3.

### The end-to-end workflow

1. A manual submission, labeled issue, or scanner proposal creates a work item.
2. An elaborator turns it into a specification using research artifacts fetched
   by the daemon.
3. I approve the specification in the attention inbox.
4. An agent implements it in an isolated workspace with no GitHub credentials.
5. After the agent exits, a proven workspace handoff carries the result into
   the hostile import boundary, which runs out of process, and then into a
   fresh checkout.
6. A trusted recipe verifies the candidate and captures evidence in a clean
   environment.
7. Independent review, finding adjudication, and yield-driven remediation
   run within explicit emergency brakes.
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

Freeside is a personal-leverage tool. Useful, correct work must be worth more
than the attention, maintenance, money, and risk it costs. The manual workflow
already shows that elaboration, implementation, and iterative review are
useful. The open question is whether Freeside can make that workflow safe,
durable, and low-attention without moving the danger into provider credentials,
CI, artifact import, stale approvals, or interruption creep.

The project succeeds only if all four claims hold:

1. **Useful, correct work per unit of my attention rises** against a
   passively logged, normalized baseline.
2. **Decision quality is preserved.**
3. **Safety invariants hold** under Section 12, verified by conformance and
   adversarial tests, never read off telemetry.
4. **Autonomy is preserved:** exceptional interruptions remain rare and trend
   down under Section 3.2.

These claims are gates, not the goal. Even if all four hold, cost and
maintenance still decide whether Freeside creates a positive return.

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
   workflow. A trust profile attests the authority a PR job actually holds.
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

1. Freeside is not an IDE or code-review surface. Code review and merging stay
   on GitHub; Freeside owns workflow decisions and approvals. A human merge
   is the current accountability checkpoint. Whether narrow, risk-bounded
   classes of change ever earn automatic merge is deliberately left open.
2. It is not a product for hypothetical users: no multi-tenancy or billing.
3. It is **not a harness**. It uses sanctioned vendor batch interfaces and never
   owns a model loop.
4. It does not modify itself at runtime. Control-plane configuration is never
   hot-modified.
5. Silent provider fallback, voice, a pipeline DSL, and briefings are out of
   scope until recorded outcomes from later phases justify them. A project
   lineup may name a switch for each failure class; that switch is explicit and
   recorded, so it is not fallback (Section 4).
6. It is neither a formal pre-build validation study nor a generic CI security
   auditor.
7. It is not a general-purpose synchronization platform. Server-authoritative
   snapshots are enough; there is no client-facing event log and no CRDT.
8. It never requires a service that Freeside operates. Phase 1 depends on no
   such service, and the fully unmanaged deployment stays first-class
   permanently. Managed infrastructure may come later (Section 5.1) to remove
   operational friction, but it is optional convenience, never control-plane
   authority. Losing it may cost convenience (reachability, delivery, or
   portable operation), never local state or standalone workflow authority.
   One scoped exception: in portable mode, replica storage carries head trust,
   meaning activation fencing and the recovery frontier (Section 5.1).

## 3. Operating principles

### 3.1 Autonomy inside the ward

Autonomy is the default. Gates exist only at trust-boundary crossings and the
two designed judgment points.

Repeated exceptional interruptions trigger a policy review. An eligible
repetition may produce a policy-change proposal. Promoting a proposal to a
standing grant requires low risk, stable preconditions, and bounded downside;
repetition alone is never enough. Safety invariants and
non-waivable gates never auto-promote and never offer a bypass.

The following classes are non-waivable:

- GitHub credential separation;
- CI trust-profile validity;
- candidate changes to automation-control paths (a reviewer-instruction
  edit is advisory, Section 5.8);
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

**Self-service rule:** when an eligible class of interruption recurs, the user
must be able to resolve the whole class through the control-plane proposal
path.

**Rein is a convenience preset, not a security dial.** When a run is created,
the preset expands into explicit resolved policy, stored with a digest and the
provenance of each key. An explicitly set key overrides the preset default, and
the override is visible.

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
mode (Section 5.7), never a bypass. Strict settings always gate `unattended`
operation.

### 3.5 Oversight

Oversight is part of my contribution, not pure overhead. It catches failures
early, so it can't be optional. Oversight also needs to be frictionless because
chores get skipped. Sections 8 and 9 define the tools for that: honest attention
telemetry and sampled decision audits.

## 4. The Attention Model

### Core records

**AttentionItem** contains:

`id`, `project_id`, immutable `created_at` (nullable only for legacy records),
`subject {subject_type: run | proposal_batch | project | system, subject_id,
run_id?}`, `type`, `priority`, `reason`,
`requested_decision`, `recommendation?`, `evidence_snapshot`, `agent_claims`,
`artifact_digests`, `decision_surface {epoch, digest}` (the daemon-owned
identity defined under Recommendation sources below; #917 carries it through
sync), `pr_head_sha`, `pr_reference? {repo, number}`, `item_version`,
`interruption_class`, `conversation_id?`, derived timing aggregates,
`expires_when`, `review_recovery_binding?`,
`codex_reenrollment_recovery_binding?`, `review_configuration_recovery?`, and
`status`.

`evidence_snapshot` holds engine facts and artifacts, and only artifacts the
verifier or the daemon produced under an approved recipe (Section 5.15). Agent
claims are labeled as claims. Cards render image attachments straight from the
artifact store, addressed by digest.

**AttentionDelivery** records one delivery attempt:

`item_id`, `device_id`, `channel`, `attempt`, `submitted_at`,
`channel_accepted_at`, `opened_at`, and `delivery_status`.

A provider accepting a notification is never called “delivered.” Any stronger
word needs a real receipt from the device. The headline attention-latency
metric is the time from opening an item to deciding it; the Section 1 measure,
useful, correct work per unit of attention, governs. An item's timing fields
are aggregates derived from its deliveries.

### Phase 1 Item Types and Actions

Approval is not a universal action.

| Item type | Available actions and behavior |
| --- | --- |
| `spec_approval` | Approve, request changes, discuss, or stop. Render the full specification. A revision shows the diff from the last reviewed version, prior comments, and claimed addressals. |
| `review_diminishing_returns` | Finish now; apply the current batch and finish; continue under specified policy; or turn a recurring preference into a project-policy proposal PR. It never mutates policy directly. |
| `review_dispute` | For a routed finding: discuss or stop; the transaction that would execute the adjudication is deferred (#1016). For an observation-only shadow finding: approve continuation without routing the finding, discuss, or stop; only approve lets the run reach readiness, and stop ends the run and raises the normal durable publication-blocked surface. |
| `finding_adjudication` | Accept the recommended route, choose an offered alternative, discuss, or stop (added with the Section 7 adjudication routing, 1B). Acceptance binds to the adjudication artifact digest and the item version; a Discuss response re-invokes adjudication against the same version bindings, and the new artifact supersedes the item. Stop leaves the run parked. |
| `review_contradiction` | Recover only the exact persisted contradiction named by the card, or leave it parked. The card renders the bound run, invocation, round, base SHA, head SHA, and immutable failure-body digest; recovery preserves the original failure evidence. |
| `review_configuration` | Adopt the review configuration (`adopt_review_configuration`), discuss, or stop. The run is parked, not terminal. Adopting authorizes one operator-approved profile supersession, limited to review configuration, for exactly the parked failure the card's binding names. The superseding profile is resolved at decision time as the repository's currently activated revision and re-gated on every read. Stop concludes the run as a configuration failure, as it always did. The card renders the same bound coordinates as `review_contradiction` plus the digest of the superseded profile. |
| `execution_failure` | Retry; retry with a predefined policy-allowed capability manifest; discuss; or stop. When the failure is classified as provider quota, credential expiry, or capacity, the card also offers retry under a qualified alternate agent, or wait (see the explicit alternate-agent retry below). |
| `agent_question` | Answer and retry, answer without retry, or stop. |
| `publish_blocked` | Rerun trust evaluation, inspect the trust failure, or stop. Which publication path a repository uses is repository configuration, never a per-item choice (revision 44). |
| `ready_for_final_review` | View the PR (navigation, not resolution), return work to the agent with feedback, `mark_seen`, dismiss, or stop. It stays active until Freeside observes merge or close, work is returned, or the item is dismissed. |
| `run_proposal` | Start, **start with changes**, decline, or snooze. “Start with changes” creates a revised proposal artifact, supersedes the original item, creates a new item version, and starts the run from the exact revised digest. It never uses unversioned ad hoc parameters. Proposals are grouped under `proposal_batch_id` with per-candidate decisions. |
| `effect_proposal` | Approve, **approve with changes**, decline, or snooze a proposed effect from the Section 5.13 registry (added in 1B with the registry; first instance: follow-up issue filings in 1B.1, with proposed watches following once their schedule kind lands, Section 5.16). Approval binds to the proposal artifact digest; “approve with changes” creates a revised proposal artifact and supersedes the item, exactly as `run_proposal`'s start-with-changes. `run_proposal` remains its own type. |
| `system_health` | Acknowledge, run doctor, stop unattended operation, or, on the notice a stop raises, resume unattended operation. A revoked Codex identity marker also offers resolve re-enrollment (`resolve_reenrollment`), but only once the marker carries the immutable binding for its exact latest verified re-enrollment operation; the command revalidates that operation and that marker occurrence inside the transaction that resolves the item. Acknowledge means seen, never resolved; it cannot clear a revoked identity. Every item declares an immutable posture. `blocking` keeps the admission gate in place until the diagnostic clears, unattended operation is explicitly stopped, or a validated configuration supersedes it. `advisory` stays open and visible without blocking unrelated unattended admission. A stop is a durable operating transition: only an explicit resume reopens unattended admission; a restart alone never does. |
| `blocked` | Consolidates external waits that exceed Section 5.12 thresholds. It is read-only. |

Section 9 governs each type's presentation: what its card leads with and what
layers below.

**Explicit alternate-agent retry.** A provider quota, credential expiry,
or capacity failure offers three resolutions beyond discuss: retry under a
qualified alternate agent (Section 5.4), wait, or stop. Qualified is
failure-specific, read from recorded facts: a quota failure needs an agent
on a different usage pool (two harness clients on one subscription share
one, so that switch is an experiment, never a hedge); an expiry or
revocation needs an enrollment with a valid generation; a capacity failure
needs a different service route. Wait leaves the run parked with the same
card until the operator returns to it. The offer is stated once here and
applies on whichever card surfaces such a failure, including a review-side
quota or expiry failure; it never widens a card's other actions. Each
switch is a new recorded attempt that preserves the original failure and
its evidence, re-evaluates cost owner and the Section 7 review-independence
rule against the new agent, and continues provider state only where the
adapter proves compatibility (Section 5.8; a different adapter is a fresh
invocation). By default the operator chooses from the card. A project
lineup may instead name the switch per failure class; it then happens and
is carded, a recorded choice like any other. Freeside never learns a
preferred agent from past switches, and a switch is never silent (Section
2 item 5; Section 14, single-provider execution capacity). Switching the
review agent opens a new convergence segment (Section 7): the new
reviewer's first pass is not the old reviewer's next round.

**Recommendation authority.** An item may carry at most one
`recommendation {action, reason, source, provenance, confidence?}`. It selects
exactly one action from the item's own `requested_decision` and states a
reason; it never widens or reorders the offered set. `source` is part of the
contract because the card renders judgment differently from fact. The
required, immutable `provenance` is a closed union with one shape per source:
`daemon_policy {rule_digest, input_digest}` for deterministic policy computed
from canonical state; `agent_judgment {judgment_site, invocation_id,
artifact_digest}` for the schema-validated output of a declared Section 5.13
judgment site; or `project_policy {policy_key, resolved_policy_digest,
application_digest}` for an explicit human or project policy choice.

Which source supplies the recommendation is derived entirely from current
authoritative state, never from the item or the caller. When it creates or
reconstructs an item, the daemon enumerates every eligible source record:
every record whose source-specific applicability gate matches the item's
current decision surface. Eligibility is counted per record: two applicable
daemon rules, two policy applications, or any other pair are two records, even
when they share a source class. Exactly one eligible record produces the
canonical recommendation. Zero or several eligible records produce no
recommendation, and the actions are offered with equal weight. There is no
precedence, source map, selector policy, ranking, or tie-break. The stored
optional recommendation must equal the exact derived output, including when
that output is absent. A mismatch invalidates only the recommendation, which
then doesn't render; the item and its decidable action set stay valid. An
eligibility change can therefore safely withdraw a recommendation that used to
lead.

Each authoritative source record commits to a daemon-owned decision-surface
identity for the item that contains it, not to `item_version`. The identity is
one persisted record per item:

`DecisionSurface {item_id, epoch, subject, requested_decision, pr_head_sha,
presented_artifact_digests, digest}`

`digest` is the value a source record commits to: the content address of the
canonical `{item_id, epoch, subject, requested_decision (sorted set),
pr_head_sha}`. It already names the item and the epoch, and Section 5.14's
`item_decision_surface_digest` is this digest. `presented_artifact_digests` is
transition state only and never enters the preimage. The record lives beside
the item, not on it; #917 projects `decision_surface {epoch, digest}` onto the
synchronized item. The mechanism satisfies four invariants:

- **Eligibility-independent:** Adding or removing an applicable source never
  advances the identity. Neither does a change in which record is the unique
  eligible one under the unique-or-none rule. The reason: a source record is
  never a member of the presented set, so admitting or removing one opens no
  epoch. A record that was once authoritative therefore never becomes
  permanently stranded.
- **Telemetry-stable:** Delivery, open, timing, status, `decided_at`,
  `expires_when`, readiness, base freshness, the commit-plan notice, the PR
  reference, recovery bindings, and the recommendation field never advance the
  identity. General `item_version`, row `entity_version`, head, and
  full-artifact approval and command gates are unrelated to it.
- **Surface-distinguishing:** A change to `subject`, `requested_decision`,
  `pr_head_sha`, or the presented artifact set advances the epoch by exactly
  one and yields a new digest. Two presented sets on one item are always
  distinct epochs and two items are always distinct ids, so two genuinely
  different presented surfaces never share an identity. A reorder of
  `requested_decision` is not a change, and a field returning to a prior value
  is a new epoch, never a reuse.
- **Non-cyclic:** The preimage holds no artifact digest. So no source artifact
  ever commits to a digest set that contains its own final `artifact_digest`,
  and the identity of a coming epoch can be computed before the artifact that
  opens it is finalized. The producer computes the next surface for the
  prospective item, writes the surface's digest into the artifact, finalizes
  the artifact, and the item write that admits it derives the same value.

The epoch starts at 1 when the item is created. Only the store's single item
writer advances it, and it advances exactly when the item's structural fields
or its presented artifact set differ from the stored record. An artifact is
presented if and only if one of the item's presentation slots references it:
`evidence_snapshot`, `agent_claims`, or a type-specific binding such as
`finding_adjudication.adjudication_digest`. An artifact referenced only by a
recommendation's provenance slot is source-only and tied to eligibility: it
never enters the presented set. An artifact referenced by both a presentation
slot and a provenance slot is presented, so superseding it is a real surface
change (this is the finding-adjudicator case). `daemon_policy` rule and input
digests and `project_policy` application records are not artifacts; they are
never members of `artifact_digests` or of the presented set, so the policy axis
cannot strand a record. Readiness, base freshness, yield history, and other
rendered facts are not surface members either; a source that depends on them
binds them through its own input digest, never through the identity.

The persisted daemon identity is the authority. A decoded or caller-supplied
value grants none. If the record is missing, or if its digest, structural
fields, or presented set disagree with the item, item reconstruction fails the
whole item closed. Checking a source record's committed digest against the
current record comes after that and is narrower: a mismatch there fails only
the recommendation closed.

The rule digest is the content address of the rule's semantics and is never
reused. Each authoritative source record must itself commit to the
decision-surface identity, so a valid source output can't be replayed onto a
foreign or newer decision surface. Each source kind carries the identity its
own way: it is part of `daemon_policy`'s canonical input; the finalized
immutable `agent_judgment` artifact carries it, and the immutable
invocation-to-artifact binding proves the source is authentic; and
`project_policy`'s daemon-authored, digest-addressed application record binds
it alongside the policy key and the resolved policy digest. `daemon_policy` and
`project_policy` source records are not themselves bound artifacts. Inputs
prepared before an invocation cannot carry a decision surface whose requested
actions derive from that invocation's output, and a caller-supplied item digest
is never authority.

For the uniquely eligible record, the daemon resolves and authenticates the
provenance, recomputes the item's full canonical artifact-digest set, requires
any provenance `artifact_digest` to appear in that full set, and requires the
source record's committed digest to equal the current decision-surface digest.
It then rederives the canonical `action`, `reason`, and optional `confidence`
from the authenticated source-and-item pair. Full
`AttentionItem.artifact_digests` equality, and every approval or command
binding, still use the complete set, including the source artifact. Equality of
the item-side binding set therefore proves containment. The artifact-side
commitment is the decision-surface digest, which names the item and the epoch
its presented set belongs to without hashing any artifact, so it never needs
the artifact's own final content hash. A foreign item binding, a source
mismatch, or a payload difference rejects the recommendation; an invalid
binding never renders.

An `agent_judgment` recommendation is a labeled proposal. The type case is the
finding adjudicator's parked batch: the item-level recommendation endorses the
accept-the-recommended-route action, while each finding's route, rationale,
producer, and confidence stay in the Section 7 adjudication artifact and are
never collapsed into this one field. `confidence` appears only when the
producer supplies it. An item without a recommendation offers equally weighted
choices. A client never infers a recommendation, and the order of
`requested_decision` carries no endorsement. Section 9 governs the
recommendation-led presentation.

### Lifecycle Rules

- Approvals bind to artifact digests and the PR head SHA. Changed inputs
  invalidate them.
- Retries supersede failures.
- Resolutions are transactional and version-checked.
- A stale submission receives a conflict and the replacement item.
- Notifications are read-only hints, never authority.
- A fault class is suggested; one tap corrects it, and it may stay unknown.
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

**Core authority and replaceable infrastructure.** The daemon and clients own
application semantics and authentication. Remote reachability (Section 5.2),
notification delivery, replica storage (Section 5.10), and external health
monitoring are replaceable infrastructure boundaries. Each has a reference
implementation the operator selects, and each may later get a Freeside-operated
managed implementation. The Section 5.10 replica-store contract is the template:
a capability-based requirement set with a named first reference backend that is
never an architectural assumption. One rule governs every such boundary: managed
infrastructure may improve reachability, availability, storage, and delivery,
but it never becomes necessary for workflow authority or local operation, and it
never increases Freeside's authority. One scoped exception is stated outright
rather than implied away. In portable mode the Section 5.10 replica store is by
design the oracle for both activation fencing and the recovery frontier. So
every replica backend, operator-selected or Freeside-managed, sits inside the
authority trust boundary for host activation and for how current the restored
frontier is. The unconditional rule covers reachability, notification delivery,
and monitoring; a managed replica backend is admissible only under the Section
5.10 contract, with that fencing trust acknowledged. Notification delivery
follows the same pattern. ntfy (hosted or self-hosted) is the Phase 1 reference
channel, and a Freeside-operated push service is a possible later channel.
Section 4's AttentionDelivery semantics stay channel-neutral: no channel
implementation type enters the contract, a provider accepting a notification
stays distinct from delivery evidence, and notifications remain
non-authoritative hints. This boundary is deliberately narrow. The authoritative
components (SQLite workflow state, conversations, AttentionItems, scheduling,
approvals, agent execution, verification, GitHub and provider credentials,
artifact authority) get no cloud seam.

### 5.2 The Daemon and Its Supervisor

`freesided` is a single static Go binary. A supervisor keeps it running. The
daemon never supervises itself, and launchd/systemd knowledge never enters
`daemon/`: unit files and their registration live with the install tooling
and the operator app.

**Supervision modes** (decider: user; revision 27):

- **Mac-first single-operator (Phase 1):** a per-user launchd LaunchAgent in
  the operator's login session, registered by the Freeside Mac app through
  `SMAppService` from a plist shipped in the app bundle, with `KeepAlive`.
  The app installs and triggers; launchd supervises. This path has no
  privileged step: the daemon drives Apple `container` and per-user
  tooling, and the operator account is the isolation boundary (state and
  credentials stay `0700`/`0600` under it, so other accounts cannot access
  them). The accepted Phase 1 cost: the daemon lives in the login session,
  so unattended operation assumes a logged-in operator. The
  terminal-launched process this replaces had the same bound.
- **Hardened (multi-user or server hosts, deferred):** a dedicated-user
  LaunchDaemon or systemd unit installed through the Section 10 elevation
  helper. It gives boot-time start, logout survival, and operator isolation.
  It stays the end state but is not scheduled in Phase 1.

**The daemon never runs as root.** One-time privileged work, such as creating
the user and installing the LaunchDaemon on the hardened path, lives in a
narrow elevation helper. Privileged services bind only to loopback or
Tailscale.

**Exit discipline.** Every deliberate stop is durable and happens in-process;
the process exits only involuntarily or to be restarted. Today's fatal-channel
writers and exit paths fall into these classes:

- **Durable stop** (close unattended admission durably through the Section 4
  gate: an operator stop appends the stop transition, and a system stop files a
  blocking `system_health` item; keep serving reads; only an explicit resume
  reopens admission, and a restart never does):
  - store I/O and correctness failures in any long-running loop (the workflow
    reconcile loop, a scheduler pass, active-resource enumeration or commit). An
    invariant on durable state recurs on restart, and a respawn loop would hide
    it;
  - local backup maintenance failure, meaning persistent disk or encryption
    damage (Section 5.10);
  - a doctor or janitor pass failure with a local or definitively classified
    cause (an unreadable operational source, revoked or broken GitHub App
    authority). Health can no longer be asserted. This settles the doctor
    source-error posture, which the operational-command packaging decision had
    deferred;
  - an externally caused pass or lane failure once it persists (the
    implementation unit sets the consecutive-failure threshold). A transient
    external failure alone never stops or exits: it retries on its cadence or
    backoff and is recorded.
- **Restart-safe exit:** a post-bind HTTP serve fault. Without the API surface
  the daemon cannot serve even read-only state, and a fresh bind plausibly
  clears the fault.
- **Process exit (involuntary):** panics and invariant violations, and startup
  failures from flag validation through migrations and the initial doctor pass.
  A startup failure before the store opens cannot record a durable stop, by
  construction. The supervisor restarts these under its throttle. Crash-looping
  shows as `started_at` churn on `/health` and, from 1B.1, as the external
  probe's alarm.

After this contract the daemon's fatal channel carries only the two exit
classes; every durable-stop condition is consumed before it reaches the channel.

**Restart policy.** Restart always, under the platform throttle. This is safe
only because every deliberate stop is in-process and durable: a restart can
resume only work the contract says is safe to resume, and Section 4's rule
stands (a restart never reopens unattended admission).

**Stop.** Supervisor stop is SIGTERM with an effectively unlimited exit timeout,
because credential-lease teardown is unbounded by design (decider: user; the
stop-wait fork is decided on the unlimited side, because any finite grace period
recreates SIGKILL mid-lease, and a bounded credential-safe teardown is deferred
hardening, not a tunable). SIGKILL and power loss remain crash-equivalent;
kill-recovery covers them.

**Reachability.** Signet's authenticated HTTP and WebSocket API is one
application protocol, exposed unchanged over every reachability mode: direct
loopback, Tailscale, and a possible future managed relay (Section 5.19).
Tailscale is the Phase 1 reference remote-reachability mechanism, not an
architectural property of Signet; neither Tailscale nor its address model is
an architectural assumption. The Phase 1 security gate is unchanged: the
production API binds only to loopback or an exact verified Tailscale-owned
address. Reachability restricts who can contact Signet; it never
authenticates anyone to it, because every mode presents the same Freeside
device credential (Section 5.14). The seam stays architectural prose: no
reachability abstraction enters the daemon until a second real
implementation exists.

**Liveness and address.**

- Unauthenticated `GET /health` returns exactly `{status, version,
  started_at}`: liveness, version-skew detection, and crash-loop evidence (a
  moving start time under a supervisor). Everything richer stays on the
  authenticated surfaces (Sections 4 and 5.14); the route tells an unpaired
  caller nothing more.
- Under supervision, the unit file sets an explicit fixed loopback listen
  address, never the ephemeral default. Bare foreground runs keep
  `127.0.0.1:0`.
- The daemon durably publishes readiness (`{api_url, pairing_code}`, today's
  one-shot stdout line) to a `0600` runtime file in the state directory on
  every start. Under a supervisor there is no terminal to read stdout, and
  same-user file readability is the same trust boundary as today's
  terminal. The stdout line remains for foreground runs.
- The away-from-host liveness probe stays outside the process (Section 5.16
  keeps process heartbeats as plain tickers). An external probe polls
  `/health` and notifies over ntfy when the daemon is unreachable or
  crash-looping; it lands in 1B.1. The local surface is the Mac app's menu
  bar presence (Section 10). The probe is replaceable monitoring
  infrastructure (Section 5.1): the Phase 1 reference is
  operator-controlled. A future managed monitor may observe relay-connector
  presence, but it reports only what it can prove: connector presence is
  not daemon health, and managed-service uncertainty is never reported as
  host failure. Monitoring stays observational and never becomes workflow
  authority.

Storage and CI invariants:

- SQLite runs with WAL, `synchronous=FULL`, `foreign_keys=ON`, and a configured
  `busy_timeout`.
- CI builds and tests on macOS and Linux; macOS jobs stay lean.

### 5.3 Execution: StageDriver and ReviewSource

Every stage is a bounded batch job. The daemon assigns an `invocation_id` to
every external start, then reconciles every later operation by that ID:

- execution: start, inspect, stream, cancel, collect;
- review: `request_review`, inspect, poll, verify.

**Execution guarantee:** one committed invocation intent produces at most one
accepted result. The workflow never advances twice.

Phase 1 uses:

- one harness adapter, **Claude Code**, in 1A. A second, the **Codex
  CLI**, joins in 1B as an execution capacity hedge (Section 11), blocked
  on its pre-adoption gates (#401). Further adapters (pi first) follow as
  consumers of the same admitted-agent contract (Section 5.4);
- one production review source, a **Freeside-invoked local Codex review**
  binding (Section 7). GitHub-native Codex review is best-effort extra
  evidence and never satisfies the review requirement; and
- permanent fakes of both interfaces.

The 1B shadow arm runs a fresh-context Claude review against the same head.
Freeside records its findings but never routes them. It is the dry run for
promoting a selectable Claude ReviewSource (#397).

**Freeside invokes review directly** (decider: user; revision 25, replacing "one
primary review source, CodexGitHubReview" and the former control-plane-triggered
review step). The 2026-07-31 live-run falsification (#427) showed that
GitHub-native Codex review has no trigger path visible to the App. Automatic
review never starts for App-authored PRs. An App-authored `@codex review`
request fails at account resolution. Reviews are head-bound, so every
remediation push needs another valid trigger. A human-PAT trigger ties
unattended operation to one person's account linkage, token lifecycle, quota,
and attribution; that trigger was rejected as a production dependency. Each
review pass is therefore a control-plane invocation reconciled by
`invocation_id` like any other stage. Invocation failure closes safely under
Section 7's classification. Nested `AGENTS.md` guidance is documented Codex
behavior. Automatic re-review of remediation heads is a standing 1B integration
test. The Claude setup token's inference-only scope is contract-tested against
the pinned CLI.

**Session durability contract:** transcripts and artifacts are durable.
Workflow recovery is guaranteed from stage inputs, workspace state, and
artifacts; provider session resume is best effort. Capabilities are fixed at
spawn. If they are not enough, the stage emits a typed request and exits.

### 5.4 Credential modes, egress profiles, and concurrency

**No GitHub write credential ever enters any workspace.**

Every run declares and records one credential mode:

| Mode | Meaning |
| --- | --- |
| `subscription_contained` | Phase 1 default. The native vendor CLI runs in the agent VM. Its credential mount is read-only where permitted. The remaining exposure is an accepted, documented residual risk. |
| `api_key_isolated` | Supported in Phase 2. |
| `local_trusted` | Permitted only for explicitly trusted inputs. |

**Credential delivery under `subscription_contained` (Claude).** The setup token
lives as a single read-only file on a per-identity credential volume. A
daemon-owned enrollment transaction authors or replaces that volume while no
execution can use the identity. Phase 1A mounts no per-identity writable Claude
state. Ward instead supplies a read-only clean `CLAUDE_CONFIG_DIR`, a narrow
per-invocation continuity mount, and per-launch scratch state (§5.8); none is
credential state, and none is reusable by a different invocation. Serializing
writers on one shared directory would not work: it would not isolate a later
invocation from settings or hooks an earlier writer persisted.

The launcher command the daemon supplies as argv reads the token file into the
vendor's environment variable at exec. The writer's spec environment carries no
credential, and the driver's fixed environment rides the launcher, not the spec.
The token value never appears in argv text, inspect reports, ward journals, or
driver state; the credential mount path and the launcher text are the only
durable traces. The credential stays ambient in the writer process tree
(children inherit it, and the mounted file is readable at agent privilege). That
is the documented residual this mode accepts, backstopped by `provider_only`
egress and export secret scanning. The vendor behaviors this path depends on are
pinned-CLI empirical contracts, re-proved on every CLI version bump, not
vendor-documented guarantees; the work unit's decision note lists them.

Secret scanning is **best effort**, deliberately. It covers supported text
formats. Size, type, provenance, and publication controls govern opaque
artifacts. Universal detection across arbitrary encodings and images is
impossible; Section 5.15 records the image residual.

Every stage also receives an egress profile from control-plane policy. Profiles
sit above the credential-mode floor and represent different risk classes:

| Profile | Access and risk |
| --- | --- |
| `provider_only` | Default. The writer has one host-only network: no direct external path and no guest DNS, and the provider API is reachable only through the daemon's allowlisting proxy. The host gateway remains a network neighbor. The production API is isolated by its loopback-or-Tailscale-owned listener gate; every other host service needs its own declared binding policy, and the ward proxy is the one intentional agent-reachable exception. |
| `provider_registry` | Opt-in per project policy; `provider_only` stays the default. The writer keeps the same single host-only network and the same daemon CONNECT proxy. The proxy also admits the project policy's short, declared set of package-registry authorities, consumed read-only (initially `proxy.golang.org`, `sum.golang.org`, `registry.npmjs.org`, `pypi.org`, and `files.pythonhosted.org`). The set is control-plane policy: a writer change to it is publish-blocked like any other control-plane path, and a per-project addition is a reviewed operator change that admits only a public package registry the project's dependency manifests resolve against. Any other authority is not a registry entry. Admitting one is a `provider_web_read` decision with that profile's explicit wider-exposure record, because an arbitrary tunnel endpoint the operator has not vetted is exactly the attacker-observable host this class excludes. Under that criterion, the risk class on its merits is: no DNS, no direct egress, no attacker-operated host, and exfiltration bounded to what those registries' own endpoints accept from a client the attacker does not control. The proxy allowlist is per authority, with the TLS server name pinned to the CONNECT authority, so a registry entry cannot reach a shared-CDN neighbor. The tunnel cannot constrain HTTP method or path, so a registry that co-hosts a write endpoint (npm publish) leaves a residual: an injected writer holding an attacker-supplied registry credential could publish workspace content there. Section 14 records the residual; a project policy may exclude such hosts. This exposure is materially narrower than `provider_web_read` and is priced separately from it, never folded into that record. |
| `provider_web_read` | Materially wider credential-exfiltration exposure. Read-only HTTP can still exfiltrate through URLs, headers, bodies, redirects, and DNS while the provider credential shares the trust domain. It requires an explicit record of the wider exposure and a small trusted-domain allowlist. |
| Clean verification | No network access. |

The 1B elaborator gets no general web access. It runs under `provider_only`
and emits typed fetch requests. The daemon fetches allowed URLs and returns
immutable, digest-addressed research artifacts, then reinvokes the
elaborator for a bounded number of iterations. This removes the broadest
credential-exfiltration surface from the injection-exposed stage, and it
makes research inputs provenance-bound, cacheable, and reproducible.
Invocations bind to artifact IDs, not live web state.

Provider concurrency has two independent controls:

`AuthIdentity {account_binding, usage_pool, budget,
auth_store_mutation_lease, max_parallel_executions, enabled, cost_owner}`

`ClientEnrollment {auth_identity_id, harness_client, route, auth_method,
credential_mode, refresh_strategy, supports_read_only_auth_snapshot,
generations[]}`, each generation carrying the exact store locator
(`auth_store_volume`), the store manifest digest, the lease fence, the
account binding, and the token expiry where the auth method exposes one
(admitted agents, below).

1. Auth-store mutation, including refresh, login state, configuration writes,
   and store replacement, is serialized per identity: the one lease fences
   every enrollment's store.
2. Inference execution has a separate parallelism limit. 1B establishes that
   limit experimentally and exposes it to WIP scheduling.

If only one execution is safe, scheduling shows that constraint instead of
hiding it in a lock. API-key fallback is always available. Vendor tooling stays
native and unmodified.

**Admitted agents.** Freeside admits what an agent consumes by digest: the
base commit, the prompt package, the vendor instructions, the policy, the
input artifacts (Sections 5.8, 5.9, 5.12). Until this revision, the agent's
own configuration was the one major input not admitted that way. An
**agent** is one operator-authored document in the control-plane tree,
reviewed as a diff, with four lines and no role:

```text
agent sol-via-pi
  who      enrollment  openai-chatgpt-A/pi    # this account, this client, this route
  through  route       openai_chatgpt_codex   # hosts, protocol, terms basis
  running  adapter     pi_json_v1@<build>     # Freeside adapter; pins the harness build
  asking   offer       gpt-5.6-sol, effort max
```

The source uses names. The canonical body, which is what gets hashed, holds
the resolved enrollment id, the route, adapter, and offer digests, and the
effort value. Names live in the tree's name-to-digest map and are never part
of a digest. Resolution validates the join: the enrollment's route and client
kind are the agent's route and the adapter's client kind; the effort is one
the offer allows and the adapter can send; and the enrollment's identity is
enabled. "Harness, model, effort" is how a client renders an agent. A
**lineup** is a policy's map of roles to agents. The project lineup, or the
deployment lineup beneath it, is the only standing selection and the only
approval. The one per-attempt selection is the Section 4 alternate-agent
card: a recorded choice among agents resolved from the same tree. It never
approves an agent the tree does not carry and never changes the lineup.

The lines:

- `who` names a record, a `ClientEnrollment`: one `AuthIdentity` × one harness
  client × one route × one auth method, carrying `credential_mode`. One
  identity has many enrollments (pi and the Codex CLI on one ChatGPT
  subscription are one identity, one lease, one budget, and two enrollments
  with distinct sanitized stores). The revision 36 rule holds: an account
  binding is unique across identities, and every enrollment and generation
  carries the account binding its identity carries. Enrollment, adoption,
  reconstruction, and admission reject a second identity for one account, and
  they reject a credential whose account differs from its identity's. So one
  subscription never holds two leases or two budgets, and no usage is
  attributed to the wrong account. Every successful store mutation (login,
  refresh, re-enrollment) appends an immutable generation entry (lease fence,
  store manifest digest, account binding, token expiry) under the enrollment
  and changes no agent; admission records the generation it mounted. The stage
  receives a daemon-owned, single-route store, never a harness's
  multi-provider home. `AuthIdentity` keeps the account binding, the usage
  pool, the account budget and concurrency limit, the one conservative
  mutation lease that fences every store mutation with enrollment id,
  generation, exact locator, and manifest digest, and two operator fields,
  `enabled` and `cost_owner`. The exact store locator, refresh strategy, and
  snapshot support that the identity carried before this revision are client
  facts; they move to the enrollment and its generations.
- `route` is a content-addressed fragment with a stable logical id: service
  operator, protocol, inference authorities, billing mode, fallback policy,
  and a dated terms basis. Editing an endpoint or the terms changes the
  route digest and every agent naming it, never the enrollment.
- `adapter` is a content-addressed fragment. It names the Freeside adapter
  build and the exact harness build it pins, and it declares the launch
  capabilities it honours in a closed vocabulary of its own (read tools,
  mutation tools, exact resume, instruction delivery, structured output,
  context severance, auxiliary-inference control, store contract per
  route). `AgentVendor`, the instruction mechanism, is derived from the
  adapter and never selected by policy.
- `offer` is a content-addressed fragment: one route's offer of one model,
  with its route model id, lineage group, `identity_stability` (pinned,
  rolling, or opaque), allowed effort levels, pricing revision, and an
  authored `not_after`. The same model through two routes is two offers.
- `effort` is a value the offer allows. The adapter translates it, and the
  run records both the requested value and the effective native value. A
  clamp is rendered as `max → xhigh`, never silently.

Fragments are operator-authored authority, so they are configuration in the
tree. Identities, enrollments, generations, admissions, and observations are
facts, so they are records. A record of a past selection is never upgraded
through current configuration.

**The stage owns the launch.** Elaboration, implementation, and review each
define a launch: writer or read-only, output contract, severance, session
mode, and an auxiliary-inference policy (`forbidden`, `declared`, or
`observed`). The adapter maps the launch to harness-native controls or
declares that it cannot. So any stage runs on any adapter whose proved
capabilities cover its launch, and an agent carries no role. Review and
experiment arms require `forbidden`; the Claude baseline runs `observed`.
An agent narrows behaviour inside a stage. It never waives or widens the
stage's floors: no GitHub write credential in a workspace, publication
credentials withheld, review's fresh context and read-only workspace,
base/head invalidation, and the role capability ceilings all hold whatever
the agent.

**Admission** puts an agent through the five steps admission already applies to
every other input:

1. *Resolve.* Resolve the name and every fragment against one control-plane
   revision, build and hash the canonical body, and validate the join. An agent
   resolves only if its closure is present in the current approved revision.
   Removing an agent from the tree withdraws it from new selection; its history
   keeps its closure by digest. An offer whose authored `not_after` precedes the
   attempt deadline does not resolve. A proposal binds `(name, digest)` and goes
   stale if either moves.
2. *Selected.* The lineup names that digest for the role, or the Section 4
   alternate-agent card records it as this attempt's choice. The admission
   snapshot says which.
3. *Proved.* Runner conformance holds (Section 5.7, unchanged), and the adapter
   build's conformance record has proved capabilities covering the launch. That
   record is one per build, produced by the stage contract suite. It proves the
   store contract too: a sanitized single-route store, read-only to the agent,
   daemon-owned refresh under the identity lease, refresh hosts reachable by the
   daemon only, and harness update and telemetry hosts absent from the proxy
   allowlist.
4. *Credentialed.* The enrollment has a valid generation: its current one, not
   retired, revoked, or marked by the credential-integrity probe. The
   enrollment's auth method says whether expiry is observable. Where it is (the
   Codex and pi OAuth tokens), the generation records the expiry, and the expiry
   must cover the attempt deadline plus margin; otherwise the daemon refreshes
   first. This rule, not trust in the harness, is what contains a harness that
   fails against a read-only store at refresh time; pi is such a harness. Where
   expiry is not observable (the Claude setup token), the generation records no
   expiry and no refresh exists; an authentication failure at use fails the
   attempt closed and raises the existing revoked-identity marker. The Claude
   baseline cuts over under that admission.
5. *Snapshot.* `ExecutionAdmission` records the agent digest, the launch digest,
   the lineup revision, the enrollment id and generation, the store manifest
   digest, the effective egress allowlist, and whether the attempt is attended.
   It derives its existing fields (identity, image, credential mode, endpoints,
   instruction delivery) from them. Reconstruction reads by digest and rechecks
   the derivations.

A new agent × launch pair runs attended until an operator, having looked at
a run, marks it unattended-eligible beside the agent in the tree (outside
the hashed body, like the name). The mark names the exact agent digest and
launch digest it was given for. A line edit makes a different agent with a
different digest, so the mark does not carry over, and the changed pair runs
attended again. The mark is the approval; there is no smoke-record type.
Harness × model is not pre-proved beyond that first run: a request works or
fails closed against the stage's output validation. An observed model,
serving operator, or route that contradicts a pinned admitted value fails
the attempt as a durable contradiction, never a log line. Pre-proving a
rolling upstream would be fiction; offers say so with `identity_stability`,
and records claim only what the route exposed.

Every run records what was requested (agent name and bound digest, one
provenance entry per role), what was admitted (the step 5 snapshot), and
what was observed (effective model and serving operator with provenance,
usage redacted and sourced, auxiliary inference, routing). Observed facts
never authorise a future selection; they are authoritative history. A
versioned **treatment digest** groups runs for comparison (Section 8). It
covers route behaviour, adapter, launch, offer behaviour, and requested and
effective effort, and it excludes enrollment, generation, cost owner,
pricing, terms, deprecation, and labels. The agent digest stays the audit
key.

The Section 7 review-independence rule reads the offers. By default the review
offer's lineage group differs from the implementation offer's. The group is
derived per vendor family and curated conservatively; the same weights through
any route are one group; unknown lineage fails closed. A project lineup may
relax the rule with a stated reason; every card and record then carries which
rule applied. This supersedes the provider-plus-identity comparison: stricter by
default, explicit when not.

The `ProviderProfile` of revision 36 is superseded. Its approval role moved
to agents and lineups in the tree, and its remaining facts, `enabled` and
`cost_owner`, are identity fields. `freesided auth` keeps the profile name
for the operator. The cutover has two steps because the daemon never writes
the control-plane tree. The first step lands the schemas, dual-read, and
enrollment adoption while the interim flag selection stays active. The
second step is the operator's: `freesided auth adopt` adopts each interim
identity's store as an enrollment with an initial generation and emits a
proposed baseline patch (the baseline agents, the deployment lineup, and
their attended-run marks, carrying resolved enrollment ids). A human commits
it. Selection activates and queued inputs are rewritten to agent digests.
The flags are removed once every baseline role admits. The baseline is
honest: today's Claude path passes neither a model nor an effort flag, so
its offer is `claude-code-native-default` with `identity_stability`
rolling-or-opaque and `effort harness_default`. Choosing an explicit Claude
model later is a change, not migration. The cutover rules of revision 36
carry forward unchanged. A nonterminal run or admission that carries an
`auth_identity_id` but no agent digest is read under a permanent legacy
rule: it keeps its admitted identity and credential mode and is never
resolved against a current agent. An interim identity that cannot be
adopted is retired; its nonterminal runs are cancelled through the Section
5.7 contract with an AttentionItem naming it, and its requests and policies
stay unbound until the operator remaps them. Agent-only selection activates
only after every such run has closed. A binding the cutover cannot classify,
rewrite, or retire fails closed.

This section fixes the principle, the objects, the cardinalities, and the
admission steps. The field-level schema, the canonical encodings, the
adapter capability vocabulary, and the lifecycle command set are settled by
the admitted-agent contract unit and the enrollment unit (#867), not listed
here. Deliberately not built: a qualification ledger with projections and
supersession (two proofs suffice, the adapter suite per build and the
attended first run per agent × launch); alias and withdrawal machinery (the
tree is the active set and git is its history); stored projections beyond
the treatment digest; named independence policies beyond the one rule and
its knob; enforcement of auxiliary inference where the baseline cannot
honour it; and a separate credential-pass record (it is a proved adapter
capability).

**Multi-subscription per provider.** Two identities of one provider (a work
and a personal subscription), each with its own enrollments and agents, are
a supported shape. Selection among them is a lineup line or a carded
per-attempt choice, never silent: no default is inferred from enrollment
order, recency, or availability. Cost owner is read from the selected
agent's identity on every selection and recorded with it, so one project can
attribute a review to one subscription and an implementation to another.
The operator owns compliance with each provider's terms for multi-account
use; Freeside attributes usage to a named identity and neither endorses nor
polices the arrangement (Section 14, subscription-terms drift).

**Observation, never authority.** A credential-bounded account probe may
record, per identity: a stable account fingerprint; a masked label for
operator display only, never written to evidence, composition manifests,
run records, or export; auth type; plan type; expiry and revocation state;
CLI version; a model and capability snapshot; the last probe time; and the
last execution time. Probe output feeds only `system_health` items,
proposals, and the operator-facing identity projection
(`freesided auth list` and the clients' display, which show the masked
label). Every probe-derived item carries the `advisory` posture, so an
observed expiry, revocation, or plan change informs the operator without
closing the unattended admission gate; the operator's explicit stop action
and the existing credential-integrity and revoked-identity markers keep
their own postures. The gate is an exclusion list: no probe value is read
by preflight, by scheduling, by the `max_parallel_executions` limit, or by any
driver. Those consumers read only the operator's explicit identity and
enrollment records, the tree, and the resolved policy. A probe that
observes a newer model, a lapsed plan, or spare capacity produces a card or
a proposed offer diff in the tree, never a changed selection. What a probe
can report is a pinned-CLI empirical contract. The Codex app-server probe
is expected to report account and plan facts, subject to the refresh-safety
spike Section 10 gates it on. The pinned Claude CLI offers a token digest
plus an auth check; plan, quota, and expiry are not observable through it,
so the Claude probe's realistic floor is integrity plus authentication, not
account state.

### 5.5 The CI Trust Boundary

An agent branch can modify scripts that a privileged GitHub Actions job later
runs. Same-repository PRs do not get the protections of fork PRs. A job's
implicit `GITHUB_TOKEN` and OIDC identity are authority even when the YAML
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
GitHub App identity model. Every trusted registration is bound to that principal
by numeric App ID; App names and slugs are display metadata, never trust inputs.
Installation-token minting fails closed unless the target repository is
onboarded and trusted, and the specific installation is recorded as known for
that repository under a known registration bound to the principal. This holds
whether the registration uses the public default or the private work-account
posture. Every worker-bound publication mint request supplies `repository_ids`
containing only the target repository's canonical numeric ID and narrows
`permissions` to the profile-approved operation. The response is untrusted until
Freeside verifies that it names exactly that repository, grants no permission
beyond the approved effective set, includes every permission the operation
requires, and has the expected bounded expiry. A missing or mismatched field
discards the token before any worker can receive it and fails closed.

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
   adjudicated findings drive remediation and reverification)
reviewed candidate ──▶ git/publish ──▶ GitHub PR (under trust profile)
```

Exactly two channels leave the agent workspace, and they never mix:

1. The **repo-change channel** carries content blobs, a normalized manifest,
   and an optional agent-proposed commit plan: how the validated changes
   group into commits, in what order, with what messages, carried as plain
   untrusted data whose schema only the importer interprets and validates,
   never as git objects. The channel permits regular files only. Symlinks,
   submodules, special files, unusual modes, and automation-control changes
   (Section 5.5) are publish-blocking; a reviewer-instruction change
   (Section 5.8) is detected and published as an advisory finding.
2. The **evidence channel** carries typed, provenance-bearing artifacts under
   Section 5.15.

The agent commits normally with git, but no trusted component ever reads or
imports anything from its `.git`: no objects, hooks, configuration, or history
as git state. What may cross is a **commit plan**: ordinary data the agent
writes at a reserved workspace path, proposing how the final validated change
set splits into commits. It crosses as a declared member of the handoff output,
so the ward's stray rule admits it and the ward's whole-output secret scan
covers it like every other exported byte, in every mode. Under `plan_preferred`,
the daemon derives the authoritative base-to-final change set itself and accepts
the plan only as an exact cover of it: every derived change in exactly one
ordered group, no unknown paths, every interpolated intermediate tree
structurally valid, and every resolved non-empty group's publishing message
screened. For a non-empty import, the daemon re-authors one clean commit per
resolved non-empty group when it accepts a plan, or one daemon-authored commit
under `single_commit` and in the enumerated `plan_preferred` fallback cases
below. A blocking failure authors no candidate. Published tree content is
confined by construction to the trusted base and the validated final snapshot,
so the tree-content publication surface equals the one a single-commit import
publishes; the screened messages are the one new published surface. Intermediate
commits are unattested ancestry, and evidence and publication identities bind to
the single candidate head (Section 5.15). Agent commit SHAs, timestamps, and
identities never cross. Publishing messages cross as validated, labeled claim
text, screened as automation-control surface under the profile's
`message_ruleset`. Under `plan_preferred`, an empty remainder's non-publishing
message skips those screening checks after the plan-wide secret scan. On a
non-empty import under `plan_preferred`, the daemon falls back to the single
clean commit when the plan is absent, or when the plan is rejected for an
enumerated agent-caused structural or non-secret screening failure. The fallback
surfaces a notice naming the reason class. A zero-change import under
`plan_preferred` takes the deliberate empty-commit path after the tolerant scan
and surfaces a present plan as present-but-not-honored. Under `plan_preferred` a
decoded secret anywhere in the plan's text is publish-blocking until remediated
(Section 3.1, non-waivable). Under `single_commit` a plan is not decoded or
honored, its presence is surfaced as a notice, and escaped credentials keep only
the ward's literal best-effort coverage. Construction is blocked before either
mode dispatches if the trusted base tracks the reserved plan path or any
descendant beneath it: the reserved name can be a Git tree even though the plan
channel itself is one regular file, and that entire namespace is excluded from
the derived change set. The walk exclusion and the preflight use a
path-component boundary; near-prefix names such as
`.freeside-commit-plan.json.bak` remain ordinary repository content. The
importer never trusts the workspace's `.git`, hooks, configuration, or
agent-written manifests. It enforces the exact base SHA, canonical paths,
allowlists, size limits, control-plane restrictions, and Section 5.4 best-effort
secret scanning.

Permanent tests include malicious manifests, commit plans, blobs, and
evidence. Trusted verification recipes load only from approved control-plane
configuration or the trusted base commit. Freeside mechanically identifies,
risk-flags, and gates changes to verification-control files.

Named residual risk: candidate test code runs inside the warded verifier.

### 5.7 The ward: runners, handoff gate, and operating modes

Runner backends declare capabilities; policy declares minimums. Freeside never
silently downgrades. Named capabilities are:

- `supports_detachable_workspace`;
- `supports_post_exit_export`;
- `supports_read_only_remount`;
- `supports_credential_volume_detach`;
- `supports_workspace_snapshot`;
- `supports_networkless_export`; and
- `supports_enforced_provider_egress`: the proven writer-egress boundary. The
  agent workspace reaches only the declared provider authorities and, under
  `provider_registry`, the declared registry authorities through the daemon's
  CONNECT proxy on a host-only network. Live probes inside the writer refute DNS
  and direct connections. The realized proxy allowlist is conformance-checked
  against the requested profile, never trusted from configuration. This
  capability attests the enforcement mechanism, which is distinct from the
  *requested* egress profile (Section 5.4).

#### The first ward gate

The actual runtime must prove this sequence:

1. Write files in an agent workspace.
2. Terminate the credential-bearing execution context.
3. Mount the workspace read-only in a fresh, credential-free context.
4. Export it without exposing provider credentials, daemon state, or host
   credentials.

Candidate mechanisms include a detachable volume, a host-controlled block
image, snapshot/export, or a separate export VM.

The declared strong class for Apple container 1.1.0 is
`fresh_vm_read_only_volume_handoff`, conditional on the conformance checks
below; the name is the runner backend's declared identity.

The same-VM fallback (terminate the agent process, detach credentials, and
export from the same VM) is not merely weaker; running it on this runtime
refutes it. Release 1.1.0 exposes no host hot-detach, and a guest unmount is not
a credential-device detach: the credential block device stays attached and
remountable. Freeside must not implement or declare that class.

#### Writer Outcome Authority

Apple container 1.1.0 exposes no process exit status. The runtime models
only running and stopped, so at the inspection surface a stopped writer is
indistinguishable from a crashed one. The exit status's value is
agent-controlled under every delivery mechanism (an agent process chooses
its own exit code), so exit status is crash and refusal detection, never
adversarial proof. Acceptance authority stays with output verification and
the export gates. What the gate trusts is freshness and delivery: it authors
a per-invocation nonce, journals it before start, and passes it in the
launcher argv. The launcher's final act writes the nonce and the CLI's exit
status to a fixed evidence path and exits with that status.

The write-once `ExecutionOutcome` is the canonical terminal authority for a
failed, canceled, or lost invocation; `ExecutionExport` is the canonical
completed authority. The ward journal is the crash bridge and cleanup
authority, not a competing execution result. Besides the nonce and
`WriterComplete`, its open record can carry a durable
`CancellationIntent {reason, recovery_capture_required}`, an optional
validated nonzero `WriterFailureStatus`, and an optional
`RecoveryCaptureDigest`. Its terminal outcomes include `completed`,
`failed`, `canceled`, and `loss`. After a restart, as in the live path, the
driver idempotently maps `completed` to `ExecutionExport` and every other
closed outcome to the matching `ExecutionOutcome`.

`WriterComplete` is the successful release predicate. It holds when the
writer is stopped or proven absent, the marker is present with the
journalled nonce and status zero, and the live daemon observed the proxy
healthy throughout the writer's life. Only that live daemon may set the bit
after all four facts hold. Recovery never reconstructs the lost proxy-health
observation from a zero marker.

A daemon-commanded cancellation durably records `CancellationIntent` before
it issues stop, and that intent takes precedence over marker classification.
After proving quiescence and satisfying any capture requirement, ward
completes teardown and closes `canceled`; the driver then converges
`ExecutionOutcomeCanceled`. Cancellation never makes the partial workspace
a publication candidate or a clean-verifier input. For a graceful portable
handoff, the intent sets `recovery_capture_required`. After quiescence,
§5.10's normalized encrypted workspace capture completes, and its verified
digest is durable in the ward journal before cleanup can erase its source or
the ward can close `canceled`. Restore exposes that recovery object only as
untrusted input to a new attempt.

For an uncommanded stop, a matching nonzero marker is terminal failure. Ward
validates the nonce and status, persists `WriterFailureStatus` before any
cleanup can erase the marker-bearing workspace, completes teardown, and closes
`failed`. Export is refused even when partial edits exist. Recovery checks the
durable amendments first and branches on them before it inspects marker state:
`CancellationIntent` takes first precedence; then an existing
`WriterFailureStatus` remains the failure classification while recovery finishes
teardown and closes `failed`, even when cleanup already erased the marker.
Marker classification runs only when neither amendment exists. After a stopped
or absence proof, a missing, malformed, or mismatched marker classifies as loss;
ward completes teardown before closing `loss`. A matching zero marker permits
recovery adoption only when `WriterComplete` was already durable: recovery
revalidates the surviving marker and absence facts but never synthesizes the
bit. Zero without that bit classifies as loss and follows the same
teardown-before-close order; nonzero closes `failed` even if a stale or legacy
completion bit exists. If any required amendment, capture, teardown, or close
fails, the journal stays open for recovery to retry. The writer's transcript is
evidence, never an outcome signal: the pinned CLI's terminal stream event can
report success alongside an authentication error, and only the exit status tells
them apart.

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

Inherited base metadata, such as the fixed `PATH` or a default `CMD` that the
daemon-supplied command replaces, is acceptable only when the probe proves
the required realized shape. A derived project image is checked again; a
compliant base does not make the extension trusted.

A reusable builder consumes the canonical repository identity, an exact
commit, and the trusted verification recipe. It derives a project image from
the approved agent base, bakes the dependency closure and tool configuration
in as files, records the repository, commit, recipe, and base-image
provenance, and returns a digest-pinned image reference. Per-project image
definitions and copied dependency manifests do not live in the Freeside
control-plane source, so a changed dependency manifest rebuilds the runtime
artifact without a Freeside source change.

The declared verification recipe runs verbatim with networking disabled. The
builder proves both that this clean run passes and that the baked dependency
material is load-bearing. For the second, a negative probe masks that
material and must fail by attempting the registry or network access the
positive run did not need. A candidate that changes the dependency closure
beyond the baked inputs fails loudly and requires a new reviewed project
image, unless the policy-gated rebuild below applies. Verification never
fetches a missing dependency.

**Policy-gated rebuild.** A dependency change stops costing a human round trip
when it stays inside the project's declared policy. The gate holds when the
candidate's dependency-manifest delta is lockfile-consistent; every added
or changed package resolves from an authority in the project policy's
declared registry set (the same set `provider_registry` exposes to the
writer, Section 5.4; the builder reads it whatever the writer's profile,
since builder fetch authorization and writer egress are different
concerns); and the verification recipe is unchanged.
Under those conditions the reusable builder rebuilds the project image from
the trusted recipe, reruns the networkless positive run and the negative
probe, and records the new provenance, and the run resumes against the new
digest-pinned reference without an AttentionItem. Any other delta (a new
authority, an unpinned or VCS source, a recipe change, a failed positive run
or probe) keeps the fail-loud path above. The gate is a machine check, never
a widening of the writer's network: the writer still never fetches during
verification, and the human still sees every dependency change in the PR
diff and in the image provenance record.

Every runnable agent-base and project-image reference is a
registry-resolvable `name@sha256:<digest>` and is admitted by digest, never
by tag. A local content-store digest without a registry identity is not a
runnable reference on Apple `container` 1.1.0. Where that runtime also
cannot use a locally built `name@digest` as a build base, the builder may
use a tag for that build-time hop only, after verifying its digest, and must
record the exact base digest in the derived image. The image supplied to
ward remains a registry-resolved digest reference.

#### Operating modes

| Mode | Requirements and limits |
| --- | --- |
| `attended_dev` | May use a weaker runner class. Disables `auto_start`, automatic publication, and unattended escalation. Reports its isolation class honestly. |
| `unattended` | Requires all of these: successful conformance including the handoff gate; a valid trust profile; an approved credential mode; every runner minimum, including the proven `supports_networkless_export` exporter boundary and the proven `supports_enforced_provider_egress` writer egress boundary; current backup health including encryption status; and no blocking `system_health` item. |

Run the full conformance suite at startup, after configuration changes, and on
the doctor's schedule. Run a lightweight probe before every unattended job.
Golden images pin CLI versions. Workspaces use VM-local disk.

Each completed, generation-current full pass durably records the backend's
proven class and capabilities with a monotonic proof generation and time. When a
recheck begins, it first durably supersedes the previous declaration, so that
declaration cannot admit while the recheck is pending. A failed pass records the
failure and invalidates the declaration. An unpersisted proof is not a proof:
the pass fails and the capabilities are never declared, because publication
follows the durable append. A recorded declaration can never exceed its class's
registered provable ceiling. An unattended admission is refused at the write
boundary unless its capability snapshot sits within the named backend's current
durable conformance record; a conformance lapse closes new admission without
making recorded history unreadable. So the declaration that gates a new
unattended admission is reconstructed from persisted conformance evidence, never
from transient process state.

Phase 1A.2 exception (owner decision, 2026-07-26): unattended admission may
waive the encryption-state dimension of backup health, and only that
dimension. Checkpoint currency, artifact closure, and restore-test age still
gate admission against the local owner-only checkpoint (§5.10). The waiver
applies only while all of the following hold, checked mechanically at
admission:

- The daemon configuration contains an explicit operator-set
  `backup_encryption_waiver` naming the exact trusted numeric repository ID it
  covers.
- The run targets exactly that repository, verified against its trusted binding
  rather than a positional notion such as "the first onboarded".
- The daemon does not yet carry the encrypted, digest-bound
  `BackupCheckpoint`. A build that carries it rejects the waiver as invalid
  configuration and retires the exception.

Admission without the waiver fails closed as before. Every admission under the
waiver records it in the run's audit record and surfaces the degraded posture as
a `system_health` item. The validated waiver configuration supersedes that
item's blocking state (the §4 supersession rule), so the item stays visible
without blocking the later admissions the waiver exists to permit. The encrypted
checkpoint must land before the Phase 1A exit; the doctor (§10) packages its
encryption check.

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
Freeside follows the configured operator-host path and snapshots the exact
regular-file bytes it reaches. The final path may be a symlink. It records
their content digest as a role distinct from the stage prompt and stores the
bytes in the artifact closure. A path that is genuinely missing records
explicit absence. A dangling, unreadable, non-regular, unstable, or oversized
source fails admission. The live host path is never mounted. Materialization
re-verifies the recorded digest. Ward then places only the admitted file (or an
empty overlay for admitted absence), read-only, at a fixed staging mount
outside the workspace.

The pinned Claude CLI keeps instructions, executable configuration, and session
data together under `CLAUDE_CONFIG_DIR`. So Phase 1A never mounts a shared
identity directory there. For each gate-mediated launch, ward creates a fresh,
clean config-root volume with exactly two pre-created empty mountpoint
directories, `projects/` and `session-env/`. Before the credential enters and
before any writer process exists, a networkless, credential-free observer
verifies the complete root manifest, including ownership, modes, entry types,
and the absence of unknown entries, links, or special files, then records its
digest and binding in the open ward journal. If observation fails, the launch
is refused. The config root is mounted read-only, and that holds even against
root in the writer.

Only two nested paths are writable. A `projects/` continuity volume is created
for one invocation, mounted at `$CLAUDE_CONFIG_DIR/projects`, and never reused
by another invocation. A fresh per-launch scratch volume is mounted at
`$CLAUDE_CONFIG_DIR/session-env` and never carried to a later launch. No other
config path is writable. Both surfaces are untrusted activity. The continuity
volume exists only because an exact same-invocation resume needs the provider
transcript, and the scratch volume carries the shell initialization that
process needs.

Ward creates every state volume under an opaque identity that is never reused,
and it refuses a pre-existing or ambiguous object. A credential-free observer
proves the continuity volume empty before its invocation's first launch, and
proves each scratch volume empty before its sole launch. It then journals their
runtime fingerprints, lifecycle bindings, exact mount targets, and expected
options. Immediately before every writer start, runtime inspection must match
three things: the journalled root, continuity, and scratch fingerprints; the
exact source objects, targets, and read-only or read-write options; and the
absence of any extra mount. Resume permits the bound continuity volume's
contents, now untrusted, but re-verifies that it is the same invocation object.
Any of these fails closed before the credential is delivered: pre-existence,
substitution, unexpected initial scratch or continuity contents, an
uninspectable object, or any mismatch.

Every gate-mediated launch uses the pinned CLI's `--safe-mode`. That flag
disables user and project instructions, hooks, plugins, MCP configuration,
skills, commands, agents, styles, workflows, themes, and keybindings. It keeps
the image-owned administrator policy at
`/etc/claude-code/managed-settings.json`. Separately, ward mounts a
digest-bound instruction bundle read-only and passes it explicitly with
`--append-system-prompt-file`. That bundle deterministically composes the
admitted host instruction (including explicit absence) with the repository
vendor instructions resolved from the exact trusted base. The composition
preserves their path scopes and precedence. Ward journals the bundle's source
digests, composition version, and result digest before launch. Instruction
files the agent modifies stay candidate diff content; they are always
risk-flagged and never launch authority.

An initial launch uses a daemon-generated UUID, supplied with `--session-id`
and journalled before the process is created. Provider resume is a separate
ward-owned launch generation in the same invocation. Ward proves the
predecessor absent while keeping the credential lease and fence. It supplies a
fresh verified config root, fresh `session-env` scratch, and a freshly
materialized instruction bundle. It remounts only that invocation's `projects/`
continuity. Then it starts `--fork-session --resume <exact-predecessor-id>
--session-id <journalled-successor-id>`. Ambient `--continue`, a non-forking
resume, an unjournalled session ID, cross-invocation continuity, and a second
process while the predecessor may still exist are all forbidden. The fork is
what makes this work. On an ordinary resume, the pinned CLI kept the
predecessor's system prompt. A fork accepted the fresh explicit bundle and
still preserved conversation continuity.

Resume is bound to one agent (Section 5.4) as well as to one invocation. A
Section 4 alternate-agent retry defaults to a fresh invocation with no
continuity remount, and the Section 5.7 operating modes do not relax this. The
adapter decides whether a successor agent may remount a predecessor's
continuity volume. It decides over the two agents' process facts: adapter
build, session format, launch, and, where the harness binds them, the
session-bound route and offer facts. A different adapter is always a fresh
invocation. Missing expected session state is a typed failure, never a silently
created session. That comparison is its own tracked unit (#873), and #408
merges after it.

A replay of an already-journalled launch adopts or reaps that exact process; it
never substitutes a resume or starts a duplicate. A resume generation fails
closed when its predecessor-absence proof, bindings, or prepared volumes cannot
be reconciled. Once each launch is absent, ward deletes its clean root and
scratch volumes. After terminal invocation capture, it also deletes the
continuity volume before close. If cleanup fails, the journal stays open for
recovery. A CLI process the agent itself spawns inside the writer is untrusted
agent activity, not a gate-mediated launch, and the export gates bound its
effects.

This topology is an empirical contract with the pinned CLI. Before a CLI
version change may enter the image, the exact image must pass these probes:
minimal-writable-state, workspace/config poison, exact-resume, fresh-invocation
isolation, crash-matrix, and live-race.

**Reviewer-instruction edits are advisory, never launch authority.** The
gauntlet detects every reviewer-instruction path mechanically, including
`AGENTS.md` at any depth, `AGENTS.override.md`, `.codex/**`, and their peers.
That detection is a mandatory minimum: a profile can widen it but never narrow
it. A detected edit lifts as an advisory finding (revision 42). It never blocks
publication and never carries a waiver. The publisher surfaces it in a PR-body
section of its own that candidate prose cannot forge. Review independence does
not rest on a block. The implementing agent and the Freeside-invoked reviewer
both compose their instructions from the exact trusted base, so the candidate's
copy cannot govern its own review. A merged edit governs later runs, and the
human merge gate, reading the surfaced advisory, is the judgment point for
that. Every other control-plane category stays publish-blocking and
non-waivable (Section 3.1).

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

One logical control plane has a stable `control_plane_id`, and one or more
enrolled hosts with distinct host identities. Exactly one host is active: a
single global execution seat. GitHub App private keys stay per-machine
credentials; the logical identity does not turn them into shared secrets. A
standby may verify replica and takeover readiness but serves no authoritative
work. It does not process inbox or restored outbox work, run agents, mutate
workflow state, or execute external effects.

**Enrolled host identity is cryptographically backed.** Reviewed at revision
35: no host identity is persisted yet, so this is a forward requirement on the
#265 domain contract, not a migration. An enrolled host record carries a stable
identifier and a host public key. The private key stays in platform protected
storage. The host proves it holds that key when it authenticates to
infrastructure services (a future relay connector, portable-store provisioning,
health attestation). Purpose-specific credentials derive from that identity or
sit beside it. No single omnipotent machine key exists, and the host key never
becomes application authority: device pairing (Section 5.14) and GitHub App
keys stay their own credentials. This is recorded now because inventing a
second notion of "machine" after installations exist would be the expensive
retrofit.

Freeside has two operating modes:

- **`standalone` is the default, zero-configuration mode.** Local SQLite and
  artifacts are the durable frontier, the active epoch is implicit, and the
  operator contract permits one machine only. Running copied standalone state
  as the same principal on two machines is out of contract, like copying a
  GitHub App PEM. If that machine and its backup are both lost, the disaster
  floor is forge reconstruction with human re-adjudication.
- **`portable` is required before a second enrolled host may activate.** A
  conforming remote store holds the durability frontier in one remote head, and
  that head's conditional writes also carry the active host identity and epoch.
  Portable-mode fencing applies only after the activation ceremony below
  completes. Standalone does not pretend to fence a second copy it cannot
  observe.

Portable mode is enabled only by a completed ceremony:

1. provision independently revocable store credentials for each enrolled host;
2. wrap the control-plane data key separately for every enrolled standby, and
   create the offline recovery wrap that Section 5.10 requires;
3. create a complete seed checkpoint and start the local append-only journal at
   the same transaction boundary, then upload and verify that checkpoint and
   every blob it references while standalone work continues;
4. quiesce authoritative local mutations, flush and verify the journal delta
   and its blob closure, then conditionally create the remote head in
   `activating` state with the complete frontier, the initial active host, and
   the initial active epoch; and
5. pass `freesided doctor` takeover-readiness checks from every enrolled
   standby, then conditionally change that same head to `portable` and resume
   authoritative work.

Until the final cutover, the control plane stays fully functional in
`standalone`. The cutover pauses acknowledged mutations until step 5 succeeds
or the candidate activation is conditionally marked abandoned. On failure,
standalone resumes from its intact local frontier. Failure never leaves a
partly fenced portable state, and no standby may activate from an `activating`
or abandoned head.

In portable mode, lease expiry is never authority. A host becomes active only
by conditionally rewriting the observed remote head to advance its active epoch
and name its own enrolled host identity. Every external effect requires that
head's current host identity and epoch. It also requires a remotely durable
intent whose referenced artifacts have reached the head's durability frontier.
If the store cannot acknowledge that frontier or validate the host and epoch,
portable external effects stop. A stale host that returns becomes passive
before it may inspect or process restored outbox work. Starting an agent
invocation counts as an external effect. So after takeover, the successor does
not start a replacement while the prior invocation may still be running. It
first cancels that invocation or proves it ended. Then it records its adoption
disposition.

Epoch fencing and credential fencing solve different problems. Ordinary
failover uses the active epoch and does not rotate GitHub credentials. Deleting
a lost or compromised host's App key stops new App authentication with that
key. When the whole installation must be fenced at once, Freeside suspends the
installation. Exclusion becomes terminal only after every outstanding
installation token expires or is explicitly revoked. Revocation cannot undo an
effect already caused with copied credentials.

The active host is the only writer, so registration bindings and pending
installation intents need no principal-wide mutation lease or binding-set
version. Instead, a pending envelope binds to `active_epoch` and a
monotonically increasing `durable_intent_revision`. The active host serializes
changes in its local transaction. It publishes the resulting intent to the
portable frontier before it redirects or produces any external effect. It
rejects an envelope from another epoch or a superseded revision.

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
- encrypted content-addressed blobs for every referenced artifact and workspace
  capture; and
- one conditional-write remote head that names the checkpoint, journal
  frontier, active host, active epoch, and complete content-addressed blob
  closure.

`RemoteHead {control_plane_id, mode, active_host_id, active_epoch,
checkpoint_id, journal_frontier, blob_closure_digest}`

Only `mode: portable` grants portable authority. `activating` and abandoned
heads are recovery evidence, not fencing or activation authority.

The head advances atomically, and only after every referenced object is durably
acknowledged. So a conversation message, decision, workflow transition, or
other result presented as committed or completed must be recoverable by another
enrolled host. An external effect in portable mode follows the same rule: its
intent and every referenced artifact reach the head's durability frontier
before it executes. This extends the Section 5.9 outbox discipline rather than
bypassing it.

The replica store contract is capability-based:

- strong read-after-write and overwrite consistency for control objects;
- conditional destination writes sufficient for remote-head compare-and-swap;
- immutable content-addressed objects;
- persisted-write acknowledgment;
- independently revocable per-host credentials with bounded, observable
  revocation; the conformance suite proves that both control-object and
  data-object operations reject a revoked credential before recovery resumes;
- declared, bounded object and metadata sizes that fit Freeside's objects; and
- no caching or sync layer in front of mutable control objects.

Every portable backend passes the same multi-client conformance suite.
Cloudflare R2 through its direct S3 API is the first reference backend because
it offers the required consistency and conditional `PutObject`; neither R2 nor
S3 compatibility is an architectural assumption. A filesystem target is always
valid for standalone backup and testing. It is portable only after the full
suite passes for the exact filesystem and mount configuration. Consumer sync
folders such as iCloud Drive and Dropbox are categorically ineligible. The
availability trade is deliberate: while the replica store is unavailable,
portable external effects stop rather than risk an unfenced effect. The
integrity trust is equally explicit. The head arbitrates which host activates,
and it names the frontier a takeover restores. So the replica backend sits
inside the authority trust boundary for host activation and for frontier
currency (the Section 5.1 scoped exception). A backend that equivocates over
heads can misdirect activation. It can also serve an older, internally
consistent head, which rolls back the restored frontier and loses or repeats
already-committed work, effect intents included. The conformance suite probes
this trust but cannot eliminate it. Checkpoint encryption keeps workflow
content unreadable to the backend, and content-addressing binds what a named
frontier contains, never which head is current.

Takeover restores a complete frontier; there is no partial mode:

- **Graceful handoff:** the active host stops new work. It cancels or waits for
  every in-flight workspace writer and proves each one ended. It flushes the
  journal, performs one normalized workspace capture, then uploads and verifies
  the resulting frontier. One conditional head write then both names that
  frontier and names the successor host while advancing the active epoch,
  transferring the seat atomically. The successor restores the resulting head
  and records explicit adoption events for in-flight attempts. An attempt whose
  writer committed a terminal result reconciles that result. In particular, a
  canceled invocation stays terminal and is never restarted or resumed;
  continuing it requires a new attempt seeded from the recovered workspace as
  untrusted input.
- **Crash takeover:** the successor conditionally rewrites the remote head to
  name itself and advance the active epoch while keeping the last complete
  frontier. It restores that frontier, records the same adoption events, proves
  or waits for any prior agent invocation to end, and reconciles from there.
  The workspace recovery point is the last successful daemon-side push. Workers
  hold no GitHub write credential, and crash mode performs no ad hoc capture.
  So every unexported change from an in-flight invocation may be lost. Periodic
  or per-turn workspace capture is not yet in contract. Losing the replica
  store itself falls back to forge reconstruction and human re-adjudication,
  not to a partial database or artifact restore.

Workspace capture uses one mechanism: a normalized, content-addressed export
that reuses the gauntlet handoff machinery, excludes credentials and trusted
`.git` state, and restores only as untrusted workspace input. Tier 1, one
capture during graceful handoff, is contractual. Periodic and per-turn capture
tiers are an evolution of trigger policy over the same mechanism. Revisit them
only after real handoffs measure capture cost and show that the loss window
since the last successful daemon-side push is unacceptable.

**Confidentiality is policy:**

`BackupPolicy {encryption_mode, key_id, destination,
retention_by_artifact_class, last_completed_checkpoint, last_restore_test}`

- Remote checkpoints are encrypted.
- Journals, artifact blobs, and workspace captures are encrypted with the
  control-plane data key before remote upload. Content addresses still identify
  verified plaintext; the remote objects hold only ciphertext.
- Encryption keys live outside agent environments.
- Backup credentials are never mounted into workspaces.
- Each enrolled host gets its own host-specific wrap of the control-plane data
  key. An operator-held recovery wrap stays offline and outside every daemon
  host, so retiring the last healthy host does not destroy the only recovery
  path.
- Retiring, losing, or compromising a host first revokes that host's replica
  credential. Portable effects stay stopped until three things happen: the
  store rejects that credential on both control and data paths, a remaining
  host selects and verifies the trusted frontier, and one head compare-and-swap
  establishes the new epoch. Then the control-plane data key and the remaining
  host wraps rotate. Revocation prevents future access; it cannot erase
  ciphertext or keys a compromised host already copied.
- GitHub App private keys are per-machine credentials under Section 10 and are
  excluded, as are provider credentials, unless a stronger recovery design
  encrypts them separately. So recovery may require reauthentication; copying a
  key from another machine is not a recovery mechanism.
- Raw transcripts have shorter retention than decisions, approved
  specifications, and audit events.
- `freesided doctor` checks checkpoint age, encryption state, artifact closure,
  and restore-test age.

Encrypted backup is required before unattended mode uses a private repository
with remote replication. A local-only development checkpoint may come first.

### 5.11 GitHub integration: reconciliation plus intake

Freeside reconciles each active GitHub resource independently with conditional
requests. Intake scanners discover new work with overlapping scans and
idempotent identities. Webhooks wait until Phase 2 and are added only if
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
  the operator approves that specification's digest. Its result names the
  source digest and artifact, the elaboration identity and policy, and the
  reserved implementation identity as separate lanes. The approval claim, and
  then the created run, carries the approved implementation specification
  digest.
- **Production acceptance identity is explicit.** The first manual submission
  derives a campaign deterministically from its content-addressed attempt-1
  implementation identity, so an exact repeat of the same submit stays
  idempotent. A deliberate retry allocates the campaign's next attempt number,
  which only ever increases. It binds the new implementation run to its exact
  terminal parent, the operator's reason, the original source digest, the
  approving elaboration run, and the unchanged approved specification digest.
  Retrying for operational reasons never requires changing the specification
  bytes. `freesided resume` is different: it targets one exact live run and
  mints no identity, and a terminal run can only continue as a deliberate new
  attempt. This command-level resume is distinct from provider-session resume
  in Section 5.7 and from AttentionItem actions in Section 4.
- `auto_start` is bounded by WIP caps. The conservative default is `propose`.
- Raw findings are immutable. Classification is a versioned annotation.
- Low-confidence materiality enters the Section 7 adjudication residue and
  defaults to continued remediation or human attention.
- Neither the classifier nor the Section 7 adjudicator can declare a finding
  fixed.
- Artifacts are typed, immutable, and digest-addressed. Approvals bind to their
  digests.
- The stall heartbeat (1B.1) is observed by ward or the daemon and may only
  accelerate a stall notice. It never resets or extends any hard budget, and
  agent output cannot influence it.

### 5.13 Deterministic Components, Judgment Calls, and the Effect Registry

The engine, not an agent, runs deterministic policy jobs:

- verification;
- evidence capture;
- research fetching;
- card facts;
- evidence publication; and
- cleanup.

Agents appear where judgment is the work: elaborator, implementer, remediator,
diagnostic, finding classifier, finding adjudicator (Section 7), reviewer,
shadow reviewer, and, later, briefer.

#### Daemon Judgment Calls

The daemon may call a model for judgment where judgment genuinely helps, but an
answer can never do anything by itself. The terminal authority modes are
exhaustive: **annotate**, **propose**, **explain**, and **choose**. Composed
inference inherits its eventual sink. Repetition, starvation, attention
creation, and telemetry reuse all count as sinks. Advisory output, meaning all
explain sites and audit telemetry, lives in an advisory store that policy
evaluation structurally cannot reach. That store stays separate from the
Section 8 policy-input telemetry.

Every call site carries exactly one per-site authority contract:

1. **Ceiling-bounded annotation** (type case: the finding classifier; the
   Section 7 finding adjudicator is the second site). The site declares its
   behavioral lattice and deterministic fallback; which outputs reduce work;
   raw-severity ceilings; second-adjudication rules; cumulative bounds on
   attention, compute, and starvation; and tests for extreme outputs and
   repeated calls. Existing classifier ceilings stay verbatim.
   Monotone-conservative annotation is a stricter subtype.
2. **Advisory-only**: human and advisory-store consumers only.
3. **Proposal** into the closed effect registry below.
4. **Bounded choice** among daemon-authored options whose worst-case effects
   were independently bounded before the call; cross-vendor driver selection is
   not choosable (standing owner decision).

Cumulative bounds compose globally: per-site budgets aggregate across sites and
runs under project-level and global windows, attributed to root lineage.
Resetting a bound requires gate-waiver-class authority, never the calling site.

Hard rules: outputs are schema-validated and producer-labeled. Nothing flows
into trust computation, transition legality, or `publish_eligible`. Every site
declares a fail-safe default. "Operable with inference down" means the control
plane stays available and fails safe. Inference-dependent steps pause or
degrade under their declared defaults. They are never promised to complete.
Every site is budgeted, untrusted-input sites carry sampled-audit telemetry,
and every site has a deterministic fake. A Section 4 item recommendation is not
a fifth authority mode. An `agent_judgment` recommendation exists only as the
schema-validated output of a site declared here, and it carries that site's
immutable invocation and artifact binding. A `daemon_policy` recommendation
binds its content-addressed deterministic rule and canonical input digest. A
`project_policy` recommendation binds the applied key and resolved-policy
digest plus its daemon-authored application record. Each authoritative source
record commits to the containing item's decision-surface identity under
Section 4. That identity is the epoch-and-digest `DecisionSurface` record. Its
digest is eligibility-independent, telemetry-stable, surface-distinguishing,
and non-cyclic. The item cannot select that record. Creation and reconstruction
derive the complete eligible-record set from current authoritative state and
apply Section 4's unique-or-none rule. A separately supplied item digest grants
no authority. Creation and reconstruction reject a source label without its
matching Section 4 provenance variant. They reject a stale or foreign
source-to-item binding. And they reject any action, reason, or confidence that
differs from the canonical projection rederived from that authenticated pair. A
`daemon_policy` recommendation is a deterministic card fact like any other.
Proposal cards keep two registers apart: "the proposal requests X" is a daemon
fact from the artifact, while agent cost, safety, and scope assertions are
labeled claims. Section 3.1's "designed judgment points" means human judgment
points.

Daemon-side inference is its own contract, not a reuse of `provider_only`. It
covers driver binding, credential handling, outbound field selection (an
explicit allowlist per site), input sensitivity classification, redaction,
provider identity, retention, size limits, and the input digests recorded per
call. No tools, no workspace, no ward container.

#### The Closed Effect Registry

Agent-requested real-world effects are anything a run, a client proposal
surface, or daemon-side inference asks the daemon to do. They exist only as
typed, digest-addressed proposal artifacts that target a closed registry of
effect kinds. Each kind has a fixed Go type, a trusted constructor, and a gate.
Effects the trusted workflow performs itself (publication, notifications,
installation maintenance) stay engine-run under Section 5.9 and the
deterministic-jobs list above; they are not proposal-gated. Proposals supply
bounded parameters, never event bodies, target identities, or authority.
Targets are daemon-selected context, or a selection among daemon-enumerated
opaque subject handles ("watch PR 42" parses as picking from daemon-enumerated
subjects).

Admission allocates and persists a daemon-generated proposal-instance ID
atomically under a stable admission idempotency key. That key is the canonical
upstream event ID, the client submission-command ID, or, for a proposal emitted
from within a run, the accepted invocation or export identity plus an emission
ordinal. A deliberate repeat gets a new command ID; retrying the same
occurrence keeps it. Semantic content never defines occurrence identity. The
instance ID is the effect identity for idempotence, ledgering, and crash
reconciliation; content digests bind approvals. Instances: `run_proposal`
(existing), follow-up issue filings (Section 5.17, 1B.1), and proposed watches
(a planned extension that lands with its schedule kind and consumer,
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
is assumed. The daemon stores only a credential hash or a device public key,
never reusable plaintext. Devices can be revoked.

Device identity is independent of network identity. Tailscale identity is never
Freeside device identity or authorization. Every supported reachability mode
(Section 5.2) presents the same Freeside device credential to Signet. So a
device that moves between reachability modes keeps its identity and needs no
authorization migration. Pairing and revocation are daemon-owned under every
mode. A managed service may transport pairing, discovery, or rendezvous traffic
(end-to-end protected and unreadable by the transport; Section 5.19), but it
can never enroll an authorized device on its own. A hosted account identity may
prove that a caller is eligible to reach a pairing endpoint; it never confers
application authority. The daemon alone turns a pairing ceremony into a device.

Every judgment-bearing mutation is:

`ClientCommand {command_id, device_id, expected_entity_version,
expected_bindings, decision_action_surface_digest?, payload}`

For an attention decision, the daemon derives and persists the exact action
surface presented to that device before accepting the command:

`DecisionActionSurface {device_id, item_decision_surface_digest,
client_capability_digest, actions}`

The record is content-addressed. `item_decision_surface_digest` is the `digest`
of the item's current Section 4 `DecisionSurface` record. `actions` is the
canonical intersection of the item's requested decisions and a
daemon-registered client-capability contract already bound to the device. A
caller-supplied action list or digest is never authority. Before accepting a
referenced surface, the daemon revalidates the device, the item's current
decision surface, the capability contract, and selected-action membership. This
record is telemetry evidence only: it cannot widen the item's actions or
authorize a command.

A retry returns the original result.

Monotonic telemetry, the credential-control surface, and attachment upload sit
outside `ClientCommand`:

- A delivery-opened receipt is an idempotent `PUT` identified by `(item,
  channel, attempt)`.
- The device identity comes only from the credential.
- The receipt records a fact and carries no judgment. It has no version
  precondition, because the delivery may advance from `submitted` to
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
steering wait until Phase 3.

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
   inspect pixels; OCR waits until Phase 2.
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
   before it publishes.
3. **The daemon treats images as opaque blobs.** It validates magic bytes,
   type, and size only. Server code never decodes an image; clients and GitHub
   render it.
4. **EvidencePublisher owns publication.** It lives in git/publish and follows
   effectively-once discipline through digest-derived names,
   check-before-create, and deterministic PR-section markers. It waits for 1B
   because the first repository is deliberately non-UI (Section 11). Phase 1A
   ships the artifact schema, provenance enforcement, and client rendering; 1B
   adds external publication with the first evidence-bearing workflow.

### 5.16 The Durable Scheduler

One scheduler owns every durable deferred check: PR watches, deadlines, and
subject-bound polls. It represents them as a closed union of schedule kinds
with fixed Go types and trusted event constructors. 1B implements only the
kinds that have 1B consumers: the PR-checks deadline, the review-wait
threshold, the base-advance staleness watch, and the installation poll, plus
the permanent trusted-config jobs (doctor, janitor; not proposable, no expiry
requirement). The staleness watch's consumer is the base-freshness fact on
`ready_for_final_review` items, which stay live until merge or close. The
doctor, the janitor, and the onboarding pending-install-or-expansion poll
already run before 1B on plain tickers under their Section 10 obligations. The
scheduler adopting them is a 1B migration that preserves those obligations,
never a precondition for them. The proposed-watch, scan-sweep, and grant-expiry
kinds are planned extensions that arrive with their consumers. Proposed watches
wait past the four 1B timer kinds, and an approved watch proposal is
representable only once its kind and consumer land. Scan activation stays
Phase 2. Stateless process heartbeats stay plain tickers. Active-resource
reconciliation (Section 5.11) also stays outside the kind union. Per-resource
conditional-request polling observes PR state, checks, merge and close, and
native review activity. That polling is a continuous process cadence on a plain
ticker, and `ready_for_final_review` items observe merge or close through it.
Schedule kinds carry durable, subject-bound deadlines and watches, never the
reconciler's cadence.

Proposed watches (Section 5.13's effect registry) require an expiry and are
bounded by a minimum cadence; per-subject, per-project, and global active-watch
caps; maximum occurrences or explicit renewal; and coalescing of proposals,
cards, and notifications.

Occurrence identity is (`schedule_id`, `generation`, `nominal_fire_at`). Missed
fires coalesce to the latest nominal occurrence with a recorded gap. Fire-time
validation runs before event construction. It covers project binding, resolved
policy, expiry, activation state (Section 5.9), operating-mode eligibility
(Section 5.7), and subject existence. Operating-mode eligibility is
kind-specific. Permanent trusted-config jobs run in every operating mode, so
the doctor and janitor keep their Section 10 obligations in `attended_dev`.
Workload kinds require the operating mode their consumer demands. The event
carries the expected schedule generation and subject version, and the consuming
handler rechecks both. A stale event is never silently discarded: the handler
recomputes and either re-arms (new generation, corrected binding) or records
proof that the condition no longer applies. Consumption and its outcome commit
in one transaction; otherwise the occurrence stays durably pending and is
redelivered. A one-shot deadline always ends fired-and-handled or explicitly
resolved. Firing never extends or preserves authority. Schedule state is
durable, queryable, and synced.

### 5.17 Follow-Up Issue Filing

Filing a GitHub issue is a fan-out effect. It can start workflows, notify
people, and wake integrations, and an issue those systems create in response
could re-enter intake as new work. Filing arrives human-gated (1B.1): every
filing takes explicit per-proposal human approval, whatever the profile state.
The policy-approved path below is the later autonomous path. A valid authority
profile is an additional precondition for that path, never a replacement for
the 1B.1 human gate.

The policy-approved path requires a digest-bound, freshness-limited issue-event
authority profile. The profile covers a complete enumerated authority surface.
An unknown or uninspectable surface makes the repository ineligible. The
profile is revalidated immediately before each creation, and it fails closed on
drift. It is a filing precondition only. The eligibility predicate: every known
transitive issue-creation or labeling path must be (a) ledgered before intake,
(b) proven unable to become intake-eligible, or (c) structurally forced to
propose. That requirement covers every path in the complete enumerated
authority surface, not merely the known paths. Otherwise filing stays
human-gated.

Intake eligibility requires event-level authority proof, checked at the final
intake transition. The proof is either ledgered proposal-instance lineage or
explicit human admission. That lineage is a daemon-authored ledger that maps to
the canonical numeric issue ID under the canonical repository ID; markers in
issue content carry zero authority. An event without proof is forced to propose
even when current configuration validates, because current-state revalidation
cannot authenticate a historical event (drift-create-revert is assumed
reachable).

Repository, filing identity, labels, and milestone derive from trusted policy
and run lineage, never from proposal text. Every agent-controlled textual field
is screened under a versioned ruleset on the Section 5.5
commit-message-screening pattern. The effect identity is the proposal-instance
ID (Section 5.13). Idempotent check-before-create and crash-after-create
reconciliation key off it. Origin and canonical issue ID live in the immutable
daemon ledger. Discovered candidates are validated (repository, App authorship,
expected ledger bindings; markers in issue text are rendering hints, never
matching keys) before adoption. A durable creation intent fences recovery and
precedes the API call. Unledgered creations serialize per repository, because a
repository is the candidate collision domain: filing identities in one
repository share App authorship and candidate-visible fields. So recovery has
at most one outstanding intent per repository to bind. It adopts the single
validating App-authored candidate created in the intent window, or proves
absence before retrying. Residual ambiguity fails closed to a durable attention
item, never a blind retry. Rate, depth, and cost caps come from resolved
policy.

Freeside-origin issues enter intake as propose, never `auto_start`, and this is
enforced at every intake observation, including after relabeling. All label
intake demotes to propose in any repository where Freeside has ever filed and
no current valid issue-event authority profile exists. Automation-created
descendants cannot be attributed there. So no unattributed labeled issue in a
Freeside-seeded repository is trusted for `auto_start`. A current valid profile
restores `auto_start` eligibility only for the non-Freeside-origin issues it
admits that pass the intake proof check. Freeside-origin issues stay
propose-only whatever the profile state.

### 5.18 The World Model: Post-Merge Recompute and Frontier Projection

After a merge, the daemon recomputes its map of the project: what completed,
what is now unblocked, and what could run in parallel. Capture hooks record
work-unit bindings, completion criteria, dependencies, and scope from 1B.0.
Projection computation and its UI land in 1B.2 (Section 11).

A merge marks a unit done only through an exact daemon-recorded work-unit
binding and completion criterion (for example, the bound issue closed by the
merged PR). Partial, stacked, or related merges do not complete units. The
frontier projection uses only explicit declarations: dependency edges, declared
path scopes, contract serialization, and merge state. It binds to a
per-resource freshness vector (reconciliation is per-resource; there is no
global cursor to wait on). It renders per-resource staleness and incomplete
coverage explicitly. "No declared mechanical conflict detected" is the
strongest daemon fact; inferred parallelism is a labeled planner claim; unknown
scope serializes. The planner judgment call waits past 1B.

### 5.19 Deferred Subsystems: Provisional Contracts

The contracts below are design constraints for deferred subsystems, recorded
now and re-reviewed at implementation. None is scheduled inside 1B.

**Scoped consent grants (deferred past 1B).** A standing permission binds: the
canonical repository ID; the effect kind; an effect-specific authority identity
union (GitHub App identity, provider auth identity, or none); the trust,
policy, and profile digests; the operating mode; cost, use, and concurrency
limits; a validity interval; and the effect constructor/schema version. A
constructor change invalidates the match. The daemon selects matching grants;
agents and runs never nominate one. Issuing, renewing, widening, or extending a
grant requires version-bound human approval or a trusted-configuration change;
inference and runs may only propose. Grants are immutable: a renewal or a
changed binding creates a new grant. The operator can always revoke a grant
directly. Before the first irreversible request, the executor does three things
atomically. It matches every binding against current state. It reserves use,
cost, and concurrency capacity. And it marks the attempt started under the
exact grant and constructor version. The durable EffectAuthorized intent is the
linearization point: it binds the grant ID, constructor version, payload
digest, active epoch, and fencing token. A revocation committed before the
intent prevents it. A revocation after the intent does not prevent
reconciliation or adoption of an effect that may already have occurred.
Reconciliation and adoption run under least authority, and anything wider
raises attention. But if reconciliation proves the irreversible request was
never sent, or that the effect is absent, no new request may be made under the
revoked grant. A new request requires a current grant, lease, epoch, and a new
intent. Reservations confer no authority, and revocation invalidates them. Use
and cost reservations (accounting) are distinct from fenced concurrency leases
(correctness). In portable mode, grant changes and authorized intents
acknowledge only after they reach the remote frontier. After takeover, a stale
fencing token permits reconciliation only; creating an absent effect requires a
new current grant, lease, epoch, and intent. The daemon-owned effect executor
enforces fencing. Grants pre-answer a risk acknowledgement only; digest- and
head-bound approvals and non-waivable gates are untouched. Until this is built,
per-run authorization continues (an accepted cost; revisit at the 1B exit).

**External findings ingestion (deferred).** Externally produced reviews are
quarantined at entry. They enter as quota-bound advisory proposals (a future
effect kind added to the Section 5.13 registry with this subsystem) with
`external_untrusted` provenance, a raw-source digest, and a reconstructed
project and head binding, never an asserted one. The authenticated ingestion
actor is recorded separately from the claimed producer. The operator-selected
ingestion target is recorded separately from the artifact's own source binding
(exact / claimed / unknown; promotion to any blocking role requires exact).
Quarantined findings cannot block readiness, enter Section 7 adjudication,
trigger remediation, or consume remediation budgets. Automatic blocking or
remediation requires source-specific admission or explicit human promotion,
deduplication, and a declared authority-site contract (Section 5.13). External
findings never satisfy ReviewSource freshness, independence, or
review-completeness.

**Pre-publication adversarial pass (deferred).** An optional adversarial
self-review before a PR opens, so the external reviewer starts from a higher
floor. It reviews the daemon-constructed publication candidate after hostile
import, never the raw workspace. Each pass binds the exact candidate head and
invocation inputs; resolved policy bounds when it stops; the pass holds no
direct remediation or publication authority. Each remediation repeats the
gauntlet, verification, and the adversarial pass itself. This is distinct from
the Section 7 review requirement, which itself anchors pre-publication
(revision 28): the Section 7 pass is required; this pass is optional and
deferred.

**Managed reachability relay (deferred, unscheduled).** A future managed relay
may provide authenticated bidirectional byte transport between an enrolled host
and its paired clients. `freesided` stays loopback-bound and holds an outbound
connector authenticated by the Section 5.9 host identity; clients use ordinary
HTTPS/WSS; the transported protocol is Signet, unchanged (Section 5.2). The
Signet channel is end-to-end protected and daemon-anchored. A relay that
terminates edge transport TLS carries only an opaque inner channel. The client
authenticates that channel against a Freeside-owned control-plane identity.
That identity is independent of relay-controlled hostnames or PKI, and it stays
stable across enrolled-host takeover (Sections 5.9 and 5.10). So a paired
client survives a graceful or crash takeover without re-pairing. The per-host
Section 5.9 key authenticates the host to the relay, never the daemon to
clients. Anchor succession is a control-plane ceremony: only the control
plane's own existing anchor authority may admit a successor, never the relay or
any hosted service. Stability across legitimate takeover never licenses one
copied, unrevocable private key; the Section 5.9 no-omnipotent-key rule
applies. Compromise recovery may rotate the anchor and force re-pairing rather
than leave a compromised anchor trusted. Connector admission and continued
routing bind to the current Section 5.9 active host and epoch. An enrolled
standby or a returning stale host holds a valid host identity but never
presents as the serving daemon, and a host that is not active refuses
authoritative Signet service regardless of routing (the Sections 5.9 and 5.10
fencing stays the authority backstop). So relay misrouting degrades to
reachability loss, never to stale state or premature command acceptance.
Pairing bootstraps from the pairing secret itself, not from any certificate the
relay could present. It must resist a relay-positioned attacker who guesses or
multiplies attempts against a short code. So Signet authentication material
(device credentials, pairing codes and grants) is never readable or replayable
by the relay, and a relay presenting a valid edge certificate cannot
impersonate the daemon to collect it. The concrete mechanism (key-succession
chains, device-pinned bindings, secret-authenticated bootstrap) is
implementation-time design under this contract's re-review. Before any relay is
accepted, that re-review must refute these paths: credential-replay,
pairing-race, pairing-secret-guessing, daemon-impersonation (including
unauthorized anchor succession), stale- or passive-host routing,
takeover-stranding, and compromised-anchor-revocation. The relay must not:
become workflow authority; interpret or independently execute AttentionItem or
ClientCommand semantics; read, retain, or replay Signet authentication
material; possess provider or GitHub credentials; persist authoritative
workflow state; grant any action unavailable through Signet; or make
Freeside-operated infrastructure necessary for standalone operation. Relay loss
is reachability loss, never control-plane state loss. A hibernating per-host
rendezvous (for example a Cloudflare Worker with a per-host Durable Object) is
a plausible first implementation. Neither Durable Objects nor any provider
enters the application architecture or protocol contract, and the relay
protocol must stay implementable by a self-hosted or third-party service.
Artifact bytes stay daemon-served over the relay by default. A delivery cache
for large artifacts is a separate deferred concern, taken up only if measured
payloads demand it, and artifact authority stays local and digest-addressed
regardless.

**Readiness registry (deferred).** When built, it is a projection over current
typed proofs, recomputed on read, never a stored ready bit. The Section 10
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

Waivers exist only inside Failed and NotRun. They apply only to required checks
in registered waiver-eligible classes, and they name the waived dimension and
granting authority. The closed set of granting authorities contains explicit
human approval and daemon-owned trusted configuration. Resolved policy alone
can't mint a waiver, and inference may only propose one. Policy may tighten
daemon-owned applicability and requiredness floors but never weaken them.
Non-waivable classes have no waiver representation.

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

**Review is a durable stage that Freeside invokes and orchestrates in the run
workflow** (decider: user; revision 25). It requests and acknowledges review,
ingests normalized findings, adjudicates them (Finding Adjudication, below),
drives remediation, reverifies, re-reviews the new head, and escalates a stalled
or exhausted loop to durable attention. The first production ReviewSource is a
Freeside-invoked local Codex invocation.
GitHub-native Codex review, when observed, is recorded as best-effort extra
evidence; it never satisfies the review requirement. The trigger
falsification behind this is recorded in Sections 5.3 and 13 and on #427.

Each review pass binds the exact base and candidate head SHAs. A new candidate
head or an advanced base invalidates the pass and requires re-review.
Integration evidence follows the same rule: a base advance also invalidates
verification and check evidence bound to the prior
base, and readiness recomputes under the Section 6 re-gate before any ready
state is restored. The pass runs with fresh context independent of the
implementing invocation, a read-only workspace, and no publication
credentials; it receives repository instructions and verification evidence,
never the implementer's reasoning history. It returns normalized findings
with severity, location, and explanation, from which a stable cross-round
identity is derived (#702). It also records the provider, model configuration,
invocation, cost owner, and completion evidence. The findings → adjudication →
remediation → reverify → re-review loop is bounded by resolved policy;
exhaustion or ambiguity produces a
durable AttentionItem, never a silent stall. Failure classification matches
the publication boundary: transient failures retry with backoff;
configuration or quota failures create attention; durable contradictions
fail loudly.

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
the owner's own drill-down use (progress pulse, forensic drill-down on
agent/forge disagreement, and disposition reconstruction) is served by computed
readiness, the run timeline, and structured dispositions, not comment
threads. The owner's condition on this resolution pins the
EvidencePublisher's first slice (Section 5.15; #525, wave 5 per Section
11): at publication the PR carries the disposition history, including review
rounds, final dispositions with reasons for declined and deferred findings,
and the readiness derivation. The merged PR is therefore forensically
self-explanatory on the forge. The condition is not an immediate
publication precondition. Until #525 lands the forge carries no
disposition history. The store carries the durable review state: ReviewRecords
(round outcomes, finding identity), raw findings, and classifications.
Per-finding dispositions with reasons and the readiness
derivation are not yet persisted anywhere; persisting them is a
prerequisite both of #525's rendering and of any pre-#525 publication
that treats the store as the authoritative disposition record. The
trigger falsification forced
neither anchor; a Freeside-invoked reviewer can review either surface. The
PR-anchored shape remains the recorded, considered-and-rejected alternative
(revision 25's fork text, docs/history/decisions.md); revisit when real
usage shows the owner can't trust review they didn't watch. That remains the
fallback.

Review activity from outside the control plane includes human maintainers,
GitHub-native Codex when it fires, and other bots. On a published PR, it is the
deferred external review response capability (Section 11; #524); it never
satisfies this section's requirement, which stays Freeside-invoked and
pre-publication.

Sequencing preserves independence (spine-confirmed on #427): the #427
implementation unit, then production runs with Claude implementing and Codex
reviewing, then the #397 Claude ReviewSource promotion, then #408 Codex
execution routing. This order prevents Codex-implements plus Codex-reviews from
becoming the default pairing. The first production Codex review pass is also
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
Once agents are admitted (Section 5.4), the independence rule reads the
two selected agents' offers: by default their lineage groups differ, a
project lineup may relax that with a stated reason, and every card and
record carries which rule applied. Switching the review agent mid-run
opens a new convergence segment, so the new reviewer's first pass is never
counted as the old reviewer's next round by the yield policy.

The classifier is never the sole safety gate:

- A raw shadow finding that claims critical or high severity and receives a
  low-confidence classifier score cannot disappear silently.
- It receives a second adjudication, deterministic or from a distinct agent, or
  becomes an AttentionItem.
- A credible critical or high shadow finding blocks ready status.

Credibility is not a separate classifier output, and its guard is
fail-safe: wherever this section says "credible", it names a
deterministic guard anchored on review-contract severity, in which
classification confidence can add protection but never remove it. A critical
or high severity finding stays credible when classification is
missing or low-confidence, because the landed classifier annotates
materiality, not finding validity. No model output can mint or strip
credibility. The boolean is total across the normalized scale: every
finding, at every severity, is credible until a distinct validity
signal exists and marks it otherwise at resolved-policy confidence. Today no
landed signal can, so credibility filters nothing and the
word's whole force is the critical/high ceiling. A finding marked
non-credible never fast-paths and never disappears: it enters the
model adjudication, where the critical/high second-adjudication
ceiling still applies.

Severity resolves against a declared normalized scale: critical, high, medium,
and low. Each ReviewSource binding declares a
deterministic mapping from its native vocabulary to that scale, and a
missing or unmapped value fails protective, treated as high. The first
instance is Freeside's normalization choice for the production `codex_local`
binding: P1 → high, P2 → medium, and P3 → low. This is Freeside's mapping
decision, not a claim about the reviewer's own semantics.

Some contamination is accepted. Freeside does not pass or fail based on routing
results.

### Finding Adjudication

**Every finding batch is adjudicated before remediation authority is
exercised** (decider: user; revision 31; #697). Direct findings-to-remediation
routing assigned nobody the judgment that decides what a finding means for
the approved work unit: a credible finding can be required by the accepted
outcome yet prohibited here by the repository's own work-unit rules, a
legitimate adjacent improvement, a contradiction of the approved
specification, or governed by instructions that are ambiguous. Sending every
finding to a remediator risks silent scope expansion; treating every
non-local fix as deferrable risks false-ready work whose acceptance criteria
depend on the deferred fix. Adjudication distinguishes whether a finding is
credible, whether the approved outcome requires it, whether the repository's
own rules permit the proposed remediation to land in this work unit, and
which safe route follows.

Each review round with findings produces one immutable, digest-addressed
FindingAdjudication artifact bound to the run, the exact finding batch and
round, the approved specification artifact digest, the trusted
repository-instruction snapshot digest, and the resolved policy digest. Its
inputs are the approved work-unit specification, the immutable raw findings
with their versioned classifications, the proposed remediation surface, the
work unit's declared path scope, repository instructions from the trusted
base, prior disposition history, and any available structured repository
facts (Section 5.18 capture); never the implementer's reasoning history.
The engine derives each finding's proposed remediation surface from that
finding's normalized locations. A model never supplies it. A batch never
shares one union surface, so an out-of-scope sibling cannot
strip an unrelated in-scope finding of its fast path. The surface is
presumptive. Its enforcement backstops are the import boundary's path-scope enforcement
and the remediator's labeled pushback, each re-entering adjudication as
structured dissent when a correct fix must exceed it. Derivation fails
closed: containment runs only over canonical repository-relative paths,
their syntax validated against the trusted root and their existence
resolved against either side of the bound base and candidate trees. A finding
on a candidate-added or candidate-deleted file therefore keeps its
deterministic route. A finding whose location is missing, non-path, or
unresolvable in both trees yields no presumptive surface and never a vacuously
contained `allowed`. When its goal relationship is `required`, it falls to
`unknown` or attention. A non-`required` finding routes by its own row, which
consumes no surface. For each finding, the artifact records two normalized
axes, a recommended route, rationale and evidence, cited repository rules,
assumptions, viable
alternatives, and open questions. The route is the decision; the axes are its
evidence. A fast-path routing decision is an engine fact and carries no
proposal or confidence field. An adjudicator proposal, which is a model-residue
entry, also records a self-assessed proposal confidence on the declared ordinal
scale. The engine labels it as model output and judges it against the same
bounded-below resolved-policy threshold. A proposal whose confidence is absent, out of scale, or
below threshold is the ceilings' low-confidence output: it is not
accepted, and the batch parks to recommendation-led attention, with
`unknown` as its compatibility representation only where compatibility
exists (`required`). The vocabulary is repository-generic. Repository-specific
language, such as a lane, serialized-contract rule, or ownership file, appears
only inside cited instruction text and explanations, never in the normalized
outcomes.

The goal-relationship axis states what the approved outcome makes of the
finding: `required`, `adjacent`, `contradictory`, or `unclear`. The
work-unit-compatibility axis states whether the repository's rules let the
proposed remediation land in this work unit: `allowed`,
`work_unit_revision_required`, `separate_work_required`,
`human_decision_required`, or `unknown`. Validity constraints replace the raw
cross product. Compatibility is present exactly when the goal relationship is
`required`, the only case where remediating here is on the table. The route is
a function of the axes:

| Goal relationship | Compatibility | Route |
| --- | --- | --- |
| `required` | `allowed` | remediator → clean verification → re-review |
| `required` | `work_unit_revision_required` | park; recommend a specification revision through the `spec_approval` revision path where prose alone must change, or a replan (a new run under a revised work unit) where the trusted work-unit scope itself must change (declared paths, dependencies, serialization), which a same-unit revision cannot alter |
| `required` | `separate_work_required` | park; recommend prerequisite work (a Section 5.17 proposal where it is an issue), wait or stop; never defer-and-ready |
| `required` | `human_decision_required` | recommendation-led `finding_adjudication` attention |
| `required` | `unknown` | park plus recommendation-led attention; no scope widening |
| `adjacent` | absent | reasoned deferred disposition; its recorded reason and the adjudication artifact carry the follow-up recommendation that the Section 5.17 human-gated proposal path (1B.1) consumes when it lands |
| `contradictory` | absent | reasoned declined disposition, or the `review_dispute` item, where the human adjudicates, when the finding's severity is critical or high (the ceilings' second-adjudication case) or the self-assessed proposal confidence is below the resolved-policy threshold |
| `unclear` | absent | recommendation-led attention with gating questions |

These eight rows are the complete valid vocabulary; implementation fixtures
enumerate them, not a sample of the cross product. A necessary finding that
is incompatible with the current work unit parks or replans the run; the
constraints make silent deferral structurally unrepresentable, because
`required` has no route to a deferred disposition.

Permission has a presumptive baseline, not an affirmative-citation
requirement: remediation whose proposed surface stays within the work unit's
declared paths is presumptively `allowed`, and `allowed` is representable
only as an engine-derived value. The deterministic declared-path containment
check is its sole producer, so model output cannot mint permission and the
adjudicator structurally cannot infer permission to exit the declared
surface. Rule interpretation is required only where remediation would exit
that surface or a rule is affirmatively implicated; for that residue,
missing, conflicting, stale, or low-confidence interpretation fails
conservatively to `unknown` or human attention. Without the presumption,
every ordinary in-scope fix would collapse to `unknown` and park the loop.
On the deterministic path an affirmatively implicated rule reaches
adjudication only through the structured residue signals below or the captured
repository facts. That residual is accepted by decision: the downstream
human merge gate, not fast-path rule interpretation, backstops it.

The stage is engine-run with a model residue (Section 5.13: deterministic
policy jobs stay engine-run; a model is invoked only where judgment is the
work). The engine derives compatibility deterministically wherever it is
mechanically decidable, starting with declared-path containment, and consumes
the classifier's materiality-with-confidence annotation as presumptive
goal-relationship evidence. Both fields use declared ordinal scales of `low`,
`medium`, and `high`. They match the landed classification vocabulary, so
stored records need no migration. The fast-path predicate is exact:
materiality and confidence must each meet or exceed their resolved-policy
dispatch thresholds. The thresholds are bounded below at `medium` or `high`,
never `low`, and default to `high`. A `low` value in either field is the
low-confidence residue case by definition.
A missing, unrecognized, or below-threshold value in either field never
fast-paths; it fails into the model residue. Every term the dispatch predicate
consumes (severity, credibility, materiality, confidence, location, and
surface) carries a declared scale, a named producer, and a fail-closed fallback; a
predicate input outside its scale is a malformed annotation, never a
routing choice. The no-model fast path is one-directional,
toward remediation. A credible, confidently material, in-surface finding
routes to the remediator with no model adjudication call. The declared paths
bound this preference for an in-surface fix, which is the loop's
normal work. Selecting the `adjacent` deferral route always takes the
model adjudication: the adjudicator is the only site that consumes the
approved specification, and a spec-blind materiality annotation cannot
decide a spec-relative route.
The model residue is that deferral direction, boundary-exiting fixes,
contradicted specifications, ambiguous goals or rules, low-confidence
classification, and structured dissent. Structured dissent includes a
remediator's labeled pushback, a human challenge, or an attempted fix rejected
by the import boundary's path-scope enforcement. Each re-enters adjudication
instead of looping silently.

The adjudicator is a Section 5.13 ceiling-bounded annotation site on the
daemon-side inference contract. Its output is a labeled proposal and
explanation, never trust computation, transition legality, publication
eligibility, or proof that a finding is fixed; it cannot widen issue scope,
alter acceptance criteria, file an issue, or declare a finding fixed.
Ceilings: a critical or high severity finding, credible or not, never routes to
a declined or deferred disposition without a second adjudication, either
deterministic or from a distinct agent, or a durable AttentionItem. Malformed,
missing, or low-confidence output is never accepted. The batch parks to
recommendation-led attention, with `unknown` as its
compatibility representation only under `required`;
adjudication consumes the review loop's resolved-policy bounds and the
cumulative Section 5.13 budgets attributed to root lineage; every site ships
a deterministic fake. Routes terminate in the landed disposition vocabulary
unchanged (fixed / declined / deferred, reasons mandatory): `fixed` still
requires a later same-base, different-head remediation review where the
finding's stable identity no longer appears. A re-emitted identity re-enters
adjudication as a failed prior fix, never as
a fresh finding whose disposition can strand the original. That absence
proof keys on a finding identity stable across the remediation rounds of
one work unit: the deterministic `domain.Finding` fingerprint (#702) over
the finding's review source, location path, and whitespace-normalized
explanation. It excludes the invocation, candidate head, severity, and line
range, which can legitimately change between rounds. A finding without such an
identity fails closed, so a finding whose fingerprint can't be computed is
never declared fixed.

Adjudicated declines and deferrals cite the adjudication artifact digest in their
recorded reason. Parked runs never publish, so the publication-time
completeness rule (#525), exactly one final disposition per finding in the
current lineage, is satisfied structurally, never by deferring a required
finding. Review completion is disposition-aware: the review requirement is
satisfied by a round where every finding carries a final disposition. `fixed`
comes through remediation re-review; declined and deferred come through their
adjudicated dispositions. A round fully dispositioned without remediation
publishes without a futile re-review
of the unchanged head. The landed clean-only publication check is the
pre-adjudication interim; the wave 6 unit extends it to this
derivation, which the #525 readiness derivation carries to the forge.

Human-facing adjudication always leads with a recommended route and why,
then assumptions, repository-rule citations, viable alternatives with their
consequences, and a small set of gating questions; a bare "what should I do?"
interruption is defective. The recommendation is a labeled model proposal;
bindings, containment verdicts, and cited instruction text are daemon facts
in a separate register (Section 5.13). The human can accept the
recommendation, choose an alternative, answer a question, challenge an
assumption, request further elaboration, discuss again, or stop and leave the
run parked. Each Discuss response re-invokes the stage against the same
version bindings and produces a new immutable artifact recording how the
recommendation changed and which feedback changed it. Conversational text
alone grants no authority. Route execution binds to the typed command, the
artifact digest, and the item version (Section 5.14). The loop is
policy-bounded and remains parked when unresolved.

Repository rules may further restrict where work lands; they can never
weaken Freeside's non-waivable safety, trust, verification, or publication
gates (Section 3.1). Admitted external review activity (#524) consumes this
same adjudication and routing rather than a second path; Section 5.19
quarantine and source-specific admission remain upstream, and a quarantined
finding never enters adjudication. Adjudication is not the deferred planner
judgment call (Section 5.18): it consumes captured work-unit facts as inputs
and never computes parallelism or frontier claims. The classifier and the
adjudicator are separate sites by decision: they differ in inputs (a finding
with code context versus the specification, declared scope, instructions,
and disposition history), in binding cadence (per-finding versioned
annotation at ingestion versus per-batch digest-bound proposal), and in
authority contract (every site carries exactly one). The 1B sampled
classification accuracy measurement requires classifier output measured
independently of routing pressure. The FindingAdjudication artifact, its
persistence and sync exposure, the `finding_adjudication` item type, the
engine dispatch, and the adjudicator site are later implementation units
(Section 11, wave 6); any change to domain types, migrations,
StageDriver/ReviewSource surfaces, or API schemas splits into serialized
`kind:contract` units with their generated consumers. The normative-term
discipline above requires a declared scale, named producer, and fail-closed
fallback for every predicate input and proposal output, with the valid and
failure cells and their interactions enumerated. This discipline is an
acceptance requirement of that wave 6 `kind:contract` unit. Its typed schema
and fixtures are the exhaustive enumeration, and this subsection states the
design's constraints, not the field catalogue.

## 8. Observability and optimization telemetry

Telemetry uses typed relational rows with stable join keys. Transcripts are
drill-down pointers, not the primary data model.

Each run records:

- stage and all governing digests;
- per-key rein preset or override provenance;
- the admitted agent and launch digests, the treatment digest, and the
  requested, admitted, and observed selection facts (Section 5.4);
- driver, credential mode, egress profile, and operating mode;
- artifacts and their provenance;
- tokens and cost, as billable cost, reported usage, and quota consumed
  where the upstream exposes a unit, with the raw usage payload kept as a
  redacted, sourced derivative;
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

1. **The ask and the facts**: `requested_decision`, the item's
   recommendation when one exists (Section 4), and deterministic card facts
   (verdicts, diff stats, counts, digests, timing). Facts are
   daemon-produced only (Section 5.13). A recommendation renders in its
   source register only after its Section 4 source-specific provenance has
   been revalidated. A `daemon_policy` recommendation renders as a card fact,
   an `agent_judgment` recommendation as a labeled proposal, and a
   `project_policy` recommendation citing its exact policy key and digest. A card
   that carries one leads with the recommendation and its reason
   ahead of secondary actions and evidence: the recommendation-led
   composition specified for `finding_adjudication` below generalizes to
   every type that carries a recommendation.
2. **The summary**: what happened, why, and what remains open, with
   uncertainty preserved; absorbable in seconds. A labeled agent claim,
   present only where the card concerns agent work: a purely mechanical card
   (`system_health`, `blocked`) carries daemon facts alone.
3. **Evidence**: the `evidence_snapshot` packet (Section 5.15). Evidence
   precedes any long-form agent text.
4. **Drill-down**: full artifacts, full specifications and diffs, and
   transcript pointers (Section 8).

A client renders only the requested decisions it can faithfully collect
and execute. An action outside the client's capability is omitted from the
action surface and recorded in the drill-down layer, so an audit still
shows what the daemon asked; when no faithful response is within the
client's capability, the card states explicitly that the decision cannot
be taken on this client and that the item stays open. An unimplemented
action is never rendered as a disabled control, and roadmap language never
appears in card copy.

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
| `finding_adjudication` | The recommended route and why, as a labeled proposal; the finding and the daemon's binding and containment facts in a separate register. | Assumptions, cited repository instructions, alternatives with consequences, gating questions, then the full artifact and code context. |
| `execution_failure` | Daemon facts: failure class and failing step. Labeled diagnostic claim: probable cause. | Log excerpt and transcript pointer. |
| `agent_question` | The question as a labeled agent claim, self-contained: what is blocked and any enumerated options. Answering never requires the transcript. | The agent's supporting context. |
| `publish_blocked` | The trust rule that failed (daemon fact). | The failing artifact or scan detail. |
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
- Recommendation override rate: the fraction of recommendation-bearing
  decisions resolved by a non-recommended action, by item type and
  recommendation source. A decision is a forced override only when its
  revalidated Section 5.14 `DecisionActionSurface` omitted the recommended
  action; it is stratified separately and never counted in this rate. A
  decision whose surface is missing or cannot be revalidated is unclassified
  and excluded from both rates, never inferred as voluntary or forced.
  A calibration signal on recommendation quality, never a target in
  either direction.
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
| `freesided doctor` | Checks conformance, the workspace-handoff gate, checkpoint encryption, backup age, artifact closure, restore-test age, and, from 1B.1, stored-credential integrity (a truncation and corruption probe). The integrity probe extends to the Section 5.4 account probe only after an empirical spike proves the Codex app-server probe runs against the access-only read snapshot and never triggers a refresh outside the mutation lease; until that spike passes, doctor reports integrity alone. Probe results are observation (Section 5.4): they file `advisory` `system_health` items and proposals, feed the operator-facing profile projection's display fields, and are read by nothing else. It runs on a schedule and files `system_health` items. |
| `freesided auth add`, `auth adopt`, `auth list`, `auth doctor`, `auth re-enroll`, `auth disable`, `auth enable` | Guided identity and enrollment lifecycle (Section 5.4); `auth doctor` ships with the account-probe unit (#868), gated on the #866 spike like the probe itself, never with the enrollment unit. `add` enrolls one harness client against one route for a new or existing `AuthIdentity`, creating a `ClientEnrollment` with its sanitized single-route store and initial generation; the transaction records the enrolled mode and the account binding `re-enroll` later compares against, under the identity's lease, and refuses when the store cannot yield it, so an expired or corrupt store is enrolled as a new identity rather than adopted. For Codex it packages the import, rotate, and snapshot sequence that `enroll-codex` exposes as separate required flags; for Claude it captures a setup token interactively, validates its length and performs an auth check before storing it (the truncation class the 1B.1 integrity probe detects), and keeps the token out of argv, shell history, logs, and client responses; for pi it captures the ChatGPT login into a one-entry store and performs a daemon-driven refresh before accepting it. `adopt` is the cutover command: it adopts each interim identity's store as an enrollment and emits the proposed baseline patch (agents, deployment lineup, attended-run marks) for the operator to commit; it never writes the tree. `list` shows identities and their enrollments with the masked label; `doctor` runs the account probe for one identity on demand once #868 lands; `re-enroll` replaces one enrollment's credential through the daemon-owned transaction while no execution can use the identity, for the same account only: where the provider exposes an account identity (Codex, pi), the transaction compares the incoming credential's account against the one recorded at enrollment, authoritatively and independent of the probe, and refuses a mismatch; where it does not (the Claude floor), the operator attests same-account and the attestation is recorded; every re-enrollment appends a generation, so later records show the credential changed. A different account is a new identity, never a re-enrollment; `disable` sets the identity's `enabled` off, withdrawing every agent on it from selection without touching credentials, and `enable` reverses it. |
| `freesided submit` | Registers a manually initiated source work item, starts elaboration, and reserves its future implementation run. |
| `freesided reattempt --parent-run <run>` or `--campaign <campaign>` | Requires an operator reason and allocates the campaign's next attempt from an already approved specification; a live parent is refused. |
| `freesided resume --run <run>` | Reattaches observation to one exact non-terminal run without creating a replacement; terminal runs are refused and point to `reattempt`. |

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

The Phase 1 reference deployment is fully unmanaged (Section 5.1): Tailscale
for remote reachability, ntfy for notification delivery, `freesided` as
workflow authority with local state and artifacts, an operator-selected
conforming replica backend when portable mode is enabled, and an
operator-controlled external probe for away-from-host monitoring. Future
managed implementations (relay, push, storage, monitoring) may substitute
convenience infrastructure without changing the authority model, a managed
replica backend carrying Section 5.10's head trust (activation fencing and
recovery frontier) as the one scoped exception; every one is deferred,
none is scheduled in Phase 1,
and the unmanaged deployment remains supported permanently.

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
- blocking candidate automation-control paths and surfacing
  reviewer-instruction edits as advisories;
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
pre-publication) → finding adjudication → yield-driven remediation and
pattern sweeps → diminishing-returns or dispute item → clean: PR under a
trust profile, carrying the Section 7 disposition history (#525) → checks →
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
- finding adjudication between classification and remediation (Section 7;
  #697);
- convergence policy and the shadow arm;
- provenance-gated EvidencePublisher;
- experimental `max_parallel_executions` per auth identity, visible to
  scheduling;
- the Codex execution driver, an execution capacity hedge against
  single-provider stalls (Section 14): the `agent-codex` agent base, the
  project images the reusable builder derives from it (Section 5.7),
  ward's second vendor topology, and the Codex adapter registration land
  as separate follow-on units behind the admitted-agent contract (Section
  5.4), sequenced after the 1A.2 exit and blocked on the #401 pre-adoption
  gates; selection is a lineup line, never silent; and
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

#### 1B.1: Decision, Operational, and Provider Closure

1B.1 spans waves 7 through 9 of this section's coordination table, the way
1B.0 spanned waves 3 through 6. Each wave proves one outcome and ends with
its own audit; the internal exit is evaluated once all three have closed.

- **The decision surface closes (wave 7).** The revision-40
  attention-presentation contracts, their daemon fact producers, and client
  adoption: every Phase 1 card action executes on Mac and iPhone, every card
  is self-contained at its Section 9 altitude, and facts stay distinct from
  claims.
- **Operation closes (wave 8).** Human-gated follow-up issue filing (Section
  5.17), consuming the follow-up recommendations recorded by adjudicated
  deferred dispositions (Section 7); the doctor credential-integrity probe
  (Section 10); the stall heartbeat (Section 5.12); the external
  daemon-liveness probe (Section 5.2); held-work and stopped-operation
  signals; the clean-machine onboarding proof; the registry egress profile
  and the policy-gated image rebuild; and re-entry of published-PR activity.
- **Providers close (wave 9).** One agent vocabulary across execution,
  review, and daemon judgment (Section 5.4); the Codex execution driver and
  its enrollment cutover; and pi elaboration.

#### 1B.2: The Initiative View

The Section 5.18 frontier projection and deterministic initiative view ship as
one minimal deterministic projection (owner decision), under Section 5.18's
rendering and coverage discipline. This placement materially overturns two
statements recorded in Section 13: Section 4's
GitHub-Projects-as-all-work-view and this section's former Phase 3 placement
for the initiative view.

Clients don't edit settings directly. Today every configuration change passes
Section 5.8's control-plane gate through an operator-authored PR or, for
recurring preferences, the Section 4 policy-proposal path. When deferred
settings surfaces ship, a configuration-change proposal kind joins the Section
5.13 registry with those surfaces as its consumer. Approval cards, never edit
forms, stay the client surface.
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

Approvals decidable from the phone covers every Phase 1 card action except
turning a recurring diminishing-returns preference into a project-policy
proposal (`convert_to_policy`), which waits for its deferred control-plane
proposal surface (Section 4) and is omitted from the client's action
surface, never rendered disabled, until that surface lands (revision 40).

### Implementation Coordination (Building Freeside with Agents)

Contracts and fakes coordinate implementation. CI keeps lanes honest.

| Wave | Shape | Work |
| --- | --- | --- |
| **0: foundations** | Serial | Module, dual-platform CI, domain package, schema and migrations, outbox, interfaces, fakes, and provisional API schema. Domain and migration PRs are exclusive. Shared-interface work is `kind:contract`. |
| **1: subsystems** | Parallel lanes | signet, gauntlet, publish, ward, and the saddle pair. |
| **2: convergence** | Integrated | Workflow engine, real driver, end-to-end fakes, and real work. The **spine** owns integration and contract adjudication. |
| **3 (1B.0): loop foundations** | Parallel lanes | Spine, serialized: the Section 5.16 scheduler (four timer kinds, trusted-job ticker migration), then Section 5.18 capture-hook recording. Ward: #401 gates 1/2/4/5 as parallel probes, then the #404 base image pinned per gate 2's outcome. App: Mac-first operator access (Section 10). |
| **4 (1B.0): the review stage** | Serial | The spine rescopes #406/#407 into review cores and execution remainders, then lands the review-selection contract core, the review ward-topology slice, #405 only if review needs a project-derived image, and #427 (landed PR-anchored under the then-open Section 7 fork, resolved pre-publication in revision 28; the implementation re-anchor is #527, unscheduled). Its close stands the minimal loop; real-backlog use begins. |
| **5 (1B.0): loop depth** | Parallel lanes | Elaborator and daemon research fetching with the spec-approval gate; label-initiator intake; the Section 5.13 classifier and diagnostic sites; the provenance-gated EvidencePublisher (first slice: the Section 7 disposition history at publication, #525); the runs list and run timeline; the `max_parallel_executions` experiment. The contract track drains the Section 6 state algebra, then the effect-registry retrofit of `run_proposal`. The supervision core consumes the revision-27 Section 5.2 contract, pulled forward by owner fiat: #454's daemon side and the app-side LaunchAgent and menu-bar unit. |
| **6 (1B.0): convergence and yield** | Integrated | Convergence policy and the Section 7 finding-adjudication routing (#697; the spine assigns its contract splits at wave planning); the Claude shadow arm with second adjudication and sampled classification accuracy; automatic re-review of remediation heads as a standing integration test; yield history on ready-for-final-review; the full chain on the real backlog. iOS on-device install (Section 10). 1B.0 exit. |
| **7 (1B.1): the decision surface** | Parallel lanes | The decision surface closes and reads from the phone. Contract-first, one serialized chain whose positions the spine assigns at planning: the revision-40 attention-presentation cluster (the Section 4 recommendation shape and Section 9 typed minimum card facts, #917, which must retire `adjudicate` or reassign it to an executable `review_dispute` transaction before client adoption; decision-surface identity, #942; per-type card facts, #724; adjudication finding context, #892; per-invocation cost observations, #901), then transaction closure for the remaining Phase 1 pending actions (#918, #919, #920, #921) and the retirement of `choose_alternate_profile` (#936), then Section 5.15 evidence metadata (#922), pairing identity facts (#923), readiness rendering (#982), and the Section 8/9 comprehension-telemetry contracts the wave-10 exit evaluation reads (#924, the first unit to slip to wave 8 if review bandwidth binds). Beside the chain: the daemon fact producers, client adoption (the provisional Swift `ActionOutcome` and mock server converge with the daemon's `discuss` and spec-approval `request_changes`), and the Section 9 summary layer (#723, stage-agent-sourced, no daemon-inference call). The adjudication-size contract (#961) is placed here or in wave 9 at planning. Deferral drain: the attention-presentation and card-fact clusters only. Exit proof: every rendered Phase 1 action executes on Mac and iPhone; no action stays pending, disabled, or decorative; every card is self-contained at its Section 9 altitude; facts stay distinct from claims. |
| **8 (1B.1): operational closure** | Parallel lanes | Freeside runs unattended, says when it is stuck, and lets published-PR activity back in. Human-gated follow-up filing with the `effect_proposal` card (Section 5.17); the doctor credential-integrity probe (Section 10); the stall heartbeat (Section 5.12); the external daemon-liveness probe (Section 5.2, #510); the held-work item (#766); the standing stopped-operation indicator (#980); device listing and revocation (#981); the clean-machine onboarding proof (#428); and the egress floor's first capabilities above it (Sections 5.4, 5.7): (a) the `provider_registry` profile, its policy field, and ward allowlist conformance, `kind:contract` because `EgressProfile` is a domain enum carried in the admission record, then (b) the policy-gated project-image rebuild in the reusable builder, `starts-after` (a) because its gate reads the registry set (a) declares; both build on merged #302 and #334. Re-entry after a ready-item invalidation (#502; the spine splits its contract half at planning) and external review ingestion on published PRs (#524) share the re-entry trigger shape and land together. Deferral drain: the operational and re-entry clusters. Exit proof: a clean machine reaches an unattended real run; daemon death, crash loops, stalls, held work, a stopped state, and external review each alert without terminal patrol or manual polling. |
| **9 (1B.1): provider diversity** | Parallel lanes; split-eligible | One agent vocabulary and a second real provider. The agent-vocabulary contract chain, positions assigned at planning: review admission and provenance (#898), the cross-lane failure model (#899), whether daemon judgment roles consume lineups (#900, decided before any utility agent exists), then agent and run facts in the clients (#979). The Codex tail: the adapter registration (#406, `starts-after` the merged admitted-agent contract #894), ward's second vendor topology (#407), the continuation compatibility digest (#873), then #397 by explicit owner decision on the wave-6 shadow evidence, then the StageDriver binding (#408, `merges-after` #873; Section 7 keeps #397 ahead of it so that Codex-implements plus Codex-reviews does not become the default pairing); the alternate-provider retry card (#869, `starts-after` #406 and #408). Ward fronts with no open prerequisite, startable at wave start or earlier by fiat: the Codex probe refresh-safety spike (#866) and guided enrollment with the two-step cutover (#867). The doctor account probe (#868) `starts-after` #406 and #866. The pi adapter, enrollment, and elaboration agent (#895) `starts-after` #897 and #867, elaboration only, with its pre-adoption gates run against the pinned build. The spine splits this wave into 9a (contracts) and 9b (adapters) at planning if the measured chain length exceeds review bandwidth; a realized split makes those halves numbered waves through a plan revision, because tracker titles must match this section's resolver pattern. Deferral drain: the agent and provider clusters. Exit proof: a real unattended Codex run and a pi elaboration; provider switching explicit in the lineup and visible in the clients; correct cost and independence records (#901); quota and capacity failures recover through the retry card, never a silent fallback. 1B.1 exit evaluation. |
| **10 (1B.2): the initiative view** | Integrated | Many work units become one picture. Typed relationship kinds in the Section 5.18 capture records (#884, `exclusive-with` every open contract unit), the frontier projection, and the deterministic initiative view rendering the dependency graph (#885). 1B exit evaluation against recorded comprehension and operational evidence. |

Wave 7's transaction closure also retires the `publish_blocked`
`choose_alternate_profile` action (#936, revision 44). Which publication path
a repository uses is repository configuration settled when the repository is
onboarded, never a per-item choice, so the card keeps rerun, inspect, and
stop. Publishing through a fork when the repository is not pushable is a plain
publication feature deferred to #1042. The phone-decidability exit holds once
no rendered action stays pending.

The deferral drain is bounded per row. Waves 7 through 9 each drain the
clusters their row names and nothing else; a deferral outside them stays in
the queue. The long tail, including the `kind:fix` items on production paths
that no 1B exit proof depends on, does not drain in 1B: it binds to Phase
2's hardening or to the issue's own trigger, and a wave sweep re-examines it
only when a scheduled unit trips its recorded boundary condition.

Review bandwidth limits parallel width. Every wave ends with a fresh-context
adversarial review by an agent given only the repository and its documents,
never this design history. `AGENTS.md` defines the issue protocol; each
wave's unit list lives in its pinned tracking issue, while this table records
shape and sequencing. The single source for resolving live wave status is a
deterministic three-state resolver over every pinned issue whose title matches
`^Wave [0-9]+ \([^)]*\) tracking$`, evaluated on the set of title matches
before filtering by issue state:

1. **Active-wave:** exactly one matching tracker, open. It resolves the
   current phase, wave, and active implementation front: its title gives the
   wave and internal exit, this table's row gives the phase and shape, and its
   Implementation order digest gives the active front. The scheduling door is
   open.
2. **Inter-wave:** exactly one matching tracker, closed. The closed tracker
   records the just-completed wave; there is no active implementation front and
   the scheduling door is closed. It is a legitimate observed state between a
   wave's close and the next wave's planning, not a defect. Explicit `Plan #N`
   and `Handle #N` fiat still proceed, because fiat is independent of wave
   state; scheduled self-selection does not, because it needs an open current
   tracker.
3. **Invalid:** zero or multiple matching trackers. This is a spine-repair
   error that must be escalated to the human, never guessed through: pinning
   alone is insufficient because other tracker types may also be pinned, and
   the resolver cannot choose among absent or competing authorities.

A wave-boundary procedure keeps exactly one wave-title-matching issue pinned;
unrelated trackers (for example the standing audit and reliability trackers)
stay pinned for their own purposes and never count toward wave state. Closing a
wave leaves its closed tracker pinned as the inter-wave marker, and the next
wave-planning operation moves that wave-title match to the new populated
tracker. Because those standing pins occupy slots under GitHub's three-pin cap,
the wave tracker holds a single swappable slot and the outgoing and incoming
trackers cannot both be pinned at once; with no atomic pin swap the transition
is non-atomic. The spine's wave-planning operation performs it idempotently and
recovery-safely, discovering and reusing any orphaned open-unpinned tracker
rather than creating a second, and an invalid wave-title cardinality is what the
resolver escalates on. The detailed interruption-safe procedure is owned by that
executor; see docs/coordination.md and #828.

The digest remains a derived view of the authoritative Dependencies fields in
the tracked unit issues. If they diverge, the unit issue wins and the tracker
is repaired in the same operation; readers still use the tracker as the one
entrypoint for live status rather than searching unit issues or stable files
for a competing projection.

Stable repository documents point to that resolution rule instead of
asserting live phase or wave status. Any competing assertion is a coherence
defect. Verify the invariant with the following sweep, whose only result must
be the resolution rule above:

```sh
grep -rniE "current (phase|wave)([^'[:alnum:]_]|$)|wave [0-9]+[^.]*underway" README.md AGENTS.md docs/
```

The 1A backlog also serves as elaborator fixtures.

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

Revision 44 ("Retire the alternate-profile action"):

1. **`publish_blocked` no longer offers `choose_alternate_profile`**
   (Sections 4, 9, 11): the card keeps rerun trust evaluation, inspect the
   trust failure, and stop. Revision 4 meant the action as a per-item pick
   among pre-approved trust profiles whose `pr_execution` mode differs
   (same-repository PR, fork PR, local only): the escape hatch when the
   audited same-repository path is blocked. Which publication path a
   repository uses is repository configuration settled at onboarding, and
   fork versus same-repository follows from whether Freeside can push to
   the repository, so nothing is chosen per run. A choice offered while a
   run is blocked is exactly where the weaker path gets picked under
   pressure, which the no-remembered-defaults and no-automatic-fallback
   rules exist to prevent; the plan already keeps `convert_to_policy` off
   the phone for the same reason. Of the four trust rules only
   `trust_profile_drift` could be helped by a different path, and its fix
   is re-approving the repository's configuration on the Mac, then
   rerunning. At this revision the daemon holds one activated profile per
   repository, never produces `fork_untrusted` or `local_only`, and offers
   the action on no item, so retiring it removes a pending enum member and
   nothing a card renders. Rejected: building the approved set
   (multi-profile approval, fork publication, and local-only handoff:
   three subsystems for one block cause); offering only the current
   profile on drift (a one-item picker over `rerun_trust_evaluation`, a
   decorative control revision 40 forbids); keeping the action pending
   (fails the wave-7 exit). Fork publication for a non-pushable
   repository is deferred as a plain publication feature (#1042). The
   wider finding, that Section 5.5's CI audit is outside an agent control
   plane's job and Section 5.8's protected paths are the control that
   matters, is recorded for its own revision (#1041), not made here.
   (User; devlog 2026-09-01-0841-retire-alternate-profile.md; #936.)

## 14. Risks

| Risk | Current response |
| --- | --- |
| Provider credentials in `subscription_contained` | Document the residual; enforce egress floors; let the daemon fetch research for the most exposed stage; provide `api_key_isolated` as the escape. |
| Registry egress under `subscription_contained` | Keep `provider_only` the default and the floor fixed; admit `provider_registry` only per project policy through the per-authority proxy allowlist with TLS server-name pinning and no DNS, to public package registries consumed read-only, with any other authority routed to the `provider_web_read` record; conformance-check the realized allowlist against the declared profile. Residual: the tunnel cannot constrain method or path, so a registry that co-hosts a write endpoint accepts an attacker-credentialed publish; exclude such hosts per project where the residual is not acceptable, and provide `api_key_isolated` as the escape for anything wider. |
| CI privilege crossing | Attest effective authority; block candidate automation changes; fail closed on drift; prohibit the daemon host as a runner. |
| Reviewer-instruction poisoning | Compose agent and reviewer instructions from the trusted base, never the candidate; detect instruction-path edits mechanically and surface them as advisories the human merge gate reads (Section 5.8). |
| **Workspace-handoff uncertainty** | Resolved by the workspace-handoff spike: the strong class is declared and conformance-gated (Section 5.7); the same-VM fallback is refuted by execution, never implemented or declared. |
| **Codex cloud review as a load-bearing dependency** | Realized 2026-07-31: the live-run trigger falsification (#427) showed no App-visible trigger path. The dependency is removed: review is Freeside-invoked (Section 7), and native review is best-effort extra evidence. |
| Single-provider execution capacity | Claude usage limits can stall real work. Schedule the 1B Codex execution driver as a hedge (Section 11); keep selection explicit as a lineup line, never silent (a lineup may name the switch per failure class, Section 4); usage remains observed telemetry (Section 8). |
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
| Prompt injection, the organizing threat | Keep write credentials out of workspaces; prove handoff; import through the out-of-process two-channel gauntlet; use trusted overlays; block automation paths and surface instruction-path edits; enforce egress floors; fetch research through the daemon; gate irreversible actions; use budgets and brakes. |

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
