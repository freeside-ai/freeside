# Every Agent Activity Is a Lineup Role

Work unit: #900. Plan revision: 65.

The plan's per-role experimentation goal covered every agent kind §5.13 lists,
and the mechanism covered three: a lineup mapped the specification,
implementation, and review stages to agents. It could not tell the implementer
from the remediator, and the daemon's judgment calls sat outside it on one
deployment-pinned binding with prompts held as Go strings. Revision 64 made the
publication author a lineup role and left the rest to #900. Revision 65 answers
#900's question with yes, for every judgment site.

This unit changes plan text and no code. It carries `kind:contract` because it
heads the agent-vocabulary chain that #1421 continues, and contract units
serialize. Owner decision (2026-09-19): `kind:contract` came off #1415, a
plan-only unit, so that this unit could start, as was done for #1414.

## Owner Decisions

The owner decided these in the design discussion recorded on #900 (comment
5742701055). The revision records them in §5.4, §5.13, and §13.

1. **One role per agent activity.** Chose a lineup line per role, mapping it to
   an agent by digest, over a harness or model setting beside the agent. The
   agent document already names the adapter, so a separate harness setting
   would be a second source of truth for the same fact.
2. **Roles sit above stages and sites.** Chose a role layer over folding sites
   into stages. A site's authority contract is what bounds a model answer, and
   a stage has no such contract to give. A stage may hold several roles and a
   role may span several sites.
3. **The lineup lists every role explicitly.** Chose explicit lines, with the
   deployment lineup as the defaults, over fallback from one role's line to
   another's. A fallback chain hides which agent ran a role and makes a
   before-and-after comparison ambiguous. The role list is closed and grows by
   plan revision.
4. **Each role has its own prompt, by digest.** Chose a second named prompt
   over per-model or per-harness overlays. An overlay makes one prompt name
   mean different text under different agents, which breaks the prompt digest
   as a comparison key. A ward role's harness-specific text already has a
   carrier, the vendor instructions, and a call prompt that must differ by
   harness is a second named prompt. This answers the question #989 parked
   behind #900.
5. **Experiments are paired, not randomized.** Chose shadow calls and
   before-and-after comparison over percentage and cohort arms. At a few dozen
   tasks a week a random split cannot separate two options; a paired
   comparison on identical input can.

## Settled Items

The discussion reached a position on each of these without the owner
confirming them one by one. The plan text follows each position except where
an item says otherwise, and merging the revision is the owner's decision on
them.

1. **Two launch shapes, split on what the agent can touch** (§5.4 Roles and
   Launch Shapes). Follows the position. The plan adds the reason one runtime
   cannot serve both: a tool-using role on the host is unsandboxed, and a call
   in the ward gains nothing the no-tools proof lacks while tying the daemon's
   fail-safe behaviour to ward availability.
2. **The no-tools proof is the call launch, proved per adapter build** (§5.4
   Admission). Follows the position. A wardless role uses Resolve, Selected,
   Credentialed, and Snapshot as stage admission does; Proved drops runner
   conformance and requires the call launch in the adapter's conformance
   record. The Claude driver's hand audit stays as an interim so today's sites
   keep running. It covers only the audited harness build, ends when the
   adapter carries the record, and admits no second driver.
3. **An unbound role** (§5.4 Admission). Follows the position, and names the
   loud warning: a `system_health` item. A failed wardless admission behaves
   the same way as a missing line. The item's posture is left to #1425,
   described below.
4. **"Not choosable"** (§5.13, bounded choice). Follows the position.
5. **Review independence over roles** (§5.4, §7). The discussion left this
   open, and the owner decided it in review; see the next section.
6. **The call record** (§5.4, §8). Follows the position, and adds the lineup
   revision and enrollment generation so a call's admission can be read back.
   The call launch's digest takes the launch position in a call's treatment
   digest. It is one per proved call launch version and the same at every
   site, so the site stays a separate comparison key.
7. **The judgment credential** (§5.4 Admission). Follows the position. The
   borrowed shadow-review token ends at the §5.4 cutover.
8. **Text revision 64 left pointing at #900** (§5.13). Both sentences now
   state the decision, and the attention-discussion role joins the list.

## Owner Decisions in Review

The owner decided these on 2026-09-19.

