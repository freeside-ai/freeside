# Protected-path config widened to the six-member §5.8 class (#177)

Spine contract unit #177, claimed by fiat 2026-07-18. Implements the
shape-only widening `2026-07-18-1800-control-plane-config-gap.md`
scoped: three new `Extra*` lists on `domain.ProtectedPathConfig` so
`prompts_and_policy`, `egress_and_trust_profiles`, and
`materiality_rules` become reachable from repository config. Importer
and verifier wiring stay #166's scope.

## Decisions

- **Field names mirror the enum members**, not new vocabulary:
  `ExtraPromptsAndPolicyPatterns` (`extra_prompts_and_policy_patterns`),
  `ExtraEgressAndTrustPatterns` (`extra_egress_and_trust_patterns`),
  `ExtraMaterialityRulesPatterns` (`extra_materiality_rules_patterns`).
  The existing four names already diverge from category strings
  (automation-control reaches `workflow_configuration`); no renaming of
  those — a merged contract is extended forward, and the encoding bump
  below already orphans no readable content. Fields are appended after
  the existing four rather than reordered: the canonical encoding pins
  struct order, and an append keeps the v1 prefix recognizable in a
  diff.
- **Encoding bump `freeside-trust-profile/v1` → `v2`**, per the
  encoding-version rule (any field-set change is a new version).
  `canonicalTrustProfile` embeds `ProtectedPathConfig`, so the three
  fields enter the digested form with no second registration point. A
  digest-stability test now pins the v2 canonical form to a literal
  digest; v1 had no such pin, so this also closes the gap where an
  accidental encoding change would only surface as golden churn.
- **v1 rows are dropped, not migrated.** Store decode re-runs
  `Validate`, which recomputes the digest under the current version and
  rejects v1-derived `profile_digest`s, fail-closed. Verified before
  relying on the issue's default: no production daemon binary exists
  (only `freeside-export` and `freeside-signet-dev`), and a filesystem
  sweep found no live database with `trust_profiles` rows. The table
  arrived in #176; rows are write-once and re-recordable by a human
  approving a profile (plan §5.5 drift recovery), so nothing
  unrecoverable is lost. Rejected: a versioned read under
  `daemon/internal/store` — machinery for rows that do not exist.
- **Round-trip coverage lives in the domain package**, a
  marshal/decode/`Validate`-recompute test over a profile populating
  all seven lists, rather than widening the store round-trip fixture:
  `daemon/internal/store` is outside this unit's declared paths (the
  gap note grows store scope only for the rejected versioned read), and
  the store tests construct profiles through the constructor at
  runtime, so they exercise v2 unchanged.

## Verification findings

- The `candidate_authorization*.golden` fixtures changed only through
  their embedded `trust_profile_digest`: the v2 version string shifts
  every profile digest even for content the new fields leave nil, which
  is the intended upgrade-drift semantics, not an accident.
- Open PR #183 (`fix/publish-trust-drift-gate`) touches
  `trust_profile.go` concurrently, but its hunks are pure appends (the
  drift comparator, line 332+) and `EvaluateTrustDrift` never reads
  `ProtectedPaths`; no textual or semantic collision with this unit's
  edits to the config struct and encoding. Merge order is free.

## Revisit when

- #166 wires the importer gates for the three new categories and finds
  the shape insufficient (e.g. needs per-category defaults expressible
  in config): that is a new contract unit, never a silent widening.
- A deployed environment ever holds trust-profile rows across an
  encoding bump: the drop-don't-migrate default above was conditioned
  on the table being empty everywhere; a populated deployment needs the
  versioned read this unit rejected.
