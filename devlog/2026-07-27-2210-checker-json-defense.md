# Checker Duplicate-Key JSON Defense and Cidfile-Less Probe Recovery (#353, #355)

## Decision

Chose a pure `jq --stream` structural duplicate-key detector inside
`scripts/check-agent-image.sh` over the issue's alternative of a small Go
helper. The checker is an operator-side script whose only runtime
dependencies are Apple `container` and jq; delegating to a Go helper would
either couple the script to a built daemon artifact or add a Go-toolchain
requirement to a pre-push check. jq's normal parser is last-value-wins on
duplicate keys, but `--stream` emits parse events before that collapse, so
the duplicates are still observable.

Detector algorithm: process stream events in document order, maintaining a
set of *completed* paths. A leaf event completes its own path; a close event
completes its parent's path. Any event whose path, or a prefix of its path,
is already completed is a duplicate key or a second top-level value, and the
document is rejected. Empty input is rejected (zero events), mirroring the
daemon's immediate-EOF rejection in `ward.RejectDuplicateJSONKeys`;
malformed or trailing input fails jq itself. Rejected two simpler designs by
counterexample: repeated-leaf-path detection misses an object replayed as a
scalar (`{"a":{"x":1},"a":2}` has no repeated leaf path), and comparing
re-serialized output cannot distinguish duplicate collapse from formatting.

Accepted delta from the daemon: keys are folded with `ascii_downcase`, an
ASCII approximation of `foldJSONKey`'s Unicode simple fold (which mirrors
encoding/json's case-insensitive field matching). Apple `container` emits
ASCII keys, so the folds agree on the real input space; a non-ASCII
case-fold collision would pass the script and still be rejected by the
daemon's gate, which remains the load-bearing check.

The defense gates every parse of runtime JSON in the script: the allowlist
report, the cleanup ownership check that precedes `container delete
--force`, and both parses (list, per-candidate inspect) in the new recovery
path. Every rejection fails closed: no field comparison, no deletion.

## Cidfile-Less Recovery (#355)

Mirrors `appleBackend.recoverOwnedContainer`
(`daemon/internal/projectimage/apple.go`) decision-for-decision: when
`container create` was attempted but no probe identity is known (no cidfile,
or unusable contents), the EXIT trap lists all containers, requires each
listed entry to carry a safely shaped `id` equal to its `configuration.id`,
re-reads ownership per candidate from that candidate's own inspection
(Apple `container list` omits labels), and force-deletes only an inspected
match on this invocation's random ownership token. Non-matching containers
are skipped silently; they are other owners' containers, not refusals.
Recovery runs only on identity loss; a cidfile that names the probe keeps
the existing narrower path.

## Verification

The regression suite `scripts/test-check-agent-image.sh` drives the checker
against a scripted `container` stand-in and is the executable spec: the
issue-mandated negative fixtures (duplicated top-level key, duplicated
nested key, duplicated `labels` object), the cleanup-only duplicate, the
recovery matrix (token match deleted, foreign skipped, ambiguous
listing/inspection refused, invalid listed identity flagged, failed
list/inspect/delete reported), and a detector battery over the adversarial
input space (duplication at every nesting shape, object/scalar replays,
case-folded duplicates, multiple top-level values, empty, malformed) with
valid-document counterexamples pinned against false positives.

Refute-first pass (destructive-path / returned-object-trust unit): an
independent fresh-context review of the pre-commit diff, prompted to
disprove the force-delete gating and fail-closed claims, with empirical
confirmation required per finding. Findings ledger:

- Confirmed and fixed: a runtime that reads stdin inside the recovery loop
  drained the candidate heredoc and silently skipped remaining orphans
  (loop calls now read `/dev/null`; suite case 25 pins it). Malformed
  listing entries (`["garbage"]`, non-string ids) projected to empty
  fields and were silently skipped instead of flagged, and an object-shaped
  listing was iterated as candidates, both contradicting daemon parity
  (array-type enforcement plus flagging; cases 17-20). Mutation testing
  showed the cleanup-side ownership gate and both `configuration.id`
  equality gates were unexercised by the first suite draft — deleting them
  passed all assertions (single-defect cases 5-7, 14-15, 18-21 added; the
  mutants are re-run and killed). The valid-document battery asserted only
  the absence of the ambiguity error (now also pins reaching the field
  comparison). `--help` printed every internal comment block (now prints
  only the leading header).
