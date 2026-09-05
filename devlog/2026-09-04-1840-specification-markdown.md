# Formatted Specifications Stay in the Claim Register

The owner chose formatted specifications in [#1125](https://github.com/freeside-ai/freeside/issues/1125)
because reading the approval object matters on the phone. Formatting is compatible
with plan §9 when the entire specification stays inside a dashed claim frame
labeled “Specification (unverified).” The daemon-bound digest stays outside that
frame. Typography conveys document structure, never verification authority.

## Rendering Decision

Use Foundation's full Markdown parser and native SwiftUI text. Reconstruct
headings, paragraphs, list items and continuations, quotes, and code blocks from
its block identities. Keep tables and HTML blocks as literal source. Plain text
and parser failure keep the previous wrapping, monospaced rendering. Parse only
the reader's existing 64 KiB preview, using that same prefix for source recovery.

Three rules apply to every rendered claim:

- Links have no action. Display each destination beside its label so a friendly
  label cannot conceal a target.
- HTML stays literal. It cannot supply styling, controls, or trusted-looking
  content outside the enclosing claim frame.
- Images show alt text only. No URL or fetched image enters the renderer.

Construct inline output from literal characters and presentation intents only.
Block metadata, link attributes, and image attributes never reach SwiftUI Text.
The bundled Plex faces have no italic variant, and requesting italic or bold
traits on the named regular face does not change its pixels. Use the native
system italic face for emphasis, the bundled semibold face for strong text, and
the bundled mono face for inline code. Production italic text uses a text style
so Dynamic Type scales it; the existing screenshot bridge supplies iOS metrics
only during macOS screenshot tests.

Rejected a web view because claim text needs no browser capabilities. Rejected
a third-party Markdown package because Foundation supplies the required block
and source metadata without another dependency. Keeping the whole document
monospaced defeats the owner's reading goal. Hiding destinations defeats the
inert-link rule's purpose. Adding italic font assets is unnecessary for this
reader when the platform already supplies a scalable italic face.

## Refutation Findings

- **Confirmed and fixed in PR review:** Empty list leaves have no character
  run, so standalone and sibling-empty markers disappeared. A detection-only
  Foundation parse adds temporary text to candidate marker-only lines. If
  additional list items become visible, preserve the complete original source;
  the temporary text never enters rendered output. Existing empty ancestors
  retain their formatting. Native heading metadata and single-line parsing
  distinguish setext underlines and spaced thematic breaks. Tests cover marker
  forms, spacing, line endings, sibling order, duplication, nesting, quoted
  markers, and literal code and HTML. Ambiguous marker-only documents may use
  exact source rather than reconstructing a second Markdown tree.
- **Confirmed and fixed in PR review:** Foundation drops empty link labels
  without a run, hiding their destinations. Detect only empty-label source
  prefixes (including linked empty images) that no parser source span covers,
  and preserve the whole input literally. Tests cover inline and reference
  targets, whitespace/newline labels, linked empty images, code examples,
  standalone images, and mixed represented/unrepresented occurrences.
- **Confirmed and fixed in PR review:** Quoted headings, code, and breaks lost
  their quote context because only paragraphs used the quote style. Wrap every
  supported leaf in its quote containers, in source order relative to list
  markers. Tests cover quoted code whitespace, headings, breaks, nested quotes,
  both list/quote orders, and empty parent-item markers inside quotes.
- **Confirmed and fixed in PR review:** A containing list item took precedence
  over its leaf, turning nested code, headings, and quotes into ordinary list
  prose. Classify the leaf first, then apply its list marker and indentation.
  Raw-first items also consume their marker so following paragraphs stay
  continuations. Items beginning with a nested list emit their parent marker
  before their children and retain that identity for later continuations.
  When such an empty ancestor begins with a raw child, preserve the whole
  input as source: a same-line literal may already contain ancestor markers,
  and reconstructing that compound structure would duplicate or omit them.
  This bounded fallback preserves source; code-first children stay formatted.
  Tests cover fenced and indented code with whitespace, nested
  depth, heading-first ordered items, quotes, breaks, and raw-first HTML.
- **Confirmed and fixed:** Sibling items share an outer list identity. Group by
  the innermost block identity; track list-item identity separately for
  marker-free continuation paragraphs. Nested and sibling regression tests
  preserve order, depth, and ordinals.
- **Confirmed and fixed:** Copying runs carried the parser's list delimiter
  attribute into text. Reconstruct runs with only inline presentation intent.
- **Confirmed and fixed:** Adjacent links to the same URL collapsed into one
  destination suffix. Link source positions now distinguish separate links.
- **Confirmed and fixed:** Foundation's full parser joined words across link
  label line breaks, shortened multiline label source ranges, and flattened
  inline formatting inside links. Keep blocks with multiline links as exact
  raw source rather than guess missing characters. Reparse single-line labels
  with Foundation's inline-only mode only when the source span reaches its
  closing bracket and the visible characters match the full parse; incomplete
  spans also fall back to the entire source block. Retain only typography.
  Regression
  tests cover both paths and check that no link or image attributes survive.
- **Confirmed and fixed:** The custom regular font hid emphasis. Explicit
  italic, semibold, and mono variants are visible in the reader screenshots.
- **Confirmed and fixed:** A fixed-size italic font would ignore production
  Dynamic Type. Use the native text-style font outside the screenshot bridge.
- **Confirmed and fixed:** Splitting source only on LF corrupted bare-CR table
  fallbacks. Slice original substring ranges for LF, CR, and CRLF instead.
- **Confirmed and fixed:** Foundation's multiline HTML end position can omit
  the closing line. Its literal payload supplies the line count; the original
  source supplies the characters and line endings. Contextual table/HTML tests
  cover all three newline forms.
- **Disproved by checks:** HTTPS, JavaScript, file, and mail links cannot retain
  link attributes. Image URLs disappear while alt text remains. Inline HTML
  remains literal and cannot create bold text; script blocks retain their
  literal tags. Code preserves its content and newlines. Header-only tables
  retain their separator row. Parser failure and plain text retain the raw
  input, and oversized specifications parse only the truncated prefix.
- **Allowed by owner decision:** Agent text can contain headings or words that
  claim approval. The fixed unverified label and dashed enclosing frame identify
  all of that content as a claim; the daemon fact stays outside.

## Revisit When

Revisit when the daemon sanitizes or structures specifications itself, or when
the app gains a shared block renderer with an equivalent claim boundary.
