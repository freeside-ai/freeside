# Carry a Selection in the Alternate-Agent Retry

Issue #2735; source note `2026-11-05-1035-select-by-enrollment-model-effort.md`
(#2593). Tier A: the retry record, its acceptance, the move's lines, and the
admission re-gate change.

Chose offers that are enrollments and a record that holds the enrollment,
model, and effort the operator chose, over keeping tree agents in the record
and offers behind a bridge, because admission since #2734 resolves a
selection, and a record that names an agent can only be admitted by
reducing a name the tree may since have renamed or removed. The wire keeps
agent names until #2808; signet translates at the edge.

## Decisions

- **The command's message is the choice's canonical JSON.**
  `{"enrollment_id":…,"model_id":…,"effort":…}`, with model and effort
  empty when the choice omits them. A replay compares the same bytes, and
  `ParseAlternateAgentChoice` refuses any other form. Rejected:
  `TaskLineSelection.Text`, because an enrollment id holds a slash, so the
  text form cannot be split back.
- **A v2 record has its own canonical encoding.** The record gains
  `selection` and its id hashes `freeside.alternate_agent_retry/v2`. A v1
  record keeps its bytes and id (goldens `alternate_agent_retry_v1` and
  `alternate_agent_retry_parked_move_v1`), decodes, and admits through the
  tree as before. Only v2 is built; the store still accepts a v1 record on
  write, since nothing builds one and the engine tests seed one to prove
  it admits.
- **`orderedEnrollments` is the one seam with #2736.** The routing policy
  still names agents, so the enrollments a policy orders for a role are the
  ones its agents reduce to. The parked task's candidates, the move's other
  roles, the moved line's anchor, and a moved line's counterpart skip all
  read the policy through it, and #2736 replaces its body. Until then, a
  tree that renames an ordered agent leaves the cited policy ordering no
  enrollment, and the anchor refuses (`TestMovedV2LineAnchorsToThePolicyOrder`).
- **An enrollment with no catalog takes only the lineup's tuple, on the
  lineup's client.** With no recorded catalog the bridge would take any
  model as named, so the offer requires the enrollment to bind the client
  the lineup line's own enrollment does, and acceptance refuses any model
  or effort but the lineup's. Rejected: offering any enrollment with no
  catalog, which could put a Claude model on a Codex enrollment.
- **The refusal names the field, never the text.** An unserved choice is
  `model_id: …` or `effort: …` wrapping
  `ErrAlternateAgentSelectionUnserved`, a 400. An unknown wire agent name
  is `ErrAlternateAgentNotOffered` with no name in it.
- **The wire renders offers as agents and drops what no agent maps to.**
  An enrollment offer shows under the tree agent that runs the
  implementer's lineup tuple on it (`AgentForSelection`). An offer no one
  agent maps to is dropped, and a card left with none renders without
  `retry_with_alternate_agent`; the stored item keeps both. Rejected:
  inventing an agent name, which the operator could choose and the daemon
  could not translate back.
- **A v1 offer is refused as not offered.** A card raised before this
  change lists agent offers; a choice names an enrollment, so it matches
  none. Such a card still renders its offers and action, and choosing one
  refuses with 400; the operator retries or stops instead. Rejected:
  translating a v1 offer, which would accept an offer the daemon derived
  under rules this change replaced.
- **The move request keeps its agent beside the selection.** The receipt's
  `agent` is the wire name as accepted, and the selection is what it
  translated to; replay compares both (`ParkedTaskMoveRequest.Same`). The
  signet move receipt renders the record with that agent, so the schema
  and goldens don't change.
- **The passed-over mark still names an agent.** `TaskLinePassedOverMark.Agent`
  is a provenance field the API pins; the failed binding's agents are
  found through the tree as before (`ranFailedBinding`).
- **The re-gate refuses the failed credential first.** A card's selection
  is refused when it runs the failed enrollment at the failed generation,
  before the rule is re-derived, because a selection names no agent to
  compare.

## Refute-First Findings

A fresh-context reviewer was asked to refute the change.

- **Confirmed and fixed: the task timeline leaked enrollment offers.**
  `GetTaskTimeline` returned each run's hold as stored, so the wire showed
  enrollment ids where the schema promises agents. Signet now renders each
  hold as the run timeline does. The integration test
  (`driveParkedImplementer`) checks both timelines show the agent.
- **Confirmed and fixed: the counterpart skip covered v1 move lines.** A
  pre-#2735 escalated specifier move line skipped the counterpart check, so
  a lineup edit no longer refused it. The skip now applies only when the
  cited record is v2, whose anchor holds the line to its lineup tuple
  (`TestOnlyAV2MoveSkipsTheCounterpartCheck`, which fails on the old code).
- **Confirmed and fixed: a deleted enrollment read as not found.**
  Acceptance resolved the choice before it requalified the offer, so a
  removed enrollment returned a 404. It now requalifies first and refuses
  the offer as no longer qualified.
- **Allowed by decision: a replay after a tree change conflicts.** A
  replayed command whose translation changed returns a conflict, not the
  first result. It fails closed, and the first decision stands.
- **Allowed by decision: the window check reads the lineup model.** It
  grants nothing; an attempt on a closed window parks or fails again.
- **Allowed by decision: the no-catalog rule holds only at acceptance.**
  It matters only if the lineup changes between acceptance and admission,
  and the selection is still the operator's choice of an offered
  credential.
- **Disproved by a check.** The v1 goldens match `main`'s bytes (`cmp`); no
  signet golden or API schema changed; the `ResolveAgentSelection` refactor
  is equivalent; no path echoes operator text; no move pins an enrollment
  the policy doesn't order; admission refuses the failed credential and
  another pool; v1 records decode; the receipt's kinded form carries no
  `selection`.
