# External Finding Replies: The Operator's Reason

Work unit: #2352. Mandatory note: it changes the API contract
(`DecisionPayload.finding_routes`) and records owner choices made in chat.

## Decision: One Reason per Replying Finding, Required

A human `adjudicate_findings` decision now carries a `reason` on each
`FindingRouteChoice`. It is required on exactly the choices that post a reply,
a decline or defer of a finding whose dispute row has `external`, and refused
on every other choice. Signet trims it and stores the trimmed text, non-blank
valid UTF-8 of at most 2048 bytes, in the human adjudication entry's
rationale. The reply prints it under `Reason:`.

Chose a per-finding field over one decision-wide `message`. Each reply goes
to a different reviewer thread, so one shared message would post the same
text under unrelated findings, or force the operator to write around that.
Chose required over optional because a decline with no reason tells the
reviewer nothing, which is the gap #2352 exists to close.

Chose a new `FindingRouteChoice` schema over adding `reason` to
`AlternativeChoice`. `choose_alternative_route` and the item conclusion keep
`AlternativeChoice`: the conclusion records routes, and the reason already
lives in the adjudication entry.

A reasonless decision keeps its stored message byte for byte (`omitempty`),
so existing records, replays, and the store's parent-key check stay valid.

## Decision: Screen the Reason at Submit and Again at Reply

Signet screens a replying reason with the same `github-issue/1` ruleset the
reply uses and refuses a failing one with a 400; the reply screens it again
before posting. The plan rejected screening at submit, on the assumption that
signet would need the run's issue-text ruleset. The replier uses the fixed
built-in ruleset, so that assumption does not hold. A fresh-context review
found the cost of screening only at reply: a reason with a mention was
accepted, then its reply was refused for good, so the reviewer got no reply
at all, where a reasonless human decline had always posted one. Screening at
submit lets the person rewrite the reason. Chose it over falling back to a
reasonless reply, which would drop the reason without telling anyone. The app does not mirror the
screen (secret detection, message rules, mentions, slash commands); it shows
the daemon's refusal.

## Departure: The Disposition Reason Keeps Its Digest

The plan (step 8, acceptance 4) had `ExternalFindingDisposition.Reason` drop
its `(finding adjudication <digest>)` suffix and carry the reason. That
record is write-once with a byte comparison, and publication continuation
re-derives every route's disposition on replay, so a new format would make
replays of existing runs conflict. Since #2369 the reply text builds from the
adjudication entry, not that field, so the reply prints the operator's reason
with no digest without touching the disposition. The acceptance's intent holds;
its wording about the field does not.

Known edge: a reason that is exactly the placeholder rationale `Human decision
recorded.` reads as no reason, and the reply omits it.

## Owner Choices

- Thread resolution after a reply moved to #2358; #2352 keeps the reason
  path.
- #2352 is `kind:contract`, since it changes the decision payload schema.

Revisit when the disposition record gets a versioned format, or when a
reviewer needs one reply covering several findings.
