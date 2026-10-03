# Approved Decisions And Preservation Requirements

All accepted treatments apply to Mac and iPhone. IDs below are stable links
between design, implementation, and verification. Existing Freeside fonts,
palette tokens, status meanings, selection, and native interaction conventions
remain the baseline.

## Accepted Treatments

| ID | Accepted Treatment And Source | Must Preserve | Reference |
| --- | --- | --- | --- |
| D01 | Meaningful inbox summaries use the existing serif face without added bold. Keep the quieter header, open count beside Inbox, and omit redundant counts and sorting explanation. V3 01 Keep supersedes V2 01/02. | Existing filters, ordering, project selection, two-line summary limit, selection treatment, and right-aligned urgency/time. No new condensed row geometry. | [Mac](evidence/v3-after-mac-typography.webp), [iPhone](evidence/v3-after-ios-typography.webp) |
| D02 | Give the task hold a distinct callout in task detail. V3 06 Keep. | Exact task state, hold reason, round, current review, guidance, Stop behavior, milestones and history. A capacity hold does not imply a human decision. Preserve the restored readable task list. | [Detail Mac](evidence/v3-after-mac-task.webp), [Detail iPhone](evidence/v3-after-ios-task.webp), [List Mac](evidence/v3-after-mac-tasks.webp), [List iPhone](evidence/v3-after-ios-tasks.webp) |
| D03 | Keep the visible Unverified label; reveal its explanation on demand. V2 05 Keep, carried through V4. | Producer distinction and discoverable, keyboard/VoiceOver-accessible explanation. Mobile access cannot depend on hover. Agent prose cannot become an unlabeled fact. | [iPhone explanation](evidence/v2-after-ios-unverified-explanation.webp); labels in D06–D09 |
| D04 | Bound long message bodies with reversible expansion in place. V2 14 Keep. | Exact complete text, author, time, ordering, attachments, copy/select behavior, and composer state. No generated summary or automatic collapse of conversation history. | [Mac](evidence/v2-after-mac-long-conversation.webp), [iPhone](evidence/v2-after-ios-long-conversation.webp), [Expanded/lower Mac](evidence/v2-after-mac-long-conversation-lower.webp), [Expanded/lower iPhone](evidence/v2-after-ios-long-conversation-lower.webp) |
| D05 | Empty task fields look empty, with external labels and a muted placeholder. V2 15 Keep. | Blank underlying values, selected project, optional name, validation, recovery, cancel, submission and keyboard behavior. Placeholder text is never submitted as entered content. | [Mac](evidence/v2-after-mac-new-task.webp), [iPhone](evidence/v2-after-ios-new-task.webp) |
| D06 | Present the question and complete separated options as one reading sequence. Fold redundant recorded context, run details and source identifiers. V4 01 Keep supersedes V3 02. | Full question/blocker, option labels, complete tradeoffs, recommendation attribution, answer controls and original supporting information. Option panels describe alternatives; they do not become selection buttons. | [Mac](evidence/v4-after-mac-agent-question.webp), [iPhone](evidence/v4-after-ios-agent-question.webp) |
| D07 | Lead final review with verification verdict and concise summary, then supported View PR. Passed checks use a compact named summary; review yield and technical bindings open on demand. V4 02 Keep supersedes V3 03 for final review. | All check names/counts/values, failed and waived leading rows, concerns, excerpt warnings, full report, evidence and supported actions. Return to agent stays secondary. View PR remains navigation. | [Mac](evidence/v4-after-mac-ready-for-final-review.webp), [iPhone](evidence/v4-after-ios-ready-for-final-review.webp) |
| D08 | Keep the substantive dispute reason and supplied claim near the actions. Fold attachment metadata and technical bindings. V4 03 Keep supersedes V3 04. | Unverified attribution, exact claim, original attachment/digest, run/round facts and existing action meanings. Never fabricate an opposing argument or infer a missing finding-to-ID association. | [Mac](evidence/v4-after-mac-review-dispute.webp), [iPhone](evidence/v4-after-ios-review-dispute.webp) |
| D09 | One finding card contains its exact message, proposed disposition, producer label and its own Reason and alternatives disclosure. Explain that Accept all dispositions applies to the bound batch. V4 04 Keep supersedes V3 05. | Finding identity, exact route/message, rationale, evidence, confidence, coordinates, assumptions, rules, consequences, alternatives and gating questions. Preserve separate source registers and existing command scope. No per-finding approval button is introduced. | [Mac](evidence/v4-after-mac-finding-adjudication.webp), [iPhone](evidence/v4-after-ios-finding-adjudication.webp), [Expanded Mac](evidence/v4-after-mac-finding-expanded.webp) |

