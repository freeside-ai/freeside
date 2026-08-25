# Recommendation-Led Attention (Plan Revision 40)

**Work unit:** plan revision adopting the UX-review response's contract
decisions. **Origin:** an external UX review of the macOS and iOS clients
(`main` @ `0e81ee2e`, 2026-08-25, 27 findings) and its design response
bundle (archived at `docs/reviews/2026-08-25-ux-review/` by PR #934; see
Inputs below).

## Decisions

1. **Generic recommendation authority on attention items** (§4, §5.13,
   §9). `recommendation? {action, reason, source, provenance,
   confidence?}`, with immutable source-specific provenance: deterministic
   content-addressed rule digest and input digest for `daemon_policy`;
   judgment site, invocation, and artifact digest for `agent_judgment`;
   policy key, resolved-policy digest, and daemon-authored application digest
   for `project_policy`. Each source record itself commits to the item's
   daemon-owned decision-surface identity, so valid source output cannot be
   replayed onto a foreign or newer surface. #916 fixes that identity's
   invariants — eligibility-independent, telemetry-stable,
   surface-distinguishing, and non-cyclic — and defers the exact
   epoch-and-digest mechanism to contract unit #942 (see the reframe finding
   below). The full item and approval binding still includes the source
   artifact digest.
   Creation and reconstruction derive every eligible source record from current
   authoritative state. Exactly one yields the canonical recommendation; zero
   or multiple yields absence and equally weighted actions, with no precedence
   or tie-break. The stored optional recommendation must equal that exact
   result. For the unique record, reconstruction requires the source-to-item
   association and rederives the canonical action, reason, and confidence from
   it. The client never infers a
   recommendation; offer order carries no endorsement; no recommendation,
   no block. Finding adjudication's
   parked batch is the type case: its item-level recommendation endorses
   the accept-the-recommended-route action, and each finding's own
   route, rationale, producer, and confidence stay in the §7 artifact.
   Rejected: client-side inference (embeds policy in Swift, the
   exact thing §5.13 centralizes); per-type ad hoc shapes (three shapes
   to audit instead of one).
2. **Truthful capability rendering** (§9). Clients filter unexecutable
   actions to drill-down, render a not-decidable-here state when nothing
   faithful remains, and never show disabled placeholders or roadmap
   copy. This replaces the shipped "Actions carrying discussion or
   parameters arrive with later units" presentation.
