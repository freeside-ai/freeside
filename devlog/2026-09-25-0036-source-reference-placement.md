# Put the Publisher Source Reference at the Head of the PR Body

For #1540, the publisher-owned source-reference section (`Closes #N`,
`Refs #N`, or the descriptive `Source issue: <url>` link) is the first part
of a recipe v2 PR body, above the authored prose. It stays a fixed,
marker-bounded section, and its heading becomes `## Source Issue` to match
the other publisher headings (`## Verification`,
`## Freeside Disposition History`). The authored-text screens compare the
heading lowercased, so author prose carrying either spelling is still
refused.

The section used to sit after the prose and before Verification (#1419
Part D). On gh-imgup PR #120 that put `Closes #113` near the bottom of the
body, while the target's PR template asks for the close keyword in **Why**.
The author can't move the reference, because plan §5.15 forbids the
publication author from writing `Closes` or `Refs`. Head placement puts the
reference directly under the title and above the author's Why, which is
what the template's placement is for. GitHub reads a close keyword anywhere
in the body source, so closure behavior is unchanged.

Rejected: splicing the reference into the author's `## Why` section.

- **The publisher never sees the target's template.** It composes the body
  from the candidate alone, and `## Why` is gh-imgup's heading, not a
  contract every target repository shares.
- **Some bodies have no Why.** The v1 fallback body (a refused authored
  text) and a label-intake literal body carry no `## Why`, so a splice would
  need a second placement rule anyway.
- **A splice needs a Markdown parser.** It would have to avoid landing
  inside a code fence, where GitHub ignores a close keyword. That turns a
  placement fix into a parser with a closure-correctness failure mode.

Open PRs published with the old layout converge on their next publication
pass: the publisher patches any body that differs from the composed one, and
the publication identity excludes the body, so the rewrite keeps one section
and no duplicate.

Revisit when the publisher reads the target repository's PR template, or a
target's template names a machine-readable slot for the close reference.
