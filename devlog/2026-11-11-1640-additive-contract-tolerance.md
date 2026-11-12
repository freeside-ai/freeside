# Responses Tolerate Additive Change; the Daemon Advertises Its Features

Issue #1266, under plan revision 116 (#3102, whose
[note](2026-11-11-1630-graceful-updates-and-version-skew.md) records the
owner decisions of 2026-11-11 on version skew), and plan revision 117, which
this unit carries. Contract and safety policy, and
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
- **Every enum a response reaches lists `unknown`, and the app decodes an
  undeclared value as it.** The listed member alone doesn't help: the
  generated Swift decoder still throws on a value it doesn't declare. The
  app conforms each such enum to `UnknownTolerantEnum`, which decodes any
  other string as `unknown` and still refuses a non-string. The
  conformances are generated from the client by a script the generate step
  runs and drift-checks, so a new response enum can't be left strict by
  hand. Rejected: hand-written conformances, which a new enum silently
  skips.
- **`unknown` never grants.** Wherever a value enables a control, marks
  something verified, or hides a warning, `unknown` takes the cautious
  branch: an unknown posture blocks, an unknown producer reads as
  unverified, an unknown requirement state reads as failed, an unknown
  decision type can't be submitted, and an unknown effect kind can't be
  approved. The app never sends `unknown` back: a route it doesn't know is
  never offered, so a finding offered only such routes can't be routed
  until the app updates. The mock server refuses a request carrying
  `unknown`, and the daemon, whose enums don't register it, refuses it too.
  One consequence: an older app editing a routing policy
  that holds a role it doesn't know sends `unknown` and gets a refusal,
  not a silent loss of the role.
- **A cache another contract wrote is dropped.** The cache records the
  contract digest it was written under; a cache from another contract keeps
  only its command ledger and telemetry queue, so the app never shows a
  state decoded under rules it no longer applies. The load reads the
  format and digest first and never decodes such a file's snapshots, so a
  snapshot lacking a field the new contract requires can't take the ledger
  with it. Rejected: decoding the old cache tolerantly,
  which would keep values the new build reads differently.
- **The daemon names its features on `/health`.** An absent list means no
  named feature is served, so a daemon older than the list reads as serving
  none. The build's registry is forced onto the response, so a caller's
  configuration can't advertise a feature the build lacks. The app reads
  `/health` each sync round, alongside the round's reads with a short
  timeout, because a restart onto a new build changes neither the sync epoch
  nor the revision; a failed read keeps the last good list, and the list
  starts empty, so a gated control appears only once a read lists it. The
  parked-task move is the first gated control.
- **An unserved command kind gets a typed 400.** `POST /commands` documents
  a 400 whose `reason` is `unknown_command_kind`, so an older daemon's
  refusal of a newer app's command reads as "update the daemon", not as a
  generic failure; any other 400 keeps the site's own message. The refusal
  returns before any service call.
- **`/health` readers tolerate added fields too.** `freesided`'s rig probe
  decoded `/health` strictly, so the new field would have hidden the
  occupant's version from its description; it now ignores unknown fields.
  The plan's unit contract missed this reader.
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
  this PR carries, as revision 117.
- **The policy check is a custom function for the pinned linter.** It adds
  no tool.

## Limits

- **A new arm in a response union is breaking.** An app can't render a
  payload shape it has never seen; #3109's compatibility level covers it.
- **A one-value enum is a constant.** The rule asks for `unknown` only on
  an enum of two or more values; widening a one-value enum, such as a
  rejection's single reason, is breaking, like a new union arm.
- **The typed refusal needs a daemon that sends it.** A daemon built before
  this unit refuses an unserved kind with a message and no reason, so the
  app shows that site's own refusal message instead of the update prompt.
- **Examples get a weaker check.** With schemas open, the linter no longer
  rejects an example that carries an unnamed field. The daemon's golden
  tests still pin the real shapes.

## Refute-First Record

An independent reviewer tried to prove the `http.go` and `/health` changes
wrong before they were committed.

- **Confirmed, fixed: the rejection schema promised every 400 repeats.** Only
  `unknown_command_kind` does; other 400s, such as a snoozed proposal, may
  clear when state changes. The description now says so, and its example
  carries the daemon's real message.
- **Confirmed, fixed: a kind registered without a dispatch arm would read
  as unserved.** The dispatch switch kept a `default`, which the exhaustive
  linter accepts as covering every kind, and the app now drops a command so
  refused. The switch has no default; the refusal follows it, and the
  linter was shown to flag a removed kind.
- **Confirmed, fixed: the rig probe's strict `/health` decode.** Found while
  listing `/health` readers; its description now survives an added field.
- **Disproved: an unknown kind changes state.** The refusal returns before
  any service call, behind the bearer check, the strict envelope decode, and
  the device check; a test shows the revision unchanged.
- **Disproved: the route or bearer order moved.** No route registration
  changed; `/health` adds one static per-build list.
- **Disproved: the forced feature list aliases mutable state.** Nothing
  writes the registry slice or the response's copy of it.
- **Disproved: request strictness weakened.** Neither new schema appears in
  a request, and tests send an unknown payload field and an unknown
  selection field and get 400.
- **Disproved: the relaxed probe decode lets an occupant mislead.** The
  description only feeds error text and gates nothing, and an occupant
  could already forge a strictly valid body.
- **Allowed by decision: unpaired callers learn the feature names.** Plan
  revision 117 records it.

A fresh-context review of the whole branch before push confirmed two
defects, both fixed: a cache from another contract still decoded its
snapshots, so a newly required field lost the ledger; and a finding route
the app didn't know was offered and sent back as `unknown`. It found no
`unknown` branch that grants and no loss of request strictness.

Revisit when a response union needs an arm the app can skip rather than
refuse, or when the generator gains a way to decode an unknown field into a
catch-all without the `additionalProperties: true` property.
