# Codex Review Contract Uses an Explicit Append-File Binding

Work unit: #479. Scope: `daemon/`, `devlog/`.

## Decision

**Chose an explicit, digest-bound `append_file` delivery value paired with
`AgentVendor` over deriving delivery from the vendor or encoding vendor-specific
paths in the shared contract.** Both Claude and Codex conform only with this
append-authority binding. Unknown vendors, missing bindings, and replace-authority
values fail admission through the typed
`ErrUnsupportedVendorInstructionBinding`; there is no fallback.

The binding joins the durable stage-input identity as encoding v3. Historical
v2 records had one possible meaning: Claude delivered the digest-bound bundle
through its append-file mechanism. That exact implicit shape remains readable
and normalizes to `append_file` at materialization. No implicit Codex shape is
accepted, so compatibility cannot admit the new vendor without its binding.

The contract deliberately stops before topology. Ward recognizes that Codex is
a valid contract vendor but refuses to launch it until #480 supplies and proves
the read-only `$CODEX_HOME/AGENTS.md` mount. No Codex StageDriver or workflow
execution admission is registered here; those remain #406's execution tail.

## Rejected Alternatives

- **Derive delivery solely from `AgentVendor`.** This would leave the authority
  mode outside the digest-bound input identity and make a later mechanism change
  an implicit reinterpretation of an existing admission.
- **Carry `$CODEX_HOME/AGENTS.md` or Claude's CLI flag in the shared value.**
  Those are ward topology details. The shared invariant is append authority via
  a delivered file; each vendor's fixed target belongs in its conformed ward
  implementation.
- **Invalidate all v2 snapshots.** The old shape is unambiguous for Claude, so
  refusing recovery would discard durable executions without closing a trust
  gap.

Revisit when a vendor needs a second append-authority delivery class, or when a
vendor-native target cannot be fixed and proved inside its ward topology.

Follow-up: #480, #406.
