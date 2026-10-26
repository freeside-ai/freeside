# Take The Specifier Decision From Structured Output

Chose schema-constrained structured output on specification launches over
rewording the prompt or tolerating a leading paragraph in the decoder
(#2314). The Wave 8 exit run's specifier (`claude-fable-5-1` at `high`)
wrote one prose sentence before its JSON, and `specify.DecodeOutput`
rejects prose by design, so the run stopped at its first stage. The prompt
package already said "no prose or Markdown fences" and sits at 4,095 of its
4,096-byte budget, so a rewording has no room and no guarantee. Tolerating a
paragraph widens a trust-boundary decoder for a presentation defect.

The schema rides on a new prompt-delivery member, `file_v4`, not on the
`file_v3` command: ward refuses to recover a rebuilt spec whose digest
differs, so an existing member's command can never change. `file_v4` adds
`--json-schema` only when the start's stage is the specification run's
stage, read from the durable intent, and is the `file_v3` command byte for
byte elsewhere. The schema literal is frozen in that command; a change to
it is a new member.

The decoder reads the result record's `structured_output` when it is
present and not null, and its `result` text otherwise, so a transcript from
an older member decodes as before. Both payloads pass `DecodeOutput`
unchanged, so identifier strictness, the one-form rule, and the size limits
did not move. The schema steers the model and decides nothing.

Refute-first pass (an independent reviewer, then a live probe):

- **Disproved by checks:** a number, string, array, or `{}` in
  `structured_output`, a failed record carrying one, and two result
  records all fail closed; null falls back to `result`; a backslash-backtick
  cannot reach the backtick repair through `structured_output`, because the
  record itself is then invalid JSON; no older member's command changed (the
  goldens are unchanged).
- **Disproved by a probe:** that the CLI might deliver `structured_output`
  as a string or refuse `"type":[...,"null"]`. `claude` 2.1.308 with
  `--output-format stream-json --verbose` and this exact schema put the
  prose in a text block and an object in `structured_output`, which the
  new decoder accepted.
- **Allowed by decision:** the schema does not encode the one-form rule
  (no `anyOf` of four variants), so the CLI accepts `{}` or two forms and
  `DecodeOutput` fails the stage. The plan limited the schema to keywords
  every validator enforces alike, and failing there is today's behavior.
- **Allowed by decision:** the transcript line bound rose from 2 to 4 MiB,
  because a schema-constrained record carries the payload twice. A stored
  result line between the two bounds, which needs a near-limit payload,
  now decodes on a re-read instead of failing.

Revisit when the pinned agent image's CLI (2.1.282 at this change) is shown
not to emit `structured_output` under stream-json; the decoder then falls
back to `result` and the preamble failure returns.
