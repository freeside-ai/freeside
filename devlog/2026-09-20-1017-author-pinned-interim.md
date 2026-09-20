# The Publication Author's Pinned-Binding Interim

Work unit: #1442. Plan revision: 67.

Two owner decisions of 2026-09-20, recorded in the plan.

## Decisions

1. **Ship the publication author on the deployment-pinned binding first, not on
   its own lineup wiring.** Revision 64 chose lineup selection for the author
   over the deployment-pinned daemon-judgment-site pattern
   (`2026-09-19-0823-publication-author-site.md`, decision 3), and revision 65
   extended that to every other judgment site. This revision changes only the
   order the author arrives in: it runs on the deployment-pinned
   `inference.Binding` first, the same interim every other judgment site is on
   today, and #1425 moves it to the lineup with all of them. The author stays a
   lineup role; the end state is unchanged.

   Chose pinned-first over lineup-first (build #1421, then #1425, before #1418)
   because real client PRs are not useful today and the lineup path is long.
   gh-imgup PR #110 merged with no `Closes` reference for its issue #82, the
   template's requirement, and the lineup path runs through #1421, #1425, #867,
   and #1426 before it lands. The pinned interim gets authored text into PRs now
   and lets #1418 build the role without waiting on that chain.

   The cost: nobody can tune the author's agent or prompt per role until #1425.

2. **The source-issue closure proposal is the first `effect_proposal`
   instance.** Chose the closure proposal over keeping human-gated follow-up
   issue filing as the first instance. Follow-up filing reuses the same card,
   and proposed watches still come last. The Section 11 Wave 8 wording changes
   to match, but the revision schedules nothing.

## Revisit when

The author's text is poor on the pinned model in the first real runs. Then
reconsider the order rather than the end state.

Note the fail-safe already on #1425: once it merges, a role with no lineup line
returns its fail-safe, which for the author means recipe v1. A daemon started
without its `-judgment-*` flags therefore renders v1, never a broken author
pass.