3. **P1-2 disposition (the review's unsupported-actions finding), a
   hybrid.** Of the ten actions `ActionOutcome` classifies `pending`:
   `continue_under_policy` lands with Wave 6 #844;
   `choose_alternate_profile` gets its own Wave 7 publication-profile
   transaction (#936, not #869's alternate-agent retry); `adjudicate` is
   likely vestigial
   (superseded by `accept_recommended_route` / `choose_alternative_route`)
   and the contract unit must retire it or reassign it to an executable
   `review_dispute` transaction before client adoption; `discuss`,
   `request_changes`, `answer_and_retry`, `answer_without_retry`,
   `return_to_agent`, and `retry_with_capabilities` get Wave 7
   transaction units; `convert_to_policy` alone is carved out of the 1B
   phone-decidability claim, because §4 already routes it through the
   deferred control-plane proposal surface and the diminishing-returns
   card stays decidable without it. Rejected: implementing every
   transaction in 1B (drags the policy-proposal surface forward);
   removing whole workflows from the exit claim (unnecessary — only one
   action is genuinely blocked on deferred scope).
4. **Placement: Wave 7, not Phase 3** (§11). Missing facts and dead
   actions are Phase 1B fundamentals; Phase 3 "Comprehension and
   Interaction" is advanced interaction. Contract-first sequencing: one
   attention-presentation contract unit precedes daemon producers and
   client adoption.

## Verification Findings That Changed the Work

- The review refute-first pass confirmed that authenticating only the
  provenance object still allowed arbitrary recommendation payload to wear
  a trusted source label. The contract therefore content-addresses daemon
  rule semantics and requires creation and reconstruction to rederive and
  compare action, reason, and confidence against every source binding.
- The next pass exposed the same class one level wider: source and payload
  authenticity did not prove applicability to the containing item. The miss
  was treating producer binding as the whole trust boundary. The final
  contract requires each authoritative source record itself to commit to the
  item's decision-surface identity: item identity, subject, requested
  decisions, sibling artifact surface, and PR head. A separately supplied item
  digest is never authority, so valid
  source output cannot be replayed onto a foreign or newer decision surface.
- A later refute pass caught the content-address cycle in asking a source
  artifact to commit to a set containing its own final digest. The uniform
  source-binding rule subtracts the recommendation provenance's own
  `artifact_digest` before hashing and changes nothing else: item-side
  binding-set equality still proves the source artifact is present in the full
  set used by approvals and commands, so the artifact-side self-hash was both
  redundant and impossible. The finalized immutable artifact carries that
  reduced digest; pre-invocation inputs cannot, because the requested actions
  are derived from the invocation output, while their immutable binding to the
  artifact still proves source authenticity. (This own-artifact subtraction
  was later found to couple the identity to eligibility and is deferred to
  #942; see the reframe finding below.)
- A subsequent pass showed that authentic provenance still did not make its
  record canonical when multiple valid records applied. The final rule derives
  eligibility from current authoritative state at record granularity: one
  eligible record yields the recommendation, while zero or multiple suppress
  it without blocking the item's actions. Rejected: caller selection and an
  implicit precedence that the plan has never defined.
- The delivery-path pass showed that general `item_version` advances when
  timing aggregates change, which would invalidate an immutable recommendation
  before the delivered card could be acted on. A first fix bound a separate
  daemon-owned `decision_surface_version` epoch, but adversarial review then
  showed that epoch's own-artifact subtraction coupled it to eligibility: when
  a second applicable source joins, unique-or-none drops the recommendation,
  the artifact input flips, and monotonicity strands the first record
  permanently. A structural agent_judgment-class subtraction reframe still
  stranded on the policy axis and could collapse distinct surfaces to one
  binding. After three same-class findings, #916 stops specifying the mechanism
  inline: it states the four required invariants (eligibility-independent,
  telemetry-stable, surface-distinguishing, non-cyclic) and defers the
  epoch-and-digest construction, with the full break analysis, to contract unit
  #942. Rejected inline: item-version binding, own-artifact subtraction,
  agent_judgment-class subtraction, regenerating source artifacts for
  telemetry, and weakening replay to pure content equality.
- The override-metric review exposed a record-layer gap: a device id and
  accepted secondary action did not show whether the client had filtered out
  the recommendation. The final contract assigns #924 a daemon-authored,
  content-addressed action surface bound to the device, current item decision
  surface, and registered client-capability contract; forced overrides are
  derived only from that revalidated record, while missing evidence stays
  unclassified.
- The design bundle's normative token sheet has two values on the wrong
  side of its own thresholds under standard WCAG math (which reproduces
  the sheet's shipped-palette numbers exactly): day `ruleStrong`
  `#B9AF92` is 1.89:1 on the card ground (sheet claims 3.1, threshold
  3.0), and dusk `waxText` `#C55F3E` is 4.25:1 (sheet claims 4.6,
  threshold 4.5). The client color unit must re-derive values and assert
  ratios in a test, exactly as the sheet itself instructs.
- The current contract carries no recommendation field
  (`requested_decision` is a flat action list), and "recommendation-led"
  appears in the plan only for `finding_adjudication`; the generic shape
  is a genuine material change, hence this revision.
- Overlapping open contract deferrals #892 (adjudication finding
  context), #893 (offered-alternatives authority), and #901
  (per-invocation cost) are consumed by, not duplicated by, the new
  contract unit; the spine positions them at Wave 7 planning.

## Inputs

The review text and the design response are archived at
`docs/reviews/2026-08-25-ux-review/` (original design handoff export:
`Freeside UX Review Planning.zip`, `design_handoff_freeside_ux_review/`):
verdicts on all 27 findings (17 adopt, 9 adopt-modified, 1 later), twelve
proposals `1a`–`1l`, the token sheet, and the work split. Four review remedies
were refused there and stand: no Dynamic Type cap, no system-color
substitution, no system-red destructive content color (wax stays; native
destructive role only on system-owned surfaces), no macOS sticky action
footer. The client work is tracked by the "Client UX recomposition" tracker
#933 (units #925–#932); the Wave 7 contract and transaction candidates are
#917–#924 and #936. Issue #939 records archive completion.

Revisit unique-or-none only when the plan defines a legitimate multi-source
relationship, such as project policy overriding a daemon default; precedence
is a material plan change, never a default to assume. Also revisit when the
policy-proposal surface lands (re-admit `convert_to_policy` to the decidability
claim), or if Wave 7 planning finds the added closure scope exceeds review
bandwidth (the telemetry contracts are the first candidate to slip to Wave 8).