6. **Review independence is a recorded fact, never an admission gate.** Chose
   "any combination of providers may serve any combination of roles, the
   implementer and the reviewer included" over a lineage rule that blocks,
   because Freeside has to keep working when one provider is out of usage or
   down. This supersedes the earlier default from the admitted-agent contract:
   the review offer's lineage group had to differ from the implementation
   offer's unless a project lineup relaxed it with a stated reason, and unknown
   lineage failed closed. The condition that changed is provider availability:
   with two vendors and usage limits, a gate on pairing turns one provider's
   outage into a stopped deployment. Extending that gate to the adjudicator
   and the drift auditor would also have forced a standing relaxation on the
   baseline, where Claude is the only call driver and also implements. What
   stays: each judging role's pairing with the writing roles
   shows on every card and record, so a same-lineage review is visible, and the
   plan keeps the advice that a judge sharing the writers' lineage is the
   pairing most likely to lose a defect.
7. **A shadow has its own allowance.** The owner confirmed the shadow-budget
   rule below. It departs from the implementation plan's "counts against the
   site's budget".
8. **Any agent may shadow any other.** A shadow may differ from its primary in
   model, effort level, harness, or provider, from the same lineage or not.
   Comparing those is what a shadow is for, and no independence rule reads a
   shadow's agent.
9. **The retry card can change a call's agent.** Chose letting the Section 4
   alternate-agent card's recorded choice select a judgment call's agent for an
   attempt over limiting the card to stage attempts, because a short quota or
   credential outage should not force a standing lineup edit. §4 and the Wave 9
   exit already read that way.

The owner left these choices to the agent's judgement (2026-09-19) and may
change them:

- **A lineup line is required only for a role that policy asks work from.**
  Chose that over an explicit "off" entry for every unused role, because it
  follows from "off is not unbound" and adds nothing to the lineup format. The
  cutover counts the same set of roles.
- **The retry card selects an agent only.** Chose "the lineup line's prompt
  still runs" over a card that binds agent and prompt, because prompts are
  named by role with no per-harness overlays, and binding a prompt would widen
  the §4 card contract. Revisit when a role really needs a harness-specific
  call prompt and the card is used to cross harnesses for it.

One residual is stated and left open for the owner:

- **A retained host administrator policy is outside the call launch proof.**
  The call launch says "no user or host configuration", yet Claude Code still
  loads its managed settings under the safe-mode flags, and the proof is per
  build, not per host, so it cannot see a file on one operator's machine. The
  plan states that residual and settles no mechanism; #1424 carries it. Two
  options, neither taken: digest the file, or its absence, into each call
  record, which cannot bind what the harness reads, because on the host the
  harness opens the live path itself and §5.8's snapshot-and-materialize has
  no ward to rely on; or require the file to be absent, which costs an
  operator who needs that policy for other uses of the harness on the same
  machine. Revisit when #1424 is scheduled.

## Where the Text Goes Beyond the Discussion

- **A call has no gate before first use, and the plan says so.** The position
  was that the sampled audit does the attended first run's job. It does not:
  the attended run gates use, and an audit is read afterwards, if at all. Chose
  to state that the site's authority contract, ceilings, and fail-safe are
  what bound a bad new agent, with the audit, where a site carries one, as the
  look after the fact and a shadow run as the optional look before. Requiring
  shadow-first for ceiling-bounded annotation sites was considered and left to
  the owner.
- **The sampled audit stays on untrusted-input sites.** §5.13 requires it for
  those sites only, and this revision leaves that rule alone. Widening it to
  every site, so that a trusted-input site also gives the operator a look
  after the fact, is new policy; it was considered and left to the owner.
- **The store contract on the host.** Stage admission's store contract is
  enforced by a ward mount and the egress proxy, and the host has neither. A
  Codex or pi call harness that refreshed its own OAuth token would break the
  identity lease. So the daemon keeps the store and refresh to itself, hands
  the call one credential for one route, and the call launch proof covers
  "touches no credential store" and "sends no non-inference traffic".
- **The unbound-role item's posture is left to #1425.** The position said only
  "warns loudly", and the plan names the `system_health` item without fixing
  its posture. The option considered and not written into the plan: a
  fail-safe is written for an outage and a missing line is a standing fault,
  so the item would be `blocking` where the fail-safe carries on without a
  judgment that policy switched on (the drift audit continues as if off), and
  `advisory` where the fail-safe already hands the work to a human or a
  conservative default, and for advisory-only roles.
- **Off is not unbound.** A role needs a line only while policy asks for its
  work. Otherwise a deployment with no shadow reviewer line would block
  review, and the unbuilt briefer would raise a standing item.
