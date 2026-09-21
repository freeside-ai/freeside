# Recipe v2 Screen and Markdown Renderer (Issue #1419, Part A)

Part A of #1419: the `freeside.client-publication/v2` screen and the live
Markdown renderer, as pure functions nothing calls yet. The owner split #1419
into five parts and chose Part A first as its own PR. This note records the
public-output design decisions; the closure/proposal trust boundary (Parts B/D)
gets its own note when that work lands.

## Chose an Allowlist Renderer With Code-Span Neutralization Over Inert `<pre>`

v1 renders the author's prose entirely inert inside `<pre>` HTML. v2 must render
*live* GitHub-Flavored Markdown while staying at least as safe. Chose to keep a
small allowlist of constructs live (headings, paragraphs, bullet/numbered lists,
emphasis, inline code, fenced code) and render everything else inert, rather
than a blocklist. Rejected a full CommonMark parser: it is more surface than the
job needs and its escapes are exactly where revision 63 found leaks.

The neutralization primitives, in order of trust:

- **Autolink tokens** (`@mention`, `#123`, `owner/repo#123`, `GH-123`, bare
  URLs, `www.` hosts, 7–40-char hex) are wrapped in inline code spans, because
  GitHub performs none of its reference substitutions inside code. `codeSpan`
  sizes the fence to any backticks the token contains, so an adversarial token
  cannot break out.
- **Raw HTML** is escaped (`html.EscapeString`), so tags and entities show as
  text. `>` is escaped too, which also neutralizes blockquotes (not on the
  allowlist).
- **Links and images** are reduced to their visible text; the target is
  dropped (contract settled item 8: nothing yet turns evidence into a public
  URL). A link the stripper misses degrades to inert text, never a live link,
  because its URL is caught by the token wrapper and its brackets are escaped.
- **Open code fences** are closed at end of prose, so an unterminated fence
  cannot swallow the publisher-owned sections Parts B/D append after the prose.

The opt-in live check (`FREESIDE_MARKDOWN_LIVE_TEST`) is the real test: it sends
the rendered corpus through GitHub's own Markdown API and asserts no active
link, image, mention hovercard, or issue/commit reference. Our unit tests pin
the bytes; only GitHub's renderer proves the neutralization holds.

**Revisit when** the EvidencePublisher unit turns an evidence artifact into a
public URL: settled item 8's "no active link or image" amendment is then lifted
for evidence references, and the renderer should link them.

## Effective v2 Body Cap Is ~23 KiB (Candidate Budget), Not the 32 KiB Storage Cap

`domain.MaxPublicationAuthoringBodyBytes` stores up to 32 KiB, but
`publish.maxCandidateBodyBytes` (~23 KiB after the publisher reserves its own
sections) is the real limit a rendered body must fit. `screenAuthoredText` runs
`publish.ValidateCandidateBody`, so a raw body above that ceiling is refused and
falls back to v1 rather than composing a PR body that cannot fit. Escaping can
still expand a sub-cap body past the ceiling, so the authoritative final-fit
check on the *rendered* bytes belongs to the Part B/D caller that composes the
PR body; the renderer itself does not enforce length and never rejects.

## Kept v1 Byte-for-Byte

`screenPublicationText` now delegates to a cap-parameterized
`screenPublicationTextWithin`; v1 keeps its 8 KiB cap and identical checks. The
shared escape corpus (`publicationUnsafeContentCorpus`) is exercised by both the
v1 metadata screen and the v2 authored-field screen, so a construct that escapes
one screen cannot slip through the other.

## Screen the Visible Form, Not Just the Source (Codex R2-P1)

Codex found a real credential-leak path: the secret scan runs on the raw
source, but the renderer keeps emphasis and code delimiters live, and GitHub
collapses those out of its visible text. So `ghp_…**zz**…` passes the source
scan (the `**` breaks the token) yet renders as an intact PAT. This also broke
"at least as strict as v1", whose inert `<pre>` never collapsed anything.

Fixed in the screen, not the renderer, so `renderAuthoredPublication` stays a
pure `(title, body)` function: `screenAuthoredFieldText` runs the content
checks in a fixpoint that, each pass, also folds out the constructs GitHub
collapses (`collapseMarkdownDelimiters`: reduce links/images to their text,
resolve backslash escapes, normalize code spans to their rendered content, strip
`*`) and HTML-unescapes, so an entity-encoded delimiter (`&#42;`) is caught on
the next pass. A reconstructed secret makes the screen refuse the field, so v2
falls back to v1.

