# Protected-path config falls short of the six-member §5.8 class: #177 filed

Spine coordination, 2026-07-18. Checking whether #166 ("block the full
control-plane path class during import") was unblocked by the now-merged #172
(#176) surfaced a gap in the merged contract: the finding-class taxonomy is
wider than the config that feeds it. This note records the gap, the choice to
file a fourth finding-remediation contract (#177) rather than patch around it,
and the proposed chain position. Status lives on the issues and #83, never here.

## The gap (four-vs-six)

- `domain.ControlPlaneCategory` (`daemon/internal/domain/enums.go`) pins the
  complete plan §5.8 control-plane class as **six** members, and
  `CandidateFinding.Validate` (`daemon/internal/domain/authorization.go`)
  requires every control-plane finding to name one.
- `domain.ProtectedPathConfig` (`daemon/internal/domain/trust_profile.go`) — the
  repository-specific, trust-anchored widening the gauntlet consumes — exposes
  only **four** `Extra*` pattern lists: automation-control, reviewer-instruction,
  git-metadata, verification-control.
- Those reach three §5.8 categories (`workflow_configuration`,
  `reviewer_instructions`, `verification_recipes`); git-metadata is an orthogonal
  integrity concern, not a §5.8 category. `prompts_and_policy`,
  `egress_and_trust_profiles`, and `materiality_rules` are **enumerable but
  unreachable**: no config field, default pattern, or gate can classify a path
  into them.
- #166's Evidence text calls out exactly these ("trusted recipe, policy,
  trust-profile, egress, prompt, or materiality-rule files ... receive no
  control-plane finding") and its required outcome is fixtures covering **each**
  protected category. So #166 cannot be completed against the shape #172
  delivered; its "Blocked by #172" line was necessary but not sufficient.

## Decisions and rejected alternatives

- **A new `kind:contract lane:spine` unit (#177), not a widening in passing.**
  `ProtectedPathConfig` is spine-owned domain territory; a `kind:fix` gauntlet
  unit (#166) must not add fields to it. Adding the three `Extra*` fields also
  changes `canonicalTrustProfile`'s field set, so it bumps the trust-profile
  digest encoding `freeside-trust-profile/v1` → `v2`. Rejected: letting #166 edit
  the domain shape (violates contract discipline and lane scope); reopening the
  merged #172 (a merged contract is extended forward, not amended).

- **The v1 → v2 bump orphans existing persisted `trust_profiles` rows; #177 must
  decide that explicitly, not leave it implicit.** Store decode
  (`store.scanTrustProfile`) re-runs `AutomationTrustProfile.Validate`, which
  recomputes the digest under the *current* `trustProfileEncodingVersion` and
  rejects any row whose stored `profile_digest` was derived under v1 — so the
  bump makes v1 rows unreadable, fail-closed. #177 therefore either scopes in
  versioned-read/migration/replay for v1 profiles, or records that dropping them
  is acceptable. Expected default: **acceptable** — the `trust_profiles` table
  was introduced in #176 (migration `0007`), holds no production rows at Phase
  1A, and rows are write-once and re-recordable by a human approving a profile
  (§5.5 drift recovery), so nothing a re-record can't restore is lost. The
  claiming spine session confirms no live environment holds v1 rows before
  relying on this; if one does, #177's scope grows to include the versioned read
  (and its declared paths grow to `daemon/internal/store`). Raised by Codex on
  #178.

- **Not the #22 on-demand widening mechanism.** #22 widens provisional *enum
  member sets* (`Priority`, `ItemStatus`, `SensitivityClass`, `Author`) where the
  plan names a field but enumerates no members. Here the enum
  (`ControlPlaneCategory`) is already complete at six; the shortfall is *struct
  fields* on `ProtectedPathConfig` plus a digest-encoding version bump. That is
  the same distinction `2026-07-18-1125-finding-contract-prereqs.md` drew for
  #171 ("a new persisted field ... not a provisional vocabulary widening, so it
  takes its own kind:contract unit"), applied to a field addition here.

- **Contract/consumer boundary.** #177 delivers the domain shape only (three new
  `Extra*` fields + `Validate`/`canonicalize` + digest-version bump + golden).
  #166 keeps the importer gates, the `importer`/`verify` `FindingKind →
  CandidateFinding{Class, Category}` mapping, and the class-oriented malicious
  fixtures. This mirrors #172(shape) → #166(wiring).

- **Proposed chain position, not scheduled.** #177 serializes in the #83
  finding-remediation contract chain (contract units are mutually exclusive). The
  exclusive lock is currently held by the active claim on #173; #171 is dormant.
  #177 wants an early slot like #172 because it unblocks a pass-A consumer
  (#166), but it serializes behind the in-flight #173. Recorded the proposed
  position on #83 and left #177 unmilestoned/dormant; final ordering and
  scheduling are the spine sweep's call, per the "serialize but do not schedule"
  discipline.

## Same resolution pattern as #172's note (analogous, not a prior ruling)

`2026-07-18-1710-trust-authorization-contract.md` ("Revisit when") applied this
resolution to a *different* consumer-discovered shape gap — "current profile"
selection semantics beyond recorded-order listing — where the missing shape
"lands in its unit (or a follow-up contract), not by widening this one silently."
That note did **not** anticipate the four-vs-six category gap specifically; #177
applies the same resolution pattern to it. Cited as analogous precedent for the
mechanism (consumer-discovered shape gap → follow-up contract), not as a prior
decision that foresaw this gap. The contract working as designed, not a defect
in #176's review.

## Revisit when

- The spine scheduling sweep runs: assign #177 (and #166) their milestone and
  chain slot per the normal scheduling operation; this note's "not scheduled"
  statement is superseded at that point.
- #177 is claimed and the implementer settles the final `Extra*` field names and
  the `v2` encoding details.
