# Strict JSON Decoding on encoding/json/v2

Decision note for #2801. The owner chose the agent's recommendation: move
`strictjson` to `encoding/json/v2` with v1 options, v2 error reporting, and
the two `jsontext` strictness options, then delete the two exact-case
duplicate-key walkers that this subsumes. The case-folding walkers, the
importer's structural walkers, and the export re-encode gate stay. See
Decision below.

Evidence comes from a scratch prototype on `main` at `46492303e`, built
with Go 1.27.3. The prototype was not committed.

## Boundaries

Tiers follow AGENTS.md's Tier A path list. "v2" below means a strict
decode with `jsontext.AllowDuplicateNames(false)`.

| Boundary | Tier | What v2 would replace | Differences that apply |
| --- | --- | --- | --- |
| `strictjson.decodeBytes`, `internal/strictjson/strictjson.go:98`; 159 calls in 103 files | A by reach (see below) | Duplicate rejection, which it lacks today. The UTF-8 pre-check, byte-limit check, and trailing-data check stay. | Last-value-wins becomes an error; name matching, merge, and error text depend on the configuration |
| `ward.RejectDuplicateJSONKeys`, `internal/ward/runtime_cli.go:481` (folds case); 28 direct calls plus 3 function-value uses | A | Only struct-field collisions | v2 checks names exactly; it misses case variants in maps, `json.RawMessage`, `any`, and ignored unknown members |
| `rejectDuplicateJSONKeys`, `internal/publish/janitor_snapshot.go:493` (exact case); callers `janitor_snapshot.go:376`, `janitor_journal.go:176` | A | All of it | None that loosen: both callers strict-decode the same payload next |
| `rejectDuplicateKeys`, `internal/inference/client.go:654` (exact case); one caller, `decodeStrictObject` at `client.go:648` | A by reach (not on the list): it decodes model output for `publication_author.go:386,415` and `adjudicator.go:326` | All of it | None that loosen: `decodeStrictObject` strict-decodes the same bytes next |
| `rejectBackupDuplicateJSONKeys`, `internal/store/local_backup.go:67` (lowercases); caller `local_backup.go:633` | A | Nothing | The caller decodes into `map[string]json.RawMessage` and picks `instructions` with `strings.EqualFold` (`local_backup.go:644`), so a case-variant pair passes v2 and then resolves by map iteration order |
| `scanCommitPlanStrings` and `preboundCommitPlan`, `internal/importer/commit_plan.go:66,293` | A | Nothing | `scanCommitPlanStrings` is the tolerant string-scan pass, which no decoder option replaces. `preboundCommitPlan` bounds array element counts before allocation, which v2 cannot, and pins `version` as the first member |
| `manifestEntryCountExceeds`, `internal/importer/evidence.go:85`; callers `evidence.go:63`, `handoff.go:82` (`loadManifest`) | A | Its duplicate-key reasoning only | v2 rejects a second `entries` at its name, before that array is allocated, but one oversized array still needs the count |
| `export.DecodeEvidenceManifest` re-encode, `internal/export/evidence.go:249`; also `internal/ward/export_verify.go:387` | A | Two of its four stated reasons (invalid UTF-8 is already pre-checked; duplicates move to decode) | Whitespace and member order still decode cleanly under v2, so the byte-equality gate stays |

`internal/strictjson/` is not on the Tier A list, but it is the decoder for
importer, export, stage, publish, and ward input. A change to what it
accepts changes what untrusted input reaches those gates, so the
unlisted-file rule makes a migration unit Tier A, and that unit adds the
package to the list.

The 28 `ward.RejectDuplicateJSONKeys` callers: `cmd/freesided/submit.go`
473, 506, 532, 1078; `preflight.go` 1216, 1233, 1271;
`manual_submission_config.go:50`; `deputy_settings.go:162`;
`submission_identity.go:239`; `internal/ward/runtime_cli.go` 731, 750, 770,
804, 854, 890; `claude_model_discovery.go:122`; `codex_model_discovery.go:89`;
`lineup_probe.go:714`; `claude_review.go:88`; `codex_review_source.go` 1184,
1962; `codex_review_usage.go:23`; `export_verify.go:387`;
`internal/exec/claude/conformance.go:251`; `internal/exec/stage/state.go:517`;
`internal/projectimage/apple.go:1248`; `lockfile_delta.go:170`. Thirteen
of them follow the walker with plain v1 `json.Unmarshal`, not `strictjson`.
Three more pass the walker as a function value to the Claude usage parser
(`internal/exec/claude/usage.go:26,33`, `internal/ward/claude_review.go:104`),
which decodes into targets that allow unknown fields and include maps
(`internal/exec/usage_claude.go`).

