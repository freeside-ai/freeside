import Foundation
import FreesideAPI
import Testing

@Suite struct FixtureTests {
    /// Independent transcription of plan §4's per-type action table plus
    /// review recovery actions from issues #580, #611, and #684. Signet's policy
    /// pins `blocked` read-only (no actions), which the schema permits
    /// since #96.
    static let planSection4: [Components.Schemas.AttentionType: [Components.Schemas.Action]] = [
        .spec_approval: [.approve, .request_changes, .discuss, .stop],
        .review_diminishing_returns: [
            .finish_now, .apply_then_finish, .continue_under_policy, .convert_to_policy,
        ],
        .review_dispute: [.approve, .discuss, .stop],
        .review_contradiction: [.recover_review],
        .review_configuration: [.adopt_review_configuration, .discuss, .stop],
        .finding_adjudication: [
            .accept_recommended_route, .choose_alternative_route, .discuss, .stop,
        ],
        .execution_failure: [.retry, .retry_with_capabilities, .discuss, .stop],
        .agent_question: [.answer_and_retry, .answer_without_retry, .stop],
        .publish_blocked: [
            .rerun_trust_evaluation, .inspect_trust_failure, .stop,
        ],
        .ready_for_final_review: [.open_pr, .return_to_agent, .mark_seen, .dismiss, .stop],
        .run_proposal: [.start, .start_with_changes, .decline, .snooze],
        .system_health: [
            .acknowledge, .run_doctor, .stop_unattended, .resume_unattended,
            .resolve_reenrollment,
        ],
    ]

    @Test func actionSetsMatchPlanSection4() {
        for (type, actions) in Self.planSection4 {
            #expect(AttentionFixtures.phase1ActionSets[type] == actions)
        }
        // blocked is pinned read-only by signet policy: it offers the
        // empty set, which the contract permits since #96.
        #expect(AttentionFixtures.phase1ActionSets[.blocked] == [])
        #expect(AttentionFixtures.phase1ActionSets.count == 13)
    }

    /// phase1Actions is the enumeration universe the cross-language policy
    /// parity suite walks; if it dropped an action, that action's cells would
    /// go unchecked. Pin it to exactly the union of the per-type sets (every
    /// action is offered by at least one type) and to a duplicate-free 31.
    @Test func phase1ActionsCoverEveryOfferedActionWithoutDuplicates() {
        let offered = Set(AttentionFixtures.phase1ActionSets.values.flatMap { $0 })
        #expect(Set(AttentionFixtures.phase1Actions) == offered)
        #expect(AttentionFixtures.phase1Actions.count == 31)
        #expect(Set(AttentionFixtures.phase1Actions).count == AttentionFixtures.phase1Actions.count)
    }

    @Test func defaultInboxCoversEveryPhase1TypeOnce() {
        let inbox = AttentionFixtures.defaultInbox()
        #expect(inbox.map(\.item._type) == AttentionFixtures.phase1Types)
        #expect(Set(inbox.map(\.item.id)).count == inbox.count)
    }

    @Test func recommendationAndDecisionSurfaceRoundTrip() throws {
        let present = AttentionFixtures.fixture(type: .finding_adjudication).item
        let absent = AttentionFixtures.fixture(type: .spec_approval).item

        #expect(present.recommendation?.value1.action == .accept_recommended_route)
        #expect(present.recommendation?.value1.source == .agent_judgment)
        #expect(absent.recommendation == nil)
        #expect(present.decision_surface.epoch == 1)
        #expect(!present.decision_surface.digest.isEmpty)

        let encoder = JSONEncoder()
        let decoder = JSONDecoder()
        let decodedPresent = try decoder.decode(
            Components.Schemas.AttentionItem.self, from: encoder.encode(present))
        let decodedAbsent = try decoder.decode(
            Components.Schemas.AttentionItem.self, from: encoder.encode(absent))

        #expect(decodedPresent.recommendation == present.recommendation)
        #expect(decodedPresent.decision_surface == present.decision_surface)
        #expect(decodedAbsent.recommendation == nil)
        #expect(decodedAbsent.decision_surface == absent.decision_surface)
    }

