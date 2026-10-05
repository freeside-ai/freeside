# Follow-Up Filing Proposal Card

Issue #1633 renders a `follow_up_filing` effect proposal on the
`effect_proposal` card, on Mac and iPhone. It is an `app/` unit: it reads the
facts #1625 added and changes no contract. The note exists because the card's
match gate decides what the client trusts in facts a server returned.

## Decisions

- **The match gate decides per effect kind, in a switch with no default.**
  Chose that over widening the closure condition with an "or filing" clause.
  The four checks every kind shares (revision, entity version, item version,
  and `artifact_digests == [proposal_digest]`) run first; each kind then has
  its own rule, and a new kind does not compile until someone decides its
  rule. `run_proposal` is refused: the task-proposal card and its own facts
  read carry that kind.
- **A filing has no head rule.** A closure must name the item's
  `pr_head_sha` as its candidate head, because the card shows that head and
  the command stamps the item's, and the two must be the same head. A filing
  binds no merge, its facts carry no head, and the card shows none, so
  nothing on screen can differ from what the command stamps. Rejected:
  requiring the item's head to be empty for a filing. The API says the head
  is empty when an item names no PR, but no daemon filing item exists yet
  (#1632 opens it), and a client rule the contract does not state would
  refuse every real filing if that item names a head.
- **A closure now also refuses a stray filing arm.** The schema says exactly
  one kind arm is non-null. Before #1625 there was no second arm to check.
  Now that the card can draw a filing, facts that carry both arms are
  internally inconsistent and fail closed. This is the one change to the
  closure rule.
- **The display follows `effect_kind`, never which arm is present.** The
  rows, the proposed text, and the Details rows each key on the kind, so they
  agree with the gate even when called with facts the gate would refuse.
- **Screened agent text is still drawn as unverified.** The title and body
  are the agent's words. The daemon screens them for directives, secrets,
  mentions, and commands; it does not check that they are true. So they draw
  under the "(unverified)" label in the dashed frame, apart from every daemon
  fact, and what the daemon did check is its own fact row ("Text
  screening").
- **The filing sections carry their own explanation sentence.** The shared
  sentence reads "Written by the agent, not checked by the daemon." Beside a
  row that says the daemon screened the text, that is false. The filing
  sections say "Written by the agent. The daemon screened this text; it did
  not check that it is true." Rejected: rewording the shared sentence, which
  changes every other card's screenshots and is the owner's call; and
  dropping the sentence, since this card type explains the register in each
  unverified section. The agent chose this wording; the plan had left the
  mismatch for the owner.
- **The body is plain text, not Markdown.** The operator approves the exact
  text that would be sent. Rendering Markdown would hide characters and let
  agent text take on structure the card did not give it. A preview of how
  the forge would render the issue is a different feature.
- **The title and body sit ahead of the actions, in full.** They stay in the
  card's facts module, which the composition places before the actions.
  Nothing is truncated: the daemon caps the body at 16 KiB, and an operator
  has to be able to read everything that would be filed.
- **The mock's action set stands in for a daemon item that does not exist
  yet.** The mock filing item offers approve, decline, and snooze. The daemon
  refuses `approve_with_changes` for a filing, but which actions the real
  item offers is #1632's decision. If that item offered
  `approve_with_changes`, the card would show a button that opens the
  closure's revision sheet.

## Refute-First Findings

Two passes tried to prove the gate and the card wrong before the gate was
committed: a mutation run that dropped one gate condition at a time, and a
fresh-context reviewer given only the diff and the intended outcome.

- **Disproved by a check: a gate condition no test pins.** Twelve mutants,
  one per condition (the four shared checks, the closure's head and
  filing-arm rules, the filing's arm, closure-arm, `supersedes`, title, and
  body rules, and the `run_proposal` refusal), each made a model test fail.
- **Disproved by reading: a closure regression.** The closure rows are the
  same rows as before, the `supersedes` row included, and the closure rule
  is the old conjunction plus the filing-arm check.
- **Disproved by reading: agent text drawn as a daemon fact.** The title and
  body draw through `Text(verbatim:)` with no line limit. Fact rows do not
  interpret Markdown, and the finding ID is a daemon-derived hash.
- **Confirmed, allowed by decision: a filing item that offered
  `approve_with_changes` would open the closure's sheet.** Nothing wrong
  could be submitted, because the sheet builds no revision without a closure
  arm and its Approve stays disabled. The daemon's action policy allows that
  action for every `effect_proposal`, so only #1632's item opener keeps it
  off a filing. Rendering what the item offers is this unit's contract, so
  the card is unchanged and the requirement is recorded on #1632.
- **Confirmed, allowed by decision: the gate accepts a filing on an item
  that names a head.** That is the "no head rule" decision above; a test
  now pins it.
- **Declined: the gate does not enforce the schema's length bounds.** An
  empty title or repository name needs a daemon that violates its own
  schema, the card would show the empty value, and the closure rule checks
  no such bounds either.
- **Declined: a label that contains ", " reads as two labels, and a
  milestone named "None" reads as absent.** Both values come from the
  project's own policy, not the agent, and the filed issue shows them
  exactly.

## Revisit When

- #1632 lands the daemon's filing item: check that it offers no
  `approve_with_changes` and what head it names.
- The facts gain the finding's message or a pull request reference; the card
  shows only the finding's ID today.
- The owner rewords the shared unverified sentence; the filing sentence
  should then fold into it.
