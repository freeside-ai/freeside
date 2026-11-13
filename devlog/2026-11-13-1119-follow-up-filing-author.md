# Follow-Up Filings Are Authored in the Work-Unit Shape

Issue #3148, plan revision 123. Tier A (untrusted model text reaches
GitHub, and the model's answer picks a label) and a plan change, so a
mandatory note.

Chose a propose-authority judgment site that drafts the work-unit form's
fields and a kind from a fixed enum, screened field by field and rendered
by the engine into fixed sections, over letting the model write the whole
issue body, because a person or planner reading a follow-up issue needs
the template's sections, and per-field screening lets one bad field fall
back without losing the rest.

## Decision

- **Site under the finding adjudicator.** `follow_up_filing_author` joins
  the finding adjudicator's sites, next to `duplicate_search`, over the
  publication author. The publication author's sites need the operator's
  prompt file and serve the pull request's prose; the role list is closed,
  so a new role was not an option. The adjudicator already reads the
  finding that the filing comes from.
- **Propose authority, no stored draft.** The draft lives in memory until
  the proposal transaction renders it into the stored title and body. A
  pass that fails before that commit asks the site again, and a committed
  proposal's text never changes, so the effect identity and digest stay
  fixed once written. A separate draft record was rejected as a second
  copy of text the proposal already stores.
- **Kind from an enum, label from a table.** The site names feature, fix,
  or chore; `FollowUpFilingKind.Label` maps it. `kind:contract` is outside
  the enum, because a contract unit invokes contract review and only the
  planner classifies one. `GateFollowUpFiling` re-derives the label set
  from policy plus the stored kind on admission and every read, so a row
  whose labels or kind were changed fails closed.
- **Dependencies from lineage, not the model.** The section says `none`,
  or `starts-after #N` for the issue the task is bound to, with a note that
  the planner confirms it. The tracker parser reads the first
  `### Dependencies` section, so letting the model write it would let
  proposal text set start order.
- **Fallbacks keep today's filing reachable.** A refused title takes the
  composed title; a refused body field takes a fixed sentence pointing at
  `### Origin`, which always carries the composed body. With the site off,
  unavailable, or answering badly, the filing is exactly what it was
  before this change. If the whole rendered body fails the screen, the
  filing falls back to the composed title and body with no kind.
- **Kind yields to a full label set.** When policy already supplies the
  maximum number of labels, the kind is dropped rather than overflowing
  the bound and stopping the lane.

## Departures From the Plan Comment

- **Section confinement.** The plan screened each field under
  github-issue/1 only. That ruleset allows headings, fences, and HTML, so
  a field could open a second `### Dependencies` or leave a fence or an
  HTML element open and swallow the lineage sections below it. A field
  that holds a tag-like `<` anywhere, or a line that opens an ATX heading,
  underlines a setext heading, or opens a fence, takes its fallback. Prose
  such as `Result<T>` falls back too; the instruction asks for plain
  prose. Origin goes last, with its heading lines escaped, so the composed
  body can't open a section either.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong. Each claim was
checked against the code or a test.

- **Confirmed, fixed: setext headings escaped confinement.** A field
  `Dependencies\n---` rendered as a heading above the real section. The
  tracker parser reads ATX headings only, so start order was never at
  risk, but a reader saw a forged section. Now refused.
- **Confirmed, fixed: mid-line HTML.** `See <details> for more` passed the
  line-start check and could collapse every later section on the rendered
  page. Any tag-like `<` is now refused.
- **Confirmed, fixed: a refused dispatch claim stopped the pass.** With
  no duplicate-search candidates, a closed workflow made the pass fail
  where it used to propose the composed filing. Drafting now ends and the
  rest compose as before.
- **Disproved:** ATX headings at any level or indent, backtick and tilde
  fences, line-start HTML, CR, tabs, NEL, U+2028/9, and format characters;
  mentions, slash commands, close keywords, and `freeside:` markers per
  field and on the whole body; a second `### Dependencies` inside Origin
  (escaped, and Origin is last); a kind outside the enum or a forged label
  set (the decoder, `Validate`, and the gate each refuse); byte identity of
  the unauthored filing, parameters, and digest (a golden pins it); the
  body bound (field caps sum to 13,312 bytes under 16 KiB); a retried pass
  changing a committed proposal (the write drops ordinals that already
  have an instance).
- **Allowed: repeat calls for an entry the write later skips as stale.**
  Continuation passes read effective routes, so drift reversals never
  reach the site; the adjudication gate reads its own artifact's routes,
  where staleness needs a later round or revision while the gate is still
  reconciling. The site's budget bounds the cost, and a guard would need a
  store export this unit's scope doesn't cover.

## Residuals Accepted

- Authored text is model words posted under Freeside's identity. It is
  screened, bounded per field, and confined to its section, and a person
  approves the card unless `gates.follow_up_filing` is off (revision 121).
- A pass that fails before commit makes another provider call; the
  dispatch claim and budget bound how often.
- A filing's `kind:*` label goes to the target repository whether or not
  it uses those labels, and GitHub creates a missing label on create. The
  contract chose policy labels plus the kind. The only switch today is the
  finding adjudicator role itself, which also turns off its other sites.
- The whole-body screen after per-field screening is a backstop no test
  reaches: a body built from fields that each pass and fixed headings has
  no input found that fails it.

Revisit when a tracker or planner starts reading another section of an
authored filing as authority, since only Dependencies is lineage-derived.
