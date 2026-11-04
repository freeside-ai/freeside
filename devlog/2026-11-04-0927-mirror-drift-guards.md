# Reflection Guards for Two Hand-Written Mirrors

Decision record for #570. Two field lists were copied by hand with no test
that failed when a field was added and the copy was not: the handoff-journal
conversion in `daemon/internal/wardstore/adapters.go`, and the field list
`exec.ReviewRequest.AuthorityDigest` hashes. Each now has a reflection test
that fails, naming the field, until the new field is handled.

## Classification

- **Tier A by path.** `daemon/internal/wardstore/` is a listed Tier A path,
  and journal records are ward's recovery authority: a field the conversion
  drops reads back as zero on recovery. The `exec` digest test alone would
  be Tier B; the unit takes the higher tier.

## Decisions

- **Chose a test-time reflection round trip over removing the mirror.**
  `wardstore` exists so neither `ward` nor `store` imports the other, so the
  copy stays. Rejected: Go struct conversion. The top-level records can't
  convert, because their nested pointer and outcome types differ, so it
  would guard only the flat nested structs, and it turns a harmless field
  reorder on one side into a compile error. Rejected: code generation,
  which adds a toolchain step to guard about fifty fields.
- **The fill gives every string, integer, and time a distinct value, and
  fails on any kind it doesn't know.** Distinct values catch a copy that
  reads the wrong source field. Failing on an unknown kind means a new slice,
  map, float, or unexported field can't pass as zero; whoever adds one
  extends the fill.
- **Bools run in several passes, one per bit of their ordinal.** A single
  all-true pass can't tell a swapped pair of bools (`WriterComplete` and
  `CancellationRequested`, say) from a correct copy. With each bool set to
  bit *k* of its ordinal in pass *k*, every bool is true in some pass and
  every pair differs in some pass.
- **Routed `MarkStatePrepared` and `MarkInstructionsPrepared` through the
  same helpers as `toStoreRecord`.** Before, they repeated the state and
  instruction field lists inline, outside anything the round trip could see.
  The owner accepted this extraction as part of the unit.
- **Chose a test with an exemption list over changing `AuthorityDigest`.**
  Hashing the request by reflection, or by default with exclusions, would
  change the digest bytes and need a version bump, and would make a new field
  authority by accident rather than by decision. The test instead requires
  every `ReviewRequest` field to have a valid mutation that changes the
  digest, or an entry in the exemption list with its reason. `RequestedAt` is
  the only exemption, and the test asserts changing it leaves the digest
  unchanged.
- **Accepted that the `RunID` mutation can't be isolated from `Admission`.**
  `Validate` requires the admission to name the request's run, so the only
  valid `RunID` change also changes the admission, and the test would still
  pass with top-level `RunID` dropped from the digest. That costs nothing
  today: `Validate` keeps `Admission.RunID` equal to `RunID`, and the
  admission is hashed.
- **Left the store's columns unguarded.** Review asked whether
  `store/handoff_journal.go`'s columns are a third copy a field can drop out
  of. They aren't a copy of record: the store reads the record back from its
  JSON body, and the columns are a projection checked against that body, so
  a new field survives without a column.

## Refute-First Findings

- **"The helper extraction changes behavior": disproved by a check.** The
  pre-change `toStoreRecord` and `fromStoreRecord`, rebuilt from `main`, and
  the new ones agreed on 200,000 generated records per direction, with
  random nil nested pointers and UTC, fixed-zone, and local times. The new
  state and instruction helpers agreed with the old inline copies on the
  same inputs.
- **"The fill can skip a field, so the guard passes falsely": disproved by
  checks.** A field added to ward's state alone, then to both sides with no
  copy line, failed the test and named the field; so did a dropped
  top-level copy, a swapped pair of bools, and a `float64` field the fill
  can't set.
- **"A field can drop out of the digest unnoticed": disproved by checks.** A
  new `ReviewRequest` field failed the coverage check by name, and removing
  `BaseRef` from the digest failed its mutation.

## Revisit When

- A journal record gains a slice, map, or other kind the fill rejects: extend
  the fill and its diff together, with a value that is distinct per field.
- The journal mirror grows enough that generation costs less than the test.
