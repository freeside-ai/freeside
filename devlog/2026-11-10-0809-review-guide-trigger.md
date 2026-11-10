# Agents Write a Review Guide When the Change Has Something to Judge

The implementer and remediator prompts now tell the agent when to write
`.freeside-evidence/review-guide.json`, where they used to say only
"Optionally". The owner chose the rule in chat on 2026-11-10 after an exit
run for #2753 found that the real run's implementer wrote no guide: the
prompt gave it no signal for when one helps.

## Decision

- **Chose the guide's own attention kinds as the trigger over a size
  threshold.** The agent writes a guide when the change has at least one
  place the owner must judge, of a kind `attention` already names
  (`tradeoff`, `contract`, `product`, `risk`, `accessibility`), or when its
  files read best in an order other than the diff's. It skips the guide
  when the diff reads clearly top to bottom with nothing to judge, such as
  a docs fix, a rename, or a bug fix in one spot. Rejected: a line-count
  or file-count threshold. A large mechanical rename needs no reading
  order, and a ten-line change to a contract does, so size predicts the
  guide's value poorly.
- **Kept the guide optional to the daemon.** This is prompt guidance only.
  A run with no guide still produces a ready item, as
  `2026-11-07-1021-review-guide-on-ready-card.md` decided, and a malformed
  guide still fails the import closed, as
  `2026-11-07-1832-review-guide-producer.md` decided. The prompts keep
  "Leave the file out rather than guess", now aimed at the guide's shape
  and holding even when the change warrants a guide, so an agent unsure
  of the shape still omits it. Rejected: making the guide required when
  the trigger holds. The daemon cannot decide whether a change has
  something to judge, so a required guide would rest on the agent's own
  claim and turn a missing reading aid into a failed stage.

## Revisit When

Exit runs show agents writing guides for changes with nothing to judge,
or still skipping them for changes that have a contract or risk stop.
