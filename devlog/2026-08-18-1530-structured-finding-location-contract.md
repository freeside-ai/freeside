# Structured Finding Location and P0 Severity in the Review Finding Contract

Work unit: #679 (`kind:contract`, wave-6 contract-chain head, tracker
#835). Widens `domain.Finding` so findings carry a machine-actionable
location and a `P0`–`P3` severity enum, the substrate for #702's
cross-round identity and the §7 adjudication surface derivation. This
note records the load-bearing decisions; the issue body is the
authoritative contract.

## Decision: Optional at the domain, strict at each boundary

`Finding.Severity` (now `FindingSeverity`) and `Finding.Location` (now
`*FindingLocation`) stay **optional at the domain level** and are
validated **per boundary**:

- `Finding.Validate` accepts an empty severity and a nil location (the
  native ingest legitimately observes third-party review comments with
  no priority badge and review-level comments with no path), but rejects
  a non-empty invalid severity and a malformed present location.
- `exec.ReviewResult.Validate` (the review-source result boundary)
  requires a valid, non-empty severity on every finding: a real review
  source always assigns priority.
- Ward normalization (the codex output trust boundary) requires a
  concrete `{path, start_line≥1, end_line≥1}` location and a P0–P3
  severity, rejecting anything else as a `ReviewFailureContradiction`.

Rejected: making location **required** at the domain and running a data
migration. The native ingest needs the nil (review-level) and whole-file
shapes, and no migration is warranted (below).

## Decision: `*FindingLocation` pointer, and its comparability cost

Location is a pointer (`pointer-for-optional`, the golden convention:
explicit `null` when absent) rather than a value type with a presence
bool. The pointer cleanly separates "no location" (nil) from "present",
which §7 derivation keys off (it fails closed on nil).

Consequence, and the non-obvious part: a `*FindingLocation` field makes
`Finding` non-value-comparable by `==`/`slices.Equal`, which compare the
pointer by identity. Two findings re-derived from the same native comment
across polls allocate distinct pointers, so a naive comparison reports a
change every tick. Fixes:

- `domain.findingsEqual` compares findings by value (dereferencing the
  location); `NativeReviewObservation.MaterialChangeFrom` uses it, so
  append-on-material-change coalescing still converges.
