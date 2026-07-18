# Finding-remediation spine contract prerequisites: filed and serialized

Spine coordination, 2026-07-18. The Wave 1 tracking issue #83 carries a
"Finding remediation lanes" comment that maps the adversarial-review
findings to lanes and names three `kind:contract lane:spine` prerequisites
**by description only, with no issue numbers**. Their consumer fixes (#164,
#166, #167, #168, #169, #170) already existed as open `kind:fix` issues, so
those consumers were blocked on contracts that did not exist as trackable,
ordered work. This note records filing the three contracts and serializing
them, and the owner choices behind it. Status lives on the issues and #83,
never here.

Filed:

- **#172** — automation trust profile + candidate authorization (trust
  profile / audit digest §5.5, immutable candidate authorization §5.6, and
  the §5.8 control-plane `FindingKind` class); blocks #166, #168, #169.
- **#173** — evidence-manifest / helper-in-image interface (second §5.6
  workspace-exit channel and the fixed helper interface); blocks #167, #170.
- **#171** — persist an authoritative decision/acceptance timestamp; blocks
  #164.

## Decisions and rejected alternatives

- **Adopt the comment's three-contract decomposition.** The three group by
  shared shape, not by consumer: #172 bundles trust profile, candidate
  authorization, and the control-plane finding class because #166/#168/#169
  all bind to the same authorization/trust surface; splitting per consumer
  would fragment one contract across three exclusive units. Rejected:
  re-deriving a different grouping from scratch (the comment's framing is
  the spine author's and holds up against the consumer bodies).

- **Chain order #172 → #173 → #171, appended after the current tail #107.**
  There is no semantic dependency among the three contracts; the order is
  serialization discipline under the contract-chain rule (at most one
  claimable at a time). #172 goes first because it unblocks the earliest
  consumers (pass A: #166, #169); #173 unblocks pass B/C (#167, #170); #171
  unblocks #164, which is pass B and already serialized behind #165, so it
  can come last. Rejected: ordering by contract "size" or filing order,
  which would delay the earliest lane work.

- **Serialize but do not schedule (no milestone).** The contracts are
  recorded in #83's chain and the comment graph and dormant until a spine
  scheduling sweep. This matches the consumers' current unmilestoned state
  and deliberately keeps the self-selection door closed (per AGENTS.md,
  milestone + tracking listing is what opens it). Rejected: scheduling into
  1A now, which would make the contracts claimable before the owner runs a
  sweep and before the consumers are scheduled.

- **#171 is a contract unit, not a #22 on-demand widening.** It needs a new
  persisted field plus a migration and replay semantics, not a provisional
  vocabulary widening, so it takes its own `kind:contract` unit and migration
  rather than riding the #22 mechanism.

## Contract-B (#172) sizing caveat

#172 is the heaviest of the three (trust profile + audit digest +
candidate authorization + control-plane finding class). If it proves too
large for one exclusive contract unit at claim time, it may split into a
"trust profile / digest" contract and a "candidate authorization +
control-plane finding" contract, kept serialized in the same chain slot.
Recorded so the split is a known option for the claiming spine session, not
a surprise.

## Revisit when

- A spine scheduling sweep runs: assign milestones and list the contracts
  (and their consumers) per the normal scheduling operation; this note's
  "not scheduled" statement is superseded at that point.
- #172 is claimed and the implementer judges the split above necessary.
