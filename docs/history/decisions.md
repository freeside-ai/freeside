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

## Revision 14

Revision 14 adopts the agent-proposed commit plan through the gauntlet:
commit structure crosses as grouping, ordering, and messages over the final
validated change set, and the daemon re-authors one clean commit per
resolved non-empty plan group.

Held from revision 13: every product decision, unchanged. §5.6's
clean-commit framing is narrowed, not dropped: the daemon still authors
every commit, one per resolved non-empty plan group instead of exactly one,
and the
single commit remains the default and the fallback. Revision 5 decision 7's
"agent commit history not preserved" clause is upheld and clarified: the
agent's real history, as git state, still never crosses; what the candidate
branch gains is agent-proposed structure over validated content.

New in revision 14 (decider in parentheses):
1. **Agent commit structure crosses the gauntlet as a proposed commit
   plan**: the agent writes a plan (ordered groups of changed paths plus
   messages) as ordinary data at a reserved workspace path; the daemon
   derives the authoritative base-to-final change set, verifies exact
   cover and structural validity of each constructed tree, and screens
   each resolved non-empty group's publishing message under the
   digest-bound §5.5 `commit_plan` mode and built-in `message_ruleset`,
   then re-authors one clean commit per resolved non-empty group; the ward's
   whole-output handoff verification covers the plan like every exported byte.
   Tree content is confined to the trusted base and validated snapshot by
   construction; screened publishing-message text is the separate new
   published surface.
   V1 ships `single_commit` (conservative default) and `plan_preferred`
   (on a non-empty import, an absent plan or an enumerated agent-caused
   structural or non-secret screening rejection falls back to one commit
   with a surfaced notice; under `plan_preferred`, zero-change imports use the
   empty-commit path after the tolerant scan and surface a present plan as not
   honored; a
   trusted-base collision at the reserved path or any descendant blocks both
   modes; under `plan_preferred`, a decoded secret anywhere in the plan's text
   stays publish-blocking per §3.1; other failure classes remain blocking);
   `plan_required`, ruleset extensions, and a run-scoped plan
   drop are deferred by decision until real usage demands them.
   The serialized-history design worked to near-completion on PR #213
   (closed as superseded) was rejected: reading hostile `.git` put a
   parser inside trusted compute whose hardening bought availability but
   no provenance, and the isolated-generator variant kept the widened
   publication surface (intermediate-only content publishing forever).
   Rejected alternatives and the full design live in the decision note.
   (User; PR #192 review, devlog
   2026-07-20-1145-gauntlet-commit-structure.md; #193.)

---

## Revision 15

Revision 15 harvests the intro review (PR #192, issue #208) into the
charter: the thesis grants agents autonomy, the objective is a positive
return, the success claims become necessary gates with an explicit
numerator, the auto-merge door stays deliberately open, oversight and
standing-grant promotion become stated principles, durability names its
cannot-safely-retry fallback, and routing inputs and manual-balancing
accounting are stated.

Held from revision 14: every decision not named below, unchanged. The §1
identity (local, durable workflow controller; agent control plane; one
owner) is unchanged; the thesis rewording changes emphasis, not scope or
authority. Non-goal 1's IDE/review-surface exclusion is narrowed in wording,
not substance: Freeside still never rebuilds review, and merging stays on
GitHub; what changes below is that the auto-merge absolute becomes an
explicitly open question.

New in revision 15 (decider in parentheses):

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

---

## Revision 16

Revision 16 establishes the multi-account GitHub App identity model: every
operator is a distinct agent principal, while registration topology is an
owner policy choice between one public personal-account App by default and
private per-owning-account Apps as the work-account opt-in.

Held from revision 15: every decision, unchanged. Personal-tool scope still
means one owner per Freeside deployment; this revision specifies how separate
operators remain separate principals and how one operator acts across several
repository-owning accounts and machines.

New in revision 16 (decider in parentheses):

1. **GitHub publication identity is a per-user principal with
   owner-selected registration topology.** The default is one public,
   personal-account-owned App per operator, installed per repository-owning
   account through GitHub's native approval and repository-selection flow;
   the opt-in work-account posture uses one private App per owning account
   when the organization must own and terminate the credential. Both
   postures bind trust to numeric App IDs, onboarded repositories, known
   registrations and installations, and trusted principals. Both postures
   additionally require an always-on installation-grant janitor
   that suspends any installation with unrecorded repository grants; public
   registrations also delete untrusted-owner installations. Unsolicited
   authority neither authorizes Freeside minting nor reaches AttentionItems.
   Each worker-bound installation-token request names exactly one canonical
   repository ID and the approved permissions, and Freeside verifies the
   returned repository, permissions, and expiry before exposing the token. A
   daemon-internal, read-only, immediately revoked janitor credential is the
   only full-installation enumeration path, is gated before mint to a trusted
   installation or metadata-matched pending envelope, verifies pending
   repository IDs only after enumeration, and is never worker-exposed. A
   bounded pending-install-or-expansion intent with no new authority serializes
   native installation and repository-selection changes with the janitor. The
   binding set, pending intent, and expiring mutation lease are principal-wide
   CAS state; competing daemons attach or wait, and multi-machine installation
   mutation fails closed without that shared authority. Both
   pending and trusted installations require selected-repository mode before
   exact ID comparison. Callback, polling, or explicit local resume can make an
   exact pending match reviewable; only the accepted local trust review promotes
   it. Its expected owner and exact repository delta are a temporary
   reconciliation exception but gain no authority. Grant drift enters terminal
   quarantine and requires deletion plus fresh
   installation; Freeside never auto-unsuspends the drifted installation.
   Keys are per-machine, individually revocable, and tracked by GitHub's
   displayed SHA-256 fingerprint; names are canonicalized
   from the manifest conversion response, same-principal multi-daemon
   concurrency is expected, and bot-user-ID-bearing `Co-authored-by` trailers
   add human-readable provenance without replacing App-ID credential checks.
   Native GitHub installation and organization approval are an explicit
   account-onboarding prerequisite; Phase 1A's one-step repository target
   begins after that prerequisite completes.
   Rejected alternatives, residuals, and revisit conditions live in the
   decision note.
   (User; devlog 2026-07-22-2124-multi-account-agent-identity.md; #244, #251.)

---

## Revision 17

Revision 17 replaces same-principal concurrent daemons and their unhosted
shared CAS ledger with an active/passive movable control plane. It preserves
per-machine GitHub App credentials while making cross-machine continuity a
durability and takeover contract.

Held from revision 16: every decision except the same-principal concurrent
writer requirement and its principal-wide installation-mutation ledger. The
identity, mint, onboarding, and janitor protections otherwise remain intact.

New in revision 17 (decider in parentheses):

1. **The control plane is movable, not concurrent.** A stable
   `control_plane_id` spans enrolled hosts, but exactly one host owns the
   global execution seat. `standalone` remains the zero-configuration,
   single-machine contract. `portable` adds a remote durability frontier and
   active-epoch compare-and-swap only after a complete activation ceremony.
   Every portable external effect requires the current epoch and a remotely
   durable intent; store unavailability therefore stops effects. Complete
   encrypted checkpoints, an encrypted append-only journal, encrypted
   content-addressed blobs, and one atomic remote head make acknowledged
   conversations, decisions, workflow state, and artifacts recoverable on
   another enrolled host. Graceful and crash takeover restore the whole
   frontier and record explicit adoption; graceful handoff quiesces workspace
   writers before capturing the normalized workspace, while crash recovery
   returns to the last successful daemon-side push and may lose all unexported
   in-flight changes. Store eligibility is capability- and conformance-based,
   with direct R2 as the first reference backend and consumer sync folders
   excluded. Host-specific data-key wraps plus an offline recovery wrap close
   the recovery path; excluding a host first revokes and verifies denial of its
   replica credential, then rotates the data key and wraps. Per-machine GitHub
   App keys remain independently revocable. The single active writer replaces
   principal-wide installation leases and binding-set versions; pending
   envelopes bind to `active_epoch` and `durable_intent_revision`.
   Rejected alternatives and revisit conditions live in the decision note.
   (User; devlog 2026-07-23-1932-movable-control-plane.md; #264.)

## Revision 18

Revision 18 scopes the §5.7 unattended backup-health gate for Phase 1A.2 so
the first real unattended runs do not wait on the encrypted checkpoint,
replacing a prose "supervised" posture with an enforceable admission
predicate.

Held from revision 17: every decision.

New in revision 18 (decider in parentheses):

1. **The 1A.2 backup-health exception is a mechanical waiver, not prose.**
   Unattended admission may waive only the encryption-state dimension of
   backup health, with checkpoint currency, artifact closure, and
   restore-test age still gating admission against the local owner-only
   checkpoint (§5.10), only while an explicit operator-set
   `backup_encryption_waiver` naming the exact trusted numeric repository ID
   it covers is configured, the run targets exactly that repository, and the
   build does not yet carry the encrypted,
   digest-bound `BackupCheckpoint`; a build that carries it rejects the
   waiver as invalid configuration, retiring the exception. Each waived
   admission is recorded in the run's audit record and surfaced as a
   `system_health` item the validated waiver configuration supersedes per
   §4, visible but not blocking. The encrypted checkpoint must land before
   the Phase 1A exit; the doctor packages its encryption check. Keeps a
   serialized contract unit off the first real runs' critical path.
   Rejected alternatives and revisit conditions live in the decision note.
   (User; devlog 2026-07-26-0957-1a2-chain-repair.md; #305.)

## Revision 19

Revision 19 makes the measured golden-image shape canonical, repairs the
project-image dependency order exposed by removing the checked-in stand-in, and
aligns the first-repository criteria with the selected target.

Held from revision 18: every decision.

New in revision 19 (decider in parentheses):

1. **Golden agent and project images share one ward-enforced realized shape.**
   Image metadata may not alter that shape; project images bake the exact
   repository dependency closure and trusted recipe configuration, prove that
   recipe verbatim without network, and reach ward only through
   registry-resolvable digest references. A build-time tag is a measured Apple
   `container` 1.1.0 compatibility hop whose exact base digest is verified and
   recorded, not execution authority.
2. **The reusable project-image builder precedes and survives onboarding
   packaging.** It is manually proven before #237's real runs, then #238 invokes
   the same primitive. A checked-in image for one repository would import
   another project's dependency churn; a second onboarding implementation
   would let the proof and packaged behavior diverge.
3. **The first repository is selected by behavior, not language.**
   `freeasinbird/gh-imgup` remains the selected target because its authority is
   representable, its trusted recipe can run offline, and it has ordinary work
   for the gauntlet. The prior Go and `go test`/`go vet` wording was a stale
   example, not an eligibility rule.
   Rejected alternatives and revisit conditions live in the decision note.
   (User; devlog 2026-07-26-2330-phase1a-image-order.md; #325, #337.)

---

## Revision 20

Revision 20 makes the operator stop of unattended operation a durable,
command-bound transition with an explicit resume action, and defines the §4
blocking/supersession semantic as durable typed state consulted in the
admitting transaction (#319, #321).

1. **Stopping unattended operation is a durable transition with an explicit
   resume.** Accepting `stop_unattended` on a `system_health` item appends a
   durable, command-bound operating transition and raises a notice offering
   the new `resume_unattended` action — the only writer of the resumed state,
   so a restart alone never resumes. The §4 blocking/supersession rule is
   durable typed state on the item, re-validated against live configuration
   at every unattended admission, and the whole operating-state gate is one
   shared predicate consulted in the admitting transaction and before any
   dispatch whose operating mode is unknowable.
   Rejected alternatives and revisit conditions live in the decision note.
   (User; devlog 2026-07-27-1846-durable-stop-and-supersession.md; #319,
   #321.)

---

## Revision 21

Revision 21 admits one operator-host instruction file as a snapshot-at-admission
control-plane source while preserving the rule that no agent-writable surface
feeds behavioral authority (#375).

Held from revision 20: every decision.

New in revision 21 (decider in parentheses):

1. **Vendor instructions mirror the operator host through an immutable
   per-run snapshot.** Admission dereferences the configured vendor instruction
   path once, records exact bytes or explicit absence, binds present content by
   digest separately from the stage prompt, and closes it under backup. Ward
   injects only that materialized file through a read-only vendor-native
   overlay; it never mounts the live host directory or inherits neighboring
   configuration. Repository auto-loaded instructions remain trusted-base
   content, and the launch contract reapplies that source at startup, recovery,
   resume, and child invocation. The host file intentionally has no repository
   approval gate or history: the operator already controls it, and each run's
   immutable digest makes drift observable and replay-stable. Rejected
   alternatives and revisit conditions live in the decision note.
   (User; devlog 2026-07-28-1908-host-vendor-instructions.md; #375.)

---

## Revision 22

Revision 22 resolves the Claude credential-delivery and writer-outcome
topology after the production driver's gate failure (#380), replacing the
refuted credential-store and instruction-mount hypotheses with a
launcher-mediated contract proved by execution on the pinned image.

Held from revision 21: every decision, with one realization amended: the
pinned CLI co-locates user instructions with writable session state, so
revision 21's read-only vendor-native overlay becomes a read-only explicit
instruction bundle under `--safe-mode`, separate from the narrow writable
resume and shell-initialization subtrees; the admitted digest remains the
trust anchor.

New in revision 22 (decider in parentheses):

1. **The Claude setup token is launcher-delivered, never spec-borne.** A
   per-identity read-only token volume is the only identity-persistent mount;
   no per-identity writable Claude state enters execution. The launcher argv
   reads the token into the CLI process environment at exec, the writer's
   spec environment is empty, and the value never enters argv text, inspect
   reports, ward journals, or driver state. Process-tree ambience is the
   documented `subscription_contained` residual.
   (User; devlog 2026-07-29-1750-claude-credential-topology.md; #380.)
2. **Writer outcome authority is a gate-authored nonce marker with a
   journalled crash bridge.** `ExecutionOutcome` is canonical for failed,
   canceled, and lost invocations; `ExecutionExport` is canonical for
   completion. Before cleanup can erase marker or workspace evidence, ward
   durably records cancellation intent or a validated nonzero status, then
   closes canceled or failed only after required capture and teardown. A
   live daemon sets `WriterComplete` only after stopped or absent, matching
   nonce, zero status, and proxy-health-throughout all hold. Recovery never
   synthesizes that bit. Cancellation intent outranks a durable failure
   status, which in turn outranks marker state; marker classification runs
   only when neither amendment exists. Missing, malformed, or mismatched
   evidence, and zero without an already-durable completion bit, fail closed
   as lost after absence proof and teardown. A
   canceled invocation remains terminal; continuation starts a new attempt
   from the restored workspace as untrusted input.
   (User; devlog 2026-07-29-1750-claude-credential-topology.md; #380.)
3. **Phase 1A isolates executable Claude configuration while retaining exact
   provider resume.** Every gate-mediated launch gets a verified read-only
   config root, fresh `session-env` scratch, `--safe-mode`, and a read-only
   explicit bundle composed from admitted host and trusted-base repository
   instructions. Only the invocation's `projects/` continuity volume crosses
   launches. Startup and forked resume use daemon-generated, pre-journalled
   exact session IDs; resume proves predecessor absence and retains the
   credential lease and fence. Recovery adopts or reaps the exact existing
   launch and never duplicates it. `InvocationChild` remains unavailable; a
   CLI process the agent itself spawns is untrusted agent activity.
   (User; devlog 2026-07-29-1750-claude-credential-topology.md; #380.)

---

## Revision 23

Revision 23 reschedules the local Codex execution driver from Phase 2 "if
useful" to committed 1B work, on the #395 feasibility spike's go verdict:
single-provider execution capacity is a demonstrated availability risk, not
a Phase-2 hypothesis.

Held from revision 22: every decision except the two this revision
overturns: revision 3's "Claude is the only local driver in Phase 1"
(item 8) and the Phase-2 optional placement of the Codex driver.

New in revision 23 (decider in parentheses):

1. **The Codex execution driver is scheduled 1B work, an execution capacity
   hedge.** Changed assumption: its Phase-2 "if useful" placement rested on
   Claude execution capacity being sufficient, and operator experience shows
   usage limits stall real work, so availability, not provider comparison,
   motivates the driver; the shadow-review experiment cannot answer a
   capacity problem. Overturns "Claude is the only local driver in Phase 1"
   (revision 3) and the Phase-2 placement. Grounded in the #395 spike's go
   verdict; adoption stays blocked on the #401 pre-adoption gates. The build
   chain (the `agent-codex` base and its derived project images, ward's
   second vendor topology, the driver binding, the selection contract)
   lands as follow-on 1B units after the 1A.2 exit. Single-provider execution capacity joins the Section 14 risk
   register. Unchanged: no automatic provider fallback (non-goal 5), and
   shadow findings stay recorded, never routed (Section 7).
   (User; devlog 2026-07-30-1942-codex-capacity-hedge.md; #396.)

---

## Revision 24

Revision 24 completes the real production publication path's visible Git
identity and failure-containment contracts after the first authorized live
proof exposed both as material gaps.

Held from revision 23: every decision except revision 16's
bot-user-ID-bearing `Co-authored-by` attribution choice, which this revision
replaces. The numeric GitHub App authority model from revision 16 remains
unchanged.

New in revision 24 (decider in parentheses):

1. **Daemon-authored commits use the selected App bot as primary author and
   committer.** GitHub therefore associates the commit with the same visible
   App principal and avatar that publishes it. Freeside resolves the canonical
   bot account and durably binds each run's attribution to the registration
   selected by the repository-scoped installation token before execution or
   import. The slug and bot user ID remain attribution metadata; numeric App
   ID, installation, repository, and token-mint checks remain authority.
   (User; devlog 2026-07-31-0907-complete-production-pipeline.md; #411.)
2. **Production-publication errors are contained per durable task.**
   Environmental and mutable-authority failures retain the immutable task and
   retry with bounded pacing; permanent external refusals create a durable
   repair hold; malformed durable reconstruction and other state
   contradictions remain fail-loud. A successful durable outcome survives a
   later lock-release failure. This replaces per-call-site fatal propagation,
   which let one external failure terminate the daemon and could only converge
   by enumerating an open-ended error surface.
   (User; devlog 2026-07-31-0907-complete-production-pipeline.md; #411.)

---

## Revision 25

Revision 25 lands the "World model, proposals, and judgment calls" revision
(#420) with #427's review-stage change folded in at the owner's 2026-07-31
reopen: daemon judgment calls with per-site authority contracts, the closed
proposed-effect registry, human-gated follow-up issue filing, the durable
scheduler, post-merge recompute and the frontier projection, the
verification state algebra, the 1B restructure into internal exits
1B.0/1B.1/1B.2 with UI surfaces, provisional contracts for deferred
subsystems, and review as a durable Freeside-invoked, Freeside-orchestrated
workflow stage.

Held from revision 24: everything except the three statements this revision
overturns — Section 4's GitHub-Projects-as-passive-all-work-view, Section
11's Phase 3 initiative-view placement, and Section 5.3's "one primary
review source, CodexGitHubReview" (with the 1B chain's
control-plane-triggered Codex review step reshaped).

New in revision 25 (decider in parentheses):

1. **Daemon judgment calls are contract-bound** (Section 5.13): terminal
   authority modes annotate / propose / explain / choose are exhaustive;
   every call site carries exactly one per-site authority contract; advisory
   output lives in a store structurally unreachable by policy evaluation;
   daemon-side inference is its own contract, never a reuse of
   `provider_only`. The control plane stays operable and fail-safe with
   inference down.
   (User; devlog 2026-07-31-1830-world-model-plan-revision.md; #420.)
2. **Agent-requested real-world effects exist only as proposals into a
   closed effect registry** (Section 5.13): fixed Go types, trusted
   constructors, gates; daemon-generated proposal-instance IDs under stable
   admission idempotency keys are the effect identity; semantic content
   never defines occurrence identity. Trusted engine-run effects stay under
   Section 5.9. (User; same devlog; #420.)
3. **Follow-up issue filing lands human-gated** (Section 5.17): the
   policy-approved path requires a complete enumerated issue-event authority
   profile; Freeside-origin issues never `auto_start`; in a Freeside-seeded
   repository without a current valid profile, all label intake demotes to
   propose. (User; same devlog; #420.)
4. **One durable scheduler owns every deferred check** (Section 5.16): a
   closed kind union, fire-time validation, transactional consumption with
   redelivery, no silent stale-event discard, and no authority from firing.
   Only the scheduler gates first real-backlog use in 1B.0.
   (User; same devlog; #420.)
5. **Post-merge recompute and the frontier projection** (Section 5.18): a
   merge completes a unit only through an exact daemon-recorded binding; the
   projection derives from explicit declarations, renders staleness and
   coverage honestly, and serializes unknown scope.
   (User; same devlog; #420.)
6. **The verification state algebra records honest degraded verdicts**
   (Section 6): waivers exist only inside Failed/NotRun under a closed
   granting-authority set; absent records block; ReadyClean and
   ReadyDegraded never flatten into one boolean downstream.
   (User; same devlog; #420.)
7. **The initiative view ships minimal in 1B.2, and GitHub Projects is no
   longer the all-work view.** Overturns the standing Section 4 statement
   and the former Section 11 Phase 3 initiative-view placement, on lived
   evidence from building Freeside with agents.
   (User; same devlog; #420.)
8. **Phase 1B restructures into internal exits 1B.0 / 1B.1 / 1B.2** with
   real-backlog use beginning during 1B.0 as soon as the minimal loop
   stands. (User; same devlog; #420.)
9. **Provisional contracts are recorded for deferred subsystems** — scoped
   consent grants, external findings ingestion, the pre-publication
   adversarial pass, the readiness registry (Section 5.19) — each re-reviewed
   at implementation. (User; same devlog; #420.)
10. **Review is a durable, Freeside-invoked and Freeside-orchestrated stage
    of the run workflow, with a local Codex invocation as the first
    production ReviewSource; GitHub-native Codex review is demoted to
    best-effort extra evidence that never satisfies the review requirement**
    (Sections 5.3 and 7). Overturns "one primary review source,
    CodexGitHubReview" and reshapes the 1B chain's control-plane-triggered
    Codex review step, per the 2026-07-31 live-run trigger falsification: no
    App-visible trigger path exists, and a human-PAT trigger was rejected as
    a production dependency.
    (User; same devlog; #420, #427.)
11. **The review-anchor fork is carried open, deliberately unresolved**
    (Section 7): pre-publication review versus the current PR-anchored
    chain. Recorded lean: pre-publication with forge checks still gating
    merge. (User; same devlog; #420, #427.)
12. **Plain-English scheduling defers past the 1B exit**, CLI-first,
    sequenced before any conversational surface.
    (User; same devlog; #420.)
13. **Smalls:** the stall heartbeat may only accelerate a stall notice and
    never extends a budget (Section 5.12); CI spend joins the maintenance
    accounting (Section 9); the doctor gains a stored-credential integrity
    probe in 1B.1 (Section 10). (User; same devlog; #420.)

---

## Revision 26

Revision 26 decomposes Phase 1B into build waves and records the operator
client-access decisions. Held from revision 25: everything except the one
statement this revision amends — "only the scheduler gates first
real-backlog use in 1B.0" widens to include the Section 7 review-stage
chain.

New in revision 26 (decider in parentheses):

1. **Phase 1B builds in six waves (3–8) mapped to its internal exits**
   (Section 11's coordination table): loop foundations, the review stage,
   loop depth, convergence and yield, operational closure, the initiative
   view. The table records shape and sequencing; each wave's unit list
   lives in its pinned tracking issue. Phase milestones stay whole:
   internal exits are not sub-milestones.
   (User; devlog 2026-08-01-1643-1b-wave-plan.md.)
2. **The Codex review substrate fronts the build** because #427 depends on
   it, verified against #401/#404/#406/#407 as written: #401 gates 1/2/4/5
   and the #404 base image land in wave 3; a review-scoped selection
   contract, the review ward-topology slice, and #427 land in wave 4, with
   the spine rescoping #406/#407 into review cores and execution remainders
   at wave-4 scheduling. The execution tail — #401 gate 3, the execution
   remainders, #405 if outstanding, #397 by explicit owner decision on
   shadow evidence, then #408 — closes in wave 7.
   (User; same devlog; #397, #401, #404, #405, #406, #407, #408, #427.)
3. **First real-backlog use gates on the review-stage chain as well as the
   scheduler** (Sections 11 and 13, amending revision 25's scheduler-only
   statement): revision 25 itself made review a required workflow stage,
   and #427's declared substrate dependencies put the review chain on the
   minimal loop's critical path. The state algebra and effect-registry
   retrofit stay off that path.
   (User; same devlog; #427.)
4. **The operator client installs Mac-first** (Section 10): direct install
   of a locally built, personal-team-signed FreesideMac with icon and real
   pairing in wave 3; iOS follows mid-1B under free provisioning; the paid
   Apple Developer Program defers to Phase 2 with APNs, because client
   correctness never depends on push (Section 5.14).
   (User; same devlog.)

---

## Revision 27

Revision 27 writes the daemon supervision contract (Section 5.2):
supervision modes by deployment class, exit discipline over a complete
fatal-writer inventory, stop semantics, and the liveness surface. Held from
revision 26: everything; the one sequencing amendment pulls the supervision
core forward from wave 7 to wave 5.

1. **Supervision is mode-scoped** (Section 5.2): Mac-first single-operator
   runs `freesided` as a per-user LaunchAgent registered by the Mac app
   through `SMAppService` (bundled plist, `KeepAlive`, no privileged step),
   superseding the LaunchDaemon-first direction endorsed in devlog
   2026-08-01-2221-supervision-contract-gap.md; the dedicated-user
   LaunchDaemon through the elevation helper is retained as the hardened
   end state. Changed assumptions: real-backlog daily operation begins at
   wave-4 close, two waves ahead of the fix parked in wave 7; Apple
   `container` is per-user tooling with unverified behavior under a
   GUI-less service account; the elevation helper drops off the Mac-first
   critical path entirely.
   (User; devlog 2026-08-05-0001-supervision-contract.md; #453, #454.)
2. **Exit discipline classifies every fatal-channel writer** (Section 5.2):
   durable in-process stops (store and correctness failures, backup
   maintenance, definitively classified doctor or janitor pass failures,
   externally caused failures once persistence is established) never exit;
   a post-bind HTTP serve fault is a restart-safe exit; panics and startup
   failures are involuntary exits. Restart-always is endorsed only over
   this inventory, and the doctor source-error posture deferred by devlog
   2026-07-30-2350-operational-command-packaging.md resolves as a durable
   stop.
   (User; same devlog; #453, #454, #435.)
3. **The stop-wait fork closes on the unlimited side** (Section 5.2):
   SIGTERM with an effectively unlimited exit timeout over unbounded
   credential-lease teardown; a bounded credential-safe teardown is
   deferred hardening, not a tunable; SIGKILL stays crash-equivalent.
   (User; same devlog.)
4. **Liveness is an unauthenticated `GET /health` plus supervised address
   and readiness publication** (Sections 5.2 and 10; api/openapi.yaml): a
   fixed loopback address in the unit file, a `0600` state-directory
   readiness file replacing the one-shot stdout line under supervision,
   the external ntfy probe in 1B.1, and the Mac app's menu bar presence as
   the local surface, with the app owning the local daemon's lifecycle.
   (User; same devlog; #453, #454.)
5. **The supervision core pulls forward to wave 5 by owner fiat**
   (Section 11, amending revision 26's wave-7 concentration for this unit
   only): #454's daemon side and the app-side LaunchAgent and menu-bar
   unit land in wave 5; the external-probe remainder stays in wave 7.
   (User; same devlog; #453, #454.)

---

## Revision 28

Revision 28 resolves the Section 7 review-anchor fork, deliberately carried
unresolved since revision 25, on the day of the first production backlog run
(2026-08-05, recorded on #482). Held from revision 27: everything; the
resolution pins one publication condition and names one deferred capability.

1. **The review anchor is pre-publication** (Sections 1, 7, and 11):
   implement → verify → review → clean: publish; the PR opens already
   reviewed, and forge checks still gate merge. This resolves the fork
   revision 25 deliberately carried unresolved. The internal loop is the
   agent's pre-push work; the PR is the collaboration surface: the PR list
   stays a decision queue, not a work queue; post-publication state is the
   expensive place to be correct (the #496/#514 ready-identity class); PR
   comments are mutable, so the authoritative ReviewRecord lives in the
   store under either anchor, and PR-anchoring would mean building both
   surfaces; and owner drill-down usage is served by computed readiness,
   the run timeline, and structured dispositions. The PR-anchored shape
   stays recorded as the fallback; revisit when real usage shows the owner
   cannot trust review they did not watch. The stage #427 landed
   PR-anchored under the then-open fork; the implementation re-anchor is
   tracked in #527.
   (User; devlog 2026-08-05-1746-review-anchor-pre-publication.md; #482,
   #427, #527.)
2. **Publication carries the disposition history as the EvidencePublisher's
   first slice** (Sections 7, 5.15, and 11): review rounds, final
   dispositions including declined and deferred with reasons, and the
   readiness derivation, so the merged PR is forensically self-explanatory
   on the forge; the owner's condition on the anchor resolution, pinning
   the slice's priority, not gating publication before #525 lands (until
   then the store carries the durable review state; per-finding
   disposition persistence precedes both #525's rendering and any
   reliance on the store as the authoritative disposition record).
   (User; same devlog; #525.)
3. **External review response is a named deferred capability** (Sections 7
   and 11): review activity arriving on a published PR from outside the
   control plane is identity-gated by an external-reviewer allowlist in
   the trust profile, normalized into the finding pipeline with source
   provenance, and drives the standard remediation → reverify → re-review
   cycle under the same convergence policy as internal rounds; it never
   satisfies the Section 7 requirement. Related to #502 as
   re-entry-after-terminal-state triggers.
   (User; same devlog; #524.)

---

## Revision 29

Revision 29 makes the admission effect of every `system_health` item explicit.
Held from revision 28: everything.

1. **System-health admission posture is explicit and immutable** (Sections 4
   and 5.7): every `system_health` item is either `blocking` or `advisory`.
   Advisory observations remain open and operator-visible without blocking
   unrelated unattended admission; blocking items preserve the prior gate and
   are the only items eligible for a validated blocking supersession. Existing
   rows migrate to `blocking`, preserving their historical meaning. Revisit
   when a third posture has a concrete admission behavior that neither posture
   nor a validated supersession represents.
   (User; devlog 2026-08-09-1739-system-health-posture.md; #625.)

---

## Revision 30

Revision 30 binds revoked Codex identity recovery to exact verified evidence.
Held from revision 29: everything.

1. **Revoked Codex identity recovery is command-backed by exact verified
   evidence** (Section 4): the marker remains a `system_health` item and gains
   `resolve_reenrollment` only after its exact latest re-enrollment operation
   is verified and immutably bound to that marker occurrence. The resolving
   command revalidates both records atomically; `acknowledge` remains seen-only,
   and no human assertion can clear the identity gate. Revisit when a provider
   requires additional durable recovery evidence beyond the current digest and
   access-token expiry.
   (User; devlog 2026-08-11-1025-codex-reenrollment-recovery.md; #684.)

---

## Revision 31

Revision 31 inserts finding adjudication between review and remediation.
Held from revision 30: everything.

1. **Every finding batch is adjudicated before remediation authority is
   exercised** (Sections 1, 4, 5.6, 5.12, 5.13, 5.19, 7, 9, 11): an
   immutable, digest-bound FindingAdjudication artifact records a
   per-finding decision — goal relationship, work-unit compatibility,
   and recommended route, with proposal vocabulary reserved for
   model-residue entries — under validity constraints that leave
   `required` no route to a deferred disposition. The engine derives compatibility
   deterministically where mechanically decidable — in-surface remediation
   is presumptively `allowed`, and `allowed` is engine-derived only — so an
   unambiguous in-scope finding routes to the remediator with no model
   call, while the deferral direction always takes adjudication; the model
   residue is a second Section 5.13 ceiling-bounded annotation site,
   separate from the classifier. A required finding incompatible with
   the current work unit parks or replans the run, never defers into a
   ready result. Revisit when wave 6 convergence measurement shows
   credible, material, in-surface findings routinely reaching the model
   residue: the deterministic dispatch predicate is then miscalibrated.
   (User; devlog 2026-08-11-1504-review-finding-adjudication.md; #697.)

---

## Revision 32

Revision 32 makes the wave tracker authoritative for live implementation
status. Held from revision 31: everything.

1. **One open pinned wave tracker is authoritative for live implementation
   status** (Section 11): stable repository documents carry the resolution
   rule, not a duplicate status value. The matching tracker's title supplies
   its wave and internal exit, the Section 11 table supplies its phase and
   shape, and the tracker's Implementation order digest supplies its active
   front. A zero- or multiple-match state fails to human repair rather than
   inference. This keeps status forge-visible and maintained at the same wave
   boundary that already replaces the tracker, without adding a local ledger
   or deriving intent from git history.
   (User; devlog 2026-08-15-1016-status-authority.md; #792.)

---

## Revision 33

Revision 33 records production acceptance identity as an explicit campaign
contract. Held from revision 32: everything.

1. **Production acceptance identity is an explicit campaign contract**
   (Section 5.12): an idempotent initial submit reserves campaign attempt 1;
   specification approval binds its accepted digest; and a terminal retry
   allocates exactly the next attempt while preserving the original source,
   raw publication-byte digest, elaboration root, and approved specification.
   Resume targets one live run and never mints an attempt. This keeps
   operational retry intent separate from specification bytes and makes every
   implementation attempt auditable to its exact parent.
   (User; devlog 2026-08-16-2238-production-attempt-identity.md; #794.)

## Revision 34

Revision 34 gives wave-tracker authority a three-state model. Held from
revision 33: everything.

1. **Wave-tracker authority resolves through a three-state resolver**
   (Section 11): over every pinned issue whose title matches the canonical
   wave-tracker pattern, evaluated before filtering by issue state, exactly one
   open match is active-wave, exactly one closed match is inter-wave, and zero
   or multiple matches are an invalid authority state for spine repair. This
   supersedes revision 32's single-open-match rule, which had no model for the
   legitimate gap between a wave's close and the next wave's planning and so
   treated that gap as broken authority: it let an explicitly authorized
   `Handle #N` session stop and made merge cleanup report reconciliation
   incomplete when there was simply no wave tracker to mutate. Fiat stays
   independent of wave state; only scheduled self-selection needs an open
   current tracker. The wave-boundary procedure keeps exactly one
   wave-title-matching tracker pinned (unrelated standing trackers coexist and
   never count), with the interruption-safe pin choreography deferred to #828.
   (User; devlog 2026-08-17-2108-inter-wave-state.md; #826.)

## Revision 35

Revision 35 ("Managed infrastructure never exceeds convenience"):

1. **Core authority and replaceable infrastructure are an explicit boundary**
   (Sections 2, 5.1): remote reachability, notification delivery, replica
   storage, and external health monitoring are replaceable infrastructure with
   operator-selected reference implementations and possible future
   Freeside-operated managed implementations; managed infrastructure may
   improve reachability, availability, storage, and delivery, but never
   becomes necessary for workflow authority or local operation, and its loss
   never invalidates local state. The one scoped exception is explicit:
   portable-mode replica storage is the oracle for activation fencing and
   the recovery frontier and sits inside the authority trust boundary,
   whoever operates it (Sections 5.1, 5.10). The Section 5.10
   capability-based replica-store contract is the template. The fully
   unmanaged deployment
   (Tailscale, ntfy, local state, operator probe; Section 10) stays
   first-class permanently, and authoritative components get no cloud seam.
   (User; devlog 2026-08-19-2138-managed-infrastructure-seams.md; #858.)
2. **Reachability is not identity** (Sections 5.2, 5.14): Signet is one
   authenticated protocol over loopback, Tailscale (the Phase 1 reference
   mechanism, not an architectural assumption), or a future managed relay;
   every mode presents the same daemon-owned Freeside device credential, and
   a managed service may transport pairing but never enroll a device. The
   deferred relay contract (Section 5.19) bounds any future relay to byte
   transport: no workflow authority, no credential possession or visibility
   (the Signet channel stays end-to-end protected through the relay and
   authenticates the daemon by a control-plane-stable Freeside identity
   independent of relay-controlled PKI), no
   authoritative state, and no Signet bypass; relay loss is reachability
   loss, never state loss. Enrolled
   host identity becomes cryptographically backed, recorded now as a forward
   requirement on the #265 domain contract before any host identity is
   persisted (Section 5.9).
   (User; devlog 2026-08-19-2138-managed-infrastructure-seams.md; #858.)

## Revision 36

Revision 36 ("Cross-round finding identity"):

Revision 36 lands the cross-round semantic finding identity the Section 7
fixed-disposition safety proof was conditioned on. Held from revision 35:
everything.

1. **The fixed-disposition absence proof keys on a deterministic finding
   fingerprint** (Section 7): the identity is `domain.Finding.Fingerprint()`
   over the review source, location path, and whitespace-normalized
   explanation, excluding the invocation, candidate head, run, severity, and
   line range that legitimately change across a work unit's same-base,
   different-head remediation rounds. It is a pure recompute-on-demand
   derivation, never stored, so both rounds compare under one version with no
   migration or schema change. It fails closed when a finding carries no such
   identity, so a finding whose fingerprint cannot be computed is never
   declared fixed; `codex_local` structurally never emits one, because
   `exec.ReviewResult.Validate` rejects an empty-message finding at the source
   boundary. Recorded fail-safe limitations: a reworded re-emission
   under-matches (enters as a new finding), and two distinct same-path,
   same-explanation findings conflate; both directions over-report
   not-fixed and never declare a persisting defect fixed.
   (User; devlog 2026-08-20-2311-cross-round-finding-fingerprint.md; #702.)

## Revision 37

Revision 37 ("Provider accounts are first-class operator objects"):

1. **Provider profiles sit over an unchanged `AuthIdentity`** (Section
   5.4): `ProviderProfile {id, name, provider, auth_identity_id,
   credential_mode, approved_model_configuration, role_eligibility,
   cost_owner, enabled, version}` is the operator-facing, versioned object, bound
   into records by immutable `id` plus version, never by name; credential
   authority stays in `AuthIdentity`, and resolved facts and digests stay
   in the composition manifest and run records. Role eligibility is on the
   profile; role binding is per run or per-project selection, so the
   Section 7 independence check compares provider and identity across two
   selections. Rejected: expanding `AuthIdentity` with configuration
   fields (credential identity and runtime configuration change for
   different reasons); binding a role on the profile (the independence
   check would read a label instead of comparing selections); the name
   `ProviderAccount` (the object does not own the credential).
   (User; devlog 2026-08-21-0405-provider-profiles.md; #863.)
2. **Multi-subscription per provider is supported and selected
   explicitly** (Section 5.4): two identities of one provider are a
   supported shape; selection among them is explicit or per-project
   policy, never silent; cost owner is re-evaluated on every selection;
   the operator owns provider-terms compliance and Freeside attributes
   without endorsing. Rejected: an inferred default profile.
   (User; devlog 2026-08-21-0405-provider-profiles.md; #863.)
3. **Probe output is observation, never authority** (Sections 5.4, 10):
   a credential-bounded account probe records fingerprint, masked label
   (display only), auth and plan type, expiry and revocation, CLI version,
   model snapshot, and last probe and execution; it feeds `system_health`
   items (always `advisory`), proposals, and the operator-facing profile
   projection's display fields only, and preflight, scheduling,
   `max_parallel_executions`, and drivers never read it; the
   profile is one-to-one with its identity and mirrors only its
   provider and enrolled credential mode, both checked at reconstruction
   and selection. The Claude
   pinned-CLI floor is a token digest plus an auth check. Rejected:
   running against operator provider homes; T3 Code's shared Codex
   shadow-home overlay; arbitrary provider environment variables (each
   makes credential state or configuration ambient and unrecorded).
   (User; devlog 2026-08-21-0405-provider-profiles.md; #863.)
4. **Switching is an explicit recorded attempt, never fallback**
   (Sections 4, 5.8): a quota, expiry, or capacity card offers retry under
   a qualified profile, wait, or stop; each switch is a new attempt that
   preserves the original failure and re-evaluates cost owner and review
   independence; preferences become project-policy proposal PRs, never
   remembered defaults; cross-profile continuation defaults to a fresh
   invocation, with a continuation compatibility digest designed as its
   own unit (#873) before #408 merges. Rejected: automatic fallback (Sections 2, 14) and
   learned defaults.
   (User; devlog 2026-08-21-0405-provider-profiles.md; #863.)
5. **Guided enrollment and the gated account probe** (Sections 10, 11):
   `freesided auth add|list|re-enroll|disable|enable` packages the Codex
   import-rotate-snapshot sequence and a guided Claude setup-token capture
   that keeps the token out of argv, history, logs, and client responses;
   re-enrollment is same-account only (checked in the transaction where
   the provider exposes account identity, operator-attested otherwise)
   and always versions the profile, so a different account is a new
   identity and profile;
   the doctor account probe, and `auth doctor` with it, waits on a spike proving the Codex app-server
   probe never refreshes outside the lease. Wave 7 gains the spike as an
   independent unit; `ProviderProfile` is the core type of #406, so
   enrollment, the probe, and the retry card `starts-after` #406, the
   probe also `starts-after` the spike, and the retry card, one unit for
   the implementation, review, and elaboration roles, also `starts-after`
   #408.
   (User; devlog 2026-08-21-0405-provider-profiles.md; #863.)

## Revision 38

Revision 38 ("The egress floor does not move"):

1. **The `provider_only` floor is a credential-containment boundary and does
   not move** (Sections 5.4, 5.7, 14). Capability above it is added as
   narrower, separately priced risk classes and as machine gates, never by
   widening the writer toward general web under `subscription_contained`.
   `provider_registry` (Section 5.4) admits a policy-declared set of
   read-only package-registry authorities through the same CONNECT proxy,
   with no DNS, no attacker-operated host, and exfiltration bounded to what
   the registries' own endpoints accept (the tunnel cannot constrain method
   or path, so a co-hosted write endpoint is a recorded residual); it is
   opt-in per project and its proven allowlist joins
   `supports_enforced_provider_egress`. The hosted agents' default shape
   (egress off or proxy-allowlisted) motivated keeping the floor; Freeside
   stays stricter only where the strictness was buying a human round trip
   rather than containment. Rejected: widening the writer to general web;
   making `provider_web_read` the default; folding the registry class into
   the `provider_web_read` exposure record.
   (User; devlog 2026-08-21-1510-registry-egress-profile.md; #871.)
2. **Dependency changes inside policy rebuild the project image without an
   AttentionItem** (Golden Agent and Project Images, Section 11): when the
   manifest delta is lockfile-consistent, every changed package resolves
   from the project policy's declared registry set (read by the builder
   whatever the writer's profile), and the recipe is unchanged,
   the reusable builder rebuilds from the trusted recipe, reruns the
   networkless positive run and the negative probe, and the run resumes
   against the new digest. Everything else keeps the fail-loud path, and the
   human still reviews the change in the PR diff and provenance record. The
   two follow-on units leave the Phase 2 list and drain in Wave 7, with the
   profile unit `kind:contract`. Rejected: leaving every dependency change a
   human gate; a setup-script phase with open internet as the hosted agents
   run, because the bake step already provides it without a
   credential-holding network phase.
   (User; devlog 2026-08-21-1510-registry-egress-profile.md; #871.)

## Revision 39

Revision 39 ("Admitted agents"):

1. **The agent is an admitted input** (Section 5.4): one operator-authored,
   content-addressed document of four role-free lines (enrollment, route,
   adapter, offer with effort), selected by a lineup line per role, admitted
   in the same five steps every other input is, and recorded with requested,
   admitted, and observed facts plus a behaviour-only treatment digest.
   The stage owns the launch, so any stage runs on any adapter whose proved
   capabilities cover it. New agent × launch pairs start attended; an
   operator's mark in the tree is the approval. Rejected: per-axis policy
   keys for harness, model, and effort (an unreviewed join); a qualification
   ledger with projections and supersession (two proofs suffice); an alias
   and withdrawal registry (the tree is the active set); a catalogue on the
   profile (route-specific availability changes on a different cadence).
   (User; devlog 2026-08-23-0825-admitted-agents.md.)
2. **One identity, many client enrollments; the profile dissolves**
   (Section 5.4). `ClientEnrollment` is identity × harness client × route ×
   auth method with `credential_mode`, each with its own sanitized store and
   append-only generations; the exact store locator leaves `AuthIdentity`;
   the lease stays on the identity and fences the exact store by enrollment,
   generation, locator, and manifest digest. `ProviderProfile`'s approval
   role moves to agents and lineups; `enabled` and `cost_owner` become
   identity fields. Changed assumption since revision 36: one harness client
   per provider account. Rejected: a second identity per client (two leases
   and two budgets on one subscription); one untyped store for all clients
   (the lease would no longer name one exact store).
   (User; devlog 2026-08-23-0825-admitted-agents.md.)
3. **Never silent, not never automatic** (Sections 2, 4, 14). A project
   lineup may name the alternate agent per failure class; the switch is a
   new recorded attempt, carded, with failure-specific eligibility (quota
   needs a different usage pool; two clients on one subscription are not a
   hedge). The human gate stays the default. Changed assumption since
   revision 36: fallback meant an unrecorded swap.
   (User; devlog 2026-08-23-0825-admitted-agents.md.)
4. **Review independence reads lineage, by default** (Section 7): the
   offers' lineage groups differ, at vendor-family granularity, unknown
   failing closed; a project lineup may relax it with a stated reason and
   the record carries which rule applied. Supersedes the provider-plus-
   identity comparison. Switching the review agent opens a new convergence
   segment.
   (User; devlog 2026-08-23-0825-admitted-agents.md.)
5. **pi is the third adapter, via elaboration first** (Sections 5.3, 11),
   on the ChatGPT subscription the owner records OpenAI as permitting
   through third-party tools, with no stability commitment on that OAuth
   interface (the route carries a dated terms basis). Source research on
   pi 0.84.2 replaced a prior design spike: it hard-fails on a read-only
   store at refresh, so admission step 4 and the Codex refresh pattern
   contain it; its severance is flag-complete; its provider ids separate
   subscription from API key. Its pre-adoption gates run against the pinned
   build when the adapter exists. Rejected: sequencing pi behind #408 (it
   is a second consumer of the contract, not a successor).
   (User; devlog 2026-08-23-0825-admitted-agents.md.)

## Revision 40

Revision 40 ("Recommendation-led attention"):

1. **A recommendation is contract, never inference** (Sections 4, 5.13, 9):
   an item may carry at most one
   `recommendation {action, reason, source, provenance, confidence?}`
   selecting one of its own requested decisions. The required immutable
   provenance is source-specific: content-addressed rule digest and input
   digest for `daemon_policy`; judgment site, invocation, and artifact digest
   for `agent_judgment` (the finding-adjudicator type case keeps per-finding
   route provenance in the Section 7 artifact); or policy key,
   resolved-policy digest, and daemon-authored application digest for
   `project_policy`. Each authoritative source record commits to the current
   item's decision-surface identity under Section 4, required to be
   eligibility-independent, telemetry-stable, surface-distinguishing, and
   non-cyclic; #942 specifies and tests the exact mechanism after two inline
   attempts (item-version binding, then an eligibility-coupled own-artifact
   subtraction) were rejected. Creation and
   reconstruction derive eligible source records from current
   authoritative state: exactly one produces the canonical recommendation;
   zero or multiple produces absence, with no precedence or tie-break. The
   stored optional recommendation must equal that exact result. For the unique
   record, the daemon requires its source-to-item association and rederives
   canonical action, reason, and confidence, rejecting any field, source, or
   item mismatch without invalidating the item's action set.
   No recommendation, no
   block: a client never infers one, and offer order carries no
   endorsement. Origin: an external UX review of the clients (2026-08-25)
   and its design response. Rejected: client-side inference from action
   order; caller-selected source; implicit source precedence; per-type ad hoc
   recommendation shapes.
   (User; devlog 2026-08-25-1154-recommendation-led-attention.md.)
2. **Cards render capability truthfully** (Section 9): a client shows only
   the requested decisions it can faithfully collect and execute, records
   filtered actions in drill-down, states not-decidable-here when no
   faithful response is in its capability, and never renders an
   unimplemented action as a disabled control or roadmap copy. Rejected:
   disabled placeholder buttons advertising unbuilt scope.
   (User; devlog 2026-08-25-1154-recommendation-led-attention.md.)
3. **`convert_to_policy` leaves the 1B phone-decidability claim**
   (Section 11): the diminishing-returns card stays decidable via finish
   now, apply-and-finish, and continue-under-policy; turning a recurring
   preference into a project-policy proposal waits for its deferred
   control-plane proposal surface and is hidden, not disabled, until then.
   Rejected: building that surface inside 1B; keeping dead controls under
   the exit claim.
   (User; devlog 2026-08-25-1154-recommendation-led-attention.md.)
4. **Wave 7 carries the attention-presentation closure** (Section 11),
   contract-first: the Section 4 recommendation shape and Section 9 typed
   minimum card facts as one serialized contract unit before producers and
   client adoption; that contract unit must retire `adjudicate` or reassign
   it to an executable `review_dispute` transaction before client adoption;
   transaction closure for the remaining Phase 1 pending actions, including
   `choose_alternate_profile` under #936 rather than #869's alternate-agent
   retry;
   evidence-metadata exposure; pairing identity facts; and the
   comprehension-telemetry contracts the wave-8 exit evaluation reads.
   Rejected: deferring missing facts and dead actions to Phase 3, which is
   advanced interaction, not missing fundamentals.
   (User; devlog 2026-08-25-1154-recommendation-led-attention.md.)
