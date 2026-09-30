# Carry the Agent's Model and Effort Into StartSpec

Work unit #1615, a contract change. It adds the fields and the one
derivation; no launch command changes. #1618 (Claude ward launch) and #1619
(judgment call launch) are the consumers, and #867's resolver is the first
production writer of an agent binding.

## Chose the Admission Binding as the Carrier

The model and effort a launch passes are recorded on
`domain.AdmissionAgentBinding` (`route_model_id`, `requested_effort`,
`native_effort`), and `exec.StartSpecFromAdmission` copies them into
`StartSpec`.

- **Why the admission.** `StartSpecFromAdmission` is the only conversion
  from an admission to a spec, and the Claude driver refuses a start whose
  spec differs from it (`cmd/freesided/claude_driver.go`). Recovery also
  rebuilds specs from the admission, never from current policy.
- **Rejected: passing the agent closure in at dispatch.** No store holds
  agents or fragments by digest, so dispatch has nothing to read, and a
  spec that carries facts the admission lacks breaks the start comparison
  and replay.
- **A stored value is never trusted alone.**
  `ValidateAdmissionAgentDerivations` recomputes all three fields with
  `DeriveAgentLaunchSelection` over the agent, adapter, and offer and fails
  closed on any difference. The resolver and the recheck share that one
  function.

## Chose Omit-When-Empty Over an Encoding Version Bump

All three fields are `omitempty`, and a launch that passes neither model nor
effort records none of them. Every existing admission keeps its bytes and
content address, so `freeside.execution.admission/v4` stays. Every existing
golden is byte-identical; a new golden pins the populated form.
`harness_default` sends nothing, so it is recorded as the absence of both
effort fields, and a binding with one effort field but not the other fails
`Validate`.

## Chose a Reserved Route Model ID for the Native Default

Names never enter a digest, so code can recognize the baseline Claude offer
(§5.4, "The baseline is honest") only by its `route_model_id`.
`claude-code-native-default` is reserved: a `claude_code` launch under it
passes no model. Any other id is passed as written.

## Chose an Identity Table for the Pinned Claude Build

The effort translation is keyed by harness client. For `claude_code` every
Freeside level maps to the same native value, and `harness_default` sends
nothing. Evidence: the Claude Code changelog records `xhigh` added "between
`high` and `max`" via `--effort`, and a `/effort max` fix, both before the
pinned 2.1.220. The pinned build was not executed. With no clamp today, the
`max → xhigh` rendering is exercised only by a constructed value.

- **Resolution's extra Claude check is inert today.** `ResolveAgentDefinition`
  refuses a `claude_code` effort the table can't send, but the table sends
  every valid level. It stays so that a pin whose table drops a level fails
  at resolution rather than at launch.
- **Codex has no table.** `TranslateEffort` refuses `codex_cli` with a typed
  error, a Codex agent derives an empty selection, and the recheck requires
  a Codex binding to carry none of the fields. Codex review keeps choosing
  its own `-m` and `model_reasoning_effort`.

## Refute-First Findings

An independent reviewer tried to break the trust-boundary change.

- **Confirmed, deferred: the recheck has no production caller.**
  `ValidateAdmissionAgentDerivations` runs only in tests, on `main` as well,
  and `StartSpecFromAdmission` copies whatever the stored binding holds.
  Unreachable today: nothing writes a binding and no launch reads the
  fields. It becomes live when #867 writes bindings and #1618 turns the
  fields into argv, so wiring the recheck is filed as its own unit rather
  than guarded here. Follow-up: #1648.
- **Confirmed, accepted: the table-unsendable resolution branch has no
  reaching test.** Every valid level translates, and source validation
  refuses invalid ones first (see the identity-table section).
- **Disproved:** existing admission bytes and ids change (every golden is
  byte-identical); strict intent decoding rejects old files (missing fields
  decode); a Codex binding or a foreign offer can carry a model past the
  recheck (the derivation checks the pinned digests and requires Codex to
  record nothing).
- **Accepted: an older daemon rejects a newer intent file.** Strict
  decoding in `internal/exec/stage` refuses the new spec fields; no such
  file exists before #867.

## Revisit When

- The ward image's `CLAUDE_CODE_VERSION` moves: re-derive the table from
  that build's `--effort` values in the same change.
- #1619's host judgment build differs from the ward pin: one table per
  client kind assumes both builds accept the same values.
- #867 writes a different id for the baseline offer: change the reserved
  constant before #867 starts.
