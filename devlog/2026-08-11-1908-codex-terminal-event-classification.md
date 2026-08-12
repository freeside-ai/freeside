# Bind Codex Failure Routing To The Terminal Event

Work unit: #492. Mandatory note: returned-object trust boundary. The external
Codex JSONL stream supplies the field that selects durable failure routing.

Chose the last successfully decoded `turn.failed.error.message` over scanning
the merged transcript. Earlier agent output, tool output, and stderr are
history, not authority for the terminal classification. Using only the last
decoded failed turn also keeps multiple-turn transcripts deterministic: a
later failure replaces an earlier one as the routing input.

Chose tolerant per-line decoding over rejecting the complete event stream.
The stream intentionally merges JSONL stdout with stderr and is read under a
size cap, so non-JSON lines and a truncated final line are expected boundary
conditions. Unknown JSON fields are ignored. If no failed turn decodes, its
message is absent, or its message is unrecognized, the classifier returns
`transient`; retrying is safer than parking the run on quota or configuration
attention without positive terminal evidence.

Kept top-level `error` events outside the trusted selection. #492 specifies
`turn.failed` as the terminal authority and records top-level errors as a
non-goal until a live fixture proves that Codex uses one without a failed turn.
Such a transcript therefore defaults to `transient` rather than widening the
authority accepted by this unit.

## Refute-First Verification

An independent fresh-context pass attacked mixed transcript history, terminal
selection, malformed and truncated lines, interleaved stderr, unknown fields,
and disagreement between the failure class and refresh-attempt label.

- **Confirmed and fixed:** refresh-attempt labeling still scanned the complete
  transcript after classification was narrowed. It now consumes the same
  selected terminal message, so historical refresh text cannot relabel an
  unrelated transient exit as an in-container credential refresh.
- **Confirmed and fixed:** duplicate or case-variant duplicate JSON object
  keys could collapse through `encoding/json`'s last-value-wins field matching
  and supply ambiguous terminal authority. Each line now passes the package's
  duplicate-key gate before decoding; ambiguous lines are skipped like other
  malformed input, and regressions cover exact and case-variant duplicates in
  the trusted message field.
- **Rejected by verification:** earlier quota, configuration, authentication,
  and refresh text cannot drive either durable class or failure label; the
  last decoded failed turn wins; malformed or truncated later lines do not
  erase the last valid failed turn; non-JSON stderr does not contribute; and
  unknown fields do not invalidate an otherwise usable failed turn. The
  table-driven regressions exercise each case.
- **Accepted by decision:** a missing or malformed failed turn, including a
  top-level `error` without one, can spend transient retries before attention.
  That cost is preferable to trusting unproven transcript text and falsely
  parking a recoverable run.

## Revisit When

A captured Codex failure proves that a top-level `error` event can be the sole
terminal record, or Codex publishes a versioned event schema with a stronger
terminal discriminator. Add the observed fixture and revise the trusted
selection explicitly rather than broadening it from speculation.
