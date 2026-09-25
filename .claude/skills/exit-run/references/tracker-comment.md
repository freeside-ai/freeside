# Tracker Comment Templates

Three records an exit run writes on the tracker. The diagram follows
`docs/tracker-format.md` (§status) with Freeside's additions in
docs/coordination.md (Tracking Issues); this file only fixes the section
order so consecutive runs read alike. Replace every bracketed placeholder;
delete a section that has nothing to say rather than leaving it empty.

## Exit-Run Comment

````markdown
<!-- freeside-<tracker>-exit-run:<run label> -->
## <Tracker Name> Exit Run: <Target> (<run label>)

**<Bottom line: can the tracker close? If not, what stands between here and
closure, in two sentences.>** Proposal: <the exit set (Required and
Investigate) in one line, the deferred set in one line>. Dispositions below
are a proposal until the owner adopts them.

*Status revision <date>: <what changed since the comment was posted>.*

Evidence basis: the run itself from `main` at `<sha>`, the state root
`<name>`, the operator's reports on <devices>, and screenshots attached to
<issues>. No unit issue's checklist was edited.

### The Run

| Step | Result |
| --- | --- |
| State root | <fresh or retained; onboarding profile; anything the current daemon refused> |
| Clients | <Mac and iPhone build commit only; no addresses, device names, or pairing codes> |
| Campaign <n>, specification | <run id; outcome; who approved, on which device, which actions the card offered> |
| Campaign <n>, implementation attempt <k> | <run id; outcome; if failed, the finding and whether an item was raised> |
| Review | <review id; model and reasoning; instruction digest; round outcomes> |
| Verifier | <verifier test name; result line; ready item and its offered actions> |
| Publication | <PR link; base to head; branch; whether merged and by whom> |
| Completion fact | <the recorded completion row after the merge, or why none> |
| Spend | <per role; total; review cost owner> |

**Actions exercised on real items:** <action (card, device)> ...
**Not evidenced this run:** <action: why; what covers it instead> ...

### Findings

Severity and exit disposition are separate. "Required" means merge before
exit; "Investigate" means diagnose before exit and fix any proven gap;
"Deferred" means deferral-labeled now and swept at the next planning.

| Issue | What's wrong | Sev | Lane | Disposition | Reason or trigger |
| --- | --- | --- | --- | --- | --- |
| #<n> | <one line> | P<1-3> | <lane> | Fixed in-run, PR #<n> | <why every run would hit it> |
| #<n> | <one line> | P<1-3> | <lane> | Required | <which stated outcome it contradicts> |
| #<n> | <one line> | P<1-3> | <lane> | Investigate | <what a proven gap would require> |
| #<n> | <one line> | P<1-3> | <lane> | Deferred | <trigger that promotes it> |

Not filed: <symptom>, covered by #<n>; <symptom>, appears covered by #<n>,
please confirm.

Conditionally blocking, needs one check before deciding: <#n: the check>.

### Start Order (Proposal For The Exit Set)

Prerequisites: <what is already merged; `main` at `<sha>`>.

**Startable now (structural only):** <units, or why none is startable>.

**Typed chains:** <every arrow is `starts-after` unless marked>. <lane
group>: #a → #b → #c. <lane group>: ...

**Cross-cutting gates:** <contract serialization; refute-first units;
shared-file one-writer chains; the four-front cap>.

**Critical path:** <chain>, <n> units.

| Front | Lane labels | Independent territory | Coordination boundary |
| --- | --- | --- | --- |
| <front> | `lane:<x>` | <paths or surfaces> | <what stops it crossing> |

```mermaid
flowchart LR
    i<A>[["#<A> <three to five words>"]]
    i<B>["#<B> <three to five words>"]
    i<C>["#<C> <three to five words>"]
    i<A> ==> i<B>
    i<B> -.-> i<C>
    classDef contract fill:#fde2c8,stroke:#c2410c,color:#000
    classDef owner fill:#e5e7eb,stroke:#6b7280,stroke-dasharray:5 5,color:#000
    classDef startable stroke:#15803d,stroke-width:3px
    classDef investigation fill:#fff2cc,stroke:#a80,color:#000
    class i<A> contract
    class i<B> startable
```

**Legend:** thick arrow critical path · arrow starts-after · double border merged · green outline startable now · orange contract · grey dashed owner-run.

**Edges:** dotted arrow `merges-after`; a `stacked-on` edge is labeled.
Yellow fill marks a required investigation. Deferred issues are omitted.

**Authority:** each unit issue's Dependencies field is authoritative; this
digest and diagram are projections, and where they diverge the issue wins
and this comment and the body index get repaired. Grounded at `main`
`<sha>`.

### What I Verified And What I Didn't

- Passed: <the pipeline end to end; harness verifier; checks on in-run fix branches>.
- Checked: <read-only inventories; source traces, with file:line>.
- Not run: <walkthrough steps not done; devices unavailable; any fix>.
- Caveat: <store edit with backup path; anything that makes the state root unclean evidence>.

### Decision-Note Disposition

<This comment records the run and proposes dispositions; it changes no
policy. If the owner adopts <the reinterpretation>, that is a lasting
decision and gets a devlog note in its own PR.> Reusable run mechanics live
outside the repository at <redacted location>.
````

## Exit-Preparation Comment (Run Blocked Before Launch)

```markdown
## Exit Preparation: <What Blocked It>

The requested exit exercise targets <target link>, in <mode>, with the
source text: > <exact text>.

**Blocked by <#n or condition>.** At `main` `<sha>`, <one sentence on why
the run cannot start>. <What a workaround would test instead, and why that
is not the requested path.>

Preparation verified: <target prerequisites closed; no open PR for it;
credentials; app access; environment>. Not verified: <list>.

No production run, submission, specification approval, or target PR was
created. <Anything that was created or spent: onboarding, images, a filed
issue.> The tracker stays open.

Resume after <condition>, using <the documented path>.
```

## Tracker Body Update

Make these edits to the body in the same operation as the comment, after
reading the current body:

- When the exit set isn't empty, add one bullet to `## Exit` linking the
  exit-run comment as the owner-approved (or proposed) exit follow-up,
  ending with `Evidence:` and the exit-set units, chained as
  `Evidence: #N, then #M` when the proof lands in steps, so "all units
  merged" is no longer read as sufficient on its own. If Exit already
  has six bullets, the format's maximum, extend the closest existing
  bullet's text and Evidence instead. A clean run adds no Exit bullet: the
  comment is the record, and the owner ticks the existing bullets.
- Add the exit set, the findings the owner made Required or Investigate
  before exit, to `## Units` as bare `- [ ] #<n>` lines under their lane
  group. On a wave tracker, listing is half of scheduling (AGENTS.md Work
  Units): set the phase milestone on each in the same edit. An ad hoc
  tracker schedules nothing and sets no milestone. A `needs-human` finding
  in the exit set goes under `### Owner-run`, unmilestoned.
- Keep a deferred finding out of Units and the diagram on either kind of
  tracker. Record the adopted dispositions in the Notes block, one bullet
  per disposition group with its date and a link to the comment, for
  example `**Deferred beyond exit (owner-adopted <date>):** #<n>, #<m>.`
  The comment's table stays the full record.
- Refresh the Status section: add the exit-set units as nodes with their
  edges and recompute **Startable now**, so the body and the comment
  project the same state.
