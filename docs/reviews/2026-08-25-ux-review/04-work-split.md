# Work split: what ships now, what needs the contract first

The organising principle: most of the review is recomposition inside the app — it changes order, weight, and wording of data the client already holds, so it can start now with no contract change. The rest is **not** a UI problem wearing a UI mask. A recommendation the client *infers*, a fact the client *parses out of prose*, or a button whose transaction does not exist would each be a worse product than the current honest mess.

Placement follows the plan's own promises: Phase 1B exits only when "approvals are decidable from the phone" (`docs/plan.md:3203`), and §9 already defines minimum decision-relevant content for every Phase 1 attention-card type (`docs/plan.md:2697`). Phase 3 ("Comprehension and Interaction") is for advanced interaction — ACP conversations, resumption, briefings, routing — not for missing facts or non-functional Phase 1 actions.

---

## Work item A — Client recomposition
**No contract change. Can start immediately, in any wave.**

| Proposal | Scope |
|---|---|
| `1b` | Colour variants + Increased Contrast, `inkFaint` retirement, contrast test |
| `1a` | Accessibility composition + Dynamic Type reflow + screenshot regression matrix |
| `1c` | Card shell: order, one-sentence ask per type, measure, help placement, pinned iPhone footer |
| `1d` | Action ranking, overflow menu, capability filtering, confirmation copy, destructive roles, icon policy |
| `1e` | Badge rules, relative time, filter counts, non-colour selection, copy menu, tooltips |
| `1f` | Tabs + independent stacks; filters move into the Inbox root |
| `1g` | Inspector, two-column card above ~1000pt, empty-pane operational summary |
| `1i` | Loading / unavailable / oversized / non-image presentation, native preview, digest copy |
| `1j` | Durable confirmation, deliberate advance, uncertain-outcome retention |
| `1k` | Refresh (pull, toolbar, ⌘R, foreground), last-updated, keyboard commands, menus, menu-bar route |
| `1l` | Two-line run rows, wrapping watch chips, pairing layout and copy |

This is the majority of the review by finding count, and all of P0-1 and P0-2.

