# Follow-Up Filing Effect Kind

Issue #1625 adds `follow_up_filing` as the third member of the closed effect
registry, with the facts a card needs to show one. It is a contract unit:
`daemon/internal/domain`, a migration, the store's proposal gate, the Signet
facts snapshot, `api/openapi.yaml`, and the generated app client move
together. Nothing proposes, files, or renders a filing yet; #1632, #1626,
#1634, and #1633 do that against this contract.

## What a Stale Source Does

**A filing whose source stops backing it stays readable and cannot be
approved.** This departs from the issue's wording and from the plan, so it
is the first thing to check.

The issue says the gate re-reads the source "on every admission and every
reconstruction" and rejects a deferred disposition that "is no longer the
finding's effective one". The plan accepted that a filing would then stop
reconstructing, "as the closure gate does". That premise is wrong: the
closure gate reads only rows that never change, and this check reads state
that ordinary review work moves. The proposal gate also runs inside every
attention-item reconstruction (`gateEvidence`), so one open filing whose
finding a later round fixed made `ListAttentionItems` fail for every item.
A probe showed the whole inbox list returning an error, and the item could
not be read to decline it.

The gate is therefore two checks:

- **Integrity, on every admission and reconstruction.** The target is
  re-derived, the text is re-screened, and the source is re-read: the
  adjudication artifact exists, belongs to the run, routes the finding the
  way the source kind says, and for a deferred source the disposition row
  recorded under that artifact exists. Every row read is append-only, so a
  proposal that passed keeps passing. A mismatch is
  `ErrFollowUpFilingSourceMismatch`.
- **Currency, at admission and at approval.** A deferred disposition must
  still be the finding's effective one, and a separate-work verdict's
  adjudication must still be its round's head revision. A failure is
  `ErrFollowUpFilingSourceStale`.

Rejected: keeping the literal every-read currency check (the outage above);
teaching the item list to skip a row that fails its gate (it would hide a
tampered row, which the list refuses on purpose).

What this leaves for later units:

- **#1632** should close a filing item when its source goes stale. Until it
  does, the card stays open and approve returns the stale error.
- **#1634** must decide whether to re-check currency between approval and
  filing. Nothing here stops an approved filing whose finding is fixed a
  minute later. The check is `gateFollowUpFilingSourceCurrent` in the store;
  exposing it is a store change in that unit.
- **#1626** and #1634 can reconstruct an approved proposal during recovery:
  no read fails because the source moved.

## Decisions

1. **One union with an arm per kind, over a second proposal type.** The
   filing rides `EffectProposal` as a third `omitempty` arm, so the digest,
   the instance table, the item binding, and the decision ledger are shared.
   The four existing proposal goldens are byte-identical, which is the check
   that adding the arm changed no existing digest. Rejected: a separate
   `FollowUpFilingProposal` row family, which would duplicate the admission
   and reconstruction gates the issue wants exercised by a second kind.
2. **The target is derived, never carried.** Repository, labels, and
   milestone come from the project row and the resolved policy
   (`follow_up_filing.labels`, `follow_up_filing.milestone`). A proposal
   whose stored target differs is refused. A present but malformed policy
   value fails closed instead of reading as "no labels": a typo must not
   silently file unlabelled issues.
3. **Repository equality is by ID only, and the card shows the project's
   name.** #1537 keeps project authority across a rename, so comparing the
   name would refuse a valid filing after one. That leaves the stored name
   unchecked, so the facts serve the project's current name instead of the
   stored one: the approver sees where the issue will be filed.
