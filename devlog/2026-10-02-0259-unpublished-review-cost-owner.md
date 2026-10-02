# Keep the Review Cost Owner Out of Published Text

Work unit #1682. The disposition history no longer renders a review round's
cost owner. This note records the publication rule, what was kept, and the
recovery behavior the change accepts.

## Decisions

- **The cost owner is recorded, never published.** Owner direction
  (2026-10-02): keep the cost owner out of PRs and other public views. A cost
  owner names a bill, and the history is written into the managed project's
  repository. Plan §7 requires the review pass to record it; a reader judging
  the review needs the provider, the configuration, and the digests, not who
  paid.
- **Chose to drop only the rendered line over removing the field.** The store
  record and the cost owner's place in the review configuration digest are
  unchanged, so existing approvals and budget attribution stand. The published
  history still carries that digest, which doesn't reveal the label.
- **Chose a test on the value over a test on the label.** The guard puts a
  distinctive cost owner in the fixture and searches every forge text the
  publisher writes for it, so a later field can't republish the value under
  another name. A check for the string `Cost owner:` would miss that.
- **Chose to keep the `freeside-disposition-history/v1` marker over a `v2`.**
  The marker names the encoding the publisher finds and replaces, and nothing
  parses the section's lines. A new version would add a second marker to
  recognize for no reader.
- **Chose no compatibility path for intents written before the upgrade.** A
  publication intent freezes the digest of the rendered section, so an intent
  the previous build committed no longer matches and recovery fails closed
  with a publication conflict. Rejected: rendering the old section for old
  intents, which would keep publishing the label on exactly those PRs. Every
  earlier change to the rendered section had the same property. Finish or
  recover in-flight publications before upgrading.

## Not Changed

- **Already published PRs** keep the line; rewriting them is a non-goal.
- **Local operator output** (`freesided auth list`, the `auth adopt` report,
  `freesided follow`) still shows the cost owner. It stays on the operator's
  machine unless pasted, which is why #1683 recommends an opaque label.

Revisit when a public surface needs to tell two bills apart, or when the API
gains a review-cost field that clients show.