**Sequence inside A** (from the document's closing section):

1. **Colour variants + IC** (`1b`) — independent of every layout change and unblocks the accessibility audit. Waits on nothing.
2. **The shell** (`1c`, `1d`, `1a` together) — the recommendation-led card, ranked actions, capability rules, and the accessibility composition are one piece of work. The shell ships now; the RECOMMENDED block lights up when Wave 7 sends recommendation authority. **Never inferred client-side.**
3. **Getting around** (`1e`, `1f`, `1g`, `1j`, `1k`) — all client-side except the row's third line, which waits on project and work-unit names.
4. **Comprehension** (`1i`, `1h`, `1l`) — gated on Wave 7 typed facts, evidence metadata, and pairing identity; search sits out until Phase 2.

Deviation from the review's roadmap: the card shell precedes the Dynamic Type reflow. Reflowing the card we are about to replace is work done twice, and the accessibility composition is a property of the new shell.

---

## Work item B — Wave 7: Phase 1 attention presentation & action contract
**Contract-first, before the Phase 1B exit. Should be a named, leading, serialised Wave 7 unit that precedes its daemon producers and client adoption.**

Proposed unit statement:

> Complete the Phase 1 attention presentation and action contract so every decision required for the 1B useful loop has sufficient structured facts, an unambiguous recommended path when one exists, and an executable or intentionally unavailable action.

Wave 7 is the right home because it is Phase 1B.1 "Operational Closure" (`docs/plan.md:3171`) and already carries follow-up effects, credential probes, stall/liveness handling, deferral draining, and the execution tail.

### B1 · Recommendation authority
Plan sites: §4 attention/action semantics (`:225`), §5.13 authority and judgment-site rules (`:1732`), §9 provenance (`:2752`).

The generic action contract does not currently distinguish permitted actions, requested decisions, the system's recommended action, and the reason for it. Conceptual shape:

```
available_actions[]
recommended_action?
recommendation_reason?
recommendation_source        # deterministic daemon policy | validated agent judgment | human/project policy
recommendation_confidence?
```

Rules this creates:
- The client must never infer that the first `requested_decision` is recommended because it appears first.
- A recommendation must state its source, because the card renders judgment differently from fact.
- Finding adjudication already carries structured recommendation data and may only need to adopt the common shape.

This is a genuine contract change, not an implementation gap: it changes what authority an attention item may express. Do it once, centrally — not per card type.

Consumers: `1c`, `1h`, and the ↩ keyboard binding in `1k`.

### B2 · Typed minimum facts per card type
Plan sites: §5.13 deterministic card facts (`:1732`), §9 card-content table (`:2737`).

§9's minimums become typed daemon projections computed from canonical state — not client parsing rules. Required backfill:

| Card type | Required projection |
|---|---|
| Spec approval | decision being requested; plan summary at the right altitude |
| Diminishing returns | round count; finding trend; cost; unresolved work |
| Review dispute | both positions; disputed claim; evidence references |
| Execution failure | failure class; failing step; diagnostic summary; retry eligibility |
| Agent question | self-contained question; relevant context; answer requirements |
| Publish blocked | failed rule; acceptable remediation paths |
| Ready for review | ask; change summary; verdicts; diff statistics; review status |
| System health | concrete health fact; impaired capability; affected scope |
| Blocked work | dependency; blocked-since; owner or clearing condition |

Division of labour: daemon computes, API exposes typed values, clients decide layout, emphasis, and progressive disclosure. Clients do not reverse-engineer facts from prose, logs, or event names. Also here: **human project and work-unit names** for `1e`'s third line.

Land as one combined attention-projection contract unit, then producer-specific implementation units — simultaneous schema PRs would collide on the shared surface.

Consumers: `1h`, `1e`.

### B3 · Transactions behind displayed actions
Plan sites: §4 action semantics, §5.14 commands and conversations (`:1819`, `discuss` at `:1901`).

Each enabled action needs: command payload, validation and authorisation, idempotency/duplicate behaviour, deterministic state transition, returned outcome, synchronisation to other clients, error and retry semantics.

Must close in Wave 7 (participate in existing Phase 1 card types or the 1B loop): `discuss`, `continue_under_policy`, `convert_to_policy`, `adjudicate`, `retry_with_capabilities`, `choose_alternate_profile`, `request_changes`, `answer_and_retry`, `answer_without_retry`, `return_to_agent`.

Notes: asynchronous discussion is 1B (§5.14 already describes the transaction); live streaming and mid-turn intervention stay Phase 3. Alternate-profile/agent retry fits Wave 7 because the wave already contains operational failure recovery.

Consumer: `1d` — and see the open decision below.

### B4 · Evidence metadata
Plan site: §5.15 (`:1955`) — the daemon already owns validating evidence metadata including actual type and size.

Needed for Phase 1B: validated media type, byte size, producer, creation time, source run or claim, integrity/availability status, and a safe disposition for unsupported content.

Consumer: `1i`. Deferred to Phase 2: OCR, richer document metadata, expanded formats, thumbnails and derived previews, risk-classified evidence, robust large-file behaviour (`docs/plan.md:3287`). Deferred to Phase 3: asking questions about an attachment, interactive exploration via ACP, evidence-informed routing.

### B5 · Operational identity and pairing facts
Plan sites: §5.14 client/device synchronisation, §10 onboarding (`:2819`).

Needed: exact pairing-code expiry, daemon/host display identity, whether the connection is local or relayed, and ideally the scope of access being granted. Pairing is the entrance to the iOS workflow; an opaque pairing process weakens "decidable from the phone" before the operator reaches the inbox.

Consumer: `1l`. Deferred: advanced identity management, takeover without re-pairing, relay-account recovery.

---

## Work item C — Wave 7 instrumentation
Not new scope: §8 and §9 already prescribe attention delivery, open-to-decision latency, interruption rate, drill-down opens by item and device, and sampled comprehension defects (`docs/plan.md:2659`, `:2787`).

Needs coordinated client event emission, event contracts, daemon ingestion and storage, and reporting queries. Land the typed contracts and collection in Wave 7; **read** them in the Wave 8 exit evaluation.

Constraint: no unrestricted clickstream. Events stay small, typed, privacy-conscious, and tied to specific product questions. Useful additions for this redesign: rate of choosing a non-recommended action, time spent on unavailable actions, rate of opening Details before acting. A lower drill-down rate is only good if sampled comprehension holds.

---

## Work item D — Wave 8: integrate and evaluate, not backfill
Wave 8 is Phase 1B.2 and centres on the initiative view and integrated exit evaluation (`docs/plan.md:3178`).

Belongs here: initiative summary projections, initiative-level counts and attention rollups, work-status synchronisation, initiative navigation identifiers, cross-client evaluation of the completed contract.

Wave 8 consumes the Wave 7 contract and answers, per card type:
1. Can this decision be made from iOS?
2. Does every action shown actually execute?
3. Can the operator identify the recommended path?
4. Can they inspect enough evidence to reject that recommendation?
5. Does the decision state stay coherent across macOS and iOS?

A missing core fact discovered here is an exit-blocking Phase 1B defect, even if the fix lands during Wave 8. The review's "Verification Criteria" section is a serviceable exit checklist as written.

---

## Work item E — Phase 2 and later
Phase 2: server-wide historical search (query API, indexing strategy, pagination contract, authorisation model — genuinely new scope, currently unplanned); OCR and derived previews; richer or risk-classified card types; confidence/risk classifications beyond §9's minimum; affected-system summaries; normalised cost or impact estimates; card grouping and deduplication metadata; notification-priority projections.

Phase 3: live and mid-turn ACP conversation, resumable conversations, generated briefings, evidence-informed routing, mature WIP management, broader automated execution.

Dividing line: **if the fact is necessary to decide a current Phase 1 item, it is Phase 1B. If it makes the decision richer, safer, or scalable across more workflows, it is Phase 2.**

Nothing in work item A depends on E. `1k` only reserves ⌘F.

---

## The one decision that cannot be made in the client

Hiding unsupported actions (`1d`) removes the noise and should ship either way — but it does **not** resolve P1-2, and it must not be allowed to look like it did. A hidden action is still an action the daemon requested that nobody can take. Two coherent resolutions, and only two:

**Option 1 — implement the transactions in Wave 7 (recommended).** The B3 list, each with full command semantics. Asynchronous discussion counts; live streaming does not.

**Option 2 — remove those workflows from the Phase 1B exit claim.** Diminishing-returns review sits inside the 1B chain, so if `continue_under_policy` and `convert_to_policy` do not execute, that card is not decidable from the phone and the exit criterion should say so explicitly.

Leaving visible-but-dead actions while retaining the current exit claim is the one outcome that contradicts the plan. The capability-mismatch state in `1d` is the honest presentation of Option 2 — not a substitute for choosing.

---

## Changes that require editing the plan (vs. issue allocation only)

**Material plan/contract changes:**
1. Generic `recommended_action` authority and provenance (B1) — defines the difference between an available and an endorsed action.
2. Server-wide historical search — new scope; Phase 2 by default.
3. Any new model-authored summary or recommendation site — each needs a §5.13 judgment-site contract (authority, schema, evidence inputs, failure behaviour, labelling, deterministic fakes).
4. New action kinds or changed action semantics — alter the attention protocol; specify before implementing.

**Already promised by the plan; needs issues and wave allocation, not normative change:** §9 minimum card facts; existing action transactions; the §5.14 `discuss` transaction; basic evidence provenance; comprehension measurement; operational health and blocked-state facts; the Phase 1B phone-decidability evaluation.

---

## Recommended sequence

```
Wave 7 contract gate
    ↓
Attention presentation contract      (B1 recommendation provenance + B2 typed facts)
    ↓
Action command contracts             (B3, existing Phase 1 actions)
    ↓
Daemon fact producers & transactions
    ↓
macOS and iOS card/action adoption   (work item A, steps 2–4)
    ↓
Telemetry collection                 (C)
    ↓
Wave 8 initiative projections
    ↓
Cross-device Phase 1B exit evaluation
```

Work item A step 1 (`1b` colour variants) runs in parallel with the contract gate — it depends on nothing.

Strongest single recommendation: make **"Phase 1 attention contract completion"** a named Wave 7 work unit and a prerequisite for the remaining card UI work. That keeps the clients thin, avoids embedding policy in Swift, and makes the Phase 1B exit criterion testable rather than visually approximated.
