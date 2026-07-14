# Wave 0 adversarial review, second pass (tracker #4)

Fresh-context adversarial review of the merged Wave 0 units (#6 #7 #8 #9
#11; exit fixes #32–#39) against the plan sections their issues cite,
user-directed. First pass was #31 (fixes PRs #40–#53), so this pass
reviewed post-fix HEAD (c7e4dea). Declared paths: `devlog/` only; the
review changes no code by instruction. Outputs: issue #55, the findings
summary comment on #4, this entry.

## Method

Hunt categories (from the direction): §4/§5.12/§5.14/§5.15 schema drift;
§5.3 invocation-id-first interface drift; weakened §3.1 non-waivable
invariants; fake-driver coverage of the failure modes the sync/kill tests
need; migration-forcing schema shapes for digest-bound approvals, per-key
policy provenance, artifact provenance; silent narrowing. Every candidate
was gated through plan text at the cited lines, docs/history/decisions.md,
the open deferral queue (#52 #41 #30 #28 #27 #24 #23 #22 #18 #15 #14 #13),
and devlog decided/deferred markers before filing. The one survivor got a
three-lens refute-first pass (code trace, spec/precedent, executable
reproduction in a scratch copy of `daemon/` — repo untouched); all three
lens verdicts and the full rejected ledger are in the #4 comment.

## Confirmed (filed)

- **#55 (P3): item status lifecycle unenforced.** Two halves, one class,
  both reproduced by execution: `PutCommand` accepts a decision bound to a
  closed item's *current* version/head/digests (`BindsSameAs` + `Offers`
  are status-blind and `RequestedDecision` is never cleared — `Validate`
  requires it non-empty), and `ValidateAttentionItemTransition` has no
  status terminality rule, so `resolved → open` at version+1 passes. The
  2026-07-14-1230 entry's status-gating deferral is thereby *partially*
  disproven: its "a decision on a superseded item is already caught as
  stale" argument covers only pre-resolution-version commands (that path
  was re-verified intact). Severity held at P3, not P2, because the spec
  lens showed the plan does not promise store-level status gating (§5.14
  test 9 assigns closed-item action suppression to the client surface;
  §3.1's "stale-approval" class does not name current-version commands),
  `PutCommand` has no non-test caller in Wave 0, and action-legitimacy
  policy is already signet territory (#23). Filed rather than left as
  devlog prose so the *corrected rationale* — the version-bump argument is
  insufficient — reaches whichever unit implements the gate.

## Rejected by verification (not re-raised)

Full list with reasons in the #4 comment; headline entries: command
payload typing and heartbeat/pairing endpoints (recorded provisional-API
deferrals, widened via kind:contract on consumer need); `PolicyKey.Value`
string typing (widening is decode- and digest-compatible, not
migration-forcing); `publish_eligible` on `Artifact` not in the §5.15
provenance block (deliberate computed-not-supplied strengthening);
process-global `ApprovedRecipes` (documented provisional, fails closed);
fakes' lack of per-call transient-error scripting (no enumerated test
requires it); `Message` without attachment digests (linkage unspecified
for Phase 1, additive if needed). Checked clean: §5.3 op-sets and
invocation-id-first signatures exact; all 27 actions map 1:1 to §4;
capability model matches §5.7 with frozen `Admission`; no
migration-forcing shape found for the three provenance/approval schemas.

## Accepted by decision (observations, not filed)

- `domain.ProposalBatchID` (ids.go:21) declared but unused; §4's batch id
  rides `Subject{proposal_batch, subject_id}`. Cosmetic; noted on #4.
- Known tracked gaps re-checked against the findings; none understated,
  no severity comments needed.

## Verification

- Passed: executable reproduction of both #55 halves (scratch copy,
  `go test ./internal/store/` green including the repro tests; v1-bound
  stale rejection confirmed intact alongside).
- Checked: every rejected candidate carries a citation (spec text,
  decision record, or recorded deferral) in the #4 comment ledger.
- Not run: no repo code/tests were changed, so no repo check applies to
  this entry beyond docs CI.

## To promote

- None. Queue swept: three open items, all docs promotions outside this
  unit's scope, none drained, no re-defer needed: the
  `approved-recipe-boundary` trust-boundary promotion (-> tracked by #52);
  the `domain-package` conventions spine review (-> tracked by #27); and
  the 2026-07-14-1519 entry's still-open store write-boundary promotion
  candidate ("write methods for `as_of_revision` entities live only on the
  revision-bumping handle"), which has no `->` marker or tracker issue and
  remains open on its source entry.
