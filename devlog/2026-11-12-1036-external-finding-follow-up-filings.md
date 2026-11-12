# External Findings Propose Follow-Up Filings

Issue #1787. It lifts decision 5 of #1767
([note](2026-10-06-1046-external-finding-adjudication.md)): a deferred or
separate-work finding that an admitted reviewer outside Freeside left now
proposes a follow-up issue, as Freeside's own findings do. Tier A by path
(`internal/store/effect_proposal.go`) and by category (untrusted reviewer
text changes what gets published), so a mandatory note.

Chose to quote the reviewer's words inside fixed Freeside framing over
copying them as written or proposing no filing, because a filing reaches
the forge under Freeside's identity, and a deferred external finding
otherwise had a durable outcome and no tracked follow-up.

## Decision

- **The skip in `planFollowUpFilings` is gone.** Both engine paths already
  reached it: the deferred path writes the external disposition and
  proposes in one transaction, and the separate-work path proposes once the
  card is accepted, before the round ends on its handoff item.
- **The store's source gate reads the stored finding row to choose the
  disposition record.** For a deferred source on an external finding,
  `gateFollowUpFilingSource` requires an `ExternalFindingDisposition`
  deferred under the same adjudication digest; any other finding keeps the
  review-disposition check unchanged. Nothing in the proposal says which
  kind of finding it names. A source naming no stored finding is a source
  mismatch.
- **Admission needs no check of its own.** Reading the adjudication re-proves
  that its cycle admits every external finding it names, and reading the
  external dispositions re-proves the same per row, so a finding the cycle
  doesn't admit fails the entry check. Both reads use the profile the
  cycle's sealed authority names, not the active one, so the gate still
  reads only append-only rows and an allowlist edit leaves old cards
  readable.
- **"Current" means the latest external row.** `gateFollowUpFilingSourceCurrent`
  accepts a deferred external source while the finding's highest-round
  external disposition is deferred under the source's adjudication.
  External dispositions have no drift reversals, and #1767 decision 6 starts
  no second cycle for an answered finding, so today there is only one row.
- **No domain change.** `domain.FollowUpFilingSource` (finding, adjudication
  digest, kind) already names an external finding; the stored row says which
  it is. No new source kind, field, or migration.

## Framing

- **Title:** `External review finding: "<first line>"`. The first non-blank
  line, whitespace collapsed, cut to 160 bytes, then `strconv.Quote`d.
- **Body:** a fixed lead naming the reviewer login (quoted, no `@`) and
  saying the quoted text is the reviewer's words, cut to at most 4096 bytes,
  and that Freeside did not write it; then `Reviewer's words:` followed by
  the `strconv.Quote`d message inside an inline code span whose backtick
  fence is longer than any backtick run in the quote; then `Location:` and
  `Adjudication rationale:` as for Freeside's findings. 4096 bytes matches what the adjudicator's input carries
  (`externalFindingModelTextLimit`), so the filing shows the words that were
  judged.
- **Quoting escapes every newline and control character.** No reviewer line
  can start a `/command`, a heading, or a code fence; the whole message reads
  as one line in the issue. The code span exists because GitHub renders
  the `\"` that stands for a reviewer's quote mark as a bare `"`: without
  it, a reviewer could close the quote on the rendered page and make the
  rest of their message read as Freeside's. Inside the span no escape,
  emphasis, link, or HTML is read, so the page shows the quoted string as
  written. The title is left unwrapped, on the reading that GitHub shows
  title escapes as written; that reading was not checked on a live issue.
  An `@name` inside the quote fails the github-issue/1 screen.
- **External fallbacks carry no reviewer text.** A blank message, a field
  the screen refuses, or a title whose escapes push it past 256 bytes takes
  a fixed external title or body. The body points at the pull request's
  review thread, because an external finding never appears in the run's
  review record, where the existing fallback points.
- **Freeside's own findings compose byte-for-byte as before.** The text
  function was split into an internal and an external draft. An old-versus-
  new comparison over 20,000 generated inputs (mentions, commands,
  `freeside:`, a token, controls, U+2028, CRLF, over-long text) found no
  difference.
- **`internal/engine/follow_up_filing.go` joins the Tier A path list**,
  because it composes untrusted text into what a person approves for
  filing.

## Refute-First Results

A fresh-context reviewer, given only the diff and the intended outcome,
tried to prove the change wrong.

- **Confirmed and fixed: a reviewer's quote mark could close the quote on
  the rendered page.** The fix is the code span above, pinned by a test
  whose message closes its own quote and adds a sentence about Freeside.
- **Allowed: `Location:` and `Adjudication rationale:` stay unquoted.** The
  path names a file in Freeside's own published head, and the rationale is
  Freeside's model output. Both carry the exposure they already carry for
  Freeside's own findings, whose filing text this unit does not change.
- **Confirmed, test comment fixed: three refusal cases are caught by the
  older entry check.** A declined row needs a decline entry, and an
  adjudication cannot name a finding its cycle does not admit, so those
  cases never reach the external branch; the test now says so. The two
  cases that do reach it (a deferral with no row, a row under another
  revision) fail when that branch is replaced with an always-true check.
- **Not testable today: "the highest round wins".** A second external row
  for one finding needs a second cycle answering it, which decision 6
  prevents, so no fixture can store one.
- **Disproved:** the table choice comes from the stored finding row; the
  internal predicate and its currency check are unchanged; every refusal is
  a source mismatch or stale; reads stay append-only; fallbacks carry no
  reviewer text; and Freeside's own text is byte-identical (the comparison
  above).
- **Allowed: every read of an external-sourced filing loads and re-checks
  the whole external disposition table.** That is the existing reader's
  behavior and a small table; a bad row there fails the read loudly, which
  is the store's rule for corrupt rows.

## Revisit When

- A second external review cycle can disposition an already answered
  finding (#1767 decision 6 changes): check that "latest row" is still the
  right meaning of current.
- A live issue shows GitHub rendering a title's `\"` as a bare quote mark:
  the title would need the same protection as the body, or a fixed title
  with no reviewer text.
