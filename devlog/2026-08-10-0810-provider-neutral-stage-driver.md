# Provider-Neutral Stage Driver

Chose one provider-neutral durable state machine in `internal/exec/stage`,
parameterized by the five behaviors the existing Claude implementation proves
vary by provider, over copying the machine for the second driver or adding
hooks for anticipated Codex needs. The port owns handoff construction, prompt
rendering, stable run and workspace identity, and the preparation-failure
status. Persistence, reconstruction, recovery, import, and result delivery
remain shared.

Kept `claude.Config`, `claude.New`, `claude.Driver`, execution replay, and the
existing sentinel identities as a compatibility adapter. Provider display and
ordinary error text are core configuration, while the historical Claude-named
sentinel text is isolated in `stage/compatibility.go`; changing those errors
during a behavior-preserving extraction would have changed observable failure
text and broken existing `errors.Is` consumers.

The reconstructed intent remains an untrusted authority. The move preserves
strict decoding, current-admission authentication, provider-derived run and
workspace re-gating, input digest checks, vendor-instruction checks, and the
fixed preparation-command gate. A pre-extraction fixture pins the persisted
JSON shape and omitted-preparation behavior.

Automated review exposed a new returned-object trust boundary at the provider
port: ward can validate a handoff's internal shape, but only the shared stage
core can compare its run, ward workspace, seed/base, image, egress,
instructions, and auth lease with the authenticated durable intent. The core
now clones every reference-bearing provider input and output before re-gating
those provider-neutral bindings. This was chosen over documenting provider
honesty or relying on ward validation, neither of which can prove agreement
with the admission the stage persists and later recovers.

The refute-first pass disproved the first narrow run/seed/base gate by showing
valid lease, image, and instruction substitutions, then disproved the widened
gate through retained output pointers and the pointer-rich `StageInputs`
input. After both sides were detached and the ward workspace relation was
cross-bound, the final enumerated pass found no remaining mutable alias or
directly representable provider-neutral binding that could be substituted.
Review then exposed the earlier provider-return boundary at state lookup: a
derived run ID reached intent and seed paths before ward validated a handoff.
The core now applies ward's single run-ID contract before the first state read,
caches that result for start construction, and validates every later persisted
or cleanup use rather than treating provider derivation as path authority.
The same boundary requires exactly one credential mount for the admitted
identity: ward authenticates that sole mount to the lease, so a provider cannot
append an unrelated read-only credential volume outside the admission binding.
Its target, manifest, and writability must also match the provider's immutable
composition policy; the current `subscription_contained` core rejects a
writable policy at construction.

Refute-first verification compared the pre-extraction implementation at
`b2d6f77` with the extracted core over 17 structured intent mutations and 512
deterministic byte-level mutations. Both produced decision digest
`3dcbff4d0c0fc67099072fcb92ce5e9960fd4623b7645e25df57667364e17151`
and structured verdicts `10000001010000000`. This rejected the hypothesized
regression that moving the reconstruction boundary changed an acceptance or
refusal decision. The compatibility sentinels' provider-named text is accepted
by decision, not generalized: preserving its identity and text is part of the
existing Claude contract, and the exception is isolated in one file.

Rejected moving the daemon composition adapters out of `cmd/freesided`: they
are already provider-neutral and reusable by a second driver, while relocating
them would widen this contract change without strengthening the extracted
seam.

Revisit when a provider needs a sixth behavior: require a concrete provider
invariant that cannot be expressed through the existing durable inputs or
configuration before widening the port.
