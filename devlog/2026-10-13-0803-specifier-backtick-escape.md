# Tolerate One Invalid Backtick Escape

For [#1984](https://github.com/freeside-ai/freeside/issues/1984), chose
decode-boundary tolerance over a repair invocation. The observed specification
was otherwise complete; dropping the invalid escape costs no model turn and
serves both specification and discussion decoding. A repair invocation would
need persisted request state, prompt changes, iteration accounting, and crash
recovery for a presentation defect.

The tolerance covers only a backslash directly before a backtick inside a
JSON string. It does not cover other invalid escapes: a regex or path may
need its backslash, so dropping it would silently change meaning. The issue's
accepted tradeoff is that an intended Markdown backslash before a backtick
may be lost. Revisit when a real run fails on a different invalid escape;
that is evidence for a repair invocation, not broader character stripping.

Fetch URLs and model-written comment IDs remain strict. When any repair
occurred, an identifier containing a backtick fails closed, including a bare
backtick whose escape was valid while some other field needed repair. This
conservative rejection is explicitly allowed by the implementation plan.
Legacy comment IDs still come from the authenticated feedback mapping.

## Refute-First Findings

- **Input-size bypass: prevented.** Repair can shrink a payload, so the
  helper leaves oversized input untouched for strict decode to reject.
  A regression supplies one byte over the limit that would fit after repair.
- **Escape and quote confusion: checked.** Escaped backslashes are consumed
  as pairs, and escaped quotes do not end strings. Fixtures pin exact text
  for valid escapes, an escaped backslash followed by a backtick, and an
  escaped backslash followed by the malformed escape.
  An independent review compared 262,144 six-byte adversarial fragments
  against an oracle that uses `encoding/json` errors to locate only invalid
  backtick escapes. Every result matched; valid JSON stayed byte-for-byte
  unchanged.
- **Other strict errors: preserved.** Fixtures reject truncation, trailing
  objects, unknown fields, invalid Unicode escapes, invalid UTF-8, other
  invalid escapes, and an escape outside a string. Decode diagnostics retain
  the bounded prefix of the raw model answer.
- **Reviving a failed attempt: disproved by caller tracing.**
  `acceptSpecificationAttempt` checks `specificationAttemptAlreadyAccepted`
  before reading output. That check accepts any matching durable terminal
  record, including the one `recordSpecificationFailure` writes.
  Discussion acceptance likewise checks its durable completion before
  reading output. This fix changes future decoding, not recorded outcomes.
