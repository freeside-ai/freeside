# Select by Enrollment, Model, and Effort

Issue #2593; plan revision 111; tracker #2603. The owner decided on
2026-11-04 that an agent selection is the tuple `{enrollment, model,
effort}`, chosen ad hoc by an authenticated operator, and that the committed
tree keeps adapter pins per client kind, a route's terms basis, and an
optional allow or deny list over discovered models.

Chose a selection tuple resolved at admission over the named agent document
of revision 39, because the document made every new combination expensive,
which defeats the reason it existed.

## What This Overturns

Revision 39 (`2026-08-23-0825-admitted-agents.md`) chose "one atomic agent
document over per-axis policy keys", so that no join ran that nobody
reviewed. An agent was four reviewed lines (enrollment, route, adapter,
offer and effort), and an offer was an authored fragment naming one model
on one route with its lineage group, `identity_stability`, allowed efforts,
and `not_after`. Revision 111 removes the agent document and the offer
fragment as selection units.

## What Changed

Revision 39's goal was to mix models, efforts, and accounts cheaply enough
to learn which does which job well. It assumed a reviewed document per
combination would be cheap. It was not:

- **A new combination cost six steps.** An offer fragment, an agent
  document, two `agents.lock` entries, a lineup line, a commit, and a daemon
  restart, because the tree loads once at startup
  (`2026-11-03-0905-agent-catalog-contract.md`).
- **The code already worked around it.** `freesided auth adopt` mints one
  agent per distinct model and effort pair (`policy/README.md`, "A writer
  role takes its model and effort from its line").
- **The review added little.** Resolution already validates the join
  mechanically: the enrollment's route and client kind must match the
  adapter, and the effort must be one the offer allows. What the reviewed
  document added on top was a human having committed a tuple the operator
  then picks per task anyway.

## What Is Kept

- **Admission by digest.** Admission still canonicalizes and hashes what
  runs, so run records, the treatment digest, and the comparison keys of
  shadow and wardless roles keep a digest to key on.
- **The credential stays the operator's choice.** Only an authenticated
  operator chooses an enrollment. Intake from an issue, a label, or
  repository content still sets nothing, and a model's answer still never
  picks a credential.
- **The reviewed tree, for what is authority.** Adapter pins (the harness
  build Freeside runs), a route's terms basis, and the model allow or deny
  list stay operator-authored configuration, reviewed as a diff.
- **Records are never upgraded through current configuration.** A past
  selection keeps the digest it was admitted under.

## Decisions

- **Chose discovery from the harness over authored offers.** The model and
  effort space comes from each harness (the Claude init record's `models`,
  Codex's `model/list`) and is stored as daemon state, a record, not
  configuration. Rejected: keeping offers as a hand-maintained model
  allowlist, which keeps today's friction. The tree's optional allow or deny
  list is its only say over models.
- **Chose per-model effort levels over the fixed `EffortLevel` enum.** A
  harness reports which levels each model accepts; one enum across vendors
  either drops real levels or invents unsupported ones. The adapter still
  records requested and native values and renders a clamp.
- **Chose to keep the lineup in the tree over making a role's default an
  operator setting.** A lineup line maps one role to its prompt and to a
  default tuple written by names (enrollment name, model id, effort level).
  It resolves at admission against the enrollment's discovered catalog and
  the model list, not at tree load, so a line whose model the harness no
  longer reports fails that role's admission and raises the ordinary card,
  as a task line that no longer resolves does today. Rejected: an
  operator-set default per role, because the prompt is control-plane text
  and its pairing with a role belongs with it in the reviewed tree, and
  because #2603's exit proof ("a model and effort the lineup never named")
  assumes the lineup still names one.
- **Chose to derive lineage from the enrollment and the model over a lineage
  field somewhere else.** Review independence (plan Section 7) read the
  offer's lineage group. It now derives the group per vendor family from the
  enrollment's client kind and the model id, curated conservatively in code
  as before; unknown stays unknown. Rejected: a lineage entry in the tree per
  model, which brings back a hand-maintained list per discovered model.
- **Chose to drop `identity_stability` and `not_after` with the offer over
  moving them to the model list.** An observed model, operator, or route
  that contradicts the admitted tuple still fails the attempt as a durable
  contradiction, which is what bounds a rolling upstream. A model the harness
  stops reporting leaves the catalog at the next discovery and is withdrawn
  from new selection, which is what an authored `not_after` did by hand.
  Rejected: an authored expiry per model, a second list to keep in step with
  the harness.
- **Chose a digest over the resolved tuple and its pin.** The selection
  digest covers the resolved enrollment id, the model id, the effort level,
  and the adapter pin the enrollment's client kind resolves to, so a changed
  pin still shows as a changed digest. Rejected: hashing names, which a
  rename would change without changing what runs, and leaving the pin out,
  which would let a harness build change under an unchanged digest. The
  route stays out: the enrollment fixes its route, the admission snapshot
  already records the effective egress allowlist its endpoints produce, and
  a terms-basis edit changes nothing that runs. The body's exact shape is
  #2596's contract.
- **Chose daemon-run discovery over reading a ward run's report.** A ward
  run's output is untrusted agent output, so the catalog comes only from a
  harness the daemon launches for discovery and is validated when written.
  A forged entry would at worst offer the operator a model the provider then
  refuses, but the catalog bounds what admission accepts, so it stays out of
  the agent's reach.
- **Chose to have the routing policy order enrollments over agents.** The
  policy orders, per role, the enrollments the router may choose among, and
  the router-sourced line takes the role's lineup model and effort on the
  chosen enrollment. An enrollment whose catalog cannot serve them is not a
  candidate for that role. This narrows revisions 99 and 107: an order of
  agents could put a different model on each pool, and now a pool whose
  harness lacks the lineup's model is never routed to for that role.
  Rejected: a model and effort per ordered enrollment, because routing
  exists to move a task between credentials when an allowance runs out, and
  a per-pool model would let a usage reading change which model does the
  work; a different model for one task stays the operator's task line or
  card choice. This also settles #2469: the policy's candidates are
  enrolled credentials, not agents the lineup or tree carries.
- **Chose escalated lines keyed by enrollment.** Each escalated line pairs
  one enrollment the specifier can run on with the model and effort it
  escalates to on that same enrollment, so escalation changes only the model
  and effort, never the credential or pool, by construction rather than by a
  check that two agents share an enrollment.
- **Kept the card's name.** The Section 4 alternate-agent retry keeps its
  name, already used by the code and #869; what it selects is now a tuple.

## Prior Art

T3 Code (`pingdotgg/t3code`, commit `dd5f98ee`): a selection is
`{instanceId, model, options}`; models and per-model effort options are
discovered from the harness and merged with a bundled manifest, and a launch
compiles the selection into a `queryIdentity` string. Its hourly manifest
fetch from a remote branch is not adopted, because it would be an unreviewed
remote input to a control-plane decision.

## Revisit When

A harness stops reporting its models or effort levels (then the tree would
need to carry them again for that client kind); the allow or deny list grows
a second use beyond bounding selection; or a vendor exposes two models under
one id whose lineage differs.
