# Elaboration Import: Finding Tolerance, Blob Persistence, Rejection Recording

Work unit for #768 (PR #776). Elaboration publishes a typed JSON
specification, never workspace content, so the import gauntlet must not
fail an elaboration invocation on incidental investigation debris, and a
rejected export must record its finding detail durably. This note records
the safety-policy call the issue delegated to the implementing unit
("whether some path classes stay fatal for elaboration ... is the
implementing unit's design call; record it") and the refute-first
verification on the credential-leak surface.

## Fatality Table Under the Specification (Elaboration) Profile

Chose to tolerate exactly `allowlist_violation`, `size_violation`,
`path_collision`, and `secret_scan_skipped`, and to keep everything else
fatal, over tolerating a wider or narrower set, because those four are the
debris an agent running the project's own build/test suite produces
(gitignored `dist/` output is out-of-allowlist and routinely over the size
and scan caps), while the rest are never plausible investigation debris.

Kept fatal for elaboration: the four inherently commit-blocking kinds
(`non_regular_change`, `invalid_path_entry`, `blob_omitted`,
`commit_plan_secret` — the tree cannot represent the candidate); `secret`
(a scanned-content match); `git_metadata_path`; and the six §5.5/§5.8
control-plane path classes (an elaborator editing CI, policy, reviewer
instructions, or git metadata is off-contract even unpublished).

Rejected alternatives:

- **Skipping the workspace import entirely for elaboration.** `finish()`
  also persists the evidence channel and agent claims that
  `readElaborationOutput`'s post-restart fallback requires, and an
  import-free terminal path would restructure recovery.
- **The importer silently dropping tolerated findings.** The importer's
  account stays honest; tolerance is the pipeline's disposition.

## Credential-Leak Surface: Unscanned Tolerated Blobs (Refute-First)

Tolerating `secret_scan_skipped` means a regular file larger than the
1 MiB scan cap passes unscanned. Codex (PR #776 review) raised that
`finish` then copied every manifest blob into the daemon CAS via
`persistRepositoryBlobs`, so a secret hidden in an oversized file would be
retained despite "nothing publishes" — a retention publish-strict never
allows, because it rejects `secret_scan_skipped` outright.

Confirmed by a fresh-context code audit: repo-channel entry blobs enter
backup artifact closure only through a `KindProductionPublicationRequested`
task's payload extractor (`productionReplayDigests`), and only production
publication (`materializeReplay`) reads them back. Elaboration mints no
publication task (the elaboration arm of `engine.RecordExecutionExport`
records the export export-only), and stage recovery validates the manifest
against `ManifestDigest` without reading entry blobs.

Chose to skip `persistRepositoryBlobs` for the specification profile
(`persistsRepositoryChannel`) over keeping the blobs, over making
`secret_scan_skipped` fatal, and over deferring, because: the elaboration
commit is vestigial (never published), so its repo blobs are dead weight;
skipping keeps unscanned tolerated content out of the CAS entirely; and it
avoids the functional gap of failing elaboration on large gitignored
debris (which making `secret_scan_skipped` fatal would reintroduce). The
audit closed the risk that this breaks backup closure or recovery.

- Confirmed: skip is safe against backup artifact closure and every
  read/recovery path (audit above).
- Rejected-by-verification: "skipping repo blobs breaks backup closure" —
  closure never walks manifest entry blobs except via a publication task
  elaboration never mints.
- Accepted-by-decision: the durable ExportRejection record and the CAS
  manifest/evidence persistence for elaboration are retained; only the
  repo-channel entry blobs are skipped.

## Rejection Detail Stays Daemon-Internal

Chose a new daemon-internal `export_rejections` table (migration 0046)
over the sync-carried `findings` review entity or structured
`AttentionItem` fields, because putting finding detail on the §5.14 sync
surface would escalate this `kind:fix` unit to `kind:contract` (api + app
clients regenerate). The failed-outcome summary is count-only for the same
reason: it flows into the client-visible `AttentionItem.Reason`, so
per-finding paths stay in the daemon-internal record and the error-level
log line, not on the synced surface (Codex PR #776 review).

Chose to record the ExportRejection *after* the authoritative failed
ExecutionOutcome commits (best-effort, `recordRejectionDetail`), not before
the released directory is cleaned, over the original design that wrote it
early and special-cased recovery to finalize it. That design fought the
recovery architecture: four review rounds each found a deeper crash window
(a rejection without its outcome recovering as "lost" and rerun; a recovery
summary diverging from the live one and conflicting on the write-once
outcome; a surviving directory re-imported under a since-changed profile
deriving a second, different rejection; an upstream `AuthenticateStart`
re-gate reached before recovery could finalize the rejection). The findings
come from the in-memory `importer.Result`, not the released directory, so
the rejection need not be written early. Writing it after the outcome means
a durable rejection can never exist without its outcome: recovery consults
only the outcome (`adoptRecordedOutcome`), all rejection-specific recovery
handling is deleted (`commitRejectedTerminal`, the `recoverExported`
rejection lookup), and a crash between the outcome and the detail write
loses only diagnostic detail, never correctness. The typed
`definitiveRejection` carries the sample from the import boundary to the
terminal-commit path, and its count-only message *is* the outcome summary,
so recovery reconstructs no summary and `recordOrConvergeOutcome` cannot
conflict. Decided with the owner after the fourth recurrence, per the
review-tail-signals-boundary rule: 3+ same-class rounds means reframe the
responsibility, not fold another patch.

Chose to bound the persisted findings (`maxPersistedRejectionFindings`,
100) plus a true `TotalFindings` count, and to order the retained sample
fatal-first, over persisting every finding in workspace order, because the
findings are candidate-controlled (kinds and long paths an adversarial
workspace can flood): an uncapped body would bloat the permanent row copied
into every backup checkpoint, and a workspace-ordered cap could crowd the
one fatal cause out of the sample with tolerated debris (Codex PR #776
rounds 4/5).

Refute-first verification of the reframe (fresh-context adversarial pass):

- Confirmed sound: the "rejection never without its outcome" invariant, no
  orphan rejection row, and write-once outcome convergence (no
  retries-forever). `adoptRecordedOutcome` short-circuits before any
  re-import, so two divergent summaries are never both written.
- Confirmed and fixed: the recovery re-import path (a surviving directory
  re-rejecting) originally fell into `reconcileIntent`'s generic catch-all,
  dropping the diagnostic row and mislabeling a correct re-derived rejection
  as `recovery failed:` on the client-visible Reason. Now routed through the
  same `commitRejection` as the live path.
- Accepted-by-decision: a crash after the directory is cleaned but before
  the outcome commits recovers as `Lost`, not `Failed`. Verified terminal
  and rerun-free — a lost elaboration invocation routes to
  `recordElaborationFailure` (a deterministic execution-failure item), never
  a fresh attempt — so the only cost is a narrow-window fidelity difference
  (status and summary text, no diagnostic row), never a wasted rerun or a
  durable inconsistency. This is the deliberate reframe trade: a rare
  crash-window fidelity loss for the deletion of the recovery-convergence
  bug class.

## Revisit When

- Any path builds a production or fake-publication replay from an
  elaboration export's manifest: `materializeReplay` and
  `productionReplayDigests` would then demand the skipped repo-channel
  blobs, so `persistsRepositoryChannel` would have to persist them again.
- Rejection detail is surfaced to the operator UI: that is `kind:contract`
  work (api + app clients) and would move finding detail onto the sync
  surface, changing both the store table's role and the count-only summary.

Follow-up: #778 (the pre-existing `AuthenticateStart` re-gate reached before
recovery finalizes a preterminal exported intent — Codex PR #776 round-5 C1,
out of this unit's scope).