## Configurations Compared

- **A, v2 defaults:** `json.Unmarshal` from `encoding/json/v2` with
  `AllowDuplicateNames(false)`, `AllowInvalidUTF8` per policy, and
  `RejectUnknownMembers` per call.
- **B, v1 options plus strictness:** `encoding/json.DefaultOptionsV1()`
  joined with the same three options. The Go 1.27.3 docs show
  `DefaultOptionsV1()` joined with `AllowDuplicateNames(false)` in a
  `Marshal` example (`v2_options.go:148`).
- **B', B with v2 errors:** B plus
  `encoding/json.ReportErrorsWithLegacySemantics(false)`. Added after B's
  full run showed the error-identity break below.

Each prototype replaced `decodeBytes`' `json.Decoder` with a
`jsontext.Decoder`, `json.UnmarshalDecode`, and one `ReadToken` that must
return `io.EOF`. Production callers match only three `strictjson`
sentinels (`ErrTrailingData` 34 times, `ErrInvalidUTF8` 6, `ErrLimitExceeded`
6), plus `io.EOF` and `io.ErrUnexpectedEOF` in one place
(`internal/ward/credential_integrity.go:166`). No production code reads
`encoding/json` error types or the text of a decode error.

## Prototype Results

`internal/strictjson` tests (`strictjson_test.go` and `ratchet_test.go`):

| Test | A | B | B' |
| --- | --- | --- | --- |
| `duplicate_keys_use_last_value` | fails: now rejected | fails: now rejected | fails: now rejected |
| `unknown_field_rejected` | fails: text is `unknown object member name`, not `json: unknown field` | passes | fails, as A |
| Every other `TestDecodeContract` case, including `empty input` (`io.EOF`), trailing data, both byte-limit cases, and tolerated invalid UTF-8 | passes | passes | passes |
| `ratchet_test.go` | passes | passes | passes |

The ratchet passes only because it recognizes the `encoding/json` import
path alone. It would not notice a v2 `Unmarshal` or a `jsontext.Decoder`
anywhere in the daemon, so any migration must teach it both.

Full `bash scripts/check.sh daemon test`, 61 packages:

| Cause | A | B | B' |
| --- | --- | --- | --- |
| Duplicate now rejected (`strictjson` last-wins case) | 1 | 1 | 1 |
| Duplicate refused at decode before the canonical gate: `export` `TestDecodeEvidenceManifest`, `importer` `TestLoadEvidenceManifest/duplicate_key` | 2 | 2 | 2 |
| Test asserts v1's `unknown field` text: `cmd/freesided` deputy settings (2), `specify` (2), `engine` (1), `strictjson` (1) | 6 | 0 | 6 |
| Truncation no longer `io.ErrUnexpectedEOF`: `ward` `TestObserveCodexStoreIntegrity` (3), `cmd/freesided` `TestDoctorCommandProbesStoredCredentialsWithoutLeakingThem` (1) | 0 | 4 | 0 |
| Failing packages | 6 | 5 | 6 |

The two duplicate-at-decode failures still refuse the input, with a
different reason. They are Tier A gate tests, so revising them is Tier A
work.

B's truncation failures are a production regression. With legacy error
reporting, `UnmarshalDecode` reports a cut-off document as a v1
`*json.SyntaxError` ("unexpected end of JSON input") that does not wrap
`io.ErrUnexpectedEOF`, while v1's own `Decoder.Decode` does. B also hides
`jsontext.ErrDuplicateName`: its duplicate error is a bare `duplicate object
member name`. B' keeps both identities.

A's case-sensitive matching and v2 merge rules broke no test. That is weak
evidence, because few tests send case variants or decode into non-zero
values, and A would silently drop a case-variant field wherever unknown
fields are allowed (17 `DecodeAllowingUnknownFields` and 4
`DecodeReaderAllowingUnknownFields` calls, mostly external formats). A
also makes `time.Duration` fields an error. B' keeps v1 behavior for all
of these.

## Gate Replacement Evidence

Scratch tests decoded each input into a struct with a `path` field, a
`map[string]string`, a `json.RawMessage`, and an `any`:

- **Exact duplicates** are rejected everywhere under A and B, including
  after escapes (`path`) and inside skipped unknown members. B' differs
  from B only in error reporting.
- **Case variants of one field** (`{"path":1,"Path":2}`, and the Kelvin sign
  `Key` against `key`) are rejected as duplicates under B and B',
  because v2 tracks seen struct fields by field id
  (`encoding/json/v2/arshal_default.go:1372`). Under A they are unknown
  members: rejected when unknown members are, silently ignored otherwise.
