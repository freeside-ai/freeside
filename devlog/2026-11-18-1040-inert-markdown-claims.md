# Inert Markdown in Published Claims

Issue #3054, deferred from #3022's refute-first review. A recorded claim in a
published pull request body rendered live Markdown on GitHub: GitHub parses
Markdown inside a raw-HTML `<code>` element, and `dispositionCode` escaped
only HTML. A reviewer message such as `see [docs](https://evil.example)
**now**` showed as a link and bold text in Verification.

Chose a second, stricter claim renderer for the body's sections over changing
`dispositionCode`, because the publication intent binds the disposition
history's bytes by digest, and a changed history would fail recovery of
every intent written before the upgrade.

## Decisions

- **Encode per character, by GitHub's behavior.** `inertClaimUnits` renders
  each character on its own, so truncation keeps whole units and never
  splits an escape. The set comes from GitHub's renderer (`POST /markdown`,
  mode `gfm`, 2026-11-18), not a specification:
  - `` \ ` * _ ~ [ ] $ `` become numeric references. Each takes part in a
    construct GitHub still parses inside `<code>`: backslash escapes,
    nested code, emphasis, strikethrough, links, images, footnotes,
    reference links, and math.
  - `:` becomes `&#58;`. It breaks bare URL autolinks (`https://`,
    `ftp://`), `mailto:`, and `xmpp:`.
  - `@` becomes `<wbr>@`. GitHub links an email address after it decodes
    references, so no entity stops it; a raw-HTML split does, and GitHub's
    sanitizer drops the `<wbr>` from the page.
  - The `.` after `www`, in any letter case, becomes `&#46;`. GitHub
    matches a `www.` autolink on the raw text. Every other `.` stays plain,
    so paths and sentences keep their readable raw form.
  - `\n` and `\r` render as `&#92;n` and `&#92;r`, which the reader sees as
    `\n` and `\r`, as before. Everything else goes through
    `html.EscapeString`, as before.
- **Two renderers, not one.** `dispositionCode` and `boundedClaim` keep
  their bytes. The disposition history and the external review reply use
  them: the publication intent binds the history's digest
  (`TestPreChangeIntentStillValidatesOnRecovery`), and a reply intent binds
  `TextDigest` of its text. Verification uses `inertClaim`; Advisories and
  Scope Decision use `boundedInertClaim`, which keeps the content-digest
  note. The plan proposed rewiring `boundedClaim` itself; that would also
  have changed the external review reply, so the change adds a renderer
  instead.
- **The external review reply is a follow-up.** A model's rationale in a
  reply has the same gap, but #3054's contract covered the body's sections,
  and switching the reply's renderer refuses a reply whose intent predates
  the upgrade. Follow-up: #3380.
- **`<pre><code>` needs nothing.** The agent-reported implementation block
  in `daemon/internal/engine/publication_metadata.go` sits in an HTML
  `<pre>` block, which GitHub does not parse as Markdown; the same hostile
  text rendered literal there.
- **Caps stay in rendered bytes.** An encoded character takes five or six
  bytes, so a punctuation-heavy claim shows fewer characters before
  ` (truncated)`. Each section's ceiling is unchanged.

## Accepted Costs

- **Noisier raw bytes.** A reader of the raw body (the API, an email's
  plain-text part) sees `&#95;` and `&#58;`. The rendered page is unchanged.
- **Recovery against a PR merged across the upgrade.** An intent written
  before the upgrade whose PR a human then merged recomposes a different
  body, so recovery stops with a publication conflict. It needs a crash
  between creating the PR and recording the outcome, and it fails closed;
  #1553, #2349, and #3022 accepted the same window. Open PRs get the new
  bytes the next time the publisher converges them.

## Refute-First Record

A fresh-context reviewer tried to refute the change before commit, rendering
its attempts through GitHub's `POST /markdown`.

- **Confirmed, fixed: two absence checks went vacuous.** The scope
  decision's credential-leak test looked for `ghp_`, and a clean
  Verification test looked for `sha256:` and an underscored id. An inert
  claim shows those as `ghp&#95;` and `sha256&#58;`, so the checks could no
  longer fire. Both now check the unescaped text.
- **Confirmed, fixed: nothing pinned the Scope Decision wiring.** Reverting
  it to `boundedClaim` passed every test. Its test now asserts an encoded
  declared path and answer.
- **Disproved by render:** `www.` in any case or after `(` or `*`; scheme
  autolinks in either case; `mailto:`, `xmpp:`, and bare email; fullwidth
  and ideographic lookalikes; a zero-width space inside `www.`; overlong
  UTF-8 forms; entity-like input such as `&#91;` and `http&#58;//`; raw
  HTML and section markers; `\v`, `\f`, NEL, U+2028, and U+2029 (none
  starts a line); `[!NOTE]`, `\(`, and `\[`; and adjacency across the
  `</code>` boundary, since publisher text beside a claim never supplies
  `.`, `@`, or `://`. Mentions, issue references, and emoji shortcodes were
  already inert inside `<code>`.
- **Disproved by reading: truncation.** A unit is at most six bytes and
  each is checked against the remaining budget; the smallest claim limit
  (256 bytes) exceeds the longest suffix, so the budget never goes
  negative.
- **Disproved by test: bound bytes.** `dispositionCode`, `boundedClaim`, and
  `truncatedClaim` are unchanged; the history golden, the recovery test,
  and the external reply goldens pass unedited.

Revisit when GitHub adds an inline construct or changes where it decodes
references: the live render in the PR's Verification is the check to rerun.