The governing principle is that the collapse must model **every**
character-dropping transform the renderer performs, not a hand-picked subset:
each such transform can splice a token the source scan missed, and enumerating
them by hand kept leaking one at a time (R2-P1 → R3-P1 → R4-P1, one per review
round). So the collapse now derives its reductions from the renderer's own
behaviour:

- Codex R3-P1: the link/image case (`ghp_…[zz](url)…` reconstructs once the
  renderer's `stripMarkdownLinks` drops the target), fixed by reusing that same
  `stripMarkdownLinks`.
- Codex R4-P1: the code-span padding case (`` ` zz ` `` renders as `zz`
  because GitHub strips one leading/trailing padding space, and a newline-padded
  span `` `\nzz\n` `` does the same after interior newlines become spaces). Blind
  backtick stripping lost the span boundaries needed to model this, so
  `collapseCodeSpans` now parses each span (reusing `findBacktickRun`) and emits
  GitHub's normalized content: interior line endings → single spaces, then one
  leading and one trailing space removed when the content begins and ends with a
  space and is not all spaces. `markdownEmphasisStripper` is left to `*` only.
  A benign interior-newline span `` `z\nz` `` renders `z z` (a space survives),
  so it stays accepted; the normalization strips padding, never the separator.

To stop hand-enumeration from being the completeness check, the opt-in
`FREESIDE_MARKDOWN_LIVE_TEST` now also runs the adversarial secret corpus through
GitHub's own Markdown API (`TestScreenRefusesWhatGitHubReconstructs`): whenever
GitHub's rendered visible text reconstructs a PAT, the screen must have refused
that body. GitHub's renderer, not our model, is the oracle for "did I miss a GFM
clause".

Chose to strip `*` and code-span padding but **not** `_`. `_` is a valid secret
character (`ghp_`) and GitHub treats it as emphasis only at word boundaries,
never intraword, so it can never splice a contiguous alphanumeric token;
stripping it would delete the literal underscore from a real prefix and make the
screen weaker. Stripping `*` and dropping backtick delimiters (never secret
characters) can only join fragments, which is the stricter, safe direction.

## Allowlist Completeness: Neutralize Strikethrough, Tables, Thematic Breaks (Codex R2-P2)

The renderer claimed "everything else inert" but left GFM strikethrough (`~`),
tables (`|`), and thematic breaks (`---`/`***`/`___`) rendering live. These are
cosmetic (none reach outside the prose), but the allowlist should be literally
true, so: `escapeInertText` now escapes `~` and `|`; a thematic-break line is
neutralized at the block level (`isThematicBreak`, checked before the list
marker because a spaced rule like `* * *` also matches a list item). Setext
underlines are neutralized by the same block rule; a setext heading is just an
alternate spelling of a heading, which is already an allowlist member, so no
fidelity is lost that `#` cannot express.

## Refute-First Record (Credential-Leak Surface)

- **Confirmed and fixed:** the visible-text reconstruction class, via emphasis,
  code spans, backslash escapes, entity-encoded emphasis (R2-P1), inline links,
  images, and reference links (R3-P1), and code-span padding and newline
  normalization (R4-P1). Each vector is an adversarial fixture in
  `TestScreenAuthoredTextRejectsDelimiterSplicedSecrets` that passes the raw
  source scan and is refused after the collapse pass; the benign interior-newline
  boundary (`` `z\nz` `` → `z z`) is pinned accepted by
  `TestScreenAuthoredTextAcceptsBenignInteriorNewlineSpan`.
- **Confirmed against GitHub's renderer:** ran the opt-in
  `TestScreenRefusesWhatGitHubReconstructs` against GitHub's Markdown API
  (anonymous). Every fixture whose rendered visible text reconstructs a PAT was
  refused by the screen; none leaked. Backslash- and entity-escaped emphasis and
  the image-alt case do not reconstruct in GitHub's visible text at all, and the
  screen refuses them anyway (safe over-approximation).
- **Disproved by check:** stripping `_` as a reconstruction vector. Traced the
  CommonMark intraword-underscore rule; a `_`-spliced token keeps a literal `_`
  and is not a valid secret format. Stripping `_` was actively harmful, so it
  was removed.
- **Allowed by decision:** bodies between ~23 KiB and 32 KiB fall back to v1
  (candidate-budget section above); the renderer performs no final-fit check
  (Part B/D owns it).
- **Independent refuter:** Codex re-reviews each push; the opt-in live GitHub
  Markdown check is the external oracle for the neutralization.
