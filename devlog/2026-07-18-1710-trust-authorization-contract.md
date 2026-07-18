# Trust profile, candidate authorization, and finding-class contract (#172)

Spine contract unit, 2026-07-18, chain head of the #83
finding-remediation contracts (filing and chain order:
`2026-07-18-1125-finding-contract-prereqs.md`). Defines the §5.5
automation trust profile and audited current-state snapshot, the §5.6
candidate authorization, and the §5.8 control-plane finding class as
domain shapes with persistence; consumers #166, #168, and #169 enforce
against them. Status lives on the issue and PR, never here.

## Decisions and rejected alternatives

- **One unit, no split.** The filing note reserved a split into "trust
  profile / digest" and "authorization + finding class" contracts. Kept
  as one: the authorization embeds the trust-profile digest and the
  finding records, so a split serializes on the same lane anyway,
  leaves the profile half consumer-less, and invents the
  canonical-digest convention twice. The work proved wide but shallow —
  three record shapes on proven patterns (`publish/identity.go`
  canonical digest, `artifact.go` computed-trust-field,
  mint-audit/0006 store rows).

- **A domain-level two-axis finding vocabulary, not a promotion of the
  package vocabularies.** `CandidateFindingClass`
  (control_plane / import_integrity / repo_change_policy / secret)
  carries the trust dispatch and `Kind` keeps the emitting package's
  own token, so `importer.FindingKind` and `verify.FindingKind` map in
  later without domain enumerating or importing them. Rejected: moving
  those vocabularies into domain (touches gauntlet scope this unit did
  not declare, and overlaps the #173 importer seam); enumerating a
  union kind list in domain (domain would chase every gauntlet
  vocabulary change forever). `ControlPlaneCategory` pins the complete
  §5.8 class as six members; #166 wires the importer to it. The
  non-waivable `import_integrity` class was split out of
  `repo_change_policy` on a Codex review finding: §3.1 makes
  artifact-integrity failure non-waivable, so content the repo-change
  channel cannot faithfully represent must never be waivable the way
  allowlist/size/collision policy findings are.

- **Self-certifying records instead of a store-side policy parameter.**
  The artifact re-gate needs the approved-recipe set threaded through
  every transaction; the new shapes instead recompute their own trust
  content (profile digest, authorization id, authorizes_publication)
  inside Validate, so the existing encode/decode boundaries fail closed
  with no new store plumbing. Chosen because the derivations close over
  the record's own bound facts — there is no external policy input to
  drift. Waivability is the one policy axis, and it is pinned to §3.1
  in the vocabulary itself (non-waivable classes make a waived finding
  unrepresentable), not to a runtime set.

- **Authorization identity is attempt-scoped, unlike publication
  identity.** `DeriveIdentity` deliberately excludes the invocation
  (content axis); the authorization deliberately includes invocation
  and created_at, because it attests what one verification run
  observed. The `UNIQUE(repo, head_sha, trust_profile_digest)` key is
  what prevents distinct attempt records from silently coexisting for
  one binding: the second one fails loudly instead of converging.

- **Owner decisions (asked and answered at plan time).** (1) Uniqueness
  is per (repo, head, profile), so a head can be re-authorized under a
  human-approved revised profile (§5.5 drift recovery) while old rows
  stay as immutable history; a strict per-head key would make drift
  recovery a destructive row deletion. (2) A failed verification still
  writes an authorization row — a truthful, non-authorizing record —
  so the outcome is durably bound and #168's gate keys on the computed
  bit, not on row absence.

- **Waivers are non-agent decision records.** `WaiverRecord.DecidedBy`
  rejects `AuthorAgent` at validation: an agent waiving findings on its
  own candidate is self-authorization. Human (`user`) and trusted
  policy (`daemon`) authors are representable; the production semantics
  of issuing one stay with #168.

- **`TokenPermissionsMode` carries `read_write`.** A single-member
  `read_only` enum would make a drifted, more permissive audit
  observation unrepresentable — the exact state the §5.5 fail-closed
  comparison must see. The profile side may also name it, as a
  deliberate human-approved posture; `AutomationChangePolicy` stays
  single-member (`block`) because §5.5 admits no other stance.

## Verification (refute-first pass)

The mandatory refute-first pass for returned-object trust boundaries
ran as a fresh-context reviewer instructed to disprove the re-gates
(forged digests/ids/bits, canonicalization holes, putImmutable
interplay with the dual unique constraints, waiver bypasses, JSON
ambiguity, lint-enforcement claims, migration constraints).

**Confirmed and fixed** (each folded into its owning commit):

1. A non-nil empty slice (pattern lists, findings) digested as nil but
   encoded as `[]`, so one content had two valid bodies and a
   struct-literal replay could hit `ErrImmutableConflict` instead of
   converging. Validate now rejects non-nil empty lists.
2. Validate accepted a non-UTC `CreatedAt`/`DecidedAt`, so one instant
   could carry two valid identities off the constructor path (the
   UNIQUE key blocked silent coexistence, but the one-identity claim
   failed). Validate now requires UTC on identity-bearing timestamps.
3. `ORDER BY` on RFC3339Nano TEXT misorders sub-second instants
   (trailing zeros trimmed: `...05Z` sorts after `...05.5Z`), and the
   profile list feeds current-profile selection. Lists now order by
   rowid (insertion order).

**Accepted by decision** (reviewer info findings, consistent with
intent): `WorkflowAudit` is an observation record, deliberately not
self-certifying, and its `audited_at` column is not cross-checked;
caller-supplied digest fields validate non-emptiness only (matches
existing domain shapes); stored bodies tolerate unknown/case-variant
JSON keys (Go decode semantics), every meaning-bearing tamper the
reviewer tried was caught by Validate's recompute.

**Refuted under attack** (properties held, demonstrated): flipped
trust bit via duplicate case-variant JSON key; waived non-waivable
finding and agent-authored waiver injected as literals; malformed-glob
early-exit in `path.Match`; forged profile digest / authorization id
across every Get and List path; putImmutable convergence under the
dual unique constraints and loud failure across them; FK enforcement
under the DSN's `foreign_keys(1)`; the no-default dispatch switches
are genuinely lint-forced (`default-signifies-exhaustive`).

## Revisit when

- #166/#168/#169 wire enforcement: if a consumer needs "current
  profile" selection semantics beyond recorded-order listing, that
  shape lands in its unit (or a follow-up contract), not by widening
  this one silently.
- The evidence-manifest contract #173 or a later unit needs the
  helper/import result digest vocabulary to align with
  `ImportResultDigest` here.