- `cloneReviewResult` (the fake's returned-object immutability boundary)
  deep-copies the location pointer; a one-level `slices.Clone` would
  alias it and let a caller mutate the committed snapshot.

The only production finding-slice equality is `MaterialChangeFrom`; the
only production finding clone is the fake. Store reconstruction decodes
fresh JSON each read (a new pointer), so it never aliases.

## Decision: whole-file (0,0) is a native-ingest shape, not a review shape

`FindingLocation` with `StartLine == EndLine == 0` and a path is the
whole-file location (a file-level native comment). Ward normalization
**rejects** it: a codex review always cites a concrete line range, and
the schema requires `start_line`/`end_line ≥ 1`, so a (0,0) or partial
range from the model is a schema-escaping contradiction, not a
whole-file finding.

## No migration (re-verified at implementation start)

Store persistence is JSON-body only (`findings.body`), so the shape
change needs no schema migration. No legacy-shape decode tolerance is
needed either: every operator store holds **zero `findings` rows**
(`state`, `state-482`, and the historical `state-237/411*` all count 0),
and `state-482`'s three `codex_review_outcomes` rows all carry an empty
findings array. An old-shape `"location":"string"` would fail closed on
reconstruction anyway (a string cannot decode into `*FindingLocation`,
and an old `"severity":"medium"` fails the enum re-validation), so the
fail-closed re-gate covers the residual risk even if the survey missed a
row. Replan trigger if a production run persists findings rows before
merge.

**Scope correction (Codex review, PR #852).** The "no legacy-shape decode
tolerance is needed" claim above was reasoned only from the standalone
`findings` table and is too broad. Two other persisted surfaces embed the
changed shapes and would fail to reconstruct under the new binary on an
in-place upgrade: `codex_review_outcomes` (the completion-evidence bump v2→v3
makes `Poll`'s re-validation reject a persisted v2 outcome, even a clean one)
and `native_review_observations` (an inline finding persists `location` as a
string that no longer decodes into `*FindingLocation`). Both are latent, not
live: no operator store holds breaking data (verified — zero
`native_review_observations` string-location rows across all stores; the v2
`codex_review_outcomes` rows belong to terminal run 482), and state is
re-initialized per wave. Owner decision (2026-08-18): the daemon does not yet
promise upgrade-in-place reconstruction of pre-change review state, so both
are **deferred to #854**, which also reassesses whether the evidence-version
bump earns its cost.

## New: `diffscope` deterministic diff-overlap check (§5.13)

New leaf package parsing a `git diff -U0` into per-path new-side changed
ranges, with an `Overlaps(*FindingLocation)` predicate that fails closed
(nil, absent path, non-overlapping range → false; whole-file accepted iff
the path is touched, including a deleted path; a line range on a deleted
path rejected). Pure functions, no git execution. Engine wiring is
#840's; this unit ships only the check and its fixture tests.

**diffscope fail-closed tightening (Codex review, PR #852).** Two review
rounds hardened the validator's fail-closed contract:

- **Context lines are rejected, not tolerated.** The contracted input is
  `git diff -U0`, which emits zero context. A space-prefixed body line is now
  a parse error rather than a both-counter consume. Rejected the alternative
  (track a new-side cursor and record only `+` lines): with no context, the
  hunk's new-side span is exactly its additions, so the whole-span recording
  is already correct and rejecting context keeps the parser minimal and fails
  closed on any non-`-U0` input. Without this, an unchanged context line
  inside a recorded new-side range would satisfy `Overlaps`.
- **`Overlaps` self-validates the location.** The exported predicate re-runs
  `FindingLocation.Validate` before consulting ranges, so a malformed range
  (a partial range such as `{0, 12}`, non-positive or inverted) fails closed
  instead of satisfying the interval test. The predicate is the acceptance
  decision and does not trust the caller to have validated.
- **Quoted-path garbage rejected.** `gitUnquote` requires the closing quote to
  end the operand or be followed only by a tab-timestamp, so `"b/x.go"garbage`
  no longer decodes to `x.go`; the raw operand then matches no finding path.

**Reframe and cap on diffscope hardening (round 5).** Five Codex rounds each
surfaced a new malformed-diff edge in diffscope. The diff is **trusted engine
output** (#840 runs `git diff -U0`), not adversarial input; the untrusted input
is the finding location, which `Overlaps` already validates. Per the tail
signal (3+ same-class rounds → reframe, not keep folding), diffscope's
malformed-diff hardening is capped here: the quoted-path fix above is the last
one folded, and further edge findings on trusted diff text get a reasoned
decline. The one round-5 finding that was **not** a diffscope edge — a
representation gap for deleted-file finding locations (the ward schema requires
positive line numbers, so §7's deleted-file routing cannot be expressed) — is
**deferred to #855** as a ward-schema/§7 design decision that also settles
#840's routing.

## Refute-first verification (returned-object trust boundary)

The ward normalization decodes external codex output and the fake clone
is an immutability boundary, so a refute-first pass (fresh-context lens,
prompted to disprove) ran before commit.

- **Confirmed and fixed — diffscope header/body ambiguity.** The first
  parser classified every line by prefix, so a hunk-body line whose
  content began with `-- ` (rendered `--- ` by the removal prefix) or
  `++ ` (rendered `+++ `) was misread as a file header, making `Parse`
  wrongly reject a valid finding (fail-closed over-reject) or misattach
  ranges. Latent (unwired until #840) but a package-contract violation.
  Fixed by consuming each hunk's declared body-line count statefully, so
  a body line is never taken for a header. Regression:
  `TestParseBodyLinesLookingLikeHeaders`, plus no-newline-marker and
  pure-rename tests.
- **Accepted by decision — pure rename records no range.** A content-free
  rename emits no hunk, so a whole-file finding on the renamed path does
  not overlap. Correct: there is no reviewed change. Pinned by
  `TestParsePureRename`; #840 may revisit if the engine needs otherwise.
- **Rejected by verification (not re-raised).** (a) `findingsEqual` +
  `MaterialChangeFrom`: no false positive or negative; the only other
  production finding-slice compare, and store reconstruction decodes
  fresh pointers. (b) Immutability: `cloneReviewResult` deep-copies each
  location; no shallow production redelivery path exists (ward re-derives
  per call). (c) Ward normalization: panic-free, rejects every
  schema-escaping/invalid location and out-of-domain severity, identity
  string stable per round, rejecting (0,0) is correct under schema
  `minimum:1`. (d) `ReviewResult.Validate`: only ward produces one, and
  historical persisted evidence used P1/P2/P3 (all valid members), so no
  reconstructed result regresses.
