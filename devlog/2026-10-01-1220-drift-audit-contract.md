# Review Drift Contract: Drift Audit Artifact and Verdict

Work unit #1665, plan §7 "Review Drift". This note covers the second part of
the contract that #1048 split: the `DriftAudit` artifact, its store accessor,
the two audit policy keys, and the round's verdict in yield history. It adds
types and persistence only. #1050 adds the audit site and #1051 the routing.
The first part's note is `2026-09-30-2107-review-drift-contract.md`.

## Chose to Show a Verdict Only Once a Later Round Exists

A round's verdict enters yield history only when the history also holds a
later round. The last round of any history never carries one.

The store compares each stored item's yield history byte for byte with its
re-encoding, and deep-equal with the history re-derived from the review rows.
A round's audit is written after the round's review record, and an item can
snapshot the history in between. If the verdict appeared as soon as the audit
row existed, that item would stop equalling its re-derived history and would
fail closed. That is the failure the diff-metrics note records for late
metrics. Metrics avoid it by being written with the round; an audit can't be,
because it is a model call that runs after the round.

With this rule a history through round N is the same before and after round
N's audit. The derivation applies it (it drops the last record's verdict
whatever the caller passes), the domain type refuses a verdict on the last
round, and the app's mock validation mirrors the refusal.

Rejected:

- **Show the verdict as soon as it is recorded.** Breaks every item that
  snapshotted the round, as above.
- **Version the snapshot or store a "verdicts as of" marker on the item.** It
  would work, but it adds a second history shape and a migration for a reader
  that only needs the verdicts of earlier rounds: the card for round N+1 shows
  what the audit said about round N.

The cost: the final round's verdict is never in yield history. A consumer that
needs it reads the artifact. #1667 carries the `drift_audit` stop cause and
card facts, which is where a `stuck` verdict on the last round surfaces.

The field is `drift_verdict,omitempty`, omitted and never null, for the reason
the first part's note gives for `diff_metrics`.

## Chose One Row per Round With No Revision

`drift_audits` is keyed by run and round and takes one row. A replay of the
same artifact is a no-op and a different artifact for the round is an
immutable conflict. The plan's fail-safe default says a failed audit produces
no verdict and the site doesn't retry beyond its budget, so a round has no
second attempt to store. Rejected: an attempt-keyed table, which would need a
rule for which attempt's verdict yield history shows.

The store also refuses a new row for a round once a later round is recorded
or failed. From that point the round's verdict is visible in yield history,
and an item may have snapshotted the history without it. This is the
diff-metrics "never backfill" rule applied to audits.

## Chose to Bind the Audit to a Findings Round

The accessor requires the round's review record to exist with outcome
`findings` and the same base and head, the run's specification and policy
digests to equal the artifact's, and every reversal to name a finding that a
review record of the run lists at or before the audited round.

- **Findings outcome.** The issue asks only that the round exists. Plan §7
  runs the audit after a round that returned findings, so an audit of a clean
  round has no producer. The plan wins where the two differ, and the stricter
  check is the one that fails closed.
- **At or before the round.** A reversal undoes an earlier fix, so its finding
  is usually from an earlier round. Bounding the lookup at the audited round
  means the check gives the same answer however many later rounds are
  recorded. Whether the named finding was actually fixed, and whether that fix
  may be reversed, is the route gate's check in #1051.
- **Run digests are the authority.** The artifact's copies of the
  specification and policy digests are compared with the run's, the
  finding-adjudication rule.

## Chose a Required Confidence and a Strict Decode

Confidence is required and reuses `AdjudicationConfidence`. The plan names the
existing `low` / `medium` / `high` scale; a second enum with the same members
would be a second registration point for one vocabulary.

The store decodes its own encoding through the strict, size-capped decoder
before writing, and every read goes through one reconstruction that re-runs
the write-time checks. No copied column and no stored digest is trusted.

`DefaultDriftAuditRoute` is a `var`, not a `const`, because the enum
registration test requires every constant of an enum type to be a member of
its `AllX` slice. `ParseDriftAuditRoute` is exported so the store's policy
decoder validates through the domain's predicate.

## Implemented the Whole Unit Without the Planned Split

The implementation plan proposed cutting #1665 into three parts (artifact and
keys; migration and accessor; yield history, API, and client) and left the
call to the owner. The owner's fiat named #1665 as one unit and no split was
applied on the forge, so the agent implemented the whole issue contract. The
commits follow the plan's three parts, so the pull request can still be cut at
a part boundary. The unit is over the size budget; the owner has not recorded
a reason for the larger size.

## Refute-First Findings

An independent pass tried to break the accessor and the artifact in a scratch
copy.

Confirmed and fixed:

- **Invalid UTF-8 in identifying strings.** The constructor accepted invalid
  bytes in the run id, base, head, and a reversal's finding id. Marshaling
  rewrites them to U+FFFD, so two audits differing only in such bytes shared a
  digest and the constructor emitted a body its own decoder read back as
  different content. `Validate` now refuses them. The store was not exposed:
  its pre-write decode already refused these bodies.

Allowed by decision:

- **A raw rewrite of a row's copied key hides the row from keyed reads.**
  After raw SQL moves a row's `run_id` or `content_digest`, the keyed reads
  for the original key return not-found and a different audit can then be
  written for that round. `ListDriftAudits`, which yield history uses, still
  fails closed, and so does a read by the new key. A writer with raw access to
  the database can delete the row outright, so this adds nothing to what they
  can already do. The diff-metrics and adjudication accessors behave the same.
- **The successor check counts later rounds by copied columns.** Same
  boundary: only a raw rewrite of a later row could make it miss.
- **`MigrateLegacyTrustProfileRunPolicy` makes a stored audit unreadable.**
  The migration changes a run's policy digest, so an audit bound to the old
  digest fails its binding check on read. Its only caller acts on legacy runs
  with no resolved policy, which can't have the audit policy key set, and no
  producer writes audits yet. Finding adjudications share the rule.
- **Reads accept non-canonical stored bytes whose body digest was
  recomputed.** They return the same logical audit and content digest, so no
  digest splits.

Disproved: wrong round, base, head, specification, or policy; unknown,
other-run, or later-round findings; later records, failures, and other runs
written after the audit; non-UTC and malformed timestamps; trailing data,
unknown fields, and oversized bodies; a null or missing reversal list; a
missing or off-scale confidence; replay over a tampered body.

## Revisit When

- A consumer needs the final round's verdict in yield history itself, not from
  the artifact or the stop cause.
- The audit site (#1050) gains a retry, which would need more than one row per
  round.
- A legacy run without a resolved policy can reach the audit site; the policy
  migration and the binding check would then conflict.
- An audit of a clean round gains a producer.
