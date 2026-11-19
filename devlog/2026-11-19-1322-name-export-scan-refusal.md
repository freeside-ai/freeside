# Name the Rule and File When the Export Scan Refuses a Handoff

**Chose to build the refusal reason from a typed, checked refusal over
keeping the reason withheld, because the withheld reason assumed a scanner
sink that was never built, so a refused handoff left the operator nothing to
act on.** This revises one decision in
`devlog/2026-07-16-1720-ward-handoff-backend.md` (its P1 "scanner error could
leak the matched secret"). Issue #3306; found by #2382's live exit run.

## What Changed in the Decision

- **The old rule:** ward replaced any scanner error with "output scan refused
  the export (details withheld)" and expected a scanner to log specifics "to
  its own audited sink". `exportscan` has no sink, and the refused export is
  deleted, so the rule and file were lost.
- **The new rule:** the scanner returns `ward.OutputScanRefusal{Rule, File}`.
  Ward reads only those fields and checks both. A rule ID outside
  `credentialpattern.All()` and a file outside the verified export's layout
  read as unnamed. Any other scanner error keeps today's withheld reason, so
  the scanner's own text, which may quote the match, still never reaches a
  reason.
- **Unchanged:** the refusal stays content evidence, so recovery's
  committed-loss rule is the same.

## Why the Names Are Safe to Show

- **The file is mapped back, not echoed.** The scanner walks the extracted
  export, so it can only name `blobs/sha256/<hex>`, `evidence/sha256/<hex>`,
  or a fixed member. Ward maps a blob to the workspace paths or evidence
  labels the verified manifests bind to it, and names fixed members by role.
- **Agent-chosen names are screened.** A workspace path or label is
  agent-controlled and could itself be a credential. A name any credential
  rule matches shows only as its `redactPath` token; every other name is
  Go-quoted, so it stays on one log line.
- **Accepted residual:** the screen catches only what the rules match, word
  anchors included, so a name like `creds_<AWS key id>.json` is shown. The
  scan has the same blind spot: it misses that name in `manifest.json` too,
  and an export that passed would publish the path anyway. The screen is no
  weaker than the gate it reports on.
- **The rule goes last in the reason.** A failure summary keeps its head and
  tail when truncated, so a long path in the middle can't push the rule out.

## Where the Reason Goes

- **Live handoff:** the failed outcome's summary and the stage driver's
  `stage failed` log already carry ward's error.
- **Recovery after a restart:** the same refusal becomes a lost outcome,
  which can't carry a summary. The stage driver now logs ward's `LossCause`
  at error level; nothing read it before.
- **Not the card.** Showing the rule and file on the execution-failure card
  needs new failure evidence fields, which #3440 owns and starts after this
  unit.

## Rejected Options

- **Parse the scanner's error text.** It would let a custom scanner's text,
  which may quote the match, into the reason.
- **Show every name as a `redactPath` token.** Safe, but an operator can't
  recompute a token without guessing the name, which is the diagnosis the
  issue asks for.

Revisit when a second `OutputScanner` implementation appears, or when the
export layout gains a member the mapping doesn't name.
