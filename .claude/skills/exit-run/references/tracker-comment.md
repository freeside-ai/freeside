# Tracker Comment Templates

Three records an exit run writes on the tracker. The format rules for the
implementation-order digest and the diagram are in docs/coordination.md
(Tracking Issues); this file only fixes the section order so consecutive
runs read alike. Replace every bracketed placeholder; delete a section that
has nothing to say rather than leaving it empty.

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

### Implementation Order (Proposal For The Exit Set)

Prerequisites: <what is already merged; `main` at `<sha>`>.

**Startable now (structural only):** <units or none>.

**Mergeable next:** <open PRs or none>.

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
  classDef contract fill:#fdd,stroke:#c66,color:#000
  classDef investigation fill:#fff2cc,stroke:#a80,color:#000
  U1[["#<merged unit>"]] --> U2["#<open unit>"]
  U2 -.-> U3["#<unit that merges-after U2>"]
  class U1 contract
```

Legend: `A --> B` means B `starts-after` A; `A -.-> B` means B
`merges-after` A; `A ==> B` means B is `stacked-on` A; `A -.- B` means A is
`exclusive-with` B. A double-bordered node (`[["#N"]]`) is merged. Red fill
marks `kind:contract` units, yellow the required investigation; colors and
borders encode no ordering. Deferred issues are omitted.

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

- Under the close condition, add one sentence linking the exit-run comment
  as the owner-approved (or proposed) exit follow-up, so "all units merged"
  is no longer read as sufficient on its own.
- Add or extend an `## Exit Follow-Up Units` list, one line per listed
  finding issue, `- [ ] #<n> — <short name>. Finding <k>; **Exit required**.`
  (or `**Investigate before exit**`). Which findings the list admits
  depends on the tracker kind. On a wave tracker, listing is half of
  scheduling (AGENTS.md Work Units): list only the exit set, the findings
  the owner made Required or Investigate before exit, set the phase
  milestone on each in the same edit, and keep a deferred finding off the
  list; its `deferral` label and the comment's table are its whole record.
  A `needs-human` finding stays unmilestoned and off the list too, whatever
  its disposition; the close condition reaches it through the comment.
  On an ad hoc tracker, which schedules nothing, list every finding with
  its disposition (`**Deferred beyond exit**` for a deferred one), matching
  the comment's table exactly.
- Refresh **Startable now** and **Mergeable next** in the Implementation
  order section, and the diagram if the body carries one, so the body and
  the comment project the same state: both cover the exit set. A deferred
  finding an ad hoc tracker lists is named in those projections as outside
  the exit (#1211 writes "deferred structurally startable ... outside this
  exit"), never projected as startable exit work.
