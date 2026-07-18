// Package domain holds the daemon's shared domain types and their validation:
// the vocabulary every lane depends on. It is spine-owned and changes only
// through kind:contract work units. It is types and validation only, with no
// storage, no I/O, and no identifier generation; the daemon assigns ids and
// sequences and the store persists these shapes.
//
// Layout, by concept:
//
//   - enums.go        named-string enumerations and their valid() checks
//   - ids.go          identifier newtypes and Digest
//   - errors.go       sentinel validation errors (wrapped with %w)
//   - attention_item.go, attention_delivery.go   the attention model (§4)
//   - artifact.go     artifacts, provenance, and the evidence gate (§5.15)
//   - finding.go      raw findings and versioned classification (§5.12)
//   - conversation.go conversations, messages, invocations (§5.14)
//   - policy.go       resolved policy with per-key provenance (§5.12)
//   - run.go          run/stage/attempt and invocation identities (§5.3)
//   - trust_profile.go automation trust profile and workflow audit (§5.5)
//   - authorization.go candidate authorization and finding classes (§5.6, §5.8)
//
// See docs/plan.md §4 (the attention model), §5.3 (execution identities),
// §5.12 (findings and policy resolution), §5.14 (conversations and sync), and
// §5.15 (evidence and provenance).
//
// # Conventions
//
// These patterns are set here for every later lane to copy (recorded for spine
// review in the Wave 0 domain devlog entry):
//
//   - Enums are named string types with a valid() predicate and an AllX slice;
//     the zero value ("") is invalid by design and rejected by Validate.
//   - Invariants are enforced at the validation boundary, not by hiding fields:
//     types keep exported, json-tagged fields so encoding/json serializes them
//     for free (golden tests, store/api round-trips), and constructors plus
//     Validate reject illegal states.
//   - Fields that must never come from a caller (an artifact's publish_eligible,
//     a message's sequence, an item's derived timing) are omitted from the
//     input/request struct, so no input path can set them; they are computed or
//     daemon-assigned and exported only on the output type.
//   - Immutability is value semantics plus the absence of mutators; a correction
//     is a new value with an incremented version, never an in-place edit.
//   - A validity switch uses default; a switch that dispatches behaviour on an
//     enum omits default so the exhaustive linter forces new members to be
//     handled (see artifact.go).
package domain