## Rules That Span The Accepted Screens

- Keep original keyword section headings. The V3 serif section headings were
  expressly rejected. Serif inbox summaries do not change heading styles.
- Reduce repeated labels and unnecessary framing before reducing text size.
  V2 18 Keep establishes this principle, not permission for a global restyle.
- Use spacing to distinguish ordinary sections; use a bounded card when it
  represents an independent item or option. Apply this to the approved
  surfaces, not every use of a shared container by default.
- Stack phone actions as in the accepted native references. Keep the existing
  responsive Mac layout and accessible reflow. Preserve action order,
  eligibility, confirmations, overflow access and minimum target sizes.
- Keep status, uncertainty, and source identity explicit. A neutral body with
  quieter metadata cannot imply a stronger verification status.

## Information Moved Out Of The Initial View

Every item below must remain reachable, including when content is unavailable
or too long. An unavailable attachment keeps its existing state and recovery
affordance; replacing it with empty space does not preserve information.

| Information | Destination | Must Remain Visible Initially |
| --- | --- | --- |
| Generic reason on question, final-review and finding cards | Recorded context disclosure below the decision | Actual question/verdict/finding and any substantive warning |
| Question/dispute source label, digest, original content | Source and supporting details, beside the corresponding claim | Claim text and producer/Unverified label |
| Agent-summary source and original rendering | Source and original report, or the existing full-report route on final review | Summary, its claim label, concerns and excerpt warnings |
| Run and binding coordinates on question/dispute/final review | Run and binding details | Stale/base-advanced state, degraded/waived state and any current decision restriction |
| Passed-check values | Passed-check disclosure | Verdict, counts and every passed-check name; exceptions remain leading rows |
| Final-review yield history | Review-yield disclosure | Readiness and the supported next action |
| Finding rationale, evidence, identity/location/binding coordinates, confidence, assumptions, alternatives and constraints | Reason and alternatives inside that same finding card | Exact finding message, disposition and producer attribution |
| Long conversation body | Read full message / collapse in place | Author, time, understandable opening and attachment affordances |

Do not fold a whole mixed module merely because it contains technical facts.
In particular, isolate commit-plan notices, stale-state warnings and missing
decision capability from routine identifiers. A production snapshot may carry
more significant facts than the clean reference fixture.

## Disposition Of Every Original Proposal

Original IDs refer to V1/V2; V3 and V4 restarted their numbering.

| Original ID | Final Disposition |
| --- | --- |
| 01 Header/filters | D01; preserve original filters, later serif treatment wins. |
| 02 Row hierarchy | D01; urgency/time remain right aligned. |
| 03 Actual question | D06. |
| 04 Less framing | Limited to D06–D09 and the span rules above; no unreviewed global flattening. |
| 05 Unverified explanation | D03. |
| 06 Checks/review yield | D07 for final review. V3's expanded passing checklist is superseded. New diminishing-returns treatment remains unapproved; preserve its current presentation. |
| 07 Existing choices | D06 and the action-preservation rules; no new hidden primary choice. |
| 08 Disputes | D08; missing arguments and ambiguous Approve remain disclosed limitations. |
| 09 Finding adjudication | D09, including stacked phone actions. |
| 10 Compact task rows | Rejected in V2; restored list in V3 06 is accepted. |
| 11 Task hold | D02. |
| 12 Recorded outcome label | Unresolved in V2; excluded. Preserve current run-outcome behavior. |
| 13 Disclosure renaming | Rejected/withdrawn; excluded. |
| 14 Long messages | D04. |
| 15 Empty forms | D05; do not infer approval of unrelated composer redesigns. |
| 16 Recovery presentation | Unresolved; excluded. Preserve complete existing recovery information. |
| 17 Pairing/system errors | Unresolved after invalid comparison; excluded. |
| 18 Repetition before smaller type | Accepted principle; no blanket reduction in font size. |

## Boundaries Of Approval

The owner approved the shown hierarchy, not new data, synthetic summaries,
changed action scope, or arbitrary truncation. Existing design tokens remain
authoritative for exact SwiftUI metrics. Record small platform-dependent
differences; get an explicit owner decision for a changed reading order,
heading treatment, default disclosure state, or action prominence.

The 520pt finding action-region budget has not been relaxed by screenshot
approval. Measure the accepted design at the test's actual 560pt card width
with realistic messages. If it cannot meet that budget without losing an
accepted property, surface the measurements through #1141; do not silently
raise the threshold, shorten the fixture, or hide the messages again.
