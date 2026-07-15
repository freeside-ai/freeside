---
run: manual
stage: signet-command-acceptance
date: 2026-07-15
branch: feat/attention-command-service
---

# Signet attention service and ClientCommand acceptance (issue #65)

First signet-lane unit (tracking issue #83), stacked on #91's store
snapshot reads (`feat/store-snapshot-meta`); fiat-assigned after its
first claim released as blocked. Declared path:
`daemon/internal/signet`.

## Decisions

**Store rejections are translated, not re-exported.** The service owns
`ErrStaleVersion`/`StaleVersionError{Replacement}` and
`ErrClosedItem`/`ClosedItemError{Item}`, translating the store's
`StaleCommandError`/`ClosedItemError` at the boundary; both service
carriers are what the API's 409 `StaleVersionRejection` renders from.
This resolves the question the status-gate note
(2026-07-14-2212) deferred to #65: `ErrClosedItem` maps to the same
409-with-canonical-item shape as staleness, distinguishable by error
class. The entity_version mismatch and the binding mismatch
deliberately share one service error: the client's recovery is
identical (re-render from the replacement), and the API contract
defines one rejection shape for both. `ErrNotFound`,
`ErrActionNotOffered`, and `ErrImmutableConflict` pass through wrapped;
they are daemon-internal outcomes with no §5.14 rendering yet.

**The provisional `expected_bindings` map is not carried.** For a
decision command the spec itself names the payload's `item_version`,
`pr_head_sha`, and `artifact_digests` as the authoritative binding set;
carrying the map too would create a second, unchecked source of the
same facts. Revisit when a non-decision command type lands (the map
exists for those).

**Replay aborts the Write.** `store.Write` bumps the revision
unconditionally at commit, so an idempotent retry captures the original
result in-tx and returns a sentinel (`errReplay`) to roll back: the
retry returns the exact original `CommandResult` with no revision burn
(pinned store-side by `TestCommandSnapshotReplayInsideWrite` in #91).
The replay check stays inside the Write, so two racers with one
command_id serialize on the store's single write connection and the
loser converges as a replay.

**`actionOutcome` classifies all 26 actions in a default-less switch.**
Four groups per plan §4. Concluding: the parameterless decisions whose
whole accepted effect is the status flip plus the record — resolving
(`approve`, `stop`, `finish_now`, `apply_then_finish`, `retry`,
`rerun_trust_evaluation`, `start`, `stop_unattended`) and dismissing
(`dismiss`, `decline`); downstream reactions are the Wave 2 engine's,
the issue's own deferral. Record-only, no item write by design
(`open_pr` "navigation, not resolution"; `acknowledge` "means seen,
never resolved"; `mark_seen`; `inspect_trust_failure` — navigation;
`run_doctor` — a system_health item "remains blocking until the
underlying diagnostic clears"). **Pending, rejected with
`ErrUnsupportedAction` before any transaction**, because the accepted
effect is not yet representable: `discuss` (#68's conversation
transaction; record-only acceptance would also let two devices commit
discuss at one item version where §5.14 test 7 requires a single
winner); `snooze` (timing update); `start_with_changes` (revised
proposal artifact + supersede transaction, plan §4);
`continue_under_policy`, `convert_to_policy`, `adjudicate`,
`retry_with_capabilities`, `choose_alternate_profile` (decision
parameters `DecisionPayload` has no field for; contract widening per
#22 when their consumers land); `request_changes`, `answer_and_retry`,
`answer_without_retry`, `return_to_agent` (content rides the
conversation channel #68 owns). Rejected alternatives: a two-group
switch (everything-but-dismiss resolves) — it would resolve items on
mark_seen/acknowledge against plan §4's explicit text; record-only
acceptance of the pending group (the initial cut, then a partial sweep
of just discuss/snooze) — Codex's rounds 1 and 2 correctly flagged
that recording a command whose effect or data is silently dropped
fails the faithful-record rule, so the whole class rejects until each
owning unit lifts it. The exhaustive linter forces a new Action member
to declare its outcome. Revisit when #23 (per-type action sets), #68
(conversations), the timing unit, or a #22 payload widening lands.

**Review dispositions (Codex, PR #94).** Round 1 confirmed and fixed:
the discuss single-winner violation (pending group). Round 1 declined:
an active-device gate on new commands — sync tests 15–16 are #67's
unit (device pairing and revocation, this lane, two units on), no HTTP
surface exists before #66/#67 so no revoked device can reach the
in-process boundary meanwhile, and the gate needs #67's device
lifecycle semantics to be testable against real revocation. Round 2
confirmed and fixed: `start_with_changes` accepted as a bare
resolution would lose the user's revisions — swept as the
data-carrying class above rather than the one cited action. Round 3
confirmed and fixed: the pending-action gate originally ran before any
transaction, so a command_id already committed for a supported action,
resubmitted with a pending one, got `ErrUnsupportedAction` instead of
the command-id-first judgment; the gate now runs inside the Write for
genuinely new ids only, so such a reuse is a changed body under an
immutable id (`ErrImmutableConflict`). Shape validation
(`NewCommand`, non-positive expected_entity_version) deliberately
stays pre-transaction: the store's own PutCommand validates before its
idempotency check, so a malformed request never consults state. Round 4
confirmed and fixed: both rejection carriers now hold the replacement
or canonical item's `store.Snapshot` alongside the item — the API's
409 renders an `AttentionItemSnapshot` (entity_version,
as_of_revision, item), so without it the HTTP projection (#66) would
need a second, race-prone read to build the promised response; the
same-transaction snapshot travels with the rejection instead.

**Openness outranks version staleness at the service too.** The service
checks status before `expected_entity_version` (then delegates binding
authority to `PutCommand`), preserving #55's recorded precedence: a
closed item at any version reports closure, never a rebind invitation.

Revisit when the sync-surface unit (#66) projects these errors over
HTTP: the 409 rendering and any snapshot envelope belong there, and
this note's error mapping is its input.
