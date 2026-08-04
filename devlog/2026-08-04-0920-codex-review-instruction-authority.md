# Codex Review Instruction Authority

Issue: #495

## Decision

Chose a versioned, explicit Codex review bundle whose authority is the admitted
operator-host snapshot plus every applicable `AGENTS.override.md` or
`AGENTS.md` selected from the exact fetched base checkout. Repository sources
are ordered by canonical path and retain their directory scopes; deeper
repository scopes take precedence, then the operator-host block is final and
global. Explicit host absence is a distinct composition input.

Chose to make the source digests, composition version, and result digest part
of `ReviewRequest` authority, the artifact closure, the launch intent, the ward
journal binding, and the normalized result. Ward reconstructs the bundle from
the content-addressed sources before credential launch, compares it with the
persisted result artifact byte for byte, and the engine reconstructs the same
authority from a fresh exact-base checkout before inspection and again before
readiness.

The operator-host source remains a stable snapshot admitted while composing
the unattended daemon generation. This unit changes that snapshot from an
untracked static mount into one explicit source of every review request; it
does not redefine host-instruction admission timing.

## Rejected Alternatives

- Rejected reading repository instructions from the candidate review
  workspace. Candidate instruction paths are untrusted diff content and cannot
  govern their own review.
- Rejected relying on Codex workspace discovery. Ward deliberately disables
  project-document discovery, so the daemon must resolve the trusted-base
  hierarchy and deliver its explicit scoped bundle.
- Rejected leaving instruction identity only in deployment configuration.
  That could not prove which repository sources governed one result or keep
  those sources in backup closure.
- Rejected resolving the open review-anchor fork in `docs/plan.md` Section 7.
  The instruction authority is valid for either anchor and #495 explicitly
  leaves that owner choice open.

## Verification Findings

The implementation preserves the existing read-only single-file mount and
byte-limit checks. Adversarial tests reject stale-base and cross-repository
request substitutions, noncanonical source order, missing source artifacts,
symlinked exact-base sources, and a tampered composed result.

The independent refute-first review confirmed four finding classes. Empty
override files now shadow sibling `AGENTS.md` files by presence; stale findings
and stale clean passes both force a new authoritative review; authenticated
pre-authority requests are torn down and superseded by a new round instead of
wedging; and started historical review bindings receive teardown-only legacy
validation so startup can reap their credential-bearing topology. That legacy
validation preserves the historical digest and every topology, ownership, and
runtime-evidence check, and it cannot authorize launch, collection, or a
result. The final independent pass was clean.

Automated PR review then identified two lifecycle gaps in the rendered
snapshot. Review snapshots now live beneath a private daemon-owned directory,
with a dedicated per-invocation directory and exclusive file creation, so a
deterministic filename can neither replace another input nor be reused after a
failed launch. Recovery receives the non-secret input-root location solely to
remove an owned invocation snapshot before it can mark a recovered outcome
ready, including when it reaps the workspace binding left by a crash before
launch-intent persistence. It does not gain authority to read, reconstruct,
or launch a review.

The final automated pass found a credential-leak path: a host instruction
source could alias the configured review-auth snapshot. Admission now compares
the opened instruction inode with a pinned forbidden host input before reading
any bytes, then revalidates the forbidden path after the read; the review-auth
snapshot is always forbidden. This rejects path, symlink, and hard-link aliases
and fails closed if credential rotation replaces the path during admission.

A later review found that a valid pending request with only an older instruction
binding must not become a permanent contradiction after host instructions
change. Ward now identifies that narrow case, tears it down, and reports a
transient supersession so the engine schedules a fresh authoritative round;
any mismatch outside the instruction binding remains a contradiction.

Backup closure now treats every review record that carries `instructions` as a
current-format `ReviewRequest`: it rejects unknown top-level and nested fields
and re-runs the full request validation before retaining its instruction
artifacts. The no-instructions legacy path remains an explicit compatibility
exception, so historical records do not make a backup incomplete.

Recovery now validates a persisted orphan workspace ID before joining it to the
private instruction-root path. A malformed stored ID therefore cannot turn
orphan cleanup into deletion outside its immediate snapshot directory.

When a legacy request and its unconvertible legacy outcome are both rejected,
cleanup now preserves the request's legacy sentinel through the outcome
rejection. The engine can therefore supersede the old round after teardown
instead of retaining a terminal contradiction.

Orphan recovery removes the instruction snapshot before deleting the workspace
binding that is its only durable identity. A snapshot-removal failure now
leaves that binding intact for a later recovery instead of stranding the
snapshot and wedging a retried review.

Revisit when Codex changes its instruction discovery or precedence contract,
or when Freeside changes the lifecycle at which operator-host review
instructions are admitted.
