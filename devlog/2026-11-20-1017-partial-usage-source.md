# Record a Killed Claude Invocation's Usage as a Partial Source

A Claude invocation that never reaches its terminal `result` envelope used to
record no usage, so failed and lost attempts looked free (#3410). Freeside now
sums the usage of the transcript's assistant messages and records it under a
new usage source, `partial_transcript`. Coverage (`complete`, `partial`,
`missing`) is derived at read time from a run's admissions and observations.

## Partial Is a Source Value

Chose a new `domain.UsageSource` value over a metric-name suffix
(`input_tokens_partial`) and over a new `partial` column, because the source
already names the route that reported a measurement, and a partial sum is a
different route: the transcript's messages rather than its terminal envelope.

- **A metric suffix** hides a flag inside a quantity's name. Every reader that
  groups by metric would have to parse the name to learn that two rows count
  the same tokens.
- **A new column** needs a migration and a domain field to carry one flag,
  and every reader would have to check it beside the source it already reads.

The `usage_observations.source` column is free text, so the new value needs
no migration. A binary from before this change refuses a database holding a
`partial_transcript` row at open (the stored-row check), so a plain downgrade
fails loudly; `freesided undo-update` restores the pre-update copy.

## Output Tokens Are Left Out of a Partial Sum

The pinned CLI (2.1.319) transcript was captured against a loopback mock whose
`message_start` and final `message_delta` reported different usage. It
confirmed the plan's assumed shape: one assistant line per content block,
every line of one API message sharing `message.id` and repeating the same
usage, so each id counts once. It also showed that the usage each line
carries is the message's usage at the start of its stream. Input and cache
counts are final there, but `output_tokens` is the stream's starting
placeholder (1), and the CLI never writes the final count to the transcript.

Chose to leave `output_tokens` out of the partial sum, so the record shows it
as unobserved, over recording the placeholder sum as a lower bound. The
placeholder counts messages, not output, and a reader comparing it with a
complete invocation's output would be misled by orders of magnitude. The
issue's acceptance asked for the output sum too; this is a departure the PR
records. A message still streaming when the writer is killed writes no line,
so its input and cache tokens are missing as well; the sum is a lower bound.

A partial sum includes subagent messages (lines with a `parent_tool_use_id`),
which are real spend. Whether a terminal envelope's `usage` counts them was
not established, so partial and complete totals are not proven comparable;
#3416's report should not treat them as equal.

## Missing Is Derived, Not Stored

An admitted invocation with no observation is `missing`. Chose deriving that
at read time from the run's execution admissions, review records, and review
failures (the set `BillableCostSoFar` already counts) over storing a marker,
because absence has no moment to record it, and a stored marker would need
reconciling with a later observation. An invocation with any terminal-source
observation is `complete`, and its totals come from those rows alone, so a
partial reading never adds to a complete one.

Revisit when a failed writer's transcript over `ward.MaxFailureTranscriptBytes`
(4 MiB) proves common, since those stay `missing`, or when the CLI starts
writing final message usage to the transcript.
