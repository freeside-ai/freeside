import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct DecisionCardCompositionTests {
    @Test func moduleVocabularyIsClosedAndShared() {
        #expect(
            Set(DecisionCardComposition.sharedModuleSet) == [
                .factBlock, .findingFacts, .recommendation, .checklist, .stageRail, .comparison,
                .yieldChart, .summary, .claims, .evidence, .details,
            ])
    }

    @Test func findingAdjudicationLeadsWithTheLabeledProposalAndDaemonFacts() {
        // §9's finding_adjudication row leads with the labeled proposal and
        // the daemon-fact register (both carried by .findingFacts), and puts
        // assumptions, cited rules, alternatives, and gating questions below
        // the action region (#984); actionInsertionIndex must therefore land
        // after .findingFacts, not after .recommendation alone.
        let composition = DecisionCardComposition.forType(.finding_adjudication)
        #expect(
            composition.modules == [
                .recommendation, .findingFacts, .factBlock, .summary, .claims, .evidence,
                .details,
            ])
        #expect(composition.actionInsertionIndex == composition.modules.firstIndex(of: .factBlock))
        #expect(composition.reviewingActionInsertionIndex == nil)
    }

    @Test func fourSpecializedCardsAreOnlyModuleOrderings() {
        #expect(
            DecisionCardComposition.forType(.ready_for_final_review).modules == [
                .recommendation, .checklist, .factBlock, .yieldChart, .summary, .claims,
                .evidence, .details,
            ])
        #expect(
            DecisionCardComposition.forType(.execution_failure).modules == [
                .recommendation, .stageRail, .claims, .factBlock, .summary, .claims, .evidence,
                .details,
            ])
        #expect(
            DecisionCardComposition.forType(.review_dispute).modules == [
                .comparison, .factBlock, .summary, .claims, .evidence, .details,
            ])
        #expect(
            DecisionCardComposition.forType(.review_diminishing_returns).modules == [
                .recommendation, .yieldChart, .factBlock, .summary, .claims, .evidence, .details,
            ])
        #expect(
            !DecisionCardComposition.forType(.review_dispute).modules.contains(.recommendation))
        let ready = DecisionCardComposition.forType(.ready_for_final_review)
        #expect(ready.actionInsertionIndex == ready.modules.firstIndex(of: .summary))
        #expect(ready.reviewingActionInsertionIndex == ready.modules.firstIndex(of: .details))
        #expect(
            DecisionCardComposition.forType(.execution_failure)
                .reviewingActionInsertionIndex == nil)
        let execution = DecisionCardComposition.forType(.execution_failure)
        let executionClaims = AttentionFixtures.fixture(type: .execution_failure).item.agent_claims
        let diagnosticIndex = executionClaims.firstIndex { $0.label == "Likely cause (unverified)" }
        #expect(execution.claimsAreProminent(at: 2))
        #expect(!execution.claimsAreProminent(at: 4))
        #expect(
            execution.claims(
                from: executionClaims, at: 2, prominentClaimIndex: diagnosticIndex
            ).map(\.label) == ["Likely cause (unverified)"])
        #expect(
            execution.claims(
                from: executionClaims, at: 4, prominentClaimIndex: diagnosticIndex
            ).map(\.label) == ["screenshot"])
        #expect(
            execution.claims(
                from: executionClaims, at: 2, prominentClaimIndex: nil
            ).isEmpty)
        #expect(
            execution.claims(
                from: executionClaims, at: 5, prominentClaimIndex: nil
            ) == executionClaims.filter { $0.label != AgentClaimLabels.summary })
        #expect(!DecisionCardComposition.forType(.review_dispute).claimsAreProminent(at: 3))
    }

    @Test(arguments: AttentionFixtures.phase1Types)
    func summaryLayerIsReservedForNonMechanicalCards(type: Components.Schemas.AttentionType) {
        let composition = DecisionCardComposition.forType(type)
        let summaries = composition.summaries(from: AttentionFixtures.fixture(type: type).item.agent_claims)

        if type == .system_health || type == .blocked {
            #expect(!composition.modules.contains(.summary))
            #expect(summaries.isEmpty)
        } else {
            #expect(composition.modules.contains(.summary))
            if type == .run_proposal {
                #expect(summaries.isEmpty)
            } else {
                #expect(summaries.count == 1)
            }
        }
    }

    @Test func reservedSummariesNeverRenderAsGenericClaims() throws {
        let item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let composition = DecisionCardComposition.forType(.ready_for_final_review)
        let summary = try #require(item.agent_claims.first { $0.label == AgentClaimLabels.summary })

        #expect(composition.summaries(from: item.agent_claims) == [summary])
        #expect(
            composition.claims(
                from: item.agent_claims,
                at: try #require(composition.modules.firstIndex(of: .claims)),
                prominentClaimIndex: nil
            ).allSatisfy { $0.label != AgentClaimLabels.summary })

        var artifactOnly = summary
        artifactOnly.text = nil
        #expect(composition.summaries(from: [artifactOnly]).isEmpty)
        #expect(
            composition.claims(
                from: [artifactOnly],
                at: try #require(composition.modules.firstIndex(of: .claims)),
                prominentClaimIndex: nil
            ) == [artifactOnly])
    }

    @Test(arguments: AttentionFixtures.phase1Types)
    func everyPhase1TypeUsesOnlyTheSharedModuleSet(type: Components.Schemas.AttentionType) {
        let composition = DecisionCardComposition.forType(type)
        #expect(Set(composition.modules).isSubset(of: Set(DecisionCardComposition.sharedModuleSet)))
        #expect(composition.actionInsertionIndex >= 0)
        #expect(composition.actionInsertionIndex <= composition.modules.count)
        if let reviewingActionInsertionIndex = composition.reviewingActionInsertionIndex {
            #expect(reviewingActionInsertionIndex >= 0)
            #expect(reviewingActionInsertionIndex <= composition.modules.count)
        }
    }

    @Test func checklistUsesNeutralSuccessAndFailureOnlyWhereTheFactFails() throws {
        let clean = try #require(
            DecisionChecklistPresentation(
                AttentionFixtures.fixture(type: .ready_for_final_review).item))
        let degraded = try #require(
            DecisionChecklistPresentation(AttentionFixtures.degradedReady().item))

        #expect(clean.rows.first?.result == .passed)
        #expect(clean.rows.first?.value == "Clean")
        #expect(clean.rows.first(where: { $0.label == "Commit plan" })?.result == .informational)
        #expect(clean.summary == "Readiness checklist: all 2 checks passed; 1 informational note.")
        #expect(
            clean.accessibilitySummary.contains(
                "Commit plan: Plan present, not honored, informational"))
        #expect(degraded.rows.first?.result == .failed)
        #expect(degraded.rows.first?.value == "Degraded")

        var invalidated = AttentionFixtures.fixture(type: .ready_for_final_review).item
        invalidated.status = .superseded
        invalidated.readiness_invalidation = .init(
            value1: .init(
                reason: .head_changed,
                bound: "cafebabe",
                observed: "feedface",
                observed_at: Date(timeIntervalSince1970: 0)))
        let invalidatedChecklist = try #require(DecisionChecklistPresentation(invalidated))
        #expect(invalidatedChecklist.rows.first?.result == .failed)
        #expect(invalidatedChecklist.rows.first?.value == "Invalidated")
        #expect(
            invalidatedChecklist.summary
                == "Readiness checklist: 1 of 2 checks need attention; 1 informational note.")
        #expect(
            invalidatedChecklist.accessibilitySummary.contains(
                "Verification verdict: Invalidated, needs attention"))

        var legacyInvalidated = invalidated
        legacyInvalidated.readiness = nil
        let legacyChecklist = try #require(DecisionChecklistPresentation(legacyInvalidated))
        #expect(legacyChecklist.rows.first?.result == .failed)
        #expect(legacyChecklist.rows.first?.value == "Invalidated")
        #expect(
            legacyChecklist.accessibilitySummary.contains(
                "Verification verdict: Invalidated, needs attention"))

        var informationalOnly = AttentionFixtures.fixture(type: .ready_for_final_review).item
        informationalOnly.readiness = nil
        informationalOnly.yield_history = nil
        informationalOnly.base_freshness = nil
        let informationalChecklist = try #require(
            DecisionChecklistPresentation(informationalOnly))
        #expect(informationalChecklist.rows.map(\.result) == [.failed, .informational])
        #expect(informationalChecklist.rows.first?.value == "Unavailable")
        #expect(
            informationalChecklist.summary
                == "Readiness checklist: 1 of 1 checks need attention; 1 informational note.")
        #expect(
            informationalChecklist.accessibilitySummary.contains(
                "Verification verdict: Unavailable, needs attention"))

        informationalOnly.commit_plan_notice = nil
        let unavailableOnly = try #require(DecisionChecklistPresentation(informationalOnly))
        #expect(unavailableOnly.rows.map(\.result) == [.failed])

        var currentWithoutVerdict = AttentionFixtures.fixture(type: .ready_for_final_review).item
        currentWithoutVerdict.readiness = nil
        let currentChecklist = try #require(DecisionChecklistPresentation(currentWithoutVerdict))
        #expect(currentChecklist.rows.first?.value == "Unavailable")
        #expect(currentChecklist.summary.contains("checks need attention"))
        #expect(!currentChecklist.summary.contains("checks passed"))
    }

    @Test func checklistDerivesUnresolvedReviewStateFromDispositions() throws {
        var dispositioned = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let terminalIndex = try #require(dispositioned.yield_history?.value1.rounds.indices.last)
        dispositioned.yield_history?.value1.rounds[terminalIndex].findings_ingested = 1
        dispositioned.yield_history?.value1.rounds[terminalIndex].new_findings = 1
        dispositioned.yield_history?.value1.rounds[terminalIndex].fixed = 1
        dispositioned.yield_history?.value1.rounds[terminalIndex].outcome = .findings
        dispositioned.yield_history?.value1.terminal_outcome = .findings

        let resolvedChecklist = try #require(DecisionChecklistPresentation(dispositioned))
        let resolvedReview = try #require(
            resolvedChecklist.rows.first(where: { $0.label == "Terminal review" }))
        #expect(resolvedReview.value == "Findings dispositioned")
        #expect(resolvedReview.result == .passed)

        var earlierUnresolved = dispositioned
        earlierUnresolved.yield_history?.value1.rounds[0].deferred = 0
        let unresolvedChecklist = try #require(DecisionChecklistPresentation(earlierUnresolved))
        let unresolvedReview = try #require(
            unresolvedChecklist.rows.first(where: { $0.label == "Terminal review" }))
        #expect(unresolvedReview.value == "1 finding unresolved")
        #expect(unresolvedReview.result == .failed)

        var cleanWithEarlierUnresolved =
            AttentionFixtures.fixture(type: .ready_for_final_review).item
        cleanWithEarlierUnresolved.yield_history?.value1.rounds[0].deferred = 0
        let cleanUnresolvedChecklist = try #require(
            DecisionChecklistPresentation(cleanWithEarlierUnresolved))
        #expect(
            cleanUnresolvedChecklist.rows.first(where: { $0.label == "Terminal review" })?.value
                == "1 finding unresolved")
    }

    @Test func stageRailSummaryStatesTheSameFailureAsTheGraphic() throws {
        let presentation = try #require(
            DecisionStageRailPresentation.failure(
                stages: ["Import", "Build", "Verify", "Publish"],
                failedStageIndex: 2))

        #expect(presentation.entries.map(\.state) == [.completed, .completed, .failed, .pending])
        #expect(presentation.summary == "Verify failed, stage 3 of 4.")
    }

    @Test func timelineSummaryPreservesVisibleMilestoneDetails() {
        let presentation = DecisionStageRailPresentation.timeline(entries: [
            .init(
                id: "build",
                title: "Build",
                detail: "Round 2",
                context: "Attempt 3",
                timestamp: "Aug 25 at 11:54 AM",
                state: .current)
        ])

        #expect(
            presentation.summary
                == "Stage history: Build, Round 2, Attempt 3, Aug 25 at 11:54 AM.")
        #expect(
            presentation.entries[0].accessibilityLabel
                == "Build, current, Round 2, Attempt 3, Aug 25 at 11:54 AM")
        #expect(
            DecisionStageRailPresentation.timeline(entries: []).summary
                == "No stage, round, or decision history recorded.")
    }

    @Test func yieldSummaryIsDerivedFromTheSameRoundCounts() throws {
        let presentation = try #require(
            DecisionYieldPresentation(
                AttentionFixtures.fixture(type: .ready_for_final_review).item))

        #expect(
            presentation.rounds.map(\.text) == [
                "Round 1: 2 new, 0 recurring",
                "Round 2: 1 new, 1 recurring",
                "Round 3: 0 new, 0 recurring",
            ])
        #expect(
            presentation.summary
                == "Review yield: Round 1: 2 new, 0 recurring; Round 2: 1 new, 1 recurring; Round 3: 0 new, 0 recurring."
        )

        let diminishing = try #require(
            DecisionYieldPresentation(
                AttentionFixtures.fixture(type: .review_diminishing_returns).item))
        #expect(
            diminishing.rounds.map(\.text) == [
                "Round 1: 4 new, 0 recurring",
                "Round 2: 1 new, 2 recurring",
                "Round 3: 0 new, 3 recurring",
            ])
        #expect(
            diminishing.summary
                == "Review yield: Round 1: 4 new, 0 recurring; Round 2: 1 new, 2 recurring; Round 3: 0 new, 3 recurring."
        )
    }

    @Test func comparisonSummaryPreservesBothPositions() {
        let presentation = DecisionComparisonPresentation(
            positions: [
                .init(title: "Reviewer", text: "The guard is required."),
                .init(title: "Agent", text: "The state is unreachable."),
            ],
            verifiableFacts: [.init(label: "Caller", value: "Store reconstruction")])

        #expect(presentation.summary.contains("Reviewer: The guard is required."))
        #expect(presentation.summary.contains("Agent: The state is unreachable."))
    }
}
