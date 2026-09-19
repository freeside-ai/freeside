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
confirming them one by one. The revision proposes plan text and the owner
decides through the PR review.

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
   the same way as a missing line. It departs from the position in one place,
   the item's posture, described below.
4. **"Not choosable"** (§5.13, bounded choice). Follows the position.
5. **Review independence over roles** (§5.4, §7). The discussion left this
   open; see the next section.
6. **The call record** (§5.4, §8). Follows the position, and adds the lineup
   revision and enrollment generation so a call's admission can be read back.
   The call launch's digest takes the launch position in a call's treatment
   digest. It is one per proved call launch version and the same at every
   site, so the site stays a separate comparison key.
7. **The judgment credential** (§5.4 Admission). Follows the position. The
   borrowed shadow-review token ends at the §5.4 cutover.
8. **Text revision 64 left pointing at #900** (§5.13). Both sentences now
   state the decision, and the attention-discussion role joins the list.

## Where the Text Goes Beyond the Discussion

- **The independence rule is one rule over writing and judging roles.** Chose
  "each judging role (reviewer, adjudicator, drift auditor) differs from each
  writing role (implementer, remediator)" over also requiring the judging
  roles to differ from each other. The error directions differ: an adjudicator
  sharing the implementer's lineage errs toward declining a valid finding,
  which loses a defect, while one sharing the reviewer's lineage errs toward
  accepting a finding, which costs a bounded remediation round. Requiring all
  three to differ needs three lineage groups, and a two-vendor deployment has
  two. §7 already said the drift audit's lineage differs from the implementing
  agent's, so this matches the plan's existing intent. The shadow reviewer is
  outside the rule because its findings never route.
- **The baseline needs a relaxation at first.** The only call driver today is
  Claude's, and Claude implements. Until a second call adapter is admitted, a
  project running the baseline needs the stated relaxation for the adjudicator
  and drift-auditor pairs. The plan says so instead of leaving the default
  rule quietly unmet. Wave 8's drift-audit site landing on the shared judgment
  model was already accepted in the discussion.
- **A split implementer and remediator forces a relaxation** in a two-vendor
  deployment, because the judging roles then have no lineage group left.
- **A call has no gate before first use, and the plan says so.** The position
  was that the sampled audit does the attended first run's job. It does not:
  the attended run gates use, and an audit is read afterwards, if at all. Chose
  to state that the site's authority contract, ceilings, and fail-safe are
  what bound a bad new agent, with the audit as the look after the fact and a
  shadow run as the optional look before. Requiring shadow-first for
  ceiling-bounded annotation sites was considered and left to the owner.
- **Sampled audit on every site.** §5.13 required it for untrusted-input sites
  only. A trusted-input site without one would give the operator no look at
  all, so the rule widens to every site.
- **The store contract on the host.** Stage admission's store contract is
  enforced by a ward mount and the egress proxy, and the host has neither. A
  Codex or pi call harness that refreshed its own OAuth token would break the
  identity lease. So the daemon keeps the store and refresh to itself, hands
  the call one credential for one route, and the call launch proof covers
  "touches no credential store" and "sends no non-inference traffic".
- **A missing line is a standing fault, not an outage.** A fail-safe is written
  for an outage. Where it carries on without a judgment that policy switched
  on (the drift audit continues as if off), a missing line would disable that
  judgment for good, so the `system_health` item is `blocking` there. It is
  `advisory` where the fail-safe already hands the work to a human or a
  conservative default, and for advisory-only roles. The position said only
  "warns loudly".
- **Off is not unbound.** A role needs a line only while policy asks for its
  work. Otherwise a deployment with no shadow reviewer line would block
  review, and the unbuilt briefer would raise a standing item.
- **A relaxation never loosens the second-adjudication ceiling.** §7 lets a
  critical or high decline pass on a second adjudication "from a distinct
  agent". No role supplies one, and the code routes the requirement to
  attention. The plan now says the ceiling is met deterministically or by an
  AttentionItem, and that an agent second adjudicator would be a new role.
  Without this, a relaxed baseline could read as two Claude-lineage calls
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
  primary off. How each bound is partitioned is #1429's. An
  earlier draft listed layers and claimed a shadow "can never" push a primary
  to its fail-safe; review found a missing layer twice (the shared project and
  global windows, then the identity's budget), so the list went. Residual: a
  vendor's usage pool is outside Freeside's metering, so a shadow sharing its
  primary's usage pool still draws on it, and isolation there means a shadow
  agent on a different usage pool (a second enrollment on the same identity
  shares the pool). Chose separate bounding over shared windows
  with a primary reserve. Rejected: requiring a different identity for every
  shadow, because it forbids the prompt-only shadow on one account. Rejected:
  a bare priority statement, because a shadow could still take the last shared
  allowance. This is the agent's proposal from review, not an owner decision,
  and it departs from the implementation plan's "counts against the site's
  budget".
- **At most one shadow line per wardless role.** §5.4 now says a wardless
  role may carry one optional shadow line beside its primary line, overridden
  per role by a project lineup and never a fallback for a missing primary
  line. The encoding is the lineup contract's (#1421). One is the smallest
  rule that makes the shadow line keyable; the owner may widen it.
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
- **Relaxing independence in the deployment lineup.** Not adopted: the
  existing knob is a project lineup's, and this revision leaves it there.

## Revisit When

- A role reaches hundreds of runs a month, or a second operator joins:
  reconsider randomized arms.
- Several roles each hold three or more standing prompts: reconsider a prompt
  folder per role.
- A third lineage group is routinely available: reconsider whether the
  adjudicator should also differ from the reviewer.
- A harness cannot switch off everything the call launch requires: that
  harness gets no call adapter; do not weaken the launch to fit it.
- A third or fourth effect kind lands, or outcome-by-role numbers cannot be
  joined: reconsider one record for every external action.