- Confirmed and fixed (automated review, PR #366 P2): capturing runtime
  output with command substitution silently strips raw NUL bytes, so
  invalid JSON containing a NUL was transformed into a valid-looking
  document before the detector saw it (reproduced: the checker exited 0 on
  such a report). Runtime JSON now lands in a private temp file at every
  capture site (allowlist report, cleanup inspection, recovery listing,
  per-candidate inspections) and every validator and parser reads the
  file; decoded identities carrying an escaped NUL, which a shell variable
  cannot hold losslessly, are additionally flagged invalid. The cidfile
  identity path still reads through a variable by design: a NUL-mangled id
  cannot authorize a delete, because the ownership gate compares it
  against the container's own inspected identity and refuses on mismatch.
- Confirmed and fixed (automated review, PR #366 P2, round 2): the
  temp-file captures introduced for the NUL fix were uncapped, so a wedged
  runtime emitting an unbounded listing could fill the TMPDIR filesystem.
  All four captures now run through a bounded helper mirroring the
  daemon's maxRuntimeOutput cap (16 MiB; overflow fails the capture
  closed, before any parse), and the cidfile identity reads are bounded at
  4 KiB, where truncation can only invalidate an id the inspection gate
  would refuse anyway.
- Confirmed and fixed (automated review, PR #366 P2, round 3, a miss in
  the round-2 fix): the bounded capture checked only the writer's pipeline
  status, so a sink failure (full filesystem, quota, file-size limit) that
  truncated the capture after a valid-looking prefix was accepted when the
  writer exited 0 (reproduced under `ulimit -f`). The capture now checks
  every pipeline status; the suite reproduces the truncation under a
  file-size limit and pins the rejection.
- Confirmed and fixed (automated review, PR #366 P2, round 4): recovery
  deleted directly on the selection inspection, while the daemon's
  recovery re-gates through deleteOwnedContainer's fresh inspection
  immediately before deletion; a replacement container under the same
  runtime ID could therefore be deleted on stale ownership evidence.
  Recovery deletion now re-inspects and re-verifies through the single
  shared ownership predicate right before the destructive act, and the
  suite pins a candidate whose ownership flips between the two
  inspections.
- Rejected by verification: detector false negatives and false positives
  (hand batteries of 19 duplicate and 22 valid documents, a 500-document
  randomized fuzz with planted duplicates, and the daemon's recorded Apple
  `container` fixtures: zero misses, zero false positives); hostile
  candidate identities reaching runtime argv (tab, newline, leading-dash,
  glob, and TSV-smuggled ids all stop at the identity checks, since @tsv
  escapes control bytes into sequences `valid_container_id` rejects).
- Deferred (automated review, PR #366 P2, round 5 → #368): a wall-clock
  bound on the runtime calls. The byte cap bounds size, not time, so a
  runtime that never closes stdout still hangs the checker; the hang class
  predates this unit on every runtime invocation, no wrong deletion or
  exit code results, and a portable fix needs a design decision (macOS
  ships no timeout(1)), so it is tracked rather than folded here.
- Accepted by decision: recovery stays identity-loss-only, mirroring the
  daemon; when a cidfile names a foreign container the cleanup refuses the
  delete and does not additionally run recovery, so an orphan behind a
  lying cidfile survives — delete safety holds, and a runtime that writes a
  foreign ID into the private cidfile is outside the trust model. The
  ASCII fold delta above. Pre-existing, unchanged: an object-shaped (non-
  array) inspect report passes the duplicate gate and fails in the field
  projection with noisy stderr, byte-identical to the prior behavior.

Revisit when: Apple `container` changes its inspect/list JSON shape (the
projection and identity checks are duplicated from the daemon's decoders on
purpose and must move with them), or when a second operator script needs the
detector (extract it to a shared helper file then, not before).
