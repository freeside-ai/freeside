# Publication Author: Omit Publisher-Owned Parts Without Comment

Work unit: #2347. Mandatory note: it changes the publication-author role
prompt (control-plane content) and revises an explicit earlier decision at
the owner's direction.

## Decision: Leave Publisher-Owned Parts Out Silently

Revises "Keep Publisher Authority and Explain Template Conflicts" in
`2026-09-23-2328-publication-author-output.md` (#1531). That decision, as
the role prompt carried it, had the author explain in visible body prose every
template requirement it could not meet, with the example "check evidence is
supplied separately by the publisher". Its reason: stored reviewer notes are not rendered, and the
author must not claim template compliance it lacks.

The owner chose on 2026-10-26 to drop that disclosure for the parts the
publisher writes: the source-issue reference above the body, and the
Verification section, control-plane advisories, scope decision, and
disposition history below it. The author now leaves out the template
sections and lines those parts cover and never mentions the omission, who
supplies those parts, or how the publisher formats them. Chose this over
keeping the disclosure (the exit-run result) and over a site-instruction-only
fix (the issue's original plan).

What changed: the earlier decision treated those requirements as unmet. The
Wave 8 exit run (#1613, freeasinbird/gh-imgup#145) showed they aren't. The
published PR has its Verification section and source reference, so the
disclosure tells the reviewer nothing they can't see and reads as a pipeline
note in a body that should read like any contributor's PR. The publisher's
fixed check format differing from the template's status prefixes is the
publisher's choice, not an author gap, so it is no longer disclosed either.

Kept from #1531: a template requirement the publisher doesn't fill still gets
a visible explanation when the author can't meet it, and the author still
keeps template sections such as Scope and Review Notes.

## Decision: Fix the Role Prompt, Not Only the Site Instruction

The issue first placed the cause in the explain site instruction and made
the role prompt a non-goal. A fresh-context review found the role prompt is
the real source: it is sent just before the site instruction, so a
site-only change would have told the model both "explain it" and "never
mention it". Both now carry the same rule. The site instruction keeps it
because it is the fixed baseline the operator-refinable role prompt sits
on; the role prompt carries the full version. A daemon test reads the
shipped role prompt and pins the rule's key clauses. It runs only when CI
selects the daemon component, which a prompts-only PR doesn't yet do
(Follow-up: #2368).

Revisit when the publisher's section set changes (for example #1533 adding a
reserved heading) or when a live publication still shows meta-commentary
about publisher-owned parts.