    @Test func readyFixturesCoverCleanAndDegradedReadiness() {
        let clean = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let degraded = AttentionFixtures.degradedReady().item

        #expect(clean.readiness?.value1._class == .ready_clean)
        #expect(clean.readiness?.value1.evaluation_set_digest == "sha256:evaluation-clean")
        #expect(clean.yield_history?.value1.rounds.map(\.round) == [1, 2, 3])
        #expect(clean.yield_history?.value1.terminal_outcome == .clean)
        #expect(degraded.readiness?.value1._class == .ready_degraded)
        #expect(
            degraded.readiness?.value1.evaluation_set_digest
                == "sha256:evaluation-degraded")
        #expect(degraded.yield_history == clean.yield_history)
    }

    @Test func diminishingFixtureCarriesReviewYieldHistory() {
        let item = AttentionFixtures.fixture(type: .review_diminishing_returns).item

        #expect(item.yield_history?.value1.rounds.map(\.new_findings) == [4, 1, 0])
        #expect(item.yield_history?.value1.rounds.map(\.recurring_findings) == [0, 2, 3])
        #expect(item.yield_history?.value1.terminal_outcome == .findings)
    }

    @Test func reviewDisputeFixtureCarriesRenderableFindingEvidence() {
        let item = AttentionFixtures.fixture(type: .review_dispute).item
        let claim = item.agent_claims.first { $0.label.hasPrefix("Shadow finding") }

        #expect(claim?.text?.content.contains("P1 shadow finding") == true)
        #expect(claim.map { item.artifact_digests.contains($0.digest) } == true)
    }

    @Test func executionFailureFixtureLabelsItsDiagnosticAsAClaim() {
        let item = AttentionFixtures.fixture(type: .execution_failure).item
        let claim = item.agent_claims.first { $0.label == "Likely cause (unverified)" }

        #expect(claim?.text?.content.contains("likely failed") == true)
        #expect(claim?.text?.content.contains("**build**") == true)
        #expect(claim.map { item.artifact_digests.contains($0.digest) } == true)
    }

    /// Pins the literal ids so the `-FreesideSelect` value list mirrored
    /// in app/README.md cannot drift silently: renaming a type or
    /// reordering the inbox must show up here as a doc-sync signal.
    @Test func defaultInboxItemIDsAreTheCanonicalSelectValues() {
        #expect(
            AttentionFixtures.defaultInboxItemIDs() == [
                "item-spec_approval",
                "item-execution_failure",
                "item-agent_question",
                "item-review_diminishing_returns",
                "item-review_dispute",
                "item-review_contradiction",
                "item-review_configuration",
                "item-finding_adjudication",
                "item-ready_for_final_review",
                "item-publish_blocked",
                "item-run_proposal",
                "item-system_health",
                "item-blocked",
            ])
        #expect(
            AttentionFixtures.defaultInboxItemIDs()
                == AttentionFixtures.defaultInbox().map(\.item.id))
    }

    @Test(arguments: AttentionFixtures.phase1Types)
    func fixtureIsValidAndOffersExactlyItsActionSet(
        type: Components.Schemas.AttentionType
    ) {
        let item = AttentionFixtures.fixture(type: type).item
        #expect(item.requested_decision == AttentionFixtures.phase1ActionSets[type])
        #expect(item.status == .open)
        #expect(item.created_at == AttentionFixtures.createdInstant)
        // artifact_digests is the daemon-derived canonical binding set:
        // the sorted, deduplicated union of evidence, claim, and typed
        // authority-binding digests.
        let union =
            item.evidence_snapshot.map(\.digest) + item.agent_claims.map(\.digest)
            + (item.finding_adjudication.map { [$0.value1.adjudication_digest] } ?? [])
        #expect(item.artifact_digests == Array(Set(union)).sorted())
        // Every reference carries valid §5.15 metadata: evidence on the run
        // channel, claims on the claim channel, non-negative sizes, and a text
        // claim whose media type agrees with its metadata.
        for artifact in item.evidence_snapshot {
            #expect(artifact.metadata.source == .run)
            #expect(artifact.metadata.size_bytes >= 0)
        }
        for claim in item.agent_claims {
            #expect(claim.metadata.source == .claim)
            #expect(claim.metadata.size_bytes >= 0)
            if let text = claim.text {
                #expect(text.media_type.rawValue == claim.metadata.media_type.rawValue)
            }
        }
    }
}
