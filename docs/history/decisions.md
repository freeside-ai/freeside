# Freeside plan decision history

The full decisions log of every plan revision, verbatim, including revisions never committed to this repository (2, 3, 5, and 6 were superseded before commit). docs/plan.md's Section 13 carries only the current revision's changes; this file is the archaeology. Grep it when re-litigating; promote to docs/decisions/ ADRs on first re-litigation, citing the revision entry here. The design dialogue behind these revisions included six external design reviews, adjudicated turn by turn; the raw review texts may optionally live in docs/reviews/.

Deciders are named in parentheses throughout. Revisions 1 through 7 were produced 2026-07-13.

---

## Revision 1 (committed as the initial docs/plan.md)


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


---

## Revision 2 (superseded before commit)


Held from v1: daemon owns workflow state, clients thin; GitHub owns source, review, merge; chat authors artifacts, engine executes artifacts; between-stage gates daemon-native; Go daemon (rationale corrected: no goroutine fault isolation; stall isolation, appliance fit, compatibility culture) and SwiftUI clients; monorepo; verification defines completion; personal-tool scope; Freeside never rebuilds diff review.

Changed in v2 (decider in parentheses):
1. **No GitHub write credential in any workspace; daemon-side push through a validating policy chokepoint.** (Review 1; accepted as v1's largest flaw.)
2. **"Bounded execution isolation"** replaces "hard isolation"; provider-credential exposure is a named residual risk pending spike D1. (Review 1.)
3. **StageDriver over vendor batch modes replaces ACP for execution; ACP returns in Phase 3 for interaction.** (Review 1; also the cleaner subscription-terms posture.)
4. **Session durability: workflow recovery guaranteed from inputs/artifacts; provider resume best-effort.** (Review 1.)
5. **Capabilities fixed at spawn; no in-session grants; request-and-exit on insufficiency.** (Review 1; strawman vs v1's intent but adopted as costless hardening.)
6. **Control-plane config loads only from approved default-branch commits; workspace copies are data.** (Review 1.)
7. **Workflow in Go; YAML is policy only; DSL deferred until three shapes repeat; typed digest-addressed artifacts; digest-bound approvals.** (Review 1.)
8. **Verifier and janitor are deterministic software; card facts computed by the engine, agent text labeled as claims.** (Review 1.)
9. **SQLite authoritative for workflow state; inbox/outbox with idempotency keys; corrected restart claim.** (Review 1.)
10. **Polling-first GitHub; webhooks later as optimization.** (Review 1; v1's webhook topology was unimplementable as drawn.)
11. **Runner backends are capability classes; no silent downgrade; SwiftUI work exempt from the pipeline until a macOS class exists.** (Review 1.)
12. **Subscription auth is a Phase 0 determination; concurrency leases; API-key mode a supported fallback; usage is observed telemetry; budgets limited to enforceable controls.** (Review 1.)
13. **Cross-provider review demoted to routing hypothesis with a paired-experiment protocol.** (Review 1.)
14. **Attention inbox is part of the control system: AttentionItem domain model with lifecycle and digest binding; minimal three-screen client and push are Phase 1; headless-first sequencing reversed.** (Review 2.)
15. **Elaboration stays in the v1 slice, severable by design; kill criterion excludes elaborator weakness.** (User choice, overriding review 1's deferral; review 2 concurred.)
16. **Discuss = stage re-invocation with a feedback artifact.** (This revision; ACP-less Phase 1 needs defined semantics.)
17. **Convergence controller deferred to Phase 2; Phase 1 ships caps plus mechanical convergence counts.** (This revision, trimming review 2.)
18. **Inbox/outbox and the two death tests are Phase 1; the full injection campaign is Phase 2.** (This revision, splitting the reviews' difference.)
19. **Devlogs split by cadence: repo protocol for human sessions, artifact-store summaries for autonomous runs, shared promotion channel.** (This revision, deviating from review 1's blanket removal, with reasons in Section 8.)
20. **Plan gating by materiality, not per-edit; materiality rules are control-plane policy.** (Review 1, with this revision's trust note.)
21. **API provisional until exercised; persisted schemas versioned; "freeze the boundary" dropped as a Phase prerequisite.** (Review 2.)
22. **Market/moat language removed from technical decisions; metric is personal net leverage.** (Review 1.)


---

## Revision 3 (superseded before commit)


Held from v2: daemon owns workflow state, clients thin; GitHub owns source/review/merge; chat authors artifacts, engine executes artifacts; gates daemon-native; Go daemon and SwiftUI clients; monorepo; verification defines completion; personal scope; capabilities fixed at spawn; control-plane loads from approved default-branch commits; typed digest-bound artifacts and approvals; SQLite authoritative with inbox/outbox; polling-first; runner capability classes without silent downgrade; SwiftUI bootstrap exemption; subscription auth via native vendor tooling with leases and API-key fallback; cross-provider review as routing hypothesis; attention inbox as control system with minimal three-screen client in Phase 1; elaboration in scope, severable (user); discuss as stage re-invocation; devlog cadence split; materiality-gated plans; provisional API; market language excluded.

Changed or added in v3 (decider in parentheses):
1. **Phase 0 deleted; safety questions become 1A implementation gates persisting as tests; baseline logging passive; trials replaced by instrumented real work.** (Review 3; accepted with baseline retention and delegable artifacts folded into 1A week one.)
2. **Decision 17 reversed: the minimal convergence controller and review_diminishing_returns / review_dispute items are Phase 1; yield, not rounds, ends review; crash retries separated from remediation.** (Review 3; accepted as fixing a v2 regression against the validated workflow.)
3. **Finding classifier named as a real 1B component with sampled accuracy telemetry; ceilings retained as guards against label-driven misbehavior.** (This revision; the review hand-waved structured findings into existence.)
4. **ReviewSource abstraction beside StageDriver; CodexGitHubReview first; local reviewer is the Phase 2 hedge.** (Review 3.)
5. **Review triggering is exclusively control-plane: auto-review preferred and digest-audited; framework-issued re-requests otherwise; no submitter-side trigger ever; fail-closed to an attention item.** (User; injection rationale recorded.)
6. **Repository CI/automation trust profiles with digest-bound audits; default audited_same_repo for owned repos; fork/staging profiles for secret-bearing repos; fail-closed publish for unaudited repos; the Mac Studio is never a self-hosted runner for managed repos.** (Review 3; fork-default corrected, runner prohibition added, this revision.)
7. **Hostile import boundary and clean-room verification as explicit subsystems; malicious fixtures permanent; first repo has baked dependencies, provider-only agent egress, no-network verification.** (Review 3.)
8. **Credential modes declared per run: subscription_contained (default, documented residual exposure), api_key_isolated (Phase 2, supported), local_trusted; Claude is the only local driver in Phase 1.** (Review 3; setup-token scope claim demoted to verify-during-1A.)
9. **Control-plane trust extended to vendor auto-loaded instructions; trusted-base overlay in remediation and local-review workspaces; instruction-file diffs always risk-flagged.** (Review 3.)
10. **Effectively-once replaces exactly-once: deterministic identities, run markers, check-before-create, reconciliation after ambiguity.** (Review 3.)
11. **Per-resource state reconciliation replaces the fictional global cursor.** (Review 3.)
12. **Type-specific attention actions; optimistic decision concurrency with version binding; full artifact rendering for spec approval; notifications as read-only hints.** (Review 3.)
13. **Go rationale trimmed to the appliance case; stall-isolation comparison removed.** (Review 3.)
14. **False-ready split into mechanical / substantive / safety, with per-class tolerances.** (Review 3.)
15. **1A and 1B exit criteria separated: 1A proves safety and durability only; the attention thesis attaches to 1B.** (This revision; the 1A slice tests ready_for_final_review before spec_approval exists.)


---

## Revision 4 (committed)


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


---

## Revision 5 (superseded before commit)


Each revision's material changes are recorded here with deciders. Revision 5 folded in the fifth external review (synchronization semantics, invocation idempotency, checkpointed backups, out-of-process gauntlet, non-waivable gates, operating modes, Linux reclassification, sequencing) and the implementation-coordination model developed in discussion.

Held from revision 4 (abbreviated): daemon owns workflow state, clients thin; GitHub owns source/review/merge; chat authors artifacts; gates daemon-native; Go + SwiftUI; monorepo; verification defines completion; personal scope; capabilities at spawn; control-plane from approved commits; digest-bound artifacts and approvals; SQLite + inbox/outbox; polling-first; capability-classed runners, no silent downgrade; SwiftUI bootstrap exemption; native vendor tooling with leases and API-key fallback; cross-provider review as routing hypothesis; attention inbox as control system; elaboration in scope, severable (user); devlog cadence split; materiality-gated plans; provisional API; yield-driven review; finding classifier named; ReviewSource with CodexGitHubReview; control-plane-only review triggering (user); CI trust profiles, audited_same_repo default, no self-hosted runner; hostile import and clean verification; credential modes; effectively-once; per-resource reconciliation; type-specific version-bound attention actions; false-ready taxonomy; 1A/1B exit split; client sync and conversations; reviewer-instruction poisoning closed; trusted recipes; initiators; optimization telemetry; portability, autonomy, and simplicity principles (user); naming stack (user).

New in revision 5 (decider in parentheses):
1. **Cache semantics corrected**: atomic bootstrap snapshots; full-cache revision distinguished from per-entity as_of_revision; revision heartbeat; two new sync tests. (Review 5.)
2. **Conversation sync is whole-snapshot in Phase 1**, curing the append-only violation of mutable statuses under ?after= reads; event-sourced conversations deferred. (Review 5.)
3. **AttentionDelivery per device/channel/attempt; item timing fields become derived aggregates.** (Review 5.)
4. **AttentionItems bind to a generic subject** (run | proposal_batch | project | system); run_proposal gains proposal_batch_id with per-candidate decisions. (Review 5.)
5. **Deterministic invocation IDs into StageDriver and ReviewSource with inspect-based reconciliation; the guarantee is one committed intent and at most one accepted result, never advancing twice.** (Review 5; "exactly one invocation" was unenforceable.)
6. **Checkpointed backups** (SQLite snapshot digest + artifact manifest digest + completion marker) as authoritative restore units, Litestream as low-RPO fill; SQLite pragmas fixed (WAL, synchronous=FULL, foreign_keys, busy_timeout). (Review 5.)
7. **The gauntlet runs out of process, unprivileged**; 1A export is one normalized change manifest, regular files only, daemon-authored clean commits; agent commit history not preserved. (Review 5.)
8. **Shadow-review safety override**: credible critical/high shadow findings block ready status; contamination accepted; disregard added to the safety-failure list. (Review 5; credibility operationalized this revision.)
9. **Non-waivable gate classes; the self-service rule and interruption-budget rule scoped to eligible classes; rein is a preset resolving into digested per-run policy, never a security dial.** (Review 5.)
10. **attended_dev vs unattended operating modes**, resolving the first-run permissiveness / fail-closed contradiction; full conformance at startup, config change, and doctor schedule; lightweight probe per unattended job. (Review 5.)
11. **Linux reclassified as a portability target** until one named deployment matrix passes; `linux_vm` named in the capability model with implementation deferred. (Review 5.)
12. **Setup/onboard/doctor built after the first real 1A run, retained as 1A exit criteria; build order resequenced accordingly; 1A described honestly as large but ordered.** (Review 5.)
13. **Fault-class capture is suggested with one-tap correction and unknown; system_health item type added; mark_seen/dismiss replace acknowledge; device credentials stored as hash/public key with a local pairing ceremony; agent-completion sequence corrected to blobs-then-transaction.** (Review 5.)
14. **Factual tightenings recorded**: nested AGENTS.md reviewer guidance is documented behavior; auto-re-review of remediation heads remains a 1B integration test; the Claude setup token's inference-only scope is documented and contract-tested against the pinned CLI; App-manifest key exchange lands directly in protected storage. (Review 5.)
15. **Implementation coordination model**: wave 0 serial hub, capability lanes with the spine role, contracts-and-fakes as the coordination mechanism, review-bandwidth-bounded width, issue protocol in AGENTS.md, 1A backlog as elaborator fixtures. (This revision; from implementation discussion with user.)


---

## Revision 6 (superseded before commit)


Each revision's material changes are recorded here with deciders. Revision 6 resolved the adversarial self-review findings and added the evidence pipeline.

Held from revision 5 (abbreviated): daemon owns workflow state, clients thin; GitHub owns source/review/merge; chat authors artifacts; gates daemon-native; Go + SwiftUI; monorepo; verification defines completion; personal scope; capabilities at spawn; control-plane from approved commits; digest-bound artifacts and approvals; SQLite + inbox/outbox; polling-first; capability-classed runners; SwiftUI bootstrap exemption; native vendor tooling, leases, API-key fallback; attention inbox as control system with sync/conversation model; elaboration in scope, severable (user); yield-driven review; ReviewSource with CodexGitHubReview; control-plane-only review triggering (user); CI trust profiles; out-of-process gauntlet with normalized manifest; credential modes; effectively-once with invocation IDs; checkpointed backups; non-waivable gates; attended_dev/unattended; Linux as portability target; resequenced build order; coordination model (waves, lanes, spine); naming stack (user); portability, autonomy, simplicity principles (user).

New in revision 6 (decider in parentheses):
1. **Per-stage egress profiles restored** (provider_only, provider_web_read; credential mode as floor; clean rooms stronger than any profile), curing the elaborator/subscription_contained contradiction. (Adversarial self-review F1.)
2. **1B shadow-review arm is a Claude-driver review stage**, supplying the comparison arm and dry-running the local-reviewer hedge. (F2.)
3. **Export helper boundary specified**: never in the live agent VM; workspace mounted read-only in a fresh credential-free context. (F3.)
4. **Preset precedence defined**: explicit keys override rein presets with per-key provenance recorded in resolved policy; example config corrected to show a deliberate, recorded override. (F4.)
5. **1A entry mechanism defined**: `freesided submit` registers a hand-approved spec as a digest-addressed artifact and creates the run. (F5.)
6. **Build-order step 6 clarified**: real items run under manually configured unattended preconditions; step 7 packages those checks. (F6.)
7. **Budget scoping defined**: unprefixed budgets bind per stage attempt; run_-prefixed bind the run; max_diff_files is cumulative versus base. (F7.)
8. **Per-wave fresh-context adversarial review added to wave exits**, mitigating reviewer monoculture. (Meta-finding, this revision.)
9. **Evidence and image pipeline (5.15)**: verifier-owned before/after capture in clean rooms; evidence channel separated from the repo-change manifest; images as opaque blobs (no server-side decoding); EvidencePublisher under effectively-once discipline with the PR Screenshots obligation; OCR scanning recorded as a deferral. (User prompt; this revision.)
10. **Nits closed**: pairing code printable to terminal (no display assumed); digest-idempotent attachment upload endpoint named; macOS CI kept lean for minute billing; Section 10 targets verified against a clean VM or spare machine. (Adversarial self-review.)


---

## Revision 7


Revision 7 folded in the sixth external review as adjudicated.

Held from revision 6 (abbreviated): daemon owns workflow state, clients thin; GitHub owns source/review/merge; gates daemon-native; Go + SwiftUI; monorepo; verification defines completion; capabilities at spawn; control-plane from approved commits; digest-bound approvals; SQLite + inbox/outbox; polling-first; capability-classed runners; SwiftUI bootstrap exemption; native vendor tooling; attention inbox as control system with the full sync/conversation model; elaboration in scope, severable (user); yield-driven review; CodexGitHubReview; control-plane-only review triggering (user); CI trust profiles; out-of-process gauntlet, normalized manifest, export-helper boundary; credential modes; effectively-once with invocation IDs; checkpointed backups; non-waivable gates; attended_dev/unattended; Linux as portability target; per-stage egress profiles; evidence pipeline; per-wave adversarial reviews; coordination model; naming stack (user).

New in revision 7 (decider in parentheses):
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

---

## Revision 8

Revision 8 records the development record-keeping migration; the product spec is unchanged.

Held from revision 7 (abbreviated): every held and new revision-7 item, unchanged, except the devlog cadence split with its shared promotion channel, which this revision replaces.

New in revision 8 (decider in parentheses):
1. **Selective decision notes replace the devlog cadence split and shared promotion channel**: a note is mandatory only for the change classes AGENTS.md's High-assurance list names; the issue tracker and git carry all active work state; historical entries are frozen evidence. (User.)


---

## Revision 9

Revision 9 records the accelerated open-source publication decision; the
product architecture and phase deliverables are unchanged.

Held from revision 8 (abbreviated): every product decision held through
revision 7 plus revision 8's selective decision-note protocol, unchanged.

New in revision 9 (decider in parentheses):
1. **Open-source publication moves from Phase 4 to Phase 1A under AGPL-3.0-or-later, including owned prior revisions**: the network-service architecture still supports the original AGPL candidate; exhausted private-repository Actions capacity changes the timing, not the product roadmap or support commitments. Repository visibility changes only after the license and historical grant land. (User; ADR 0001.)

---

## Revision 10

Revision 10 codifies the brand register; product architecture and phase
deliverables are unchanged.

Held from revision 9 (abbreviated): every product decision held through
revision 8 plus revision 9's accelerated open-source publication, unchanged.

New in revision 10 (decider in parentheses):
1. **The brand register is codified as identity policy**: the tagline evolves to "the harness runs the agent; you hold the reins" (control as a held state, not a transfer); Freeside is fixed as a proper noun outside URL/daemon contexts; the two-ground visual register (light = Freeside, dark = Straylight), the signet-box mark, and the accent grammar (bronze/tawny as one metal in two ages; green reserved for the semantic palette) are adopted. Rationale and the complete rejected-alternatives record live in the brand decision note. (User; devlog 2026-07-17-0050-brand-register.md.)

---

## Revision 11

Revision 11 makes the networkless exporter boundary a named runner/policy
contract.

Held from revision 10 (abbreviated): every product decision held through
revision 9 plus revision 10's brand register, unchanged.

New in revision 11 (decider in parentheses):
1. **A networkless-export capability becomes binding**: add
   `supports_networkless_export` to §5.7 so unattended policy can require the
   exporter egress boundary without naming Apple container's mechanism. The
   ward implementation and live runtime proof remain #78's responsibility.
   (User; #78.)

---

## Revision 12

Revision 12 records the workspace-handoff outcomes: the declared strong class
and the network-free exporter precondition for unattended mode.

Held from revision 11 (abbreviated): every product decision held through
revision 10 plus revision 11's networkless-export capability, unchanged,
except revision 7's clause declaring the same-VM fallback a weaker class,
which decision 1 below refutes and supersedes.

New in revision 12 (decider in parentheses):
1. **The strong handoff class is declared**: §5.7 names
   `fresh_vm_read_only_volume_handoff` for Apple container 1.1.0, conditional
   on the conformance checks, and records the same-VM fallback as refuted by
   execution on this runtime (no host hot-detach; a guest unmount is not a
   credential-device detach), never to be implemented or declared. This
   supersedes revision 7's clause that named the fallback a declared weaker
   class. (User;
   docs/spikes/workspace-handoff.md, devlog
   2026-07-14-2113-wave1-planning.md; #79.)
2. **The network-free exporter becomes an explicit unattended precondition**:
   the `unattended` mode row names the proven `supports_networkless_export`
   boundary, closing the spike's open exporter-network boundary at the policy
   level. (User; #78, #79.)

---

## Revision 13

Revision 13 specifies comprehension: §9 grows from two lines into a normative
presentation specification.

Held from revision 12: every product decision, unchanged. §9's original
"present evidence packets first" is reinterpreted rather than dropped: the
short labeled summary now leads above the evidence packet, and evidence
precedes long-form agent text.

New in revision 13 (decider in parentheses):
1. **Comprehension is specified as a first-class attention concern**: a
   four-layer card ordering (ask and daemon facts, labeled summary, evidence
   packet, drill-down), three required digests (change summaries, plan
   altitude, digested review feedback), per-item-type leads for all ten
   Phase 1 types, summary provenance (deterministic card facts from the
   daemon under §12's mechanical false-ready; judgment summaries as labeled
   claims from the stage agent, never in `evidence_snapshot`, with promotion
   to an independent briefer on recurring audited summary-evidence
   contradictions), and comprehension metrics paired against correctness.
   Rejected alternatives (daemon-templated, verifier-produced, and
   independent-summarizer-now provenance) live in the decision note. (User;
   PR #192 review, devlog 2026-07-20-1137-comprehension-spec.md; #194.)

---

## Revision 14 (current)

Revision 14 records the serialized-history adoption for the gauntlet
repo-change channel.

Held from revision 13 (abbreviated): every decision held through revision 12
plus revision 13's comprehension specification, unchanged, except §5.6's
singular clean-commit framing, which decision 1 narrows to one clean commit
per non-empty normalized first-parent state transition, and revision 5
decision 7's "agent commit
history not preserved" clause, which decision 1 supersedes: history now
crosses as validated serialized data (the never-trust-workspace-`.git`
invariant is unchanged).

New in revision 14 (decider in parentheses):
1. **Agent commit structure survives the gauntlet as serialized data**: the
   §5.6 repo-change channel gains an optional serialized commit history,
   carrying commit boundaries and messages as ordered, validated data, never
   git objects. The daemon re-authors one clean commit per non-empty
   normalized first-parent state transition, gated per repository by trust
   profile; importer enforcement
   (control-plane classification, secret scanning) applies to every commit
   in a chain, since intermediate content publishes even when absent from
   the final tree; evidence and publication identities still bind to the
   single candidate head. (User; devlog
   2026-07-20-1145-gauntlet-commit-structure.md; #192, #193.)