- **Same-lineage pairing never loosens the second-adjudication ceiling.** §7
  lets a critical or high decline pass on a second adjudication "from a distinct
  agent". No role supplies one, and the code routes the requirement to
  attention. The plan now says the ceiling is met deterministically or by an
  AttentionItem, and that an agent second adjudicator would be a new role.
  Without this, a same-lineage lineup could read as two Claude-lineage calls
  declining a critical finding.
- **A shadow doubles exposure and must not starve its primary.** A shadow
  sends the site's allowlisted fields to a second route, so the site's
  sensitivity and redaction rules bind that route. The plan states a
  requirement, not a list of budget layers: a shadow must never cost a primary
  call its answer, so a shadow's draw on every bound Freeside itself meters is
  kept apart from its primary's, and a shadow that cannot be admitted or is
  short of its own allowance is skipped, never the primary. A shadow line's
  failure never makes its role unbound; without that sentence, "admitted like
  any call" plus the unbound-role rule read as a bad shadow line switching the
  primary off. How each bound is partitioned is #1429's. Rejected: listing the
  budget layers and claiming a shadow "can never" push a primary to its
  fail-safe, because an enumerated list is easy to leave incomplete (the shared
  project and global windows and the identity's budget were each missed once).
  Residual: a
  vendor's usage pool is outside Freeside's metering, so a shadow sharing its
  primary's usage pool still draws on it, and isolation there means a shadow
  agent on a different usage pool (a second enrollment on the same identity
  shares the pool). Chose separate bounding over shared windows
  with a primary reserve. Rejected: requiring a different identity for every
  shadow, because it forbids the prompt-only shadow on one account. Rejected:
  a bare priority statement, because a shadow could still take the last shared
  allowance. The owner confirmed this rule (decision 7 above).
- **At most one shadow line per wardless role.** §5.4 now says a wardless
  role may carry one optional shadow line beside its primary line, overridden
  per role by a project lineup and never a fallback for a missing primary
  line. The encoding is the lineup contract's (#1421). The owner chose one for
  now (2026-09-19). It is a cost limit, not a structural one: each shadow call
  links to its primary and draws on the shadow allowance, so allowing several
  later changes no record, and #1421 keys the shadow line so more can be added
  without migrating a lineup.
- **A site's fixed instruction is not an overlay.** Revision 64 lets each
  publication-author site be told its output shape by a short fixed per-site
  instruction. That instruction is the daemon's, belongs to the site, and is
  the same under every agent and lineup line, so the prompt digest stays a
  valid comparison key. Making the selector a digest-bound part of the role
  prompt was the option not taken.
- **The call-shadow rule leaves the shadow reviewer alone.** The shadow
  reviewer is a ward role under §7: its findings never route, and a credible
  critical or high shadow finding still blocks ready status.

## Rejected Options

- **One runtime for every role.** Rejected: the two shapes are safe for
  opposite reasons, and neither runtime serves the other's roles.
- **Folding sites into stages.** Rejected: it would drop the per-site
  authority contract that bounds each answer.
- **Harness as a setting apart from the agent.** Rejected: the agent document
  already pins the adapter and its harness build.
- **Fallback from one role's lineup line to another's.** Rejected: it hides
  what ran and blurs comparison.
- **Per-model or per-harness prompt overlays.** Rejected: see decision 4.
- **Percentage or cohort arms.** Rejected: see decision 5. §5.4 keeps "stored
  projections beyond the treatment digest" unbuilt, and arms would reverse it.
- **A hand audit as the standing no-tools proof.** Rejected: it does not
  survive a harness update, and it cannot scale to Codex and pi.
- **The sampled audit as a replacement gate for the attended first run.**
  Rejected: it gates nothing, and calling it a proof would overstate it.
- **Review independence as an admission gate.** Rejected by the owner
  (decision 6): it stops the deployment when one provider is unavailable.

## Revisit When

- A role reaches hundreds of runs a month, or a second operator joins:
  reconsider randomized arms.
- Several roles each hold three or more standing prompts: reconsider a prompt
  folder per role.
- The records show same-lineage review missing defects that a different
  lineage catches: reconsider a warning on the card, not a gate.
- One shadow per role cannot separate the options being compared: raise the
  limit by plan revision.
- A harness cannot switch off everything the call launch requires: that
  harness gets no call adapter; do not weaken the launch to fit it.
- A third or fourth effect kind lands, or outcome-by-role numbers cannot be
  joined: reconsider one record for every external action.
