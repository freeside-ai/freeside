# The Daemon No Longer Reads Review Records From Before Review Admission

Review admission (f4451241b and b6844800b, 2026-10-11) made every review
record and Codex review request carry the admission it ran under. The
readers for the older shapes stayed so a database from before then would
still resume. This note records why they go now and what that leaves. It
covers family 6 of #3266, split out as #3324.

## Decisions

- **Remove the readers rather than keep them for old databases.** The owner
  decided on 2026-11-16 that prod history is disposable and prod starts on a
  fresh database (#3265), so nothing needs to read these shapes. No database
  a current build can open holds one (Evidence). The removed shapes:
  - Review and shadow-review records without an admission, which carried the
    reviewer as `provider`, `model_configuration`, and `cost_owner` strings.
  - Codex review requests without instructions, without an admission, with
    the `codex_explicit_bundle_v1` instruction bundle, or naming an
    `evaluated_sha`.
  - The supersession rule that let a review recorded before request inputs
    existed (fa13740ec, 2026-11-06) stand against a request that has them.
- **An old shape now fails loudly, never reads as the current one.** Every
  request reader decodes strictly, so `evaluated_sha` is an unknown field
  and refuses the row. The record decodes ignore the retired strings; a
  legacy record is refused because it has no admission. A record or request
  without an admission, and a request without instructions or with the v1
  bundle, decode but fail `Validate`, which every reader now calls; there
  is no longer a separate rule for retained requests. A pre-inputs review no
  longer stands. Its source no longer counts it as superseded, and its
  authority disagrees with the engine's fresh request. A review still in
  flight is reported as a contradiction, which holds the run for an
  operator; a finished one leaves the gate pending on the authority
  re-check. Both fail closed. The fake review source's state file now
  decodes strictly too, so a fake intent naming `evaluated_sha` refuses
  instead of dropping the field.
- **A current record and request keep their bytes and digest.** The
  admission stays a pointer under the same JSON name, and the request's
  authority digest keeps the `review-request-authority-v4` version.
  `TestCurrentReviewRequestAuthorityDigestIsPinned` landed before the field
  removal and passes unchanged after it, and the current goldens are
  unchanged.
- **Backup refuses an old request; restore copies it.** The local backup's
  artifact closure now reports a request without instructions, or with the
  v1 bundle, as an invalid instruction closure, so the backup fails instead
  of skipping that request's artifacts. A same-version restore still copies
  old rows, but the first read of one fails to decode or validate. No such
  backup is known to exist.
- **Kept, because they belong to other units or still have live use:**
  `LegacyOpenTasks`; `legacyCodexReviewReason` and
  `readinessLegacyExhaustionOpen` (#3323); the ward's legacy topology
  versions (#3326); `DecodeRetainedCodexReviewRequestFacts`, which only the
  pre-migration-67 request-time recovery reads, and which now refuses
  `evaluated_sha` through the strict decode; `storedReviewAdmission`'s
  not-found path, which fails closed; and the verification and engine
  re-entry `EvaluatedSHA`, which is live verification, not review.

## Revises the Merge-Path Note

`devlog/2026-11-05-2315-retire-review-merge-path.md` kept `evaluated_sha` on
`exec.ReviewRequest` as the read-only `LegacyEvaluatedSHA`, and rejected
deleting it because lenient decoding would weaken the strict decode and a
missed reader would fail a backup. That reasoning assumed old rows still had
to read. Under the owner's fresh-database decision they don't, so the field
goes and the strict decode stays: a stored request naming a merge is now
refused at decode rather than at launch. The digest of every request without
the field is unchanged, so no admitted run moves.

## Evidence

- **Last writers.** By `git log -S` over `daemon/`:
  - Record strings and records or requests without an admission: last
    written before f4451241b and b6844800b (2026-10-11).
  - Requests without instructions: before b8ff69d00 (2026-08-04).
  - The v1 bundle: before 83a7386ef (2026-08-25).
  - `evaluated_sha`: before ee965c993 (#2660), with 34f586883 and
    e5016c797 removing the remaining merge review.
  - Today the only request writer is the engine's production review gate.
    The gate builds records through `domain.NewReviewRecord` and the shadow
    review through `domain.NewShadowReviewRecord`; both need an admission.
- **Instance counts (2026-11-17, branch build, counts only).** Each database
  was counted from a SQLite backup-API copy deleted after use; fake review
  state files were counted under each root.
  - **Dev instance:** `check-database` passes (schema 137 migrated to 146);
    0 rows of any removed shape, 0 requests in its snapshot, 0 fake state
    files.
  - **Kept roots at schema 120 or later (9):** 0 rows, 0 files. Two pass
    `check-database`. Seven fail it on kinds this unit doesn't touch (check
    proof, schedule occurrence, artifact, attention item), identically under
    `main`'s build (4481f907a).
  - **Kept roots below the migration baseline (27, schema 22 to 104):** no
    current build opens them. Fourteen hold removed shapes: 25 records
    without an admission (all carrying the retired strings), 37 requests
    without an admission, and 13 v1-bundle requests in two roots. None hold
    `evaluated_sha` or a shadow-review record of the old shape.
- **Prod.** Not read by an agent for this unit. #3265 records prod started
  on a fresh database.

## Refute-First Record

- **A current request's digest changes (disproved by a check).** The pinned
  digest test was committed against the code with `LegacyEvaluatedSHA` and
  passes against the code without it.
- **A current writer still produces a removed shape (disproved).** The
  writer search above finds none.
- **A removed shape reads as a current one somewhere (disproved).** Each
  reader that decoded a request (review evidence, workflow observation, the
  backup closure, the ward journal) now calls `Validate`. Tests at the
  review evidence view, the backup closure, and the ward journal refuse a
  request without an admission or instructions; the workflow observer calls
  the same `Validate` and has no test of its own.

Revisit when a database from before review admission needs to resume: it
needs a migration path first, and then these readers.
