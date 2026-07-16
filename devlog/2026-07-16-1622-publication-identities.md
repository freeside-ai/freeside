# Publication identities and reconciliation: derivation scheme and convergence rules

Work unit: #81 (lane: publish). Scope: `daemon/internal/publish`.
Mandatory note: the identity-derivation scheme is this unit's recorded
decision, and GitHub-response-driven convergence is returned-object
trust-boundary work.

## Identity derivation (the recorded decision)

A publication identity is sha256 over a versioned canonical JSON
encoding (`freeside-publication/v1`) of {repo, base ref, source head
SHA, sorted artifact digest set, optional verification recipe digest}.
Derived names: branch `freeside/publish/<first 16 digest hex>`, PR
marker `<!-- freeside:publication-identity=sha256:<full hex> -->` as
the body's final line.

- **The producing invocation ID is excluded from the identity.**
  Identity answers "what result should exist on GitHub"; a *new*
  invocation over the same candidate (crash recovery, operator re-run)
  must converge on the one existing branch and PR. Including the
  invocation ID would mint a fresh identity per attempt — exactly the
  duplication §5.9 forbids. The invocation ID lives on the attempt
  axis instead: the outbox intent key `publish/<invocation_id>/<kind>`.
  Rejected alternative: identity = f(invocation), which makes retries
  of one invocation trivially convergent but cross-invocation
  convergence impossible.
- **PR title and body are excluded.** Wording fixes must converge onto
  the same branch and PR (the publisher patches drift back), not mint
  new ones. Rejected alternative: hashing the full PR content, which
  turns every caption edit into a new publication.
- **Branch truncation to 16 hex (64 bits)** keeps refs readable; a
  prefix collision within one repository's publication set is
  negligible, and the PR marker carries the full digest, so a
  collision surfaces as `ErrPublicationConflict` (branch at a foreign
  SHA) or `ErrForeignResource` (marker mismatch), never as silent
  adoption.
- The canonical encoding is a fixed Go struct marshaled with
  `encoding/json`; the golden fixture pins the derived digest, which
  pins the encoding transitively. Any encoding change is a version
  bump: two builds must never derive different identities for the same
  candidate.

## Outbox coupling: a publish-owned port, not a store import

`IntentLedger` mirrors the store's `EnqueueOutbox` but stays an
interface in this package. Decisive reason: `EnqueueOutbox` rides the
Write transaction that commits the decision the effect belongs to
(§5.14), and transaction composition belongs to the Wave 2 engine, not
this package — a publish-owned store adapter would have to open its
own transaction, splitting intent from decision. Matches #80's
`Recorder` precedent. The Wave 2 wiring is a thin adapter; #120 (now
merged) already provides the dispatch-side scan/mark methods.

## Convergence rules over returned objects (fail closed)

GitHub responses drive convergence decisions, so every rule refuses
rather than guesses:

- Branch at the candidate head: converged, reuse. Branch at any other
  commit: `ErrPublicationConflict`; the publisher never force-pushes
  over unknown external state.
- PR on the publication branch converges only when its body parses to
  exactly this identity's marker; a marker-less or foreign-marked PR is
  `ErrForeignResource`. `ParseMarker` accepts exactly one distinct
  well-formed marker (strict `sha256:` + 64 lowercase hex) and fails
  closed on ambiguity.
- A closed marked PR is `ErrPublicationConflict`: closing was a human
  decision; recreating or reopening would override it silently.
- A recorded intent whose payload names a different identity than the
  current derivation means an invocation ID was reused for different
  content: `ErrPublicationConflict`, nothing dispatched.
- Artifacts re-gate through `domain.EligibleForEvidenceSnapshot`
  against the caller-supplied approved-recipe set before any external
  effect; the decoded `PublishEligible` bit is never trusted, and every
  head-bound artifact must name exactly the candidate head
  (`ErrHeadMismatch`, §5.15 rule 2).

Reconciliation state (per-resource ETag validators) is deliberately
in-memory: validators are a bandwidth/rate-limit optimization, never
correctness; after restart the first poll per resource is
unconditional and re-establishes them.

## Refute-first verification pass

One fresh-context refuting lens over the full diff (returned-object
trust boundary), attacking marker spoofing, convergence races,
identity collisions, ETag staleness, credential leaks, outbox
discipline, gate bypass, and test honesty. Dispositions:

- **Confirmed, fixed**: marker-shaped prose in the candidate body
  (e.g. a quoted marker on its own line) would make the publisher's
  own PR fail marker parsing on retry and deadlock convergence as
  `ErrForeignResource`. Fix: after deriving the identity, the composed
  PR body must parse back to exactly that identity's marker, or the
  publication fails before any effect.
- **Confirmed weaker-than-comment, fixed**: the list re-check verified
  the head branch name but not the head repository, so a fork PR
  copying our branch name and (public) marker could be adopted if the
  server's filter misbehaved. Fix: re-check `head.repo.full_name`;
  fork PRs are skipped, not adopted.
- **Fixed (TOCTOU narrowing)**: a marked PR whose head SHA differs
  from the candidate head (branch moved between the ref check and the
  PR check) now refuses as `ErrPublicationConflict` instead of
  converging under the wrong commit.
- **Fixed**: an unsolicited 304 (answered to a request that sent no
  validator) had the reconciler fabricate a "confirmed" observation
  from an empty cache; now refused at the returned-object boundary.
- **Fixed (provenance honesty)**: the candidate-level recipe digest is
  now required to equal every artifact's gated recipe digest, so the
  identity cannot record a recipe the evidence was not verified under.
- **Accepted-by-decision**: identity inputs are not canonicalized
  (repo case, `main` vs `refs/heads/main`). The publisher's one caller
  is the daemon itself supplying a configured repo/base string
  verbatim; canonicalization for hypothetical disagreeing callers is
  speculative. Consequence if violated: two identities for one logical
  candidate (duplicate branch/PR), no trust impact.
- **Rejected-by-verification** (no finding after honest attack):
  canonical-encoding injection (JSON string escaping closes it),
  outbox ordering (intent strictly precedes the first dispatch on
  every path, tested), invocation-reuse misclassification (value
  equality over identity and coordinates only), domain-gate bypass,
  credential/content leaks in new error paths (status+path only).
- **Noted, not pursued**: `listPRsByHead` reads one page (100) of PRs
  per head; exceeding that for a single publication branch is not a
  realistic 1A state.

An owner-run review round (2026-07-16, post-Codex) adjudicated five
more findings on the same returned-object class:

- **Fixed**: reconciler pull observations now surface every
  identity-bound coordinate (head ref/commit/repository, base
  ref/repository) — without them a retarget advanced the ETag while
  the observation showed no change, and every later 304 confirmed the
  changed resource as "unchanged".
- **Fixed**: ref reads validate the returned ref name and ref creates
  verify the echoed ref and commit, the same binding discipline the PR
  paths already had.
- **Fixed**: every forge response decode requires exactly one JSON
  document (trailing data fails closed, mirroring DecodeIntent); a
  JSON null pull list is rejected rather than read as an authoritative
  empty list; a decision-driving list row without a positive number
  and required fields fails the read instead of becoming a
  "successful" PR #0.
- **Fixed**: created and patched PR content is verified stored-as-sent
  (title and body); a normalizing store would otherwise report
  converged and silently re-patch on every later publication. The
  pre-patch check still tolerates drift — that is what the patch
  repairs.
- **Accepted-by-decision**: the reconciler stays data-race-safe but
  not linearizable. The mutex guards the cache, not the
  read–fetch–update sequence; an out-of-order concurrent poll can at
  worst cache the older internally consistent {validator, observation}
  pair, which the next conditional poll re-syncs, and the cache is an
  optimization correctness never depends on. Serializing would hold a
  lock across network I/O. Rejected alternative: per-resource request
  serialization. Intended usage (one poller per resource) is now
  documented on the type.

## Out-of-scope residuals

- The approved-recipe set's own provenance (who approves recipes) is
  the verifier lane (#75); this unit takes the set as trusted input.
- Kill-test matrix and crash-recovery scans over pending intents: #82.
- Store-backed IntentLedger adapter and its transaction placement:
  Wave 2 engine assembly.
- The opt-in live test (`live_test.go`) still covers only minting; a
  live publish exercise would create real branches/PRs and belongs
  with #82's kill-test demonstration.

Revisit when: the EvidencePublisher (1B) adds PR-section content
beyond the marker line — the deterministic-marker parse rule and the
drift-patch rule must then distinguish owned sections from human
edits.