- **Case variants in maps, `RawMessage`, `any`, or ignored unknown members**
  pass under A and B.

So `jsontext`'s exact-name check cannot replace the case-folding
`ward.RejectDuplicateJSONKeys` as a drop-in. Under B' it covers what the
walker's comment says it is for, keys that would target the same Go field,
but only where the bytes are decoded into structs through B'. The 13
callers that decode with plain `json.Unmarshal`, the 3 function-value uses,
and any caller whose bytes also go to a map, a digest, or a later decode,
need their own analysis.

`jsontext.WithByteLimit` and `WithDepthLimit` are commented out in Go
1.27.3 (`jsontext/options.go:272`, go.dev/issue/56733). They cannot replace
`strictjson`'s byte-limit check, and they would not bound element counts
for the importer walkers even once they exist.

## Options

Sizes are against the 1,500-line unit budget, counting tests.

1. **No migration.** No units. Duplicate keys stay last-value-wins at every
   `strictjson` boundary that has no walker of its own.
2. **`strictjson` only, B'.** One Tier A unit, about 250 lines: switch
   `decodeBytes` to B'; add a `strictjson` duplicate sentinel wrapping
   `jsontext.ErrDuplicateName`; rewrite the header; flip the last-wins test
   and add case-variant, nested, and truncation cases; teach the ratchet
   `encoding/json/v2` and `jsontext`; update the six text assertions and the
   two Tier A gate tests; fix the comments in `DecodeEvidenceManifest` and
   `loadManifest` that cite last-wins; add `internal/strictjson/` to the Tier
   A list.
3. **`strictjson` plus the duplicate-key gates.** Option 2, then:
   - **3a, exact-case walkers (recommended).** One Tier A unit, about 150
     lines, mostly deletions, `starts-after` option 2's unit: remove the
     janitor and inference walkers and their ratchet allowlist entries. The
     usage parser's injected walker is the case-folding ward one and stays.
   - **3b, `ward.RejectDuplicateJSONKeys` (not recommended now).** Two Tier
     A units of about 400 to 600 lines each, split `internal/ward/` and the
     other callers, covering all 31 uses: move each walker-plus-decode pair
     onto `strictjson` where the target is a struct, keep the walker where
     it is not.
   - **The backup walker, importer walkers, and export re-encode gate stay**
     under every option, for the reasons in the table.

Configuration A is the wrong choice for options 2 and 3: it changes name
matching and merge semantics at 159 call sites with no test signal, to
save one option. B is wrong because it breaks the credential-integrity
truncation check and hides duplicate errors.

## Recommendation

Option 2 under B', then 3a. Chose B' over A because it gets the same error
identity while keeping v1's name matching, merge, and `time.Duration`
rules, so the only change to what decodes successfully is duplicate
rejection, which #2801 asks for. Chose it over B because B silently breaks
a Tier A truncation check. Chose to stop at 3a because those two walkers are
fully subsumed, while 3b is per-call-site Tier A analysis to delete a walker
that is cheap, correct, and stricter than v2.

B' changes how failures behave, per `ReportErrorsWithLegacySemantics`
(`v2_options.go:448-485`); the full suite hit none of these, and option 2's
unit checks each:

- **Partial writes.** On a syntax error, v1 leaves the destination
  untouched; B' may already have written fields. A caller that reads the
  destination after an error would see them.
- **First error stops.** v1 keeps decoding after a type error and reports
  the first; B' stops at it.
- **Wrapped method errors.** An `UnmarshalJSON` error is wrapped in a
  `SemanticError`. `errors.Is` still reaches it, but the text changes.
- **Type errors at runtime.** Ambiguous case-insensitive names and malformed
  tag options, which v1 ignored, become errors.

## Interaction With #2837

Neither unit waits for the other. Under options 2 and 3, `loadManifest`'s
strict decode rejects duplicates, so whichever of #2837 and the migration
lands second drops last-wins from `loadManifest`'s rationale. If #2837
chooses canonical-or-reject, the re-encode still has to close whitespace
and member order. `manifestEntryCountExceeds` stays either way.

## Decision

Owner decision, 2026-11-08, recorded on #2801: option 2 plus 3a, under
configuration B', listed on tracker #2804. 3b is not chosen.

Follow-up: #2887 (`strictjson` on v2), then #2888 (remove the exact-case
walkers).

Revisit when `encoding/json/v2` gains a case-insensitive duplicate-name
check or an element-count limit, or when every `ward.RejectDuplicateJSONKeys`
caller decodes only into structs through `strictjson`.
