import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct DecisionCardCompositionTests {
    /// Section 9: an agent question is answerable on its own, so the labeled
    /// question claim renders before the actions rather than in the lower
    /// supporting sections.
    @Test func agentQuestionLeadsWithItsTypedDecisions() {
        let composition = DecisionCardComposition.forType(.agent_question)

        #expect(
            composition.modules == [
                .recommendation, .agentQuestion, .facts, .factBlock, .summary, .claims,
                .evidence, .details,
            ])
        let lead = try? #require(composition.modules.firstIndex(of: .agentQuestion))
        #expect(lead.map { $0 < composition.actionInsertionIndex } == true)
        // The decisions artifact and any supporting context wait below the
        // actions, so nothing unrelated stands between the ask and answering.
        let question = AttentionFixtures.fixture(type: .agent_question).item
        let claims = composition.claims(
            from: question.agent_claims, at: 5, prominentClaimIndex: nil)
        #expect(claims.map(\.label).contains(AttentionFixtures.agentQuestionClaimLabel))
        #expect(
            composition.modules.firstIndex(of: .claims).map { $0 > composition.actionInsertionIndex }
                == true)
    }

    @Test func mechanicalCardsCarryNoAgentProse() {
        for type in [Components.Schemas.AttentionType.system_health, .blocked] {
            let composition = DecisionCardComposition.forType(type)
            #expect(!composition.modules.contains(.summary))

            let item = AttentionFixtures.fixture(type: type).item
            #expect(composition.summaries(from: item.agent_claims).isEmpty)
            #expect(!item.agent_claims.contains { $0.text != nil })
            #expect(
                !AttentionDisplay.cardFacts(
                    item, now: AttentionFixtures.createdInstant
                ).isEmpty)
        }
    }

    @Test func moduleVocabularyIsClosedAndShared() {
        #expect(
            Set(DecisionCardComposition.sharedModuleSet) == [
                .facts, .agentQuestion, .specRevision, .specification, .factBlock, .findingFacts,
                .recommendation, .checklist, .stageRail, .comparison, .yieldChart, .summary,
                .claims, .evidence, .details,
            ])
    }

    @Test @MainActor func revisedSpecificationLeadsWithTypedRevisionFacts() throws {
        let item = AttentionFixtures.revisedSpecification().item
        let revision = try #require(item.spec_revision?.value1)
        let composition = DecisionCardComposition.forType(.spec_approval)

        #expect(revision.iteration == 2)
        #expect(revision.diff.lines_added == 2)
        #expect(revision.diff.lines_removed == 1)
        #expect(
            composition.modules == [
                .recommendation, .specRevision, .summary, .facts, .specification, .factBlock,
                .claims, .evidence, .details,
            ])
        #expect(
            composition.modules.firstIndex(of: .specRevision).map {
                $0 < composition.actionInsertionIndex
            } == true)
        // §9 leads this card with a plan-altitude summary and puts the full
        // specification below it, so the summary layer that now carries the
        // reason has to render above the action region (#1098).
        #expect(
            composition.modules.firstIndex(of: .summary).map {
                $0 < composition.actionInsertionIndex
            } == true)
        #expect(
            composition.modules.firstIndex(of: .specification)
                == composition.actionInsertionIndex)
        #expect(
            composition.claims(
                from: item.agent_claims,
                at: try #require(composition.modules.firstIndex(of: .claims)),
                prominentClaimIndex: nil
            ).allSatisfy { !AgentClaimLabels.isApprovalMaterial($0.label) })
        #expect(DecisionDetailView.specificationClaim(in: item)?.label == "Specification")
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
                .recommendation, .findingFacts, .facts, .factBlock, .summary, .claims,
                .evidence, .details,
            ])
        #expect(composition.actionInsertionIndex == composition.modules.firstIndex(of: .factBlock))
        #expect(composition.reviewingActionInsertionIndex == nil)
    }

    @Test func fourSpecializedCardsAreOnlyModuleOrderings() {
        #expect(
            DecisionCardComposition.forType(.ready_for_final_review).modules == [
                .recommendation, .checklist, .factBlock, .yieldChart, .facts, .summary, .claims,
                .evidence, .details,
            ])
        #expect(
            DecisionCardComposition.forType(.execution_failure).modules == [
                .recommendation, .stageRail, .facts, .claims, .factBlock, .summary, .claims,
                .evidence, .details,
            ])
        #expect(
            DecisionCardComposition.forType(.review_dispute).modules == [
                .comparison, .factBlock, .facts, .summary, .claims, .evidence, .details,
            ])
        #expect(
            DecisionCardComposition.forType(.review_diminishing_returns).modules == [
                .recommendation, .yieldChart, .facts, .factBlock, .summary, .claims, .evidence,
                .details,
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
        #expect(execution.claimsAreProminent(at: 3))
        #expect(!execution.claimsAreProminent(at: 6))
        #expect(
            execution.claims(
                from: executionClaims, at: 3, prominentClaimIndex: diagnosticIndex
            ).map(\.label) == ["Likely cause (unverified)"])
        #expect(
            execution.claims(
                from: executionClaims, at: 6, prominentClaimIndex: diagnosticIndex
            ).map(\.label) == ["screenshot"])
        // With no caller-chosen prominent claim, the readable diagnostic claim
        // still leads and the attachment stays supporting context.
        #expect(
            execution.claims(
                from: executionClaims, at: 3, prominentClaimIndex: nil
            ).map(\.label) == ["Likely cause (unverified)"])
        #expect(
            execution.claims(
                from: executionClaims, at: 6, prominentClaimIndex: nil
            ).map(\.label) == ["screenshot"])
        // The dispute card's claims render below its actions, so they never
        // count as prominent.
        #expect(!DecisionCardComposition.forType(.review_dispute).claimsAreProminent(at: 4))
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

    /// The daemon's typed facts inform the decision, so no composition may
    /// offer its actions above them. This is the invariant the card shell used
    /// to buy by pinning the fact section under the ask for every type, which
    /// also overrode each type's own ordering (#1004 review).
    @Test(arguments: AttentionFixtures.phase1Types)
    func factsNeverRenderBelowTheActions(type: Components.Schemas.AttentionType) throws {
        let composition = DecisionCardComposition.forType(type)
        let facts = try #require(composition.modules.firstIndex(of: .facts))

        #expect(facts < composition.actionInsertionIndex)
        if let reviewing = composition.reviewingActionInsertionIndex {
            #expect(facts < reviewing)
        }
    }

    /// Each type keeps its own lead: the readiness verdict, the failing stage,
    /// and the disputed positions all outrank the identifier-shaped facts that
    /// sit last before the actions.
    @Test func eachTypeLeadsWithItsOwnModuleNotWithItsFacts() {
        for (type, leading) in [
            (Components.Schemas.AttentionType.ready_for_final_review, DecisionCardModule.checklist),
            (.execution_failure, .stageRail),
            (.review_dispute, .comparison),
            (.review_diminishing_returns, .yieldChart),
            (.agent_question, .agentQuestion),
            (.finding_adjudication, .findingFacts),
        ] {
            let composition = DecisionCardComposition.forType(type)
            #expect(
                composition.modules.firstIndex(of: leading).map { lead in
                    composition.modules.firstIndex(of: .facts).map { lead < $0 } ?? false
                } == true,
                "\(type) must lead with \(leading), not with its fact rows")
        }
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
        #expect(clean.summary == "Readiness checklist: all 5 checks passed; 1 informational note.")
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
                == "Readiness checklist: 3 of 6 checks need attention; 1 informational note.")
        #expect(
            invalidatedChecklist.accessibilitySummary.contains(
                "Verification verdict: Invalidated, needs attention"))

        var legacyInvalidated = invalidated
        legacyInvalidated.readiness = nil
        legacyInvalidated.readiness_detail = nil
        let legacyChecklist = try #require(DecisionChecklistPresentation(legacyInvalidated))
        #expect(legacyChecklist.rows.first?.result == .failed)
        #expect(legacyChecklist.rows.first?.value == "Invalidated")
        #expect(
            legacyChecklist.accessibilitySummary.contains(
                "Verification verdict: Invalidated, needs attention"))

        var informationalOnly = AttentionFixtures.fixture(type: .ready_for_final_review).item
        informationalOnly.readiness = nil
        informationalOnly.readiness_detail = nil
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
        currentWithoutVerdict.readiness_detail = nil
        let currentChecklist = try #require(DecisionChecklistPresentation(currentWithoutVerdict))
        #expect(currentChecklist.rows.first?.value == "Unavailable")
        #expect(currentChecklist.summary.contains("checks need attention"))
        #expect(!currentChecklist.summary.contains("checks passed"))
    }

    @Test func checklistListsEveryRequirementWithItsWaiverAndBoundCoordinates() throws {
        let clean = try #require(
            DecisionChecklistPresentation(
                AttentionFixtures.fixture(type: .ready_for_final_review).item))
        #expect(
            clean.rows.map(\.label) == [
                "Verification verdict", "Bound to", "clean-verification", "independent-review",
                "Commit plan", "Terminal review",
            ])
        #expect(
            clean.rows[1] == .init(label: "Bound to", value: "cafebabe on main@deadbeef", result: .passed))
        #expect(clean.rows[2] == .init(label: "clean-verification", value: "Passed", result: .passed))

        let degraded = try #require(
            DecisionChecklistPresentation(AttentionFixtures.degradedReady().item))
        #expect(degraded.rows.first == .init(label: "Verification verdict", value: "Degraded", result: .failed))
        #expect(
            degraded.rows.first(where: { $0.label == "license-headers (optional)" })
                == .init(label: "license-headers (optional)", value: "Not run (advisory)", result: .failed))
        #expect(
            degraded.rows.first(where: { $0.label == "repo-change-policy" })
                == .init(
                    label: "repo-change-policy",
                    value: "Failed, waived for repo_change_policy by explicit human approval, waiver waiver-1",
                    result: .failed))
        #expect(degraded.summary == "Readiness checklist: 3 of 7 checks need attention; 1 informational note.")

        // The daemon's invalidation demotes the verdict and its bound
        // coordinates and shows both sides of the divergence.
        let stale = try #require(DecisionChecklistPresentation(AttentionFixtures.staleReady().item))
        #expect(stale.rows[0] == .init(label: "Verification verdict", value: "Invalidated", result: .failed))
        #expect(stale.rows[1] == .init(label: "Bound to", value: "cafebabe on main@deadbeef", result: .failed))
        #expect(
            stale.rows[2]
                == .init(label: "Head changed", value: "bound cafebabe, observed feedface", result: .failed))
        #expect(stale.rows[3].result == .passed)

        // A base advance the watch observed is the other staleness axis: the
        // verdict is still the daemon's, but it no longer describes the base.
        var advanced = AttentionFixtures.fixture(type: .ready_for_final_review).item
        advanced.base_freshness = .init(
            value1: .init(
                base_ref: "main", admitted_base_sha: "deadbeef", observed_base_sha: "0badf00d",
                advanced: true, observed_at: AttentionFixtures.createdInstant))
        let advancedChecklist = try #require(DecisionChecklistPresentation(advanced))
        #expect(
            advancedChecklist.rows[0]
                == .init(label: "Verification verdict", value: "Clean, stale", result: .failed))
        #expect(advancedChecklist.rows[1].result == .failed)
        #expect(
            advancedChecklist.rows.first(where: { $0.label == "Base freshness" })
                == .init(
                    label: "Base freshness", value: "Advanced past deadbeef, now 0badf00d",
                    result: .failed))
        #expect(
            AttentionDisplay.shortRevision("0123456789abcdef0123456789abcdef01234567") == "0123456789ab")
        // A base ref and a "repository_id#pr_number" identity are the other
        // coordinates an invalidation carries, and both differ in the tail, so
        // neither is truncated: a shortened pair would render two different
        // coordinates identically and hide the change the row names.
        #expect(AttentionDisplay.shortRevision("1071234567#1074") == "1071234567#1074")
        #expect(AttentionDisplay.shortRevision("1071234567#1075") == "1071234567#1075")
        #expect(
            AttentionDisplay.shortRevision("release/2026-09-candidate")
                == "release/2026-09-candidate")
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

    /// Only `spec_approval` carries the agent's summary in `reason`, so only
    /// that card drops its Context section; every other type keeps rendering
    /// the daemon's own context fact (#1098).
    @Test func onlySpecificationApprovalsCarryTheirSummaryAsReason() {
        #expect(DecisionCardComposition.reasonIsAgentSummary(.spec_approval))
        for type in AttentionFixtures.phase1Types where type != .spec_approval {
            #expect(!DecisionCardComposition.reasonIsAgentSummary(type))
        }
    }

    /// The fixtures reproduce the producer relation the predicate above
    /// depends on: `acceptSpecification` writes `reason` and the
    /// `freeside.summary` claim from the same agent summary, first iteration
    /// and revision alike (#1098).
    @Test func specificationFixturesRepeatTheDaemonsSummaryRelation() throws {
        for item in [
            AttentionFixtures.fixture(type: .spec_approval).item,
            AttentionFixtures.revisedSpecification().item,
        ] {
            let summary = try #require(
                item.agent_claims.first { $0.label == AgentClaimLabels.summary }?.text?.content)
            #expect(item.reason == summary)
        }
    }

    /// A specification approval persisted before summary claims carries its
    /// `Specification` claim alone, so the card has no unverified layer to
    /// move the reason into and keeps Context rather than dropping the text
    /// entirely (#1098).
    @Test func legacySpecificationApprovalsKeepTheirContextSection() {
        let composition = DecisionCardComposition.forType(.spec_approval)
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(!composition.rendersContext(for: item))
        item.agent_claims.removeAll { $0.label == AgentClaimLabels.summary }
        #expect(composition.rendersContext(for: item))
    }

    /// A type only drops Context because its summary layer renders the same
    /// text, so every such type has to compose that module and render it in
    /// the lead: dropping Context moved the reason out of the position §9
    /// reserves for a plan-altitude summary, and the summary module has to
    /// take that position back (#1098).
    @Test func typesThatMoveTheirReasonLeadWithTheSummaryModule() throws {
        for type in AttentionFixtures.phase1Types
        where DecisionCardComposition.reasonIsAgentSummary(type) {
            let composition = DecisionCardComposition.forType(type)
            let summaryIndex = try #require(composition.modules.firstIndex(of: .summary))
            #expect(summaryIndex < composition.actionInsertionIndex)
        }
    }

    /// The iOS sticky footer offers the recommended action exactly when the
    /// button is off screen. Reordering the block to put the reason above the
    /// button (#1107) made the block's own frame the wrong thing to measure:
    /// a long reason leaves the block's top on screen with the button below
    /// the fold, which has to count as not visible.
    @Test func theRecommendedActionIsVisibleOnlyWhileItsButtonIsOnScreen() {
        let viewport: CGFloat = 800

        // Fully on screen.
        #expect(
            DecisionDetailView.recommendationActionVisible(
                frame: CGRect(x: 0, y: 300, width: 560, height: 44),
                viewportHeight: viewport))
        // Pushed below the fold by a long reason, with the block's top still
        // on screen: the regression this guards.
        #expect(
            !DecisionDetailView.recommendationActionVisible(
                frame: CGRect(x: 0, y: 900, width: 560, height: 44),
                viewportHeight: viewport))
        // Scrolled off the top.
        #expect(
            !DecisionDetailView.recommendationActionVisible(
                frame: CGRect(x: 0, y: -60, width: 560, height: 44),
                viewportHeight: viewport))
        // Straddling each edge still counts as reachable.
        #expect(
            DecisionDetailView.recommendationActionVisible(
                frame: CGRect(x: 0, y: -20, width: 560, height: 44),
                viewportHeight: viewport))
        #expect(
            DecisionDetailView.recommendationActionVisible(
                frame: CGRect(x: 0, y: 780, width: 560, height: 44),
                viewportHeight: viewport))
        // Without the scroll space, as on the macOS inspector, the top-edge
        // test stands alone.
        #expect(
            DecisionDetailView.recommendationActionVisible(
                frame: CGRect(x: 0, y: 900, width: 560, height: 44),
                viewportHeight: nil))
    }
}
