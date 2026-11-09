# Drop the Exact-Case Duplicate-Key Walkers

Decision note for #2888, carrying out option 3a from
[`2026-11-08-1212-json-v2-strict-decoding.md`](2026-11-08-1212-json-v2-strict-decoding.md):
delete `rejectDuplicateJSONKeys` (publish janitor snapshot and journal) and
`rejectDuplicateKeys` (inference `decodeStrictObject`), now that
`strictjson` refuses every repeated member name at any depth.

## Decision

**Chose to move `strictjson.Decode` ahead of the key-presence checks over
leaving the order as it was.** In `DecodeInstallationAuthorityDocument` and
`decodeJanitorJournal`, the order is now: UTF-8 check, `strictjson.Decode`,
`requireAuthoredKeys` or `requireJournalKeys`, then validation.

The presence checks read the payload through v1 `json.Unmarshal` into maps,
where a repeated key keeps its last copy. With the walker gone and the old
order kept, a duplicate whose last copy omits a required key would reach the
presence check first and be refused as a missing key. That is still a
refusal, but it names the wrong defect and makes the presence check depend
on input it can't read correctly. Decoding first means the presence checks
only ever see duplicate-free input, and every lone duplicate is refused as a
duplicate (`strictjson.ErrDuplicateName`).

**Rejected: keeping the walkers as a belt-and-braces pre-pass.** Each ran on
the same bytes its caller strict-decodes next, so it added a second
tokenizer to keep in step with the first and no refusal of its own (see the
comparison below).

## Refute-First Result

A scratch differential harness (not committed) reconstructed the three old
decoders from `origin/main` at `564e3bd0e` and ran old and new over the same
generated inputs: 300,000 each for the snapshot, the journal, and
`decodeStrictObject` into a target with nested structs, a pointer, a
`json.RawMessage`, and a map. The generator mutated valid seeds by
duplicating members (often with a narrowed or replaced value), duplicating
with an escaped name (`"\u0076ersion"`) or a case variant, deleting members,
replacing values, adding unknown members, duplicating or dropping array
elements, and, for one input in twenty, truncating, appending trailing data,
inserting `0xff`, or overwriting a byte.

- **Confirmed: accept and reject decisions are identical** on all 900,000
  inputs, and every accepted input decodes to the same value. About 45% of
  snapshot and journal inputs carried a duplicate the old walker would
  refuse.
- **Confirmed: the snapshot keeps `ErrInstallationAuthoritySnapshot`** on
  every refusal where the old code had it, and every journal refusal keeps
  its `installation janitor journal:` prefix.
- **Disproved: "every duplicate is now reported as a duplicate."** Where an
  input has a duplicate and another defect, the new code reports whichever
  the decoder reaches first in the stream: a type mismatch or trailing data
  ahead of the duplicate wins. The old walker reported the duplicate first.
  Allowed: the input is refused either way, and no caller branches on which
  defect is named. This is the error-precedence change the issue contract
  names.
- **Disproved: escaped names slip past `strictjson`.** `jsontext` compares
  unescaped names, so `"version"` and `"\u0076ersion"` are refused as
  duplicates, as the walker refused them.
- **Disproved: case variants change the outcome.** The old walker treated
  `"version"` and `"Version"` as distinct names, but `strictjson` refuses
  case variants that match one struct field as duplicates, and the old
  pipeline ran `strictjson` too. So both refuse such a pair, and the
  harness saw no difference.

Two new test cases fail with decode left after the presence check and pass
with the reorder: a snapshot whose later `pending` copy omits keys, and a
journal whose later `quarantined` copy omits `recorded_at`.

Revisit when `strictjson` stops refusing duplicate names, or when either
presence check starts reading anything a strict decode hasn't already
accepted.
