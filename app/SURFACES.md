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
  plus the shared ask-first decision shell, recommendation slot, action
  hierarchy, and the recommendation-led finding-adjudication card and typed
  route picker.
- **Generic:** seven of the fourteen card types use only the shared decision
  shell; four additional types compose card-specific orderings from the shared
  graphic module set.
- **Not yet:** one card type (`effect_proposal`), conversations, the evidence
  and spec viewers, proposal batches, the
  initiative view, the remaining blocked-reason and waiver readiness display,
  push notifications.
- **Open:** twelve placement questions, listed at the end.

## Screens

| Screen | Status | Notes |
| --- | --- | --- |
| Pairing | Done | One-time code semantics and grouping, editable system device-name prefill, host/expiry/connection slots, and helper copy naming the host Devices list and per-decision audit record. No credential-like grant value renders. §15 styling: serif title on iOS, wax failure label, accent button. |
| Inbox | Done | iPhone keeps Inbox and Runs as persistent tabs with independent stacks; Inbox owns its scope/project filters, non-zero open badge, and urgent summary. Three-line scanning rows show type and exceptional badges, a type-written summary, project/work-unit context, and coarse relative time. Scope/project counts share the list predicate; selected rows use a leading bar, wash, and border. Context menus retain identifiers and evidence digests. macOS keeps the split-view source list. Rebuilds fully once a missed notification is detected; today the 15-second heartbeat detects it, and a refresh on return to foreground is Not yet (plan §5.14). |
| Decision detail | Done | One shell orders ask, optional recommendation, actions, and decision context without repeating the navigation title. On macOS, facts, bindings, claims, evidence, and details live in a closed-by-default inspector whose section state persists; recommendation and actions move to a second card column at 1,000pt. Inbox context-menu requests select the item and reveal its technical details. Actions rank by job and unavailable requests are recorded in Details. Accessibility Dynamic Type stacks facts and actions and collapses lower sections. Per-card layouts are tracked under Cards. |
| Freshness banner | Done | §15 styling: tinted wash with a mono keyword; unreachable and sync-failing take the accent, revoked takes wax. |
| Run list | Done | Filter by project; two semantic lines separate stage/round from the current hold or milestone, and attached watch chips wrap without truncation. §15 styling: outcome chips (ready is quiet, in progress water, blocked accent, failed wax, not observed dashed). |
| Run timeline | Done | §15 styling: accent-washed hold card, milestone rail, invocation status chips. |
| Mac menu bar | Done | Mono key template icon with the badge dot top-right; state line, facts, and explanation first, actions and Quit grouped last. Daemon readiness today; doctor results and 1B.1 signals come later (plan §10). |
| Conversation / Discuss | Not yet | API exists (`getConversation`, `uploadAttachment`); no UI. Plan §5.14. |
| Evidence packet viewer | Not yet | Detail attachments render explicit loading, image, non-image, unavailable, and too-large states with copyable digests and memory-only open sheets. The full provenance-labeled packet viewer is missing; #922 adds typed metadata. Plan §9, §5.15. |
| Spec and diff viewer | Not yet | Diff from last reviewed version, prior comments, claimed addressals. Plan §4, §9. |
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
| `spec_approval` | approve, request changes, discuss, stop | Generic | #710; spec and diff viewer |
| `review_diminishing_returns` | finish now, apply then finish, continue under policy, convert to policy | Done for the shared yield-chart module; production chart facts remain data-gated | #724, #844 |
| `review_dispute` | adjudicate, discuss, stop | Done for the equal-position and daemon-fact composition; production position facts remain data-gated | #724, #855, #917 |
| `finding_adjudication` | accept the recommended route, pick an alternative, discuss, stop (§7 widens this to answering questions, challenging assumptions, and asking for more detail) | Done for the proposal, producer-specific model/engine/mixed-origin labels, daemon facts, typed route actions, and alternatives; Discuss UI is Not yet | #840 executes the chosen route; Conversation / Discuss under Screens |
| `review_contradiction` | recover the exact contradiction, or leave parked | Generic (recovery details shown) | |
| `review_configuration` | adopt the configuration, discuss, stop | Generic (recovery details shown) | |
| `execution_failure` | retry, retry with capabilities, discuss, stop | Done for the shared stage-rail and diagnostic-claim composition; production stage and timing facts remain data-gated | #869 adds "retry with another provider profile" for quota, expiry, and capacity failures: a profile picker, with cost owner and review independence shown before confirming; #917 supplies typed stage and timing facts |
| `agent_question` | answer and retry, answer without retry, stop | Generic | #724 |
| `publish_blocked` | rerun trust check, choose another profile, inspect the failure, stop | Generic | |
| `ready_for_final_review` | view PR, return to agent, mark seen, dismiss, stop | Done for the readiness checklist and per-round yield composition; change summary remains data-gated | #724, #917; remaining readiness display below |
| `run_proposal` | start, start with changes, decline, snooze | Done for actions and facts; the full proposal artifact and the revised-digest diff are Not yet | Batch grouping (see Screens) |
| `effect_proposal` | approve, approve with changes, decline, snooze; target picked from a daemon-supplied list | Not yet | Lands with the §5.13 effect registry in 1B |
| `system_health` | acknowledge, run doctor, stop or resume unattended, resolve re-enrollment | Generic (posture badge and re-enrollment details shown) | #868 (account-probe items), #867 (retired-identity items) |
| `blocked` | read only | Generic | |

