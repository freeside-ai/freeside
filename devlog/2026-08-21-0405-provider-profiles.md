# Provider Profiles

Work unit: #863 (plan revision 37). Adds an operator-facing, versioned
`ProviderProfile` over the existing `AuthIdentity` so provider accounts
can be named, enrolled, inspected, and deliberately switched without
changing credential containment. The principle the revision establishes:
credential authority stays in `AuthIdentity`; resolved facts and digests
stay in the composition manifest and run records; probe output is
observation, never configuration authority; switching account or provider
is always an explicit, recorded operator choice, never fallback.

## Decision: A Separate Object, Not a Wider Identity

`AuthIdentity` carries the credential facts: store locator, mutation
lease, refresh strategy, snapshot support, execution limit. The operator's
runtime choices (model configuration, role eligibility, display name,
enabled state) change for different reasons and on a different cadence: a
token rotates without the model choice moving, and a model edit must never
enter the credential transaction. Putting both in one record would make
every configuration edit a credential-store mutation under the lease.

Rejected: expanding `AuthIdentity` with configuration fields. Rejected:
the name `ProviderAccount`; the object does not own the credential, and
"account" would suggest it does. The profile lands later as a sync-carried
domain type in its own `kind:contract` unit; this revision drafts no
schema beyond the field list. The profile carries an immutable,
daemon-issued `id`; records bind `id` plus version, and the operator
name is a unique, resolvable label, never the binding (Codex review
finding on PR #864, round 2).

## Decision: Eligibility on the Profile, Binding in Selection

A profile states which roles it may serve; the role a run actually uses
is chosen per run or by per-project policy and recorded with the
composition manifest. The §7 independence check then compares provider
and `auth_identity_id` across the implementation and review selections.

Rejected: a role field on the profile. The independence check would read
a label the operator typed rather than compare what actually ran, and one
subscription could not serve as implementer on one project and reviewer on
another without duplicate profiles.

## Decision: Multi-Subscription Is Supported, Selection Is Never Silent

Two identities of one provider (work and personal) are an ordinary shape.
No default profile is inferred from enrollment order, recency, or
availability; selection is explicit or per-project policy, and cost owner
is re-evaluated on every selection. Freeside attributes usage to a named
profile; compliance with a provider's multi-account terms is the
operator's, consistent with the §14 subscription-terms-drift risk.

## Decision: Observation Never Becomes Authority

The account probe may record a stable fingerprint, a masked label, auth
and plan type, expiry and revocation, CLI version, a model snapshot, and
last probe and execution times. The gate is an exclusion list rather than
a permission list: preflight, scheduling, `max_parallel_executions`, and
every driver read only the operator's profile and identity records and
resolved policy. Probe-derived `system_health` items are always
`advisory`: a blocking posture would close the admission gate and make
the probe scheduling authority through the back door (Codex review
finding on PR #864). The profile's `provider` must equal its identity's
immutable provider, and the identity must exist, checked at
reconstruction and selection (same review). Re-enrollment is same-account only
and always increments the profile version with the enrollment operation
bound, so a credential swap can never silently move attribution to
another subscription; a different account is a new identity and profile
(round 3). For Claude the account check is an operator attestation,
because the pinned CLI exposes no account identity. Profile and identity
are one-to-one, and the profile mirrors exactly two identity facts,
provider and enrolled credential mode, both re-checked at reconstruction
and selection (round 4; the round-1 sweep covered provider only, and
the widened rule now names every repeated fact). The trusted source for
the enrolled mode and account binding is an identity-bound enrollment
record that #406 introduces and #867 writes at enrollment and adoption
(round 10 showed no current record carries either fact: the Codex
re-enrollment journal holds operation coordinates, digest, and expiry
only, and Claude has none), so `AuthIdentity` stays unchanged and a
corrupted profile cannot assert a mode; names resolve
to the `id` once, at authoring time, and policies persist the `id`
(round 7). Profile-to-identity is at most one until adoption and exactly
one after it; `auth enable` reverses `disable` (round 8). Field-level
schema, validation, migration of pre-profile identities, and the full
command set are routed to #406 and #867 by a standing sentence in §5.4,
the closure pattern from PR #699, so further field-level findings are
settled in those units rather than by enumerating the plan. The masked label is display-only and never enters
evidence, manifests, run records, or export. A probe that sees a newer
model or a lapsed plan produces a card or a proposal PR, never a changed
selection.

The Claude floor is recorded as a pinned-CLI empirical claim: the CLI
offers a token digest plus an auth check, not plan or quota. The Codex
app-server probe is expected to report account facts but is gated on a
spike proving it runs against the access-only snapshot and never refreshes
outside the mutation lease, because a diagnostic that can refresh is a
credential mutation outside the lease contract.

Rejected: running agents against the operator's own provider homes
(ambient credential and settings state, the §5.4 containment this plan
already refused). Rejected: T3 Code's shared Codex shadow-home overlay
(a shared writable home re-creates the cross-invocation settings and hook
leakage §5.4 describes). Rejected: arbitrary provider environment
variables as configuration (unrecorded, unversioned, and outside the
composition manifest).

## Decision: Switching Is an Attempt, Never Fallback

A quota, expiry, or capacity card offers retry under a qualified profile,
wait, or stop. Each switch is a new recorded attempt preserving the
original failure, re-evaluating cost owner and review independence, and
continuing provider state only where compatibility is proven. A recurring
preference becomes a project-policy proposal PR in the
`review_diminishing_returns` shape.

Rejected: automatic fallback, already excluded by §2 item 5 and §14 and
held. Rejected: remembered defaults learned from past switches; a learned
preference is a silent selection, which the multi-subscription decision
forbids.

Revisit when: before #408 merges, design a continuation compatibility
digest (tracked as #873, which #408 merges after). Cross-profile
continuation defaults to a fresh invocation until then.

## Verification: Refute Lens and Finding Dispositions

This unit changes prose, not code, so the AGENTS.md refute-first pass
for credential-surface code does not run a harness here; the code-level
pass belongs to #406 (the enrollment record and its reconstruction
invariants are the returned-object trust boundary), #866, #867, #868,
and #869. The independent refute lens
for the design was the Codex review on PR #864, twenty-six rounds, each read
against the unit's principle. Dispositions:

- Confirmed and fixed: probe-derived items must be `advisory` (a
  blocking posture would re-enter the admission gate); the profile
  needs an immutable `id` (name reuse breaks binding); provider and
  credential mode must mirror the identity and profile-identity must
  be one-to-one (a diverging profile fact or a shared identity breaks
  the containment record or the re-enrollment version claim);
  re-enrollment must be same-account, and adopting a legacy identity
  must establish the account binding transactionally or refuse; the
  §11 edges must match the filed issues; deferrals, including the
  continuation digest, must be filed before handoff; the masked label's
  display consumer must be named in every statement of the observation
  allowlist; a `starts-after` relation gates a whole unit, so the retry
  card (#869) starts after both #406 and #408 rather than carrying a
  half-unit relation; the enrolled credential mode's trusted source is
  an identity-bound enrollment record that no current code writes, so
  #406 must introduce it and #867 migrate to it rather than the plan
  claiming it exists; policies persist the profile `id`;
  profile-to-identity is at most one until adoption;
  `auth enable` must exist to reverse `disable`; this record must cover
  every round (the round-9 finding); the record must be a deliverable of
  a named unit, not an asserted current fact (round 10); adoption writes
  that record rather than requiring it to pre-exist (round 11);
  `auth_identity_id` is immutable after creation (round 12); the
  enrolled `(provider, account binding)` is unique across identities
  (round 13); the profile-only selection gate switches on in #867 with
  adoption and migration, not in #406 (round 14); the cutover also
  covers persisted pre-profile bindings (round 15, the round-14 clause
  had scoped the interval to identities only), split into records
  (nonterminal runs and admissions keep their admitted binding under an
  explicit legacy-read rule) and inputs (requests and policies are
  rewritten atomically to the adopted profile so every later selection
  passes the full profile gates; round 16, the round-15 clause had
  applied legacy-read to inputs too, which would have bypassed the
  gates or stranded them); an unadoptable interim identity is retired at
  the cutover and its inputs stay unbound until the operator explicitly
  remaps them, never remapped automatically (round 17), and holds
  nothing live: its nonterminal runs stop in the cutover transaction,
  because without an enrollment record nothing can fence them against a
  same-account replacement (round 18, the round-17 clause had let them
  finish), through the §5.7 cancellation contract, with profile-only
  activation gated on verified teardown (round 19, a transactional
  "stopped" record does not stop a process); `cost_owner` is a profile
  field, since the interim `-review-cost-owner` flag was its only source
  (round 19); `auth doctor` ships in #868 behind the #866 spike, not in
  #867, and legacy-read is permanent for terminal history (round 20);
  #406 joins the code-level refute pass and #869 covers the elaboration
  role too (round 21); the record-only legacy reader covers retired
  identities' history as well, and §13 names all three #869 roles
  (round 22, both echoes of earlier wording); queued inputs carry no
  identity today, so the cutover consumes the daemon's `-auth-identity`
  and `-review-auth-identity` flags as the one pre-profile selection and
  maps inputs per role from the profiles adopted over them (round 23,
  the round-16 clause had assumed a per-record identity without
  checking the code); that flag also covers elaboration, and migrated
  inputs persist the profile `id` alone, re-gated at each selection,
  never pinned to a version (round 24, both corrections to the round-23
  clause); the implementation-flag profile's `cost_owner` is
  operator-supplied at adoption, since `-review-cost-owner` belongs to
  the review identity (round 25). Round 26 (the one-adoption
  cost-owner source) was deferred to #867 by owner decision: rounds 14
  to 26 were #867 cutover mechanics, and further ones are routed there
  rather than specified in §5.4 sentence by sentence.
- Accepted by decision: the Claude same-account check is an operator
  attestation, because the pinned CLI exposes no account identity; the
  attestation is recorded in the transaction.
- Rejected by verification: none; every finding was a real gap.

## Held

Everything else from revision 35: the Codex refresh, lease, and snapshot
contract; the Claude launcher; egress floors; image qualification; Wave 6
scope and the #835 tracker.

Follow-up: #867 (guided enrollment), #866 (probe refresh-safety spike),
#868 (doctor account probe), #869 (explicit alternate-provider retry
card), #873 (continuation compatibility digest).
