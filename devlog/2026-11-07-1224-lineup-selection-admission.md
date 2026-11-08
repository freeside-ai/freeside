# Admit the Lineup's Selection, Not an Agent

Issue #2734; source note `2026-11-05-1035-select-by-enrollment-model-effort.md`
(#2593). Tier A: admission, the wardless admission, and the binding's
recheck change.

Chose a lineup line that names `<enrollment>/<model>/<effort>/<prompt>@<digest>`
and an admission that resolves it against the store and the enrollment's
catalog, over keeping agent names on the admission path behind a bridge,
because a line that carries a tuple cannot be admitted by code that
resolves a name. The agent documents and offers stay in the tree for the
wire (#2598), the routing policy (#2736), and the alternate-agent record
(#2735); admission reads none of them.

## Decisions

- **The escalated key names the enrollment verbatim.**
  `lineup.role.specifier.escalated.<enrollment-id>`, with the id's `/`
  kept. Rejected: an ordinal key, because a project lineup that overrides
  `escalated.1` could replace another enrollment's line; and an id-to-name
  encoding, because identity ids have no character rule to encode from. A
  policy key has no character rule beyond non-empty, and the lineup file
  splits at whitespace, so the slash is safe. Escalation changes the model
  and effort, never the credential.
- **A new binding's agent digest is its selection digest.** The plan's
  record of a past selection needs a digest, and the selection is what was
  admitted. A binding recorded under an agent digest keeps its bytes and
  id and is never re-resolved. Its agent closure recheck stays, with its
  tests, but no production path runs it any more: it only ever ran inside
  the admission that recorded the binding, and every new admission is
  under a selection. #2599 removes it with the agent documents.
- **The binding drops the pricing revision and the requested effort.** No
  offer is read, so there is no pricing revision or offer deadline to
  record or check, and a selection names a harness effort string with no
  Freeside level to request. `pricing_revision` stays optional on decode.
  The native effort still records what the launch sends.
- **A usage observation with no pricing revision records `none`.**
  `usage_observations.pricing_revision` is `NOT NULL CHECK (<> '')`
  (migrations 0061 and 0102), and a table rebuild is a data-rewriting
  migration this unit does not need. Rejected: an empty string, which the
  check refuses. Revisit when a migration next rebuilds that table.
- **Lineage derives in code.** `domain.LineageGroupOf` maps `claude_code`
  with a `claude-` model to `anthropic` and `codex_cli` with a `gpt-` or
  `codex-` model to `openai`, and leaves every other pair unknown. Rejected: reading
  the offer's lineage group, which puts an agent back on the path.
  Unknown stays unknown, so the independence check treats it as it treats
  any unknown lineage.
- **Agents that reduce to one selection are one agent.** The bridge
  (`AgentForSelection`) used to refuse two tree agents on one selection as
  ambiguous. Baseline twins are one definition under two names, so the
  bridge now admits them, and it compares only agents whose adapter fits
  the role's launch shape, so a ward agent and a call agent on one model
  never compete.
- **A judgment call picks the one adapter that sends its effort.**
  `SelectAdapterForCall` takes the client's adapters with no read or
  mutation tools that can send the line's effort, and refuses none or more
  than one. A call is still admitted only under an audit that names that
  adapter's build. Both call adapters send `harness_default`, so a line at
  that effort is unbound while a tree holds both. Rejected: preferring the
  newer adapter, because no field orders adapters and an unordered choice
  is the one admission must never leave to the tree. The role fails closed
  with its system_health item, and the upgrade steps name the fix.
- **Prelaunch resolves a task line through the store when it can.**
  Prelaunch runs before the deputy's start adopts the lineup's
  enrollments, so a fresh root's store holds none. A line on an enrollment
  the store holds is resolved as admission resolves it; one the store
  lacks passes only when the lineup names its enrollment. Rejected: a
  store-only check, which fails every fresh root; and a lineup-only check,
  which passes an effort or model the enrollment cannot run.

## Checks This Retires

Each was a check on an agent that no selection has:

- The offer-deadline refusal: no offer is read. `AdmissionCheckAgentOfferDeadline`
  stays in `AdmissionCheck` because stored refusals carry it.
- `engine.ResolveAgent` by agent name: preflight's second-subscription
  check reads the second enrollment's generation and mark directly.
- The twin-ambiguity refusal: twins admit (above).
- The stale escalated-digest refusals: an escalated line names a model,
  not an agent digest, so a triage line is now refused when it selects
  other than the lineup's escalated counterpart on its enrollment.

## Refute-First Findings

A fresh-context reviewer tried to refute the diff before commit. It found
no path that runs a role on an enrollment other than its line's, changes
the credential on escalation, or prints hidden operator text, and it
confirmed by decoding the four pre-change goldens that an old binding keeps
its id.

- **Confirmed, fixed:** preflight resolved a writer line without picking
  its adapter, so an effort no adapter sends passed preflight. It now picks
  the adapter and resolves the selection.
- **Confirmed, fixed:** no fixture pinned a pre-change binding's bytes, and
  nothing tested the refusal of a selection binding carrying a pricing
  revision or requested effort. Both are now tested.
- **Confirmed, allowed by decision:** a judgment line at `harness_default`
  is unbound while a tree holds both call adapters (above). The docs now
  say so.
- **Confirmed, fixed:** the docs said a legacy binding's recheck still
  runs; they now say it is never re-resolved.
- **Plausible, declined here:** a task line on a catalog alias can be
  offered an alternate running the same model under its full id. The
  alternate's pool and generation checks still apply, and #2735 replaces
  that record's offers.
