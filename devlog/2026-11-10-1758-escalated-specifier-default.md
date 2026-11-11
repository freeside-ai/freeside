# The Real Run's Default Sets an Escalated Specifier

Owner decision of 2026-11-10, made in session after #3024 merged. It
revises the default real-run lineup in
`devlog/2026-11-10-1639-exit-run-real-lineup.md`, which left
`writers.specifier_escalated` absent. The rule lives in
docs/production-walkthrough.md, Choose The Real Run's Writers.

## Decisions

- **Escalated specifier: `claude-fable-5-1` at `medium`.** Chose to set it
  over leaving it absent. It follows
  `devlog/2026-11-03-1039-specifier-triage.md`: Opus 5.5 for most
  specifications and Fable 5.1 for the hardest, picked by the triager.
  Setting it means a real run exercises triage, and a hard target gets the
  larger model. The other default picks are unchanged.
- **`medium`, not the triage note's `high`.** The owner picked `medium`,
  matching the rest of the default lineup; the triage note's Fable 5.1 at
  `high` was the deployment pick for every escalated task, while this
  default only has to carry an exit run. A tracker that needs `high` pins
  it.

## Evidence

- Fable 5.1 needs Claude Code 2.1.251 or newer, and 2.1.282 ran it
  (`devlog/2026-10-22-1558-cli-pin-reproof.md`). The pin is now 2.1.319,
  so it should serve Fable too, but no probe has shown that.
- The triager is a judgment role, and any triage failure escalates, so a
  run with judgments off would send every task to Fable. The walkthrough
  says to keep judgments on with the escalated specifier.
- The lineup probe doesn't launch the escalated specifier
  (`writerSelections` lists only the three base writer roles), so a
  refused escalated model would surface only when triage first escalates.
  #3037 tracks probing it; the walkthrough names the gap until then.

Revisit when triage shows Fable at `medium` too weak or too costly for the
tasks it escalates.
