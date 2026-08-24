import Foundation
import OpenAPIRuntime
import Testing

@testable import FreesideAPI

/// Direct unit tests for the pure contract validators extracted from the
/// mock actor (#205). The transport-level suites reach these only as a
/// coarse HTTP 500/422, so any breach satisfies them; here each predicate
/// is pinned in isolation to the exact breach string it returns (and `nil`
/// for a valid input), so a regression that trips the wrong invariant, or
/// silently accepts a bad one, is caught at its source.
@Suite struct MockContractValidationTests {
    @Test func runAttemptLineageMatchesDaemonValidation() {
        let snapshot = RunFixtures.defaultRuns().first {
            $0.run.id == RunFixtures.activeRunID
        }!
        #expect(
            MockContractValidation.runSnapshotBreach(snapshot, serverRevision: 12) == nil)

        var missingReason = snapshot
        missingReason.run.attempt_reason = nil
        #expect(
            MockContractValidation.runSnapshotBreach(missingReason, serverRevision: 12)
                == "inconsistent production attempt lineage")

        var noncontiguous = snapshot
        noncontiguous.run.stages[0].attempts[1].number = 3
        #expect(
            MockContractValidation.runSnapshotBreach(noncontiguous, serverRevision: 12)
                == "invalid stage attempt")
    }

    // MARK: - itemValidityBreach

    @Test func validItemHasNoBreach() {
        let item = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(MockContractValidation.itemValidityBreach(item) == nil)
    }

    @Test func healthPostureIsExplicitAndTypeScoped() {
        let fixture = AttentionFixtures.fixture(type: .system_health).item
        #expect(fixture.posture?.value1 == .advisory)
        #expect(MockContractValidation.itemValidityBreach(fixture) == nil)

        var missing = fixture
        missing.posture = nil
        #expect(
            MockContractValidation.itemValidityBreach(missing)
                == "system_health item lacks posture")

        var wrongType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongType.posture = .init(value1: .blocking)
        #expect(
            MockContractValidation.itemValidityBreach(wrongType)
                == "posture on a non-system_health item")

        var supersededAdvisory = fixture
        supersededAdvisory.blocking_supersession = .init(
            value1: .init(kind: .backup_encryption_waiver, repository_id: 42))
        #expect(
            MockContractValidation.itemValidityBreach(supersededAdvisory)
                == "blocking_supersession on an advisory system_health item")
    }

    @Test func pullRequestReferenceIsExactAndTypeScoped() {
        let fixture = AttentionFixtures.fixture(type: .ready_for_final_review).item
        #expect(fixture.pr_reference?.value1.repo == "owner/repo")
        #expect(fixture.pr_reference?.value1.number == 123)
        #expect(MockContractValidation.itemValidityBreach(fixture) == nil)

        var missing = fixture
        missing.pr_reference = nil
        #expect(
            MockContractValidation.itemValidityBreach(missing)
                == "ready_for_final_review item lacks pr_reference")

        var wrongType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongType.pr_reference = fixture.pr_reference
        #expect(
            MockContractValidation.itemValidityBreach(wrongType)
                == "pr_reference on a non-ready_for_final_review item")

        var invalidRepo = fixture
        invalidRepo.pr_reference?.value1.repo = "owner/../repo"
        #expect(
            MockContractValidation.itemValidityBreach(invalidRepo)
                == "invalid pr_reference repo")

        var invalidNumber = fixture
        invalidNumber.pr_reference?.value1.number = 0
        #expect(
            MockContractValidation.itemValidityBreach(invalidNumber)
                == "non-positive pr_reference number")
    }

    @Test func readinessSummaryIsReadyScopedAndLegacyOptional() {
        let clean = AttentionFixtures.fixture(type: .ready_for_final_review).item
        #expect(MockContractValidation.itemValidityBreach(clean) == nil)
        #expect(clean.readiness?.value1._class == .ready_clean)

        let degraded = AttentionFixtures.degradedReady().item
        #expect(MockContractValidation.itemValidityBreach(degraded) == nil)

        var legacy = clean
        legacy.readiness = nil
        #expect(MockContractValidation.itemValidityBreach(legacy) == nil)

        var emptyDigest = clean
        emptyDigest.readiness?.value1.evaluation_set_digest = ""
        #expect(
            MockContractValidation.itemValidityBreach(emptyDigest)
                == "empty readiness evaluation_set_digest")

        var wrongType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongType.readiness = clean.readiness
        #expect(
            MockContractValidation.itemValidityBreach(wrongType)
                == "readiness on a non-ready_for_final_review item")
    }

    @Test func itemValidityBreachNamesTheFailedInvariant() {
        var empty = AttentionFixtures.fixture(type: .spec_approval).item
        empty.id = ""
        #expect(MockContractValidation.itemValidityBreach(empty) == "empty id")

        var noProject = AttentionFixtures.fixture(type: .spec_approval).item
        noProject.project_id = ""
        #expect(MockContractValidation.itemValidityBreach(noProject) == "empty project_id")

        var zeroVersion = AttentionFixtures.fixture(type: .spec_approval).item
        zeroVersion.item_version = 0
        #expect(MockContractValidation.itemValidityBreach(zeroVersion) == "non-positive item_version")

        var zeroCreatedAt = AttentionFixtures.fixture(type: .spec_approval).item
        zeroCreatedAt.created_at = daemonZeroInstant
        #expect(MockContractValidation.itemValidityBreach(zeroCreatedAt) == "zero created_at")

        // A head-bound evidence artifact must name the same head as the
        // item; only the item's head moves here, so the binding diverges.
        var headMismatch = AttentionFixtures.fixture(type: .spec_approval).item
        headMismatch.pr_head_sha = "deadbeef"
        #expect(
            MockContractValidation.itemValidityBreach(headMismatch)
                == "head-bound evidence names a different head than the item")

        // A second evidence entry reusing the first's id trips the
        // duplicate check inside the evidence loop.
        var duplicateEvidence = AttentionFixtures.fixture(type: .spec_approval).item
        duplicateEvidence.evidence_snapshot.append(duplicateEvidence.evidence_snapshot[0])
        #expect(
            MockContractValidation.itemValidityBreach(duplicateEvidence)
                == "duplicate evidence artifact id")

        // artifact_digests must be the sorted, deduplicated union of every
        // rendered digest; dropping one breaks the canonical-union invariant.
        var wrongUnion = AttentionFixtures.fixture(type: .spec_approval).item
        wrongUnion.artifact_digests = []
        #expect(
            MockContractValidation.itemValidityBreach(wrongUnion)
                == "artifact_digests is not the canonical union of rendered digests")
    }

    @Test func reviewRecoveryBindingIsExactAndTypeScoped() {
        let fixture = AttentionFixtures.fixture(type: .review_contradiction).item
        #expect(MockContractValidation.itemValidityBreach(fixture) == nil)

        var missing = fixture
        missing.review_recovery_binding = nil
        #expect(
            MockContractValidation.itemValidityBreach(missing)
                == "review_contradiction item lacks review_recovery_binding")

        var wrongType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongType.review_recovery_binding = fixture.review_recovery_binding
        #expect(
            MockContractValidation.itemValidityBreach(wrongType)
                == "review_recovery_binding on a non-review_contradiction item")

        var empty = fixture
        empty.review_recovery_binding?.value1.invocation_id = ""
        #expect(
            MockContractValidation.itemValidityBreach(empty)
                == "empty review_recovery_binding field")

        var zeroRound = fixture
        zeroRound.review_recovery_binding?.value1.round = 0
        #expect(
            MockContractValidation.itemValidityBreach(zeroRound)
                == "non-positive review recovery round")

        var wrongHead = fixture
        wrongHead.pr_head_sha = "deadbeef"
        #expect(
            MockContractValidation.itemValidityBreach(wrongHead)
                == "review recovery binding disagrees with item subject or head")
    }

    @Test func reviewConfigurationRecoveryIsExactAndTypeScoped() {
        let fixture = AttentionFixtures.fixture(type: .review_configuration).item
        #expect(MockContractValidation.itemValidityBreach(fixture) == nil)

        var missing = fixture
        missing.review_configuration_recovery = nil
        #expect(
            MockContractValidation.itemValidityBreach(missing)
                == "review_configuration item lacks review_configuration_recovery")

        var wrongType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongType.review_configuration_recovery = fixture.review_configuration_recovery
        #expect(
            MockContractValidation.itemValidityBreach(wrongType)
                == "review_configuration_recovery on a non-review_configuration item")

        var empty = fixture
        empty.review_configuration_recovery?.value1.superseded_profile_digest = ""
        #expect(
            MockContractValidation.itemValidityBreach(empty)
                == "empty review_configuration_recovery field")

        var zeroRound = fixture
        zeroRound.review_configuration_recovery?.value1.round = 0
        #expect(
            MockContractValidation.itemValidityBreach(zeroRound)
                == "non-positive review configuration recovery round")

        var zeroRepository = fixture
        zeroRepository.review_configuration_recovery?.value1.repository_id = 0
        #expect(
            MockContractValidation.itemValidityBreach(zeroRepository)
                == "non-positive review configuration recovery repository_id")

        var wrongHead = fixture
        wrongHead.pr_head_sha = "deadbeef"
        #expect(
            MockContractValidation.itemValidityBreach(wrongHead)
                == "review configuration recovery disagrees with item subject or head")
    }

    @Test func codexReenrollmentRecoveryIsExactAndTypeScoped() {
        let fixture = AttentionFixtures.fixture(type: .system_health).item
        #expect(MockContractValidation.itemValidityBreach(fixture) == nil)

        var wrongType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongType.codex_reenrollment_recovery_binding =
            fixture.codex_reenrollment_recovery_binding
        #expect(
            MockContractValidation.itemValidityBreach(wrongType)
                == "codex_reenrollment_recovery_binding on a non-system_health item")

        var empty = fixture
        empty.codex_reenrollment_recovery_binding?.value1.auth_store_digest = ""
        #expect(
            MockContractValidation.itemValidityBreach(empty)
                == "empty codex_reenrollment_recovery_binding field")

        var zeroFence = fixture
        zeroFence.codex_reenrollment_recovery_binding?.value1.lease_fence = 0
        #expect(
            MockContractValidation.itemValidityBreach(zeroFence)
                == "non-positive codex re-enrollment lease fence")

        var missingAction = fixture
        missingAction.requested_decision.removeAll { $0 == .resolve_reenrollment }
        #expect(
            MockContractValidation.itemValidityBreach(missingAction)
                == "codex re-enrollment binding lacks resolve_reenrollment")

        var missingBinding = fixture
        missingBinding.codex_reenrollment_recovery_binding = nil
        #expect(
            MockContractValidation.itemValidityBreach(missingBinding)
                == "resolve_reenrollment lacks codex re-enrollment binding")
    }

    @Test func findingAdjudicationBindingIsExactAndTypeScoped() {
        let fixture = AttentionFixtures.fixture(type: .finding_adjudication).item
        #expect(MockContractValidation.itemValidityBreach(fixture) == nil)

        var missing = fixture
        missing.finding_adjudication = nil
        #expect(
            MockContractValidation.itemValidityBreach(missing)
                == "finding_adjudication item lacks its binding")

        var wrongType = AttentionFixtures.fixture(type: .spec_approval).item
        wrongType.finding_adjudication = fixture.finding_adjudication
        #expect(
            MockContractValidation.itemValidityBreach(wrongType)
                == "finding_adjudication binding on a different item type")

        var noProposals = fixture
        noProposals.finding_adjudication?.value1.proposals = []
        #expect(
            MockContractValidation.itemValidityBreach(noProposals)
                == "finding_adjudication has no proposals")

        var repeatedRoute = fixture
        repeatedRoute.finding_adjudication?.value1.proposals[0].offered_alternatives[0].route =
            .decline
        #expect(
            MockContractValidation.itemValidityBreach(repeatedRoute)
                == "finding_adjudication alternative repeats the recommended route")

        var modelWithoutConfidence = fixture
        modelWithoutConfidence.finding_adjudication?.value1.proposals[0].confidence = nil
        #expect(
            MockContractValidation.itemValidityBreach(modelWithoutConfidence)
                == "finding_adjudication model proposal lacks confidence")

        var blankExplanations = fixture
        blankExplanations.finding_adjudication?.value1.proposals[0].cited_rules = [""]
        blankExplanations.finding_adjudication?.value1.proposals[0].assumptions = ["  "]
        blankExplanations.finding_adjudication?.value1.proposals[0].open_questions = ["\n"]
        #expect(MockContractValidation.itemValidityBreach(blankExplanations) == nil)

        var modelAllowed = fixture
        modelAllowed.finding_adjudication?.value1.proposals[0].goal_relationship = .required
        modelAllowed.finding_adjudication?.value1.proposals[0].compatibility = .init(value1: .allowed)
        modelAllowed.finding_adjudication?.value1.proposals[0].route = .remediate
        modelAllowed.finding_adjudication?.value1.proposals[0].offered_alternatives = []
        #expect(
            MockContractValidation.itemValidityBreach(modelAllowed)
                == "finding_adjudication model proposal mints allowed")

        var engineWithConfidence = fixture
        engineWithConfidence.finding_adjudication?.value1.proposals[0].producer = .engine
        engineWithConfidence.finding_adjudication?.value1.proposals[0].goal_relationship = .required
        engineWithConfidence.finding_adjudication?.value1.proposals[0].compatibility = .init(
            value1: .allowed)
        engineWithConfidence.finding_adjudication?.value1.proposals[0].route = .remediate
        engineWithConfidence.finding_adjudication?.value1.proposals[0].offered_alternatives = []
        #expect(
            MockContractValidation.itemValidityBreach(engineWithConfidence)
                == "finding_adjudication engine proposal carries confidence")

        var engineOutsideFastPath = fixture
        engineOutsideFastPath.finding_adjudication?.value1.proposals[0].producer = .engine
        engineOutsideFastPath.finding_adjudication?.value1.proposals[0].confidence = nil
        #expect(
            MockContractValidation.itemValidityBreach(engineOutsideFastPath)
                == "finding_adjudication engine proposal is not the deterministic fast path")

        var noAlternatives = fixture
        noAlternatives.finding_adjudication?.value1.proposals[0].offered_alternatives = []
        #expect(
            MockContractValidation.itemValidityBreach(noAlternatives)
                == "finding_adjudication has no offered alternatives")
        noAlternatives.requested_decision.removeAll { $0 == .choose_alternative_route }
        #expect(MockContractValidation.itemValidityBreach(noAlternatives) == nil)

        var incompatibleRoute = fixture
        incompatibleRoute.finding_adjudication?.value1.proposals[0]
            .offered_alternatives[0].route = ._defer
        #expect(
            MockContractValidation.itemValidityBreach(incompatibleRoute)
                == "finding_adjudication alternative is incompatible with proposal axes")

        var emptyConsequence = fixture
        emptyConsequence.finding_adjudication?.value1.proposals[0]
            .offered_alternatives[0].consequence = ""
        #expect(
            MockContractValidation.itemValidityBreach(emptyConsequence)
                == "finding_adjudication alternative has an empty consequence")

        var blankConsequence = fixture
        blankConsequence.finding_adjudication?.value1.proposals[0]
            .offered_alternatives[0].consequence = "  \n"
        #expect(
            MockContractValidation.itemValidityBreach(blankConsequence)
                == "finding_adjudication alternative has an empty consequence")
    }

    // The text-claim carrier (#217): the daemon recomputes the claim digest
    // over the content bytes, so the mirrored checks here are the empty
    // content, the byte cap, and the binding rule. The invalid-media-type
    // and invalid-UTF-8 arms are unrepresentable over the generated shapes
    // (the enum pins the two media types; Swift String is always valid
    // UTF-8). The happy path rides validItemHasNoBreach: every non-mechanical
    // fixture now carries a text claim.
    @Test func textClaimBreachesNameTheFailedInvariant() {
        // The fixture's summary claim is agent_claims[1]; index 0 stays the
        // referenced screenshot claim.
        var tampered = AttentionFixtures.fixture(type: .spec_approval).item
        tampered.agent_claims[1].text?.content = "tampered summary"
        #expect(
            MockContractValidation.itemValidityBreach(tampered)
                == "claim digest does not match its text content")

        var emptyContent = AttentionFixtures.fixture(type: .spec_approval).item
        emptyContent.agent_claims[1].text?.content = ""
        #expect(
            MockContractValidation.itemValidityBreach(emptyContent)
                == "empty claim text content")

        // Inline text is barred from the high-sensitivity tier (§5.14
        // no-high-sensitivity-at-rest: CachedState persists item metadata
        // to disk); only the provenance flips, so the sensitivity bar is
        // the one invariant the seed breaks.
        var highSensitivity = AttentionFixtures.fixture(type: .spec_approval).item
        highSensitivity.agent_claims[1].provenance = .head_bound(
            .init(
                producer_class: .agent,
                producer_invocation_id: "inv-high",
                head_binding: .head_bound,
                source_head_sha: "cafebabe",
                verification_recipe_digest: nil,
                sensitivity_class: .high_sensitivity
            ))
        #expect(
            MockContractValidation.itemValidityBreach(highSensitivity)
                == "high-sensitivity claim carries inline text")

        // The digest is recomputed over the oversize content so the size cap
        // is the one invariant the seed breaks.
        var oversize = AttentionFixtures.fixture(type: .spec_approval).item
        let bigContent = String(repeating: "a", count: 65537)
        oversize.agent_claims[1].text?.content = bigContent
        oversize.agent_claims[1].digest = MockContractValidation.sha256Digest(of: bigContent)
        #expect(
            MockContractValidation.itemValidityBreach(oversize)
                == "claim text exceeds the inline size cap")
    }

    // Pins the derivation to a fixed vector (FIPS 180-2 "abc"), so the Swift
    // twin and domain.ClaimText.ComputeDigest can only agree or both be
    // wrong in the same published way.
    @Test func sha256DigestMatchesTheKnownVector() {
        #expect(
            MockContractValidation.sha256Digest(of: "abc")
                == "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
    }

    // Every fixture passes the full validity check, each text claim's digest
    // recomputes from its content, and the summary claim appears exactly on
    // the types that carry §9's summary layer (the purely mechanical
    // system_health, blocked, and the exact one-carrier run proposal stay text-free).
    @Test(arguments: AttentionFixtures.phase1Types)
    func fixtureTextClaimsBindTheirContent(type: Components.Schemas.AttentionType) {
        let item = AttentionFixtures.fixture(type: type).item
        #expect(MockContractValidation.itemValidityBreach(item) == nil)
        for claim in item.agent_claims {
            guard let text = claim.text else { continue }
            #expect(claim.digest == MockContractValidation.sha256Digest(of: text.content))
        }
        let hasText = item.agent_claims.contains { $0.text != nil }
        #expect(hasText == (type != .system_health && type != .blocked && type != .run_proposal))
    }

    // MARK: - itemPolicyBreach

    @Test func validPolicyHasNoBreach() {
        let item = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(MockContractValidation.itemPolicyBreach(item) == nil)
    }

    @Test func blockedOffersNothingAndAnyActionIsRejected() {
        let blocked = AttentionFixtures.fixture(type: .blocked).item
        #expect(blocked.requested_decision.isEmpty)
        #expect(MockContractValidation.itemPolicyBreach(blocked) == nil)

        var blockedWithAction = blocked
        blockedWithAction.requested_decision = [.stop]
        #expect(
            MockContractValidation.itemPolicyBreach(blockedWithAction)
                == "action stop is not allowed for blocked")
    }

    @Test func nonBlockedMustOfferAnAllowedAction() {
        var empty = AttentionFixtures.fixture(type: .spec_approval).item
        empty.requested_decision = []
        #expect(MockContractValidation.itemPolicyBreach(empty) == "no offered actions")

        // `retry` is an execution_failure action, outside spec_approval's set.
        var stray = AttentionFixtures.fixture(type: .spec_approval).item
        stray.requested_decision = [.retry]
        #expect(
            MockContractValidation.itemPolicyBreach(stray)
                == "action retry is not allowed for spec_approval")
    }

    // MARK: - validate (command well-formedness)

    @Test func validCommandDoesNotThrow() throws {
        let snapshot = AttentionFixtures.fixture(type: .spec_approval)
        try MockContractValidation.validate(command(against: snapshot))
    }

    @Test func malformedCommandThrowsWithTheReason() {
        let snapshot = AttentionFixtures.fixture(type: .spec_approval)

        expectMalformed(reason: "empty command_id") {
            var c = command(against: snapshot)
            c.command_id = ""
            return c
        }
        expectMalformed(reason: "empty device_id") {
            var c = command(against: snapshot)
            c.device_id = ""
            return c
        }
        expectMalformed(reason: "empty item_id") {
            var c = command(against: snapshot)
            c.payload.item_id = ""
            return c
        }
        expectMalformed(reason: "non-positive item_version") {
            var c = command(against: snapshot)
            c.payload.item_version = 0
            return c
        }
        expectMalformed(reason: "non-positive expected_entity_version") {
            var c = command(against: snapshot)
            c.expected_entity_version = 0
            return c
        }
        expectMalformed(reason: "empty artifact digest") {
            var c = command(against: snapshot)
            c.payload.artifact_digests = [""]
            return c
        }
        expectMalformed(reason: "empty attachment digest") {
            var c = command(against: snapshot)
            c.payload.attachments = [""]
            return c
        }
        expectMalformed(reason: "duplicate attachment digest") {
            var c = command(against: snapshot)
            c.payload.attachments = ["sha256:a", "sha256:a"]
            return c
        }

        let adjudication = AttentionFixtures.fixture(type: .finding_adjudication)
        expectMalformed(reason: "invalid alternative_choices") {
            var c = command(against: adjudication)
            c.payload.action = .choose_alternative_route
            return c
        }
        expectMalformed(reason: "finding adjudication input on accept") {
            var c = command(against: adjudication)
            c.payload.action = .accept_recommended_route
            c.payload.alternative_choices = [
                .init(finding_id: "review-finding-17", route: ._defer)
            ]
            return c
        }
        expectMalformed(reason: "invalid run_proposal_revision") {
            var c = command(against: AttentionFixtures.fixture(type: .run_proposal))
            c.payload.action = .start_with_changes
            c.payload.run_proposal_revision = .init(
                value1: .init(
                    intent: .implement_subject,
                    expected_cost_units: 25,
                    scope: .init(
                        component_count: 2,
                        declared_path_count: 3,
                        touches_control_plane: false
                    )
                )
            )
            c.payload.alternative_choices = [
                .init(finding_id: "review-finding-17", route: .dispute)
            ]
            return c
        }
        expectMalformed(reason: "invalid snooze_until") {
            var c = command(against: AttentionFixtures.fixture(type: .run_proposal))
            c.payload.action = .snooze
            c.payload.snooze_until = Date(timeIntervalSince1970: 1_786_506_245)
            c.payload.alternative_choices = [
                .init(finding_id: "review-finding-17", route: .dispute)
            ]
            return c
        }
    }

    // MARK: - snapshotBreach (metadata + evidence policy re-gate)

    @Test func validSnapshotUnderApprovedRecipeHasNoBreach() {
        let snapshot = AttentionFixtures.fixture(type: .spec_approval)
        #expect(
            MockContractValidation.snapshotBreach(
                snapshot, approvedRecipes: [AttentionFixtures.approvedRecipeDigest]) == nil)
    }

    @Test func snapshotBreachReGatesMetadataAndPolicy() {
        let approved: Set<String> = [AttentionFixtures.approvedRecipeDigest]

        var zeroEntity = AttentionFixtures.fixture(type: .spec_approval)
        zeroEntity.entity_version = 0
        #expect(
            MockContractValidation.snapshotBreach(zeroEntity, approvedRecipes: approved)
                == "non-positive entity_version")

        var zeroRevision = AttentionFixtures.fixture(type: .spec_approval)
        zeroRevision.as_of_revision = 0
        #expect(
            MockContractValidation.snapshotBreach(zeroRevision, approvedRecipes: approved)
                == "non-positive as_of_revision")

        // The evidence gate re-runs against the trusted approved set, never
        // the row's word: an empty set approves nothing.
        let unapproved = AttentionFixtures.fixture(type: .spec_approval)
        #expect(
            MockContractValidation.snapshotBreach(unapproved, approvedRecipes: [])
                == "evidence artifact art-log-spec_approval recipe is not approved")

        // publish_eligible is policy-computed; under an approved recipe a
        // stale false is corrupt reconstructed data.
        var staleBit = AttentionFixtures.fixture(type: .spec_approval)
        staleBit.item.evidence_snapshot[0].publish_eligible = false
        #expect(
            MockContractValidation.snapshotBreach(staleBit, approvedRecipes: approved)
                == "evidence artifact art-log-spec_approval carries a stale publish_eligible bit")
    }

    // MARK: - timingBreach

    @Test func zeroDeliveryTimingHasNoBreach() {
        let timing = Components.Schemas.TimingSummary(
            delivery_count: 0,
            first_submitted_at: nil,
            first_accepted_at: nil,
            first_opened_at: nil,
            submit_to_first_open: nil
        )
        #expect(MockContractValidation.timingBreach(timing) == nil)
    }

    @Test func fullyDerivedTimingWithAgreeingSpanHasNoBreach() {
        let submitted = Date(timeIntervalSince1970: 1_752_000_000)
        let opened = submitted.addingTimeInterval(60)
        let timing = Components.Schemas.TimingSummary(
            delivery_count: 1,
            first_submitted_at: submitted,
            first_accepted_at: nil,
            first_opened_at: opened,
            submit_to_first_open: 60 * 1_000_000_000
        )
        #expect(MockContractValidation.timingBreach(timing) == nil)
    }

    @Test func timingBreachNamesTheFailedInvariant() {
        let submitted = Date(timeIntervalSince1970: 1_752_000_000)
        let opened = submitted.addingTimeInterval(60)

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: -1, first_submitted_at: nil, first_accepted_at: nil,
                    first_opened_at: nil, submit_to_first_open: nil)) == "negative delivery_count")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 0, first_submitted_at: submitted, first_accepted_at: nil,
                    first_opened_at: nil, submit_to_first_open: nil))
                == "timing without deliveries carries endpoints")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 1, first_submitted_at: nil, first_accepted_at: nil,
                    first_opened_at: nil, submit_to_first_open: nil))
                == "deliveries without first_submitted_at")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 1, first_submitted_at: submitted,
                    first_accepted_at: submitted.addingTimeInterval(-1), first_opened_at: nil,
                    submit_to_first_open: nil)) == "first_accepted_at before first_submitted_at")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 1, first_submitted_at: submitted, first_accepted_at: nil,
                    first_opened_at: opened, submit_to_first_open: nil))
                == "submit_to_first_open missing")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 1, first_submitted_at: submitted, first_accepted_at: nil,
                    first_opened_at: opened, submit_to_first_open: 1))
                == "submit_to_first_open disagrees with its endpoints")
    }

    // MARK: - deliveryBreach

    @Test func validDeliveryHasNoBreach() {
        let delivery = submittedDelivery()
        #expect(
            MockContractValidation.deliveryBreach(
                delivery, serverRevision: 1, hasParentItem: true) == nil)
    }

    @Test func deliveryBreachNamesTheFailedInvariant() {
        var zeroEntity = submittedDelivery()
        zeroEntity.entity_version = 0
        #expect(
            MockContractValidation.deliveryBreach(
                zeroEntity, serverRevision: 1, hasParentItem: true) == "non-positive entity_version")

        // The row's as_of_revision may not run ahead of the server.
        #expect(
            MockContractValidation.deliveryBreach(
                submittedDelivery(), serverRevision: 0, hasParentItem: true)
                == "as_of_revision outside the server revision")

        #expect(
            MockContractValidation.deliveryBreach(
                submittedDelivery(attempt: 0), serverRevision: 1, hasParentItem: true)
                == "non-positive attempt")

        // A delivery row exists only for an existing item; an orphan is
        // unrepresentable daemon state.
        #expect(
            MockContractValidation.deliveryBreach(
                submittedDelivery(), serverRevision: 1, hasParentItem: false) == "no parent item")

        // submitted_at is required and never the type's zero instant.
        #expect(
            MockContractValidation.deliveryBreach(
                submittedDelivery(submittedAt: daemonZeroInstant), serverRevision: 1,
                hasParentItem: true) == "submitted_at is unset")
    }

    // MARK: - Helpers

    /// A well-formed client command bound to `snapshot`, matching the
    /// transport suite's builder shape.
    private func command(
        against snapshot: Components.Schemas.AttentionItemSnapshot
    ) -> Components.Schemas.ClientCommand {
        .init(
            command_id: "cmd-1",
            device_id: "device-mock",
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .init(
                item_id: snapshot.item.id,
                action: snapshot.item.requested_decision[0],
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests
            )
        )
    }

    private func expectMalformed(
        reason: String,
        _ build: () -> Components.Schemas.ClientCommand,
        sourceLocation: SourceLocation = #_sourceLocation
    ) {
        #expect(sourceLocation: sourceLocation) {
            try MockContractValidation.validate(build())
        } throws: { error in
            guard let malformed = error as? MockServer.MalformedCommandError else { return false }
            return malformed.reason == reason
        }
    }

    private func submittedDelivery(
        attempt: Int = 1,
        submittedAt: Date = Date(timeIntervalSince1970: 1_752_000_000)
    ) -> Components.Schemas.AttentionDeliverySnapshot {
        .init(
            as_of_revision: 1,
            entity_version: 1,
            delivery: .submitted(
                .init(
                    item_id: "item-spec_approval",
                    device_id: "device-1",
                    channel: "ntfy",
                    attempt: attempt,
                    submitted_at: submittedAt,
                    delivery_status: .submitted
                ))
        )
    }

    /// Go's `time.Time{}` zero instant ("0001-01-01T00:00:00Z"), which
    /// `AttentionDelivery.Validate` rejects as an unset submitted_at.
    // swift-format-ignore: NeverForceUnwrap
    private var daemonZeroInstant: Date {
        var components = DateComponents()
        components.year = 1
        components.month = 1
        components.day = 1
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        return calendar.date(from: components)!
    }
}
