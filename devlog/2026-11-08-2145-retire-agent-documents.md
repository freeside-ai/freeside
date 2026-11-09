# Retire Agent and Offer Documents (#2599)

The agent tree now holds routes, adapters, an optional per-route model list,
the lineup, and a lock over the fragments. Agent documents, offer fragments,
and their lock lines are refused, with `freesided auth migrate-tree` as the
fix. This note records the contract choices the plan settled and the ones
implementation added. The owner can veto any of them.

## Contract Choices From the Plan

- **The API loses `AgentCatalog.agents` in this unit.** We considered a
  separate wire unit and rejected it: a contract PR carries its required
  consumers, and the app wouldn't compile against a client whose required
  field vanished.
- **The list lives at `models/<route-name>`, not under
  `fragments/routes/<route>/models`.** The issue proposed the second path,
  but it can't exist beside the route file `fragments/routes/<route>`. The
  list is operator text outside `agents.lock`, like `lineup`. The tree
  revision covers it, but the lock doesn't.
- **Retired records refuse or order nothing; we don't migrate them.** This
  covers v1 task lines, v1 alternate-agent retry records, and agents-form
  routing revisions. A store migration can't read the tree, and nothing
  writes those forms any more. The encodings still decode, and admission
  refuses each one by record id. An agents-form revision orders no
  enrollment, so the router falls back to the lineup.
- **`migrate-tree` rewrites no lineup line.** Lineup lines have held
  selections since #2734. The command deletes the agent and offer files and
  their lock lines and nothing else, so the patch is easy to review.

## Choices Made During Implementation

- **The adopt flags `-pricing-revision` and `-offer-not-after` are accepted
  and ignored.** The issue said adopt keeps its flags. Deleting them would
  break the deputy, which passes its `agent_tree.pricing_revision` and
  `agent_tree.offer_not_after` settings through to adopt. Removing the flags
  and settings together is a follow-up (#2916).
- **The app's undiscovered-enrollment fallback reads the lineup instead of
  being deleted.** The issue listed the fallback for removal. With no
  fallback, an enrollment the daemon hasn't discovered offers no model. The
  lineup's own selection on such an enrollment would then read as a stale
  choice ("no longer offers any model"), though the daemon admits it. The
  lineup is the one set of selections the tree still carries, so it takes
  the agents' place.
- **The Agents pane flags an agents-form order instead of hiding it.** Such
  an order now orders nothing. The pane used to flag only names the catalog
  lacked; it now flags every agent name ("Agent names order nothing; choose
  enrollments"). An operator can then see why the role falls back to the
  lineup. A first edit starts the role unordered instead of translating
  names through catalog agents.
- **The review-admission recheck no longer prints model names.** While
  converting its tests to v2 selections, I found the recheck's refusal in
  `cmd/freesided/agent_cutover.go` printing the line's model and effort, and
  the admitted snapshot's. A v2 line's model is operator text and can hold a
  pasted credential. The refusal now prints `RoleAgent.Describe()`: the
  lineup's text, or an operator selection only after a recorded catalog
  matched it. This leak predates #2599; the test conversion exposed it.

- **The app drops a cached catalog that still carries `agents`.** The
  generated decoder rejects unknown keys, so every catalog cached before
  this unit would have thrown and discarded the whole cache, the command
  ledger included. The load path drops only the catalog, and the next
  heartbeat restores it. The cache format stays 12, because a missing
  catalog already triggers its own bootstrap.

## Refute-First Review

A fresh-context reviewer tried to refute the diff. Each finding and its
outcome:

- **Confirmed, fixed:** the cache discard above. Also: the account-binding
  refusal lost its only test on a live resolution path when the agent
  tests went, so `TestResolveAgentSelectionRefusals` gained the case. And
  the v1 card refusal now tells the operator to choose again, as the issue
  asked.
- **Allowed by the contract:** the list matches the id as the line names
  it, so a deny list naming `claude-opus-5-5` does not stop a line naming
  the alias `opus`. The issue settled this, and both READMEs say it. The
  owner can revisit it.
- **Allowed by the contract, follow-up #2917:** the router, the policy set
  check, and alternate-agent offers ignore the list. The list binds only
  at admission, so a route that excludes the lineup's model gets its task
  refused, not passed over. It fails closed, but it costs availability.
- **Allowed:** a v1 router or move line refuses with "not ordered", not the
  v1-line message, because the move anchor's check runs first. It still
  refuses by record id.
- **Deferred:** the Settings summary still shows an agents-form order's
  names as the order (#2918; outside this unit's paths). The admission
  recheck's capability, credential-mode, and vendor legs have no test; that
  predates this unit (#2919).

## Revisit When

- The route model list must also filter what clients offer. Today it binds
  only at admission (a non-goal here).
- A later contract unit drops the v1 task-line, v1 retry, and agents-form
  routing encodings from `domain/` and `store/`.