4. **Title and body are screened text with the ruleset named beside them.**
   `github-issue/1` is the `github/1` rules plus three refusals: an `@name`
   token, a line that opens with `/`, and U+2028 or U+2029 anywhere (a line
   break to some raw readers and to none of the screen's line splitting).
   The gate re-screens on every read, so the stored verdict is a record
   and never a trusted bit. Email-like text is refused by the mention rule;
   the false positive is accepted because an issue body that pings a
   stranger is the worse error and the proposer can rewrite the text.
5. **A separate-work source must name its round's head revision.** A
   separate-work verdict writes no disposition row, so nothing else pins
   which revision carried it. The issue only asks for a matching entry;
   without the head check a verdict the adjudicator later amended away
   would still back a new filing. It is a currency check (see above).
6. **`BindProposalItem` reads the instance kind.** A closure still needs a
   prospective merge and now refuses without one; a filing refuses with
   one; a task-proposal instance cannot bind to an `effect_proposal` item at
   all. Before this the function assumed every bound instance was a closure.
7. **Facts are nullable arms per kind, not a discriminated union.**
   `EffectProposalFactsSnapshot` gains a required, nullable
   `follow_up_filing` next to the closure fields. A `oneOf` would be the
   tidier schema, but it would rewrite the closure wire shape and the
   generated Swift types every existing client decodes, for a card that has
   two kinds.
8. **Only approve and decline apply to a filing.** `approve_with_changes`,
   `start`, and `start_with_changes` are refused with the existing
   command-mismatch error. Editing a filing before approval is a card
   decision for #1633 and would need the screen and target gates on the
   edited text.

## Deviations From the Plan

- **Body cap is 16 KiB, not 32 KiB.** The publication screen's candidate
  budget is 16 KiB, so a larger cap would admit a body the screen then
  refuses for size.
- **Verdict wire values are `passed` and `rejected`.** The plan named only
  `passed`. The store admits only `passed`; `rejected` exists so the enum
  can describe a withdrawn text without a contract change.
- **The Signet `proposal.go` kind switches are unchanged.** The plan said to
  add the case. Those switches already return an error from `default` for
  any kind that is not a task proposal, which is the behaviour a filing
  needs.
- **The app shows the kind label and drops the facts.** The card belongs to
  #1633. Until then a filing item reads "Follow-up issue filing" with its
  actions disabled, and no closure rows.

## Refute-First Findings

A fresh-context reviewer was asked to refute the diff. Nothing was reachable
in production, because nothing proposes or opens a filing yet; the outcomes
are judged against the units that will.

- **Confirmed and fixed: a stale source failed every item read.** See "What
  a Stale Source Does".
- **Confirmed and fixed: U+2028 and U+2029 hid a slash command.** A title
  or body could carry `/close` after one and pass. The ruleset now refuses
  both characters.
- **Confirmed and fixed: the card showed an unchecked repository name.**
  Decision 3.
- **Tests added for behavior that held but was not pinned:** an approved
  filing is not a closure approval, takes no policy closure approval, and
  does not authenticate a task start.
- **Allowed by decision: a separate-work source needs no acceptance record
  and ignores later rounds.** A parked finding that a later round re-lists
  and fixes still backs a filing. The issue defines this link as the entry
  alone, and whether a later disposition retracts a separate-work verdict
  is routing policy for #1632.
- **Allowed by decision: findings deferred through the diminishing-returns
  finish cannot back a filing.** Their entries carry a route other than
  `defer`, and the issue requires a `defer` entry. Read from the code, not
  run. #1632 owns which findings are proposed.
- **Allowed by decision: snooze is not refused for a filing.** The issue
  lists approve and decline as the decisions and refuses the other three;
  snooze is the card's deferral, shared by every effect proposal.
- **Disproved or unreachable:** digest instability, a proposal carrying two
  arms, a second kind assumed by any reader of the two older kinds, the
  migration dropping an index or dependent, and spec-to-Go shape drift were
  each tried and held. `@` followed by an HTML comment or tag, and a
  fullwidth `＠`, pass the mention rule; none is `@name` to a raw reader, and
  none was shown to notify anyone.

## Revisit When

- A filing is approved and its finding is then fixed before the issue is
  filed: #1634 decides whether that filing still goes out.
- A person should be able to approve a filing whose source went stale. The
  currency check at approval would then need an override, not removal.
- A third card kind appears: decision 7's nullable arms stop scaling at
  three, and the `oneOf` rewrite is then worth its cost.
- A project files follow-ups into a repository other than its own: the
  target derivation reads one project row.
