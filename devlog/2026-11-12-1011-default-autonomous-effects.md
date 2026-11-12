# Effects Are Autonomous by Default; Follow-Up Filing Needs No Card

Issue #3151, plan revision 121. A safety-policy and material plan change
recording owner decisions, so a mandatory note. Docs only; nothing here is
built yet.

Chose to make the autonomous path the default for every effect that has
one, with a per-project, per-effect human gate, and to let a follow-up
filing or a comment on an existing issue dispatch with no card, over
keeping per-proposal approval for filings, because tracker #3132's goal is
no pause for approval beyond the pull request, and the fan-out a filing
causes is already held by other means.

## Decision

- **Default-autonomous, gated per effect.** Where an effect has an
  autonomous path and a human-decision path, the autonomous path is the
  default. Each effect has its own `gates.<effect>` key; there is no global
  switch. Absent or false is autonomous; a value that doesn't parse turns
  the gate on, as `gates.source_issue_closure` already does. An unmet
  precondition takes the human path with a notice naming it, never a
  silent fallback to a card. Caps stay on either path. Judgment items
  (`review_dispute`, `finding_adjudication`) are not gates.
- **Two policy layers.** Project setting, then general setting, then
  built-in default, with the winning layer as each key's provenance. Keys
  the Workflow settings pane exposes resolve from the store layers only;
  the rest keep resolving from the submission's policy file. Caps are
  per-project defaults, not a shared budget.
- **Autonomous filing.** A filing dispatches without a card when
  `gates.follow_up_filing` resolves off and the trust profile has accepted
  the `issues` set of the issue-automation audit. With the gate on, the
  card path is unchanged.
- **`issue_comment` effect kind.** A comment on an issue the task is not
  bound to targets a daemon-enumerated handle, rides the filer's intent
  ledger, is gated by `gates.issue_comment`, and needs the `issue_comment`
  audit accepted on either path. A comment on the bound issue stays
  ungated.
- **Duplicate search.** Before a deferred or separate-work finding
  proposes a filing, the adjudication site picks "new" or one listed open
  issue; a duplicate yields a comment.

## The Reversed Rule

Section 5.17 said: "Filing arrives human-gated (1B.1): every filing takes
explicit per-proposal human approval, whatever the profile state." Its
autonomous path also needed an issue-event authority profile proving every
transitive issue-creation path inert.

The assumption that changed: a person reading each proposal was the thing
holding fan-out. It isn't. Ledger lineage keeps a Freeside-filed issue from
re-entering intake as fresh work, the screen bounds the text, policy fixes
the repository, labels, and milestone, the audit pins what workflows a
filing starts, and caps bound rate and depth. The authority profile's bar
(prove every creation path inert) is one Freeside's own repository can't
meet, so it would have kept filing human-gated indefinitely.

The profile keeps its intake role: it is still the only thing that restores
`auto_start` for labeled issues Freeside didn't file in a repository where
Freeside has filed. Every intake rule stands.

## Planner Choices

From #3151's Planning Notes, kept by this revision:

- **Key names follow the code.** `gates.follow_up_filing` and
  `gates.issue_comment`, matching `gates.spec_approval` and
  `gates.source_issue_closure`, not the draft's `*.human_gate` names.
- **The audit gains an `issues` set** (`allow_issue_workflows`), because a
  filed issue starts `issues`-triggered workflows, not `issue_comment`
  ones. Only autonomous filing needs it accepted; the card path doesn't, as
  today. No unit builds it yet, so every filing takes the card until one
  does.

## Implementer Choices

Each narrows the contract's wording to keep an existing safety rule; the
owner can revise any of them.

- **Stage gates are not effects.** `gates.spec_approval` and
  `gates.before_final_review` share the `gates.` namespace, but the daemon
  requires `gates.spec_approval` and the rein preset sets both true.
  Reading "absent means autonomous" onto them would remove the
  specification approval by default, which neither the owner decision nor
  #3151 asks for.
- **The rule names its effects.** `task_proposal` is a Section 5.13
  effect whose autonomous path is `auto_start`. Read broadly, an absent
  key would loosen intake, a #3151 non-goal, so the rule lists the closure,
  filing, and `issue_comment` and leaves `task_proposal` to intake mode.
- **A both-path precondition refuses, not cards.** The `issue_comment`
  audit binds both paths, so a card could never dispatch; an unaccepted
  audit is a definite refusal, as for the specification comment.
- **The comment kind takes the specification comment's screen,** which
  rejects mentions and command shapes, since a comment notifies the target
  issue's subscribers. A profile reviewed before revision 121 doesn't
  approve the kind, because its `issue_comment` acceptance covered only
  the specification comment.
- **Duplicate search is its own judgment site.** #3151 named the
  adjudication site, but a site carries exactly one authority contract and
  the finding adjudicator's is ceiling-bounded annotation.
- **Declined: refusing a pane-exposed key in a submission file.** The
  owner chose store-only resolution for those keys; how the pane shows a
  shadowed file key is #3145's to decide.

## Direction

- **Owner, 2026-11-12 (tracker #3132's planning notes):** autonomous by
  default with a per-effect setting for the safer path; workflow settings
  in two layers.

## Rejected

- **Keep the per-proposal card for Freeside's own repository.** #3132's
  goal rules out a pause beyond the pull request.
- **One global human-gate switch.** It couples unrelated effects; a project
  that wants a person on comments needn't also want one on filings.

Revisit when an autonomous filing or comment causes fan-out the caps,
audit, and lineage did not bound.
