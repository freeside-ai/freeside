# Daemon Inference Judgment Sites (#656)

The finding classifier and execution diagnostic use a new daemon-side
inference boundary, separate from stage execution and `provider_only`. The
boundary has no workspace, tool, ward, verification-room, or container
surface. Each registered site fixes its authority mode, outbound field
allowlist and sensitivity, fail-safe output, retention, input/output limits,
cumulative budgets, audit sampling, and output validator. Provider and model
identity come from the daemon binding, never the returned object.

## Decisions

- **Keep the classifier independent from Wave 6 adjudication.** The issue
  comment predated the accepted #697/PR #699 decision that classifier output
  is a spec-blind annotation consumed later by a separate adjudicator. This
  unit persists the landed low/medium/high materiality and confidence
  annotation. A Codex P1 (normalized to high) or raw critical/high finding
  without high confidence routes to the existing human `review_dispute`
  attention path. The model cannot declare a finding fixed, derive
  credibility, or select a disposition. Folding adjudication or remediation
  routing into this unit was rejected as both a scope violation and a second
  authority contract at one site.
- **Own cumulative budgets inside the inference boundary.** A durable,
  owner-only atomic state file tracks calls, compute proxy, and created
  attention under site, project, global, and root-lineage windows. This avoids
  a migration in `internal/store`, which is a shared-contract change the issue
  explicitly forbids. Process-only counters were rejected because restart
  would reset the bound; using the main store was rejected because it would
  silently widen the unit's contract surface. A random ledger epoch is
  anchored beside the main database, outside the inference state directory,
  so loss or replacement of inference state disables calls instead of
  resetting budgets. Resets are absent from the site API, so the caller cannot
  grant itself more budget.
- **Keep advisory data in a separate bounded store.** Diagnostic claims and
  sampled audit rows are producer-labeled, append-only by identity, and
  retention-bounded in their own file. Policy, trust, store, and observation
  packages have no advisory import. The inference service exposes only a
  write path; no advisory read participates in trust, transition legality, or
  publication eligibility.
- **Fail safe without making inference an availability dependency.** Missing
  drivers, schema violations, duplicate keys, oversize data, allowlist or
  sensitivity mismatches, and exhausted budgets select the site's
  deterministic fallback. Classification falls back to high materiality and
  low confidence; diagnostics are skipped. The already-durable review or
  execution-failure path continues.
- **Reserve cost and sampling before provider work.** Each call durably
  reserves its maximum compute and starvation allowance before invoking the
  driver. Audit selection belongs to that exact reservation ordinal, not to
  attacker-controlled input. A failed sampled call transfers its audit debt
  atomically to the next call at the same site; it neither drops the sample
  nor deadlocks unrelated sites. Drivers must honor cancellation, while the
  client also admits at most one non-conforming orphan per site.

## Refute-First Verification

Independent review confirmed and drove fixes for: conservative normalization
of unknown and native Codex severities; strict revalidation of reconstructed
classifications; pre-call compute and starvation reservation; pre-sink
attention accounting; deletion-resistant ledger anchoring; reservation-bound
audit sampling and recovery; physical advisory pruning; fail-safe startup on
corrupt optional state; ambiguous-write shutdown; and bounded cancellation of
non-conforming drivers. The reviewer rejected as already closed the proposed
returned-object bypasses after lattice, producer-note, duplicate-key, size,
and UTF-8 validation were verified. No model-returned fixed verdict or
execution capability is present.

Automated review additionally found and closed two recovery-surface
defects: classifier retries now preserve the first persisted attention
routing decision, and open disputes expose only executable actions until
Wave 6 adjudication lands. A fresh refute pass caught and closed the upgrade
seam for disputes persisted before that action fix; open legacy items are
repaired in place, while closed items remain terminal.

A later review found that idle inference-call metadata outlived its declared
retention. Maintenance now prunes those records while retaining only a
non-sensitive per-site audit obligation. Refute-first concurrency review
drove the ledger to version 2 and replaced site-wide activity guesses with
exact call-ID protection, so maintenance and cross-site reservations cannot
prune a call that can still complete its audit, while a timed-out orphan
cannot retain project, lineage, producer, or digest metadata indefinitely.

## Rejected Alternatives

- Reusing `exec.ReviewSource` or the stage driver would expose workspace and
  execution capabilities that §5.13 excludes.
- Importing the `signet` or `publish` secret types would couple lane
  ownership. The inference package follows their redacting-value pattern
  locally and keeps credential reveal at the driver call.
- Relying on `encoding/json` or `strictjson` alone would retain duplicate
  object keys with last-value-wins behavior. The returned-object boundary
  rejects duplicates before strict, size-bounded decoding.

## Revisit When

Revisit the binding when a production inference provider is selected. Driver
selection is daemon composition, not model choice; the standing ban on
cross-vendor selection remains. Revisit storage only if operations require
budget state to join the encrypted main-store backup and the corresponding
shared-contract change is approved.
