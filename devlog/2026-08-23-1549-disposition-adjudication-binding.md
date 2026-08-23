# Typed Adjudication Binding For Final Dispositions (#703)

## Decisions

- **Bind declined and deferred dispositions with a typed digest.** Chose an
  `adjudication_digest` field on `ReviewDispositionRecord` over parsing a digest
  from the free-form reason because the immutable record must make missing and
  legacy authority structurally invalid. Fixed dispositions continue to bind
  only to the remediation review that proves the finding absent.
- **Re-derive authority on every store read.** Chose resolution through
  `GetFindingAdjudication` plus run, round, finding, and route checks over
  trusting the decoded digest because the artifact getter already revalidates
  its content address, exact finding batch, instruction snapshot, approved
  specification, and resolved policy. The same gate runs on writes and complete
  disposition-table reconstruction, including publication's history load.
- **Authorize the final route from typed axes.** The artifact query checks
  whether the entry's goal relationship and compatibility admit `decline` or
  `defer` under the complete Section 7 table. It deliberately does not require
  the final route to equal the entry's recommendation: a contradictory entry
  may recommend dispute while the same typed row still admits decline as an
  operator-selected alternative.
- **Keep the binding in the persisted body.** Chose no schema migration or
  trigger because every production write and read crosses the Go re-gate, and
  the body change makes an old reason-only row fail closed. A trigger would
  duplicate the artifact authorization logic while adding no current writer
  boundary.

## Verification Findings

- Refute-first fixtures reject missing and malformed digests, dangling
  artifacts, artifacts from another run or round, absent finding entries,
  decline/defer cross-pairings, and a reason-only row inserted directly in
  SQLite. Existing artifact binding fixtures reject wrong finding batches and
  wrong instruction, specification, or policy digests before such an artifact
  can grant disposition authority.
- A valid artifact-bound decline and deferral survive restart and reconstruct
  through the same gate. Publication rendering records the typed artifact
  digest, while fixed dispositions continue to render only their remediation
  review binding.
- No production disposition writer exists at this revision, and dispositions
  remain outside sync, the API, and the app. Strict validation therefore does
  not require a compatibility writer or a cross-component contract change.

Revisit when a disposition writer can bypass the Go store boundary, or when
dispositions become sync- or API-carried and need this typed field projected
across another contract.
