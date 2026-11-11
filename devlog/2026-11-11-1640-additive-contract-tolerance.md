# Responses Tolerate Additive Change; the Daemon Advertises Its Features

Issue #1266, under plan revision 116 (#3102, whose note records the owner
decisions of 2026-11-11 on version skew). Contract and safety policy, and
Tier A through `daemon/internal/signet/http.go`.

Chose an asymmetric contract, tolerant in responses and strict in requests,
over keeping both sides strict, because Freeside now develops Freeside: the
daemon's contract moves many times a day and the iPhone app lags it, and a
strict response contract turned every additive change into a failed sync.

## Decision

- **Responses tolerate added fields.** Every object schema a response can
  reach drops `additionalProperties: false`; request-only schemas keep it.
  The four schemas used in both directions open too, because the app
  decodes them, and the daemon's `strictjson` decoding still refuses unknown
  fields in them on the way in. The keyword is deleted, not set to `true`,
  which would make the Swift generator add a property to every type.
- **Union arms are ordered, not discriminated.** `CommandRejection` is the
  one union without a discriminator, and the generated decoder takes the
  first arm that decodes. Strict decoding used to keep a refused line change
  from reading as a stale Stop; once open, only the arm order does, so
  `TaskLineChangeRejection` now precedes `StaleTaskRejection`. Rejected: a
  discriminator field on the rejections, a wire change for a problem the
  order solves.
- **The lint enforces the policy.** A custom vacuum function, run by the
  existing `lint` step, tells response schemas from request schemas by
  walking `paths` and following `$ref`s. It reads the unresolved document
  (`resolved: false`): vacuum resolves a custom rule's document by default,
  which inlines every `$ref` and hides which named schema a response
  reaches. A `policy` step lints fixtures that must fail each rule, so a rule
  that stops matching fails CI instead of passing silently.
- **Kept whole.** The owner kept the unit as one PR rather than the
  three-unit split its plan proposed (about 2,500 authored lines against the
  1,500 budget).

## Settled at Planning

The issue body lists four defaults the owner could veto; none was vetoed.

- **Tolerance takes effect when this unit lands.** The app has no
  digest-equality refusal to keep until #3109; it reads `/health` only to
  explain a sync read that already failed. A breaking change still fails the
  read and shows the mismatch state until #3109 replaces it with the floor.
- **The first registered feature is `parked_task_move`.** The list needs
  one real name for its gating and tests to mean anything.
- **The feature list changes the plan's `/health` sentence.** An unpaired
  caller now also learns the feature names. That is a material plan change
  this PR carries.
- **The policy check is a custom function for the pinned linter.** It adds
  no tool.

## Limits

- **A new arm in a response union is breaking.** An app can't render a
  payload shape it has never seen; #3109's compatibility level covers it.
- **Examples get a weaker check.** With schemas open, the linter no longer
  rejects an example that carries an unnamed field. The daemon's golden
  tests still pin the real shapes.

Revisit when a response union needs an arm the app can skip rather than
refuse, or when the generator gains a way to decode an unknown field into a
catch-all without the `additionalProperties: true` property.
