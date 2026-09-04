# Client Surfaces

What the Freeside app (Mac and iOS) has to show, and how far along each piece
is. The plan (`docs/plan.md`) defines every surface; this file only tracks
status. Design decisions, layout, and wording are deliberately absent: the
**Open Questions** section is the agenda for a later UX pass.

**Keeping it current:** when a PR adds or changes something the app shows,
update its line here in the same PR. A contract unit that could answer an
open question either answers it or says it is leaving it open.

Status words: **Done** (built, both platforms), **Generic** (shows up in the
inbox and detail view with the standard layout, nothing specific to it yet),
**Not yet** (defined by the plan or API, not built), **Later** (a later
phase), **Open** (the plan implies it but doesn't say where it goes).
Mac and iOS match on every line unless a line says otherwise.

## At a Glance

- **Done:** pairing, inbox, decision detail, run list with watches and
  deadlines, run timeline, freshness banner, Mac menu bar, the sync
  and command-retry machinery, and the run-proposal card with its
  "start with changes" and snooze sheets (its full-artifact view is not),
  plus the shared ask-first decision shell, the contract recommendation
  rendered in its source register, action hierarchy, per-type typed card
  facts, the recommendation-led finding-adjudication card and typed
  route picker, plus the text-only conversation thread and composer.
- **Generic:** six of the fourteen card types use only the shared decision
  shell; five additional types compose card-specific orderings from the shared
  graphic module set.
- **Not yet:** one card type (`effect_proposal`), conversation attachments, the evidence
  packet viewer, proposal batches, the
  initiative view, push notifications.
- **Open:** twelve placement questions, listed at the end.

## Screens

| Screen | Status | Notes |
| --- | --- | --- |
| Pairing | Done | One-time code semantics and grouping, editable system device-name prefill, and helper copy naming the host Devices list and per-decision audit record. Pairing details render the daemon's own facts for the entered code (`POST /pairing/preview`): host display name, code expiry, connection mode (Local or Tailscale), and the granted operator scope; an empty or rejected code shows a neutral prompt, never a reason. No credential-like grant value renders. Both detail layouts draw the shared fact row, so a value longer than 40 characters stacks under its label. §15 styling: serif title on iOS, wax failure label, and Pair as the filled primary control, disabled in the rule-bordered faint recipe until the preview facts return. |
| Inbox | Done | iPhone keeps Inbox and Runs as persistent tabs with independent stacks; Inbox owns its scope/project filters, non-zero urgent badge, and urgent summary. The Open caption states: "Newest first; overdue leads. Priority breaks ties within the past hour, past day, and older." Resolved describes newest decision time with creation time as its fallback; All explains both rules and that open items lead. Open items lead concluded items; overdue open items lead, followed by age bands (under one hour, under one day, older or undated), priority, newest creation time, then server order. Concluded items use newest conclusion time (creation time if absent), then server order. The clock is sampled at each full rebuild or successful unchanged-revision confirmation, and next-item navigation follows the same order. Time-only refreshes retain local conclusion feedback until a full list rebuild. Three-line scanning rows show type and exceptional badges, a type-written summary, project/work-unit context, and coarse relative time. Scope/project counts share the list predicate; selected rows use a leading bar, wash, and border. Context menus retain identifiers and evidence digests. macOS keeps the split-view source list with the same urgent count. Rebuilds fully once a missed notification is detected; the 15-second heartbeat, foreground activation, reachability recovery, pull to refresh, toolbar, and ⌘R all use the coalesced refresh path (plan §5.14). |
| Decision detail | Done | One shell orders ask, the daemon's reason as its answer, then each type's own module ordering, with the typed facts always above the action region and never repeating the navigation title. A type whose reason is the agent's own summary rather than a daemon-authored fact renders no Context section, so that text appears once in the unverified summary layer; `spec_approval` is the only such type, and an item carrying no summary claim keeps Context so its reason is never lost. A recommendation leads with one label carrying its register and the daemon's confidence, then the agent-claim disclaimer where the register is agent judgment, then the daemon's full reason under Why, then the action it recommends, with the provenance digests one disclosure away below it. A reserved agent summary renders as its own unverified layer after authenticated context, with its producer invocation visible, while other agent claims stay separate. A resolved decision leaves a neutral six-second receipt outside the card, announces before the delayed advance, which returns to the inbox by default (macOS clears the selection so the detail column shows the operational summary) or opens the next open item when the operator turned that on, and can reopen the concluded item without changing the inbox filter; uncertain submissions retain Retry and never advance. On macOS, the card carries the §9 typed facts and the inspector carries only what the card omits: claims, evidence, and technical bindings, closed by default with its section state persisted; while that inspector is open the card's own Evidence module collapses to one "N attachments → inspector" pointer row, so the packet renders in one place at a time; recommendation and actions move to a second card column at 1,000pt. Inbox context-menu requests select the item and reveal its technical details. Actions rank by job and unavailable requests are recorded in Details. Accessibility Dynamic Type stacks facts and actions and collapses lower sections, and a fact value longer than 40 characters stacks under its label at every type size, so the narrow inspector's digests and bindings wrap across the full width instead of a squeezed trailing column. Per-card layouts are tracked under Cards. |
| Freshness banner | Done | §15 styling: tinted wash with a mono keyword; unreachable and sync-failing take the accent, revoked and a stale last-update threshold take wax. The last-updated indicator stays visible in the macOS toolbar and iOS inbox: "Updated recently" while fresh, then a coarse minute-or-coarser relative time once stale. |
| Run list | Done | Filter by project; two semantic lines separate stage/round from the current hold or milestone, and attached watch chips wrap without truncation. §15 styling: outcome chips (ready is quiet, in progress water, blocked accent, failed wax, not observed dashed). |
| Run timeline | Done | §15 styling: accent-washed hold card, milestone rail, invocation status chips. |
| Mac menu bar | Done | Open Freeside, Show Inbox with its count, and the shared urgent count lead; daemon readiness and lifecycle actions have their own section, with Quit last. The mono key template icon keeps the daemon-state badge dot top-right. Doctor results and 1B.1 signals come later (plan §10). |
| Conversation / Discuss | Done for text | Every discuss-capable card can open a text composer and render the ordered thread, attachment digests, and awaiting-agent state. Threads bootstrap, persist in the disposable cache, refetch after submit, and converge on heartbeat. Uploading attachments from the composer is Not yet. Plan §5.14. |
| Evidence packet viewer | Not yet | Each attachment state is decided from the reference's daemon-validated §5.15 metadata (media type, size, availability) before any fetch: loading, image, non-image, no-bytes `unavailable` (bytes_absent), too-large, and a retryable `fetchFailed` distinct from the daemon-reported no-bytes state, with a typed media-type/size caption, copyable digests, and memory-only open sheets. The full provenance-labeled packet viewer is still missing. Plan §9, §5.15. |
| Spec and diff viewer | Done | Initial and revised approvals present the daemon-bound specification as the approval object in a dedicated scrollable reader. A revision leads with its authenticated iteration, prior iteration, complete line counts, and prior-comment-to-unverified-addressal mapping; a separate unified-diff reader distinguishes hunks, additions, removals, and context while collapsing later hunks and stating when the payload is truncated. Plan §4, §9. |
| Proposal batch | Not yet | Several proposals decided one by one in one place. Plan §4. |
| Initiative view | Not yet | Phase 1B.2. Plan §5.18, §11. |
| Project detail, past-work history, schedules page, consent grants | Later | Explicitly after 1B. Plan §11. |
| Usage, briefings, WIP views, ACP attachment | Later | Phase 3. |
| Widgets, App Intents, Live Activities | Later | Phase 4. |
| Settings editor | Never | Configuration changes arrive as approval cards, not forms. Plan §11. |

## Cards

One line per attention item type. "What you can do" is the plan's §4 action
list; what each card shows at each layer is the plan's §9 table. "Coming"
lists open issues that will change the card.

| Card | What you can do | Status | Coming |
| --- | --- | --- | --- |
| `spec_approval` | approve, request changes, discuss, stop | Done for text request-changes and discuss transactions; the superseded card links to the next open specification once it syncs. The card renders no Context section: the daemon writes its reason from the agent's summary, which the unverified summary layer already shows once, labeled, and that summary layer renders above the actions so plan §9's plan-altitude lead survives the removal; a legacy approval persisted without a summary claim keeps Context. Initial and revised cards present the daemon-bound specification below the actions; revised cards lead with authenticated iteration and diff counts plus prior comments mapped to clearly unverified addressal claims, with the bounded unified diff in its own reader. | |
| `review_diminishing_returns` | finish now, apply then finish, continue under policy, convert to policy | Done: the card leads with cost so far from the typed field and the shared yield-chart module, which draws its per-round text and, where bars render, a two-token legend keying their fills, with no prose restatement beneath; convert to policy is omitted from the action surface and recorded in the drill-down (revision 40 carves it out of the Phase 1 claim) | #844 |
| `review_dispute` | discuss, stop (plus approve for observation-only shadow findings) | Done: the card leads with the typed run, round, disputed findings, and completion evidence beside the equal-position composition, which draws the two positions alone with no prose restatement beneath | #855 |
| `finding_adjudication` | accept the recommended route, pick an alternative, discuss, stop (§7 widens this to answering questions, challenging assumptions, and asking for more detail) | Done for the proposal, producer-specific model/engine/mixed-origin labels, daemon facts, typed route actions, alternatives, and text discussion; each finding leads as one collapsed row (id, recommended route, goal relationship, confidence) under its producer register, and opening a row expands its proposal and daemon facts in place, above the actions | #840 executes the chosen route |
| `review_contradiction` | recover the exact contradiction, or leave parked | Generic (recovery details shown) | |
| `review_configuration` | adopt the configuration, discuss, stop | Generic (recovery details shown) | |
| `execution_failure` | retry, retry with capabilities, discuss, stop | Done: the card leads with the typed outcome, failing stage, and invocation beside the stage-rail and diagnostic-claim composition; retry with capabilities is omitted until #921 lands its transaction | #869 adds "retry with another provider profile" for quota, expiry, and capacity failures: a profile picker, with cost owner and review independence shown before confirming |
| `agent_question` | answer and retry, stop (the producers do not offer answer without retry) | Done: the card leads with the daemon-typed decisions, each rendered in the unverified claim register the question prose comes from and opening with its own question in the serif, then what it blocks, then the enumerated options with the agent's recommendation marked as an unverified claim on that option alone; the asking stage and the blocker kind render once as fact rows below the decisions, never as a preface above them; on an implementation-stage question the answer names the retry_implementation route | revise_specification waits on the campaign-identity decision that lets a revised specification mint a fresh implementation run |
| `publish_blocked` | rerun trust check, inspect the failure, stop | Done: the card leads with the failed trust rule or hold reason from the typed field; the alternate-profile action is retired from the vocabulary (#936, plan revision 44) | |
| `ready_for_final_review` | view PR, return to agent, mark seen, dismiss, stop | Done: the card leads with the readiness checklist. Its first line is the verdict word followed by the counts of the rows beneath it; those rows run failed and waived first, then advisory, then notes, with the passed ones behind a disclosure closed by default whose closed line still names each. The checklist carries the bound head and base, every requirement of the evaluated set with its state, each waiver's id, dimension, and granting authority, and a stale verdict shown against the observed head or base. Per-round yield follows, keeping the typed diff stats last before the actions, alone: the bound head and base render in the checklist's "Bound to" row and in Details, never a third time as facts. That row sits inside the collapsed passed disclosure while the binding is current, its label still on the closed line and its coordinates one disclosure away; it leads the checklist as a failed row once the daemon marks the binding stale, so the coordinates are on the face of the card exactly when they contradict the verdict. Change summary remains data-gated and return to agent is omitted until #919 | |
| `run_proposal` | start, start with changes, decline, snooze | Done for actions and facts, including the declaration-bound path count shown read-only in revisions; the full proposal artifact and the revised-digest diff are Not yet | Batch grouping (see Screens) |
| `effect_proposal` | approve, approve with changes, decline, snooze; target picked from a daemon-supplied list | Not yet | Lands with the §5.13 effect registry in 1B |
| `system_health` | acknowledge, run doctor, stop or resume unattended, resolve re-enrollment | Done: the card and row lead with the typed diagnostic code and the capability it impairs, daemon facts only (posture badge and re-enrollment details shown) | #868 (account-probe items), #867 (retired-identity items) |
| `blocked` | read only | Done: the card and row lead with the typed wait, its coarse duration, and the blocking item or pull request, daemon facts only; the exact wait start stays in the technical bindings | |

## Rules Every Card Follows

| Rule | Status |
| --- | --- |
| Anything an agent wrote is visibly labeled as a claim, never shown as fact (plan §9) | Done: claims, agent summaries, and an agent-judgment recommendation each render in a labeled unverified register, and the mechanical `system_health` and `blocked` cards carry daemon facts alone |
| Readiness shows as Blocked, ReadyClean, or ReadyDegraded with waiver IDs and who granted them, never a plain yes/no (plan §6) | Done: a ready card shows clean or degraded, the evaluation-set digest, the bound head and base, and every requirement's state, with each waived requirement naming its waiver's id, dimension, and granting authority; a blocked verdict never becomes a ready card, because the daemon holds the run instead of creating a ready item, so the client renders its per-requirement states from the daemon's typed detail and never derives a reason from the verdict class; the verdict leads that module on a line of its own, followed by the counts of the rows below it, and the passed requirements sit behind a disclosure closed by default whose closed line still names each, so no requirement leaves the surface |
| Severity uses one scale: critical, high, medium, low (plan §7) | Not yet |
| Images load from the artifact store by digest; agent images are labeled claims (plan §4, §5.15) | Done: every attachment has an explicit bounded state, and images expand into a memory-only zoomable sheet |
| Evidence from an older head is not shown as current after a remediation head (plan §5.15) | Partial: a ready verdict and its bound head and base render as stale, beside the observed values, once the daemon records `readiness_invalidation` or `base_freshness.advanced`; evidence attachments from an older head are Not yet (#922) |
| Commit-plan notices (fallback, present-but-not-honored) appear as a labeled "Commit plan" fact (plan §5.6) | Done |
| Fault-class capture at resolution: a suggested value, one tap to correct, allowed to stay unknown (plan §4) | Not yet |
| A stale submission swaps in the replacement item and says so (plan §4) | Done |
| Actions that matter are disabled until current state is confirmed; no offline approvals in Phase 1 (plan §5.14) | Done |
| A recommendation is rendered only when the contract supplies one; offer order never implies a recommendation | Done: the card reads `AttentionItem.recommendation`, revalidates its source-specific provenance, and renders daemon and project policy as card facts and agent judgment as a labeled proposal; #1002 produces the first one |
| Consequential stop, decline, and dismiss actions require an explicit destructive confirmation; navigation and loss-risk actions alone use icons | Done |
| Notifications are hints only; a late one for a resolved item opens current state with no stale action (plan §4, §5.14) | Not yet (no push channel until Phase 2) |
| A fact row stacks its value under its label once the value passes 40 characters (`--fs-fact-row-stack-threshold`), at every Dynamic Type size | Done: one shared `FactRow` covers the decision card and macOS inspector, both pairing detail layouts, and the operational summary, so a digest, an invocation id, or a `file:line` wraps across the row's full width instead of a narrow trailing column; an accessibility size still stacks every row whatever its length; the readiness checklist's rows follow the same rule, so a waiver sentence stacks under its requirement label, and the checklist's verdict line stacks with them |

## Behind the Scenes

Client state the plan specifies. Mostly invisible, but each affects what the
user sees.

| Behavior | Status |
| --- | --- |
| A daemon restore or host takeover (new `sync_epoch`) wipes the cache and re-bootstraps | Done |
| A partial fetch never marks the cache current; heartbeat catches missed invalidations | Done |
| Every decision is an idempotent command; a retry after a lost response returns the original result | Done |
| Conversation snapshots bootstrap and persist with the cache; a discuss refetches immediately and heartbeat converges the next reply | Done |
| Manual, foreground, reachability, and pull refreshes share one single-flight path; overlapping requests join the same daemon round | Done |
| Metadata in protected storage, only the device credential in Keychain, attachments never written to disk | Done |
| Loopback and Tailscale transports | Done, but each endpoint pairs separately today; switching without re-pairing is Not yet |
| Relay transport, surviving host takeover without re-pairing (plan §5.19) | Later |
| Light (Freeside) and dark (Straylight) palettes; status colors never borrow the accent (plan §15) | Done for every Done screen: contrast-safe text/wash/border tokens with Increased Contrast cuts, the three faces, bordered state chips, and quiet-neutral success (`devlog/2026-08-21-1430-design-language-restyle.md`); the macOS window title, segmented controls, and menu-bar status item (badge included) stay system chrome. iOS has no icon |
| Schedules synced and shown on the run list | Done; a schedules page is Later |
| Deterministic screenshot coverage across the inbox, every Phase 1 card, decision banners, the shared unavailable state, macOS inspector/reflow/operational summary, runs, timeline, and pairing at six Dynamic Type sizes | Done; pixel digests fail with inspectable PNG dumps and record only through `FREESIDE_RECORD_SCREENSHOTS=1` |
| Comprehension telemetry: capability registration, the daemon-derived action surface, and typed decision-path events (plan §8, §9) | Done: session-start capability registration, per-item action-surface adoption in the decision ranking, and a persisted best-effort event queue emitting card_opened, details_opened_before_acting, not_decidable_here_shown, action_taken, and recommendation_override. `drill_down_opened`'s emission (the AttachmentRow trigger) and the per-delivery "Opened" receipt (`reportDeliveryOpened`) are the remaining view wiring |
| Device revocation | Not yet; API exists (`revokeDevice`), no screen |

## Open Questions

The plan implies each of these but doesn't say where it lives. They stay
open until a unit answers them; answering is a design decision.

1. **Editing a proposal before approving.** Today a sheet on `run_proposal`. Is that also right for `effect_proposal` and for batches?
2. **Where project- and system-level items go.** There is no project or system screen until the deferred ones land.
3. **Turning a repeated preference into a policy PR.** One action today; no authoring or preview flow is described.
4. **A standing "unattended stopped" indicator.** A stop is a durable state; one inbox item isn't a persistent signal. Menu bar, banner, or both? Tracked by #980 (plan §11, wave 8).
5. **Snoozed items.** Where do they go and how do they come back?
6. **Notification grouping and badges.** No grouping key or badge rule exists; push arrives in Phase 2.
7. **Device list and revocation.** Revocation exists in the API; no list endpoint and no screen. Tracked by #981 (plan §11, wave 8).
8. **Per-run isolation class, credential mode, and egress profile.** The plan says "report honestly"; the run timeline is the likely home. Tracked by #979 (plan §11, wave 9), which also carries harness, model, effort, cost owner, and the independence rule.
9. **Daemon unreachable.** The external ntfy alert has no in-app counterpart beyond showing unreachability.
10. **Trust-profile review at onboarding.** Answered by plan revision 41: CLI-only; the app never shows it.
11. **Comprehension-defect capture** for sampled decision audits (plan §8, §9). The daemon records a defect via `freesided comprehension record-defect`; finding them stays manual (a sampled decision-audit workflow is a non-goal), and there is no in-app capture surface.
12. **Fault-class capture placement.** On the card at resolution, or a follow-up prompt?

Search and export of items or evidence are not in the plan at all; they
are named here only so nobody assumes they are.
