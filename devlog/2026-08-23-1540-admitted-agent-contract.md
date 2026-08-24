# Admitted-Agent Contract

Work unit: #894 (plan §5.4 revision 39; source note
devlog/2026-08-23-0825-admitted-agents.md). A `kind:contract` change on
a returned-object trust boundary, so a note is mandatory. Run as one
unit by owner fiat (recorded on tracker #835, 2026-08-23) rather than
the plan's three-way split; the fiat also inserted #894 at the head of
the remaining repo-wide contract chain, ahead of #838 → #692 → #839 →
#713.

## Decisions

**Interim client facts stay on the identity, in a named optional
carrier.** §5.4 moves the store locator, refresh strategy, and snapshot
support to the enrollment and its generations; the live interim flag
path still needs them before #867's adoption creates any enrollment.
Chosen: a value-typed `InterimClientFacts` field on `AuthIdentity`,
present exactly for pre-adoption identities, emptied by adoption, and
kept comparable so the wardstore convergence checks (`identity ==
stored`) stay exact — a pointer field would silently turn them into
address comparisons. Rejected: a hard move with migration-synthesized
enrollments (pre-empts #867's adoption semantics and invents store
manifest digests nobody measured); store-level legacy accessors
(hides a domain-relevant transition state in persistence and widens
every consumer interface).

**The `'none'` refresh-strategy column marker.** 0013's
`CHECK (refresh_strategy <> '')` cannot be dropped without a table
rebuild, and the migration harness runs with foreign keys always on
(children reference `auth_identities`), so a rebuild is unavailable.
A post-adoption identity writes the marker `'none'` — deliberately
outside the `RefreshStrategy` enum — and the reconstruction
cross-check compares through the same mapping. First tried: keeping
the CHECK and calling post-adoption identities unwritable "dormancy";
rejected because this unit's own enrollment accessors must persist
account-bound, interim-free identities to be testable at all.

**Enrollments and offers reference routes by tree name, never by
digest.** §5.4: an endpoint edit changes the route digest and every
agent naming it, never the enrollment — so the enrollment's `Route`
is the stable tree name. The same reasoning keeps the route out of
the offer's digest: which route an offer is authored under is a tree
fact (its directory), supplied to resolution as context. Otherwise a
route edit would cascade through every offer digest too, and a name
would enter a digest, which §5.4 forbids.

**Launch-to-capability mapping requires nothing of `observed`
auxiliary inference.** `forbidden` and `declared` require
`auxiliary_inference_control`; `observed` asks nothing of the
harness. This is what lets the Claude baseline run while its harness
cannot honour a stricter policy, and it means a review launch
(`forbidden`) on an adapter that never proves the control capability
fails admission closed — the §5.4 posture, resolved by policy
authorship at cutover, not by the schema.

**The admission's attended flag is stored and cross-checked, not
derived.** The §5.4 step 5 snapshot lists it; the admission already
carries `OperatingMode`. Chosen: carry both, with validation refusing
disagreement (`Attended == (mode != unattended)`), so the snapshot
stays self-contained and drift is unrepresentable. Rejected: deriving
it on read (the snapshot would no longer say what was admitted
without consulting a rule that could later change).

**Treatment digest excludes billing mode.** §5.4 lists exclusions
(enrollment, generation, cost owner, pricing, terms, deprecation,
labels) without naming billing mode; it is a commercial fact like
pricing and terms, so it is excluded, pinned by a test that moves
billing mode, pricing revision, terms basis, and `not_after` together
and asserts the treatment digest does not move.

**Lineup values are `<agent-name>@<agent-digest>`.** Admission step 2
says the lineup names the digest; the name half keeps the diff
human-readable. A value carrying only a name is refused, because it
could follow a tree edit nobody approved for the role.

## Verification

`go build ./... && go test ./... && go vet ./... && golangci-lint run`
clean at each commit; every acceptance bullet has a named test
(names-in-canonical-body rejection, join-validation rejections, v4
round-trip plus legacy-reader assertions, capability-excess and
expiry-margin typed errors, stage-role resolver exhaustiveness pinned
against the engine constant, lineup key validation, migration
narrowing of a legacy identity row).

Refute-first pass (returned-object trust boundaries: enrollment
generation reconstruction, admission derivations): one independent
adversarial lens instructed to disprove the cross-checks, re-gates,
account rule, lease fencing, encoding versioning, and migration
rewrite. Dispositions:

- **Confirmed, fixed:** an unbound (nil-binding) lease fence was
  accepted for enrollment-store generation appends — per-identity
  serialization held, so no integrity break, but the fence did not
  name the store it guarded. `AppendEnrollmentGeneration` now fails
  closed on an unbound fence, with a test pinning the refusal.
- **Confirmed, fixed (Codex review):** the bound fence's generation
  and manifest coordinates were recorded but never enforced at append,
  so a lease bound to stale or fabricated coordinates could stamp a
  mutation of superseded bytes as the newest generation. The append
  now re-gates the binding against the store's current state — the
  newest ordinal (zero names the bootstrap append; the binding's
  validation floor widened to allow it) and, past bootstrap, that
  generation's manifest digest — with tests pinning the
  consumed-binding, fabricated-generation, and superseded-manifest
  refusals.
- **Confirmed, fixed (Codex review, two rounds):** the closure
  recheck never authenticated the launch or offer legs —
  `ValidateAdmissionAgentDerivations` took neither fragment, so a
  binding naming an unrelated or wrong-stage launch digest, or an
  agent whose pinned offer was never resolved, passed the advertised
  recheck. It now accepts the resolved launch (with the role the
  admission's stage resolves to) and the resolved offer,
  authenticates both digests against the binding and the agent, and
  re-runs the joins the closure itself expresses: effort allowed by
  the offer, effort sendable by the adapter, enrollment client driven
  by the adapter, launch requirements within the adapter's declared
  capability set (the proved-set gate keeps the conformance record at
  admission step 3). A follow-up adversarial refute pass on the
  widened recheck confirmed the declared-capability join and an
  overclaiming binding comment (the image is not a closure-derivable
  recheck; corrected), and re-established the boundary for the rest:
  enrollment and generation authenticity belongs to the re-gating
  store read; tree facts outside every digest (the route names the
  offer and enrollment are authored under, the lineup revision's
  content) and admission-time policy gates (identity enabled, offer
  expiry) remain the #867 resolver's recorded scope.
- **Confirmed, fixed (Codex review):** an agent-bound admission could
  omit instruction provenance — `StageInputs` nil, a pre-v2 snapshot,
  or the implicit Claude v2 delivery — and the recheck's guarded
  vendor comparison treated the absence as success. Those nil shapes
  are valid history for legacy admissions only; a v4 admission is
  newly written, so `Validate` now requires the explicit
  vendor-instruction snapshot on every agent-bound record and the
  recheck compares its vendor unconditionally, with tests pinning the
  three refused shapes and the vendor-mismatch refusal.
- **Rejected by verification** (attacks attempted, code held):
  legacy-reader downgrade of a v4 admission (column-nulling and
  body-stripping both trip the cross-check or the content-address
  recomputation); v4 re-gate bypass through a broken enrollment;
  encoding-version ambiguity or body collision across v1–v4;
  two-identities-per-account through races, empty-binding edges, or
  upserts; stale-fence and regressed-clock lease appends; fragment
  digest type confusion; JSON canonicalization abuse; every probed
  legacy-row shape through the 0052 body rewrite, including the
  `'none'` marker's non-collision.
- **Accepted by decision:** `ValidateAdmissionAgentDerivations` (the
  full closure recheck, including egress-within-route) has no
  production caller yet — the store re-gates the identity, credential
  mode, and generation legs it can resolve, and the closure recheck's
  consumer is the #867 admission resolver. Deliberate under the
  dormant-schemas contract; the function and its fixtures land now so
  the cutover consumes a proven boundary.

## Revisit When

The #867 cutover lands (adoption should empty `Interim` and widen the
identity transition rule to allow it); the pi unit (#895) registers
its harness client member; a launch needs an auxiliary-inference
stance the three-value policy cannot express; or the first real
lineup authorship finds the `name@digest` value format wanting.
