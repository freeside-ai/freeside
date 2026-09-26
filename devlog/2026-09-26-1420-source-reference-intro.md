# Drop the Heading From the Publisher Source Reference

For #1553, the publisher-owned source-reference section keeps its place at
the head of a recipe v2 PR body but loses its `## Source Issue` heading. The
marker-bounded section now holds one paragraph, so the close reference reads
as the description's lead line. A recommended close joins its provenance
sentence to that line: `Closes #114. The client recommended this issue; the
approver confirmed it.` This revises only the heading chosen in
`2026-09-25-0036-source-reference-placement.md` (#1547); the head placement
and that note's rejection of splicing into the author's `## Why` stand.

The owner decided this while reviewing gh-imgup PR #121 in the #1445
run-114 exit run: "I don't _really_ want a 'source issue' heading on the PR,
I just want the source issue in the top intro/description section."

None of the reasons against the Why splice apply to an unheaded lead line.
It needs no knowledge of the target's template, works for bodies with no
Why, and needs no Markdown parser: the line still starts at column zero,
outside any code fence, where GitHub reads a close keyword.

Chose to keep refusing `## Source Issue` in authored prose (any case) over
refusing only the marker. The unheaded lead sits directly above the
author's prose, so an author section headed "Source Issue" would read to
the person merging as the publisher's record of the close reference. Keeping
the refusal also leaves what authors may write, the engine's unsafe-content
corpus, and the fake publication contract unchanged.
`publicationtext.SourceReferenceHeading` stays exported as a reserved
heading the screen checks and the publisher no longer renders. This was the
planner's choice, left open to an owner veto that was not exercised.

Open PRs published with the #1547 headed layout converge on their next
publication pass with one body update, as the pre-#1540 layout did.

Revisit when the reserved heading has had no rendered use long enough that
refusing it in authored prose costs more than the confusion it prevents, or
when the publisher reads the target's PR template.
