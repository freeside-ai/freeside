# One Credential Rule List for Both Scanners

#2773 moves the importer's and the export scan's credential rules into one
leaf package, `daemon/internal/credentialpattern`, and gives the importer
the export scan's broader GitHub token rule. This note records why, and
what the refute-first pass found.

## Decision

- **Chose one shared list over a test that pins two copies.** One copy
  can't drift; a pin test only notices drift after someone writes it, and
  the two lists had already differed since each was written (2026-07-16
  and 2026-07-28) with no record of why. Each rule carries the importer's
  id (persisted in import findings), the export scan's refusal name (shown
  to the operator), and the export scan's pattern.
- **Chose the export scan's GitHub rule, `gh[pousr]_[A-Za-z0-9]{16,}`, over
  keeping the importer's `\bgh[pousr]_[A-Za-z0-9]{36,}\b`.** The export
  scan was already the stricter list and refuses a handoff first, so handoff
  behavior doesn't change. But `importer.ContainsSecret` alone screens
  inference replies, specification text, and publication prose, and on
  those paths three shapes passed: a token glued to a preceding word
  character (`GH_TOKEN_ghp_...`), one followed by `_`, and one 16 to 35
  characters long. The reasoning is the Anthropic rule's
  (`devlog/2026-07-30-0952-anthropic-secret-rule.md`): a token embedded in
  another string is still dangerous, and a word boundary is an unsupported
  source of false negatives. The 16-character floor keeps a bare prefix and
  short mentions out.
- **Kept every other rule's pattern byte-identical**, including the word
  boundaries on `github_pat`, `slack_token`, and `aws_access_key_id`.
  Revisiting those is out of scope; no evidence here shows a miss.
- **Listed `internal/credentialpattern/` as Tier A** under "Import and
  handoff": it decides what the export scan refuses.
- **Accepted the cost.** The broader rule refuses any `ghp_`-style run of 16
  or more letters and digits, even inside a longer word. Agent replies and
  prose aren't in the repository, so the planning grep can't rule out a
  legitimate refusal there. Such a refusal fails closed on paths that
  already fail closed, which is the trade this note makes.

## Refute-First Findings

- **Disproved by check: the export scan's decisions change.** Old and new
  lists, reconstructed from `cf5d493b3`, ran over 2,000,000 generated
  inputs seeded with every rule's prefix and near-misses. The export scan's
  refuse-or-pass decision matched on every input, and every pattern but
  the importer's GitHub rule is byte-identical to its old copy.
- **Allowed by decision: a refusal's name can change.** The export scan now
  checks rules in the importer's order (Anthropic, GitHub, GitHub PAT, AWS,
  Slack, PEM, GCP) rather than its own. A refusal still happens, but a chunk
  holding two credential classes can report the other one's name. The run
  found 2 such inputs in 2,000,000, both multi-class; no single-class input
  changed name.
- **Confirmed: the importer flags more.** It differed only on the GitHub
  rule, only where the old importer rule missed, and only where the old
  export rule matched (333,471 of 2,000,000 inputs). That is the intended
  change.
- **Disproved by check: the broader rule refuses literal repository text.**
  On `cf5d493b3`, `git grep -E 'gh[pousr]_[A-Za-z0-9]{16,}'` minus the old
  rule's matches leaves only deliberate token fixtures in seven test files
  and `publish/testdata/token-response.json`. None feeds a `ContainsSecret`
  screen that expects the text to pass.
- **Confirmed: the grep misses fixtures built in code, and some were sized
  to the old floor.** The publication screens' splice tests
  (`engine/publication_render_v2_test.go`, `publicationtext/screen_test.go`,
  `inference/publication_author_test.go`) build `ghp_` + 18 characters,
  a markup construct, then 16 more, so that only the rejoined visible text
  reached the old 36-character floor. Under the shared rule the first
  piece is a token by itself: the benign interior-newline case failed, and
  every reject case passed without exercising reconstruction. The fixtures
  now use 8 and 6 characters, the same shape against the 16-character
  floor, and a check confirmed each raw body passes the rule while its
  rejoined text matches. The opt-in live oracle's `renderedPATRe` follows
  the rule's floor (`{16,}`). The planning contract had said no such
  fixture existed; these test files joined the unit's scope.
- **Fixed: no test pinned refusal names.** A fresh-context review found a
  renamed or swapped `Name` would pass every test. `TestRuleNames` now pins
  the seven names `patternName` used to return.
- **Declined: shared compiled patterns are mutable.** `All()` copies the
  slice, but a caller calling `Pattern.Longest()` would change both
  scanners' matching. No caller does; the review found none.

## Revisit

The Anthropic note's revisit trigger, "when the ward and importer scanners
gain a shared rule registry", has fired with this change; its decision
stands, now in the shared list.

Revisit when a third scanner needs credential rules, when a vendor changes
a token's prefix or length, or when a legitimate refusal from the broader
GitHub rule shows up on a screen path.