## Rules Every Card Follows

| Rule | Status |
| --- | --- |
| Anything an agent wrote is visibly labeled as a claim, never shown as fact (plan §9) | Done for the claims section; the contract's text carrier exists, but per-card summaries are neither produced nor laid out yet, #723 |
| Readiness shows as Blocked, ReadyClean, or ReadyDegraded with waiver IDs and who granted them, never a plain yes/no (plan §6) | Partial: ready final-review items show clean/degraded and the evaluation-set digest; blocked reasons and waiver identities are Not yet |
| Severity uses one scale: critical, high, medium, low (plan §7) | Not yet |
| Images load from the artifact store by digest; agent images are labeled claims (plan §4, §5.15) | Done: every attachment has an explicit bounded state, and images expand into a memory-only zoomable sheet |
| Evidence from an older head is not shown as current after a remediation head (plan §5.15) | Not yet |
| Commit-plan notices (fallback, present-but-not-honored) appear as a labeled "Commit plan" fact (plan §5.6) | Done |
| Fault-class capture at resolution: a suggested value, one tap to correct, allowed to stay unknown (plan §4) | Not yet |
| A stale submission swaps in the replacement item and says so (plan §4) | Done |
| Actions that matter are disabled until current state is confirmed; no offline approvals in Phase 1 (plan §5.14) | Done |
| A recommendation is rendered only when the contract supplies one; offer order never implies a recommendation | Done in the client shell; no production recommendation field exists yet, #917 |
| Consequential stop, decline, and dismiss actions require an explicit destructive confirmation; navigation and loss-risk actions alone use icons | Done |
| Notifications are hints only; a late one for a resolved item opens current state with no stale action (plan §4, §5.14) | Not yet (no push channel until Phase 2) |

## Behind the Scenes

Client state the plan specifies. Mostly invisible, but each affects what the
user sees.

| Behavior | Status |
| --- | --- |
| A daemon restore or host takeover (new `sync_epoch`) wipes the cache and re-bootstraps | Done |
| A partial fetch never marks the cache current; heartbeat catches missed invalidations | Done |
| Every decision is an idempotent command; a retry after a lost response returns the original result | Done |
| Metadata in protected storage, only the device credential in Keychain, attachments never written to disk | Done |
| Loopback and Tailscale transports | Done, but each endpoint pairs separately today; switching without re-pairing is Not yet |
| Relay transport, surviving host takeover without re-pairing (plan §5.19) | Later |
| Light (Freeside) and dark (Straylight) palettes; status colors never borrow the accent (plan §15) | Done for every Done screen: contrast-safe text/wash/border tokens with Increased Contrast cuts, the three faces, bordered state chips, and quiet-neutral success (`devlog/2026-08-21-1430-design-language-restyle.md`); the macOS window title and segmented controls stay system chrome. iOS has no icon |
| Schedules synced and shown on the run list | Done; a schedules page is Later |
| Deterministic screenshot coverage across the inbox, every Phase 1 card, macOS inspector/reflow/operational summary, runs, timeline, and pairing at six Dynamic Type sizes | Done; pixel digests fail with inspectable PNG dumps and record only through `FREESIDE_RECORD_SCREENSHOTS=1` |
| "Opened" receipts per delivery, and drill-down counts per card (plan §5.14, §8) | Not yet; API exists (`reportDeliveryOpened`) |
| Device revocation | Not yet; API exists (`revokeDevice`), no screen |

## Open Questions

The plan implies each of these but doesn't say where it lives. They stay
open until a unit answers them; answering is a design decision.

1. **Editing a proposal before approving.** Today a sheet on `run_proposal`. Is that also right for `effect_proposal` and for batches?
2. **Where project- and system-level items go.** There is no project or system screen until the deferred ones land.
3. **Turning a repeated preference into a policy PR.** One action today; no authoring or preview flow is described.
4. **A standing "unattended stopped" indicator.** A stop is a durable state; one inbox item isn't a persistent signal. Menu bar, banner, or both?
5. **Snoozed items.** Where do they go and how do they come back?
6. **Notification grouping and badges.** No grouping key or badge rule exists; push arrives in Phase 2.
7. **Device list and revocation.** The API exists; no screen.
8. **Per-run isolation class, credential mode, and egress profile.** The plan says "report honestly"; the run timeline is the likely home.
9. **Daemon unreachable.** The external ntfy alert has no in-app counterpart beyond showing unreachability.
10. **Trust-profile review at onboarding.** CLI-only today; unclear whether the app ever shows it.
11. **Comprehension-defect capture** for sampled decision audits (plan §8, §9).
12. **Fault-class capture placement.** On the card at resolution, or a follow-up prompt?

Search and export of items or evidence are not in the plan at all; they
are named here only so nobody assumes they are.
