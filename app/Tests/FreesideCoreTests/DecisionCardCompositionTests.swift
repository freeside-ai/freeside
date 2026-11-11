import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

@Suite struct DecisionCardCompositionTests {
    @Test func durableStopDrawsAllRecoveryGuidanceUnderTheAsk() throws {
        let item = AttentionFixtures.durableStop()
        let composition = DecisionCardComposition.forType(.system_health)
        #expect(composition.drawsReason(for: item))
        #expect(DecisionCardComposition.reasonPlacement(for: item) == .underAsk)
        let reason = try #require(DecisionCardComposition.reason(for: item))
        #expect(reason.text == item.reason)
        #expect(!reason.isAgentWritten)
        #expect(reason.text.contains("Acknowledge and Run Doctor leave unattended admission closed."))
        #expect(reason.text.contains("choose Resume Unattended on each durable-stop item"))
        #expect(reason.text.hasSuffix("A restart alone does not reopen it."))
    }

    /// Section 9: an agent question is answerable on its own, so the labeled
    /// question claim renders before the actions rather than in the lower
    /// supporting sections.
    @Test func agentQuestionLeadsWithItsTypedDecisions() {
        let composition = DecisionCardComposition.forType(.agent_question)

        #expect(
            composition.modules == [
                .recommendation, .agentQuestion, .facts, .foldedFacts, .factBlock, .summary,
                .claims, .evidence, .details,
            ])
        let lead = try? #require(composition.modules.firstIndex(of: .agentQuestion))
        #expect(lead.map { $0 < composition.actionInsertionIndex } == true)
        // The decisions artifact and any supporting context wait below the
        // actions, so nothing unrelated stands between the ask and answering.
        let question = AttentionFixtures.fixture(type: .agent_question).item
        let claims = composition.claims(
            from: question.agent_claims, at: 6, prominentClaimIndex: nil)
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
                .facts, .foldedFacts, .agentQuestion, .specRevision, .specification, .factBlock,
                .findingFacts, .recommendation, .stopCause, .checklist, .stageRail, .comparison,
                .yieldChart, .summary, .reviewGuide, .claims, .evidence, .details,
            ])
    }

    @Test @MainActor func revisedSpecificationDrawsItsChangeAndItemAboveTheActions() throws {
        let item = AttentionFixtures.revisedSpecification().item
        let revision = try #require(item.spec_revision?.value1)
        let composition = DecisionCardComposition.forType(.spec_approval)

        #expect(revision.iteration == 2)
        #expect(revision.diff.lines_added == 2)
        #expect(revision.diff.lines_removed == 1)
        #expect(
            composition.modules == [
                .recommendation, .summary, .specRevision, .specification, .facts, .factBlock,
                .claims, .evidence, .details,
            ])
        // Frames 5.1 and 7.2: the summary, the change since the last
        // revision, and the specification item (with the conversation it
        // carries) are what the approval weighs, so all three render above
        // the action region, in that order. The summary still carries the
        // reason there (#1098).
        let leading = try [DecisionCardModule.summary, .specRevision, .specification].map {
            try #require(composition.modules.firstIndex(of: $0))
        }
        #expect(leading == leading.sorted())
        #expect(leading.allSatisfy { $0 < composition.actionInsertionIndex })
        #expect(DecisionCardComposition.placesConversationWithSpecification(for: .spec_approval))
        #expect(
            composition.claims(
                from: item.agent_claims,
                at: try #require(composition.modules.firstIndex(of: .claims)),
                prominentClaimIndex: nil
            ).allSatisfy { !AgentClaimLabels.isApprovalMaterial($0.label) })
        #expect(DecisionDetailView.specificationClaim(in: item)?.label == "Specification")
    }

    @Test func findingAdjudicationLeadsWithItsFindingCards() {
        // Plan §9 revision 78 (visual audit D09): one card per finding leads,
        // and the item's recommendation sits with the batch action below
        // them. Each card holds everything about its finding, so the card
        // has no fact block: no finding content renders from another module
        // or below the action region.
        let composition = DecisionCardComposition.forType(.finding_adjudication)
        #expect(
            composition.modules == [
                .findingFacts, .facts, .recommendation, .summary, .claims, .evidence, .details,
            ])
        #expect(!composition.modules.contains(.factBlock))
        // Unchanged from #984: the actions come after the finding cards, and
        // there is no reviewing action.
        #expect(composition.actionInsertionIndex == composition.modules.firstIndex(of: .summary))
        #expect(
            composition.modules.firstIndex(of: .findingFacts).map {
                $0 < composition.actionInsertionIndex
            } == true)
        #expect(composition.reviewingActionInsertionIndex == nil)
    }

    @Test func fourSpecializedCardsAreOnlyModuleOrderings() throws {
        // Plan §9 and survey card 4b (R10): the labeled change summary,
        // then the agent's reading order (revision 112), then the diff and
        // the verdict reached on it, and the review yield follows the
        // actions.
        #expect(
            DecisionCardComposition.forType(.ready_for_final_review).modules == [
                .recommendation, .summary, .reviewGuide, .facts, .checklist, .factBlock, .yieldChart,
                .claims, .evidence, .details,
            ])
        #expect(
            DecisionCardComposition.forType(.execution_failure).modules == [
                .recommendation, .facts, .claims, .stageRail, .factBlock, .summary, .claims,
                .evidence, .details,
            ])
        // Plan §9 revision 78 (visual audit D08): the supplied claim leads
        // beside the positions, ahead of the daemon's facts about the run.
        #expect(
            DecisionCardComposition.forType(.review_dispute).modules == [
                .comparison, .claims, .findingFacts, .factBlock, .facts, .summary, .claims, .evidence,
                .details,
            ])
        #expect(
            DecisionCardComposition.forType(.review_diminishing_returns).modules == [
                .recommendation, .stopCause, .yieldChart, .facts, .factBlock, .summary, .claims,
                .evidence, .details,
            ])
        #expect(
            !DecisionCardComposition.forType(.review_dispute).modules.contains(.recommendation))
        let ready = DecisionCardComposition.forType(.ready_for_final_review)
        // Revision 78 (D07): the review yield opens on demand below the
        // actions, and View PR follows the verdict it rests on instead of
        // closing the card.
        #expect(ready.actionInsertionIndex == ready.modules.firstIndex(of: .yieldChart))
        #expect(ready.actionInsertionIndex == 6)
        #expect(ready.reviewingActionInsertionIndex == 5)
        #expect(
            try #require(ready.modules.firstIndex(of: .reviewGuide))
                == #require(ready.modules.firstIndex(of: .summary)) + 1)
        #expect(try #require(ready.modules.firstIndex(of: .summary)) < ready.actionInsertionIndex)
        #expect(
            try #require(ready.modules.firstIndex(of: .evidence)) < #require(ready.modules.firstIndex(of: .details)))
        #expect(
            try ready.reviewingActionInsertionIndex == #require(
                ready.modules.firstIndex(of: .checklist)) + 1)
        #expect(
            DecisionCardComposition.forType(.execution_failure)
                .reviewingActionInsertionIndex == nil)
        let execution = DecisionCardComposition.forType(.execution_failure)
        let executionClaims = AttentionFixtures.fixture(type: .execution_failure).item.agent_claims
        let diagnosticIndex = executionClaims.firstIndex { $0.label == "Likely cause (unverified)" }
        #expect(execution.claimsAreProminent(at: 2))
        #expect(!execution.claimsAreProminent(at: 6))
        #expect(
            execution.claims(
                from: executionClaims, at: 2, prominentClaimIndex: diagnosticIndex
            ).map(\.label) == ["Likely cause (unverified)"])
        #expect(
            execution.claims(
                from: executionClaims, at: 6, prominentClaimIndex: diagnosticIndex
            ).map(\.label) == ["screenshot"])
        // With no caller-chosen prominent claim, the readable diagnostic claim
        // still leads and the attachment stays supporting context.
        #expect(
            execution.claims(
                from: executionClaims, at: 2, prominentClaimIndex: nil
            ).map(\.label) == ["Likely cause (unverified)"])
        #expect(
            execution.claims(
                from: executionClaims, at: 6, prominentClaimIndex: nil
            ).map(\.label) == ["screenshot"])
        // Revision 78 (D08): a dispute leads with its claim, so its first
        // claims module is prominent and the supporting one below the
        // actions is not.
        let dispute = DecisionCardComposition.forType(.review_dispute)
        #expect(dispute.claimsAreProminent(at: 1))
        #expect(!dispute.claimsAreProminent(at: 5))
    }

    @Test(arguments: AttentionFixtures.phase1Types)
    func summaryLayerIsReservedForNonMechanicalCards(type: Components.Schemas.AttentionType) {
        let composition = DecisionCardComposition.forType(type)
        let summaries = composition.summaries(from: AttentionFixtures.fixture(type: type).item.agent_claims)

        if type == .system_health {
            #expect(!composition.modules.contains(.summary))
            #expect(summaries.isEmpty)
        } else {
            #expect(composition.modules.contains(.summary))
            if type == .task_proposal || type == .effect_proposal {
                // Store-derived one-carrier items (a single proposal digest, no
                // agent claims) reserve the summary module but carry no summary
                // claim, matching the daemon's item shape.
                #expect(summaries.isEmpty)
            } else {
                #expect(summaries.count == 1)
            }
        }
    }

    private static let unverifiedContexts: [DecisionCardComposition.UnverifiedContext] =
        DecisionCardComposition.UnverifiedContext.Platform.allCases.flatMap { platform in
            [false, true].map { recommended in
                .init(platform: platform, drawsUnverifiedRecommendation: recommended)
            }
        }

    /// R7 and R25: every type explains its unverified label on demand, and
    /// from one control. A card names each place it draws a visible
    /// unverified keyword once, in reading order, and the first carries the
    /// control, so no card has two and none with a visible keyword has
    /// none (plan §9 revision 82).
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func unverifiedExplanationIsOnDemandFromOneControlOnEveryCard(
        type: Components.Schemas.AttentionType
    ) {
        let item = AttentionFixtures.fixture(type: type).item
        let composition = DecisionCardComposition.forType(type)
        for context in Self.unverifiedContexts {
            let slots = composition.unverifiedSlots(for: item, in: context)
            #expect(Set(slots).count == slots.count)
            #expect(composition.infoSlot(for: item, in: context) == slots.first)
            if context.drawsUnverifiedRecommendation {
                #expect(!slots.isEmpty)
            }
        }
    }

    /// The control sits on the first unverified keyword the operator reads:
    /// the eyebrow where the card leads with the agent's question, otherwise
    /// the summary or the claim the card leads with. On macOS a claim that
    /// is not the card's lead reads beside the actions.
    @Test func theExplanationControlSitsOnTheFirstUnverifiedKeyword() throws {
        func infoSlot(
            _ type: Components.Schemas.AttentionType,
            on platform: DecisionCardComposition.UnverifiedContext.Platform
        ) -> DecisionCardComposition.UnverifiedSlot? {
            DecisionCardComposition.forType(type).infoSlot(
                for: AttentionFixtures.fixture(type: type).item, in: .init(platform: platform))
        }
        func module(
            _ module: DecisionCardModule, of type: Components.Schemas.AttentionType
        ) throws -> DecisionCardComposition.UnverifiedSlot {
            .module(try #require(DecisionCardComposition.forType(type).modules.firstIndex(of: module)))
        }

        for platform in DecisionCardComposition.UnverifiedContext.Platform.allCases {
            #expect(infoSlot(.agent_question, on: platform) == .eyebrow)
            #expect(
                infoSlot(.ready_for_final_review, on: platform)
                    == (try module(.summary, of: .ready_for_final_review)))
            #expect(
                infoSlot(.spec_approval, on: platform) == (try module(.summary, of: .spec_approval)))
            #expect(
                infoSlot(.review_dispute, on: platform)
                    == (try module(.claims, of: .review_dispute)))
            // A routed dispute carries no claim, so its findings keyword is
            // the first unverified one in reading order.
            #expect(
                DecisionCardComposition.forType(.review_dispute).infoSlot(
                    for: AttentionFixtures.routedReviewDispute().item, in: .init(platform: platform))
                    == (try module(.findingFacts, of: .review_dispute)))
            // The failure card reads its diagnostic in the card on both
            // platforms (frame 5.3), so that keyword carries the control.
            #expect(
                infoSlot(.execution_failure, on: platform)
                    == (try module(.claims, of: .execution_failure)))
        }
    }

    /// A card whose only unverified keyword is a disclosure's own label has
    /// no label to carry the control, so it names no slot and the section
    /// explains itself when opened. The health card's one claim is an
    /// attachment, which both platforms list in the Evidence fold; a
    /// readable claim joins it there on a phone (#2607), so it names no slot
    /// either, while the Mac lists it beside the actions.
    @Test func aCardWithOnlyFoldedUnverifiedKeywordsNamesNoSlot() throws {
        var item = AttentionFixtures.fixture(type: .system_health).item
        let composition = DecisionCardComposition.forType(.system_health)
        let claims = try #require(composition.modules.firstIndex(of: .claims))
        #expect(claims >= composition.actionInsertionIndex)

        #expect(composition.infoSlot(for: item, in: .init(platform: .phone)) == nil)
        #expect(composition.infoSlot(for: item, in: .init(platform: .mac)) == nil)

        item.agent_claims[0].text = .init(media_type: .text_sol_plain, content: "The daemon restarted cleanly.")
        #expect(composition.infoSlot(for: item, in: .init(platform: .phone)) == nil)
        #expect(composition.infoSlot(for: item, in: .init(platform: .mac)) == .actionRegion)
    }

    /// An agent-written reason under the ask is the first thing the agent
    /// says on its card, so it carries the label and the control (plan §9,
    /// Summary Provenance).
    @Test func anAgentWrittenReasonUnderTheAskIsLabeled() throws {
        var untyped = AttentionFixtures.fixture(type: .agent_question).item
        untyped.agent_question = nil
        let reason = try #require(DecisionCardComposition.reason(for: untyped))
        #expect(reason.isAgentWritten)
        #expect(reason.label == "Agent reason")
        for platform in DecisionCardComposition.UnverifiedContext.Platform.allCases {
            #expect(
                DecisionCardComposition.forType(.agent_question)
                    .infoSlot(for: untyped, in: .init(platform: platform)) == .reason)
        }

        var legacy = AttentionFixtures.fixture(type: .spec_approval).item
        legacy.agent_claims.removeAll { $0.label == AgentClaimLabels.summary }
        #expect(
            DecisionCardComposition.forType(.spec_approval)
                .infoSlot(for: legacy, in: .init(platform: .phone)) == .reason)
    }

    /// Plan §9 revision 82: the card's details carry the full reason on
    /// every type, including a reason the card draws nowhere else, and an
    /// agent-written one keeps its label there.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func detailsCarryTheFullReasonOnEveryType(type: Components.Schemas.AttentionType) throws {
        let item = AttentionFixtures.fixture(type: type).item
        let reason = try #require(DecisionCardComposition.reason(for: item))
        #expect(reason.text == item.reason)
        #expect(reason.isAgentWritten == [.spec_approval, .agent_question].contains(type))
        #expect(reason.label == (reason.isAgentWritten ? "Agent reason" : "Reason"))
    }

    @Test func detailsCarryTheBindingLineADiminishingCardDoesNotDraw() throws {
        var item = AttentionFixtures.fixture(type: .review_diminishing_returns).item
        item.reason =
            "Review yield has remained low under the resolved policy.\n"
            + #"Binding: {"run_id":"run-1","round":3}"#
        #expect(!DecisionCardComposition.forType(item._type).drawsReason(for: item))
        #expect(try #require(DecisionCardComposition.reason(for: item)).text == item.reason)
    }

    /// Visual audit D06 and D08: the question and dispute cards fold their
    /// run and binding coordinates; every other type keeps its facts beside
    /// the decision. Either way each fact has exactly one destination.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func everyCardFactHasOneDestination(type: Components.Schemas.AttentionType) {
        let item = AttentionFixtures.fixture(type: type).item
        let now = Date(timeIntervalSince1970: 0)
        let facts = AttentionDisplay.cardFacts(item, now: now)
        let placement = DecisionFactPlacement(
            item, includesCommitPlan: false, now: now, pools: [], locale: .current, timeZone: .current)
        let folds: [Components.Schemas.AttentionType] = [.agent_question, .review_dispute]

        #expect(DecisionCardComposition.foldsRoutineFacts(type) == folds.contains(type))
        #expect(placement.visible + placement.folded == facts)
        #expect(placement.folded == (folds.contains(type) ? facts : []))
    }

    /// Placement hands the pools to the facts, so the quota failure's Other
    /// Agents row shows each offer's reading beside the decision (#3030).
    @Test func quotaFailureShowsTheOffersReadings() throws {
        let now = Date(timeIntervalSince1970: 1_767_400_000)
        let locale = Locale(identifier: "en_US_POSIX")
        let utc = try #require(TimeZone(identifier: "UTC"))
        let pools = UsageFixtures.alternatesUsagePools(now: now)
        var item = AttentionFixtures.quotaFailure().item
        item.alternate_agents = UsageFixtures.alternateAgentOffers()

        let placement = DecisionFactPlacement(
            item, includesCommitPlan: false, now: now, pools: pools, locale: locale, timeZone: utc)
        let row = try #require(placement.visible.first { $0.label == "Other Agents" })
        let reading = try #require(
            AgentChoiceDisplay.reading(
                for: UsageFixtures.readingSecondEnrollmentID, pools: pools, now: now, locale: locale,
                timeZone: utc))
        #expect(row.value.hasPrefix("\(UsageFixtures.readingSecondEnrollmentID) · \(reading)\n"))
        #expect(placement.folded.isEmpty)
    }

    /// The ready card keeps an elapsed deadline (#2752) visible under Facts
    /// after the Change row, above the checklist and View PR.
    @Test func elapsedDeadlineIsAVisibleReadyFact() {
        let now = AttentionFixtures.createdInstant.addingTimeInterval(18 * 3_600)
        let placement = DecisionFactPlacement(
            AttentionFixtures.readyWithElapsedChecksDeadline().item, includesCommitPlan: false, now: now, pools: [],
            locale: .current, timeZone: .current)
        #expect(placement.visible.map(\.label) == ["Diff", "Checks"])
        #expect(placement.folded.isEmpty)
    }

    @Test func foldedFactsAreTheRoutineCoordinates() {
        let now = Date(timeIntervalSince1970: 0)
        let question = DecisionFactPlacement(
            AttentionFixtures.fixture(type: .agent_question).item,
            includesCommitPlan: true, now: now, pools: [], locale: .current, timeZone: .current)
        let dispute = DecisionFactPlacement(
            AttentionFixtures.fixture(type: .review_dispute).item,
            includesCommitPlan: true, now: now, pools: [], locale: .current, timeZone: .current)

        #expect(question.visible.isEmpty)
        #expect(question.folded.map(\.label) == ["Stage", "Blocked on"])
        #expect(dispute.visible.isEmpty)
        #expect(
            dispute.folded.map(\.label)
                == ["Run", "Round", "Disputed findings", "Completion evidence"])
    }

    /// A notice is not a coordinate: it stays visible on a card that folds
    /// its facts, and it is never drawn where a checklist already carries it.
    @Test(arguments: [Components.Schemas.AttentionType.agent_question, .review_dispute])
    func commitPlanNoticeNeverFolds(type: Components.Schemas.AttentionType) {
        var item = AttentionFixtures.fixture(type: type).item
        item.commit_plan_notice = .init(value1: .present_but_not_honored)
        let now = Date(timeIntervalSince1970: 0)
        let notice = AttentionDisplay.FactRow(
            "Commit plan", AttentionDisplay.label(.present_but_not_honored))

        let shown = DecisionFactPlacement(
            item, includesCommitPlan: true, now: now, pools: [], locale: .current, timeZone: .current)
        #expect(shown.visible == [notice])
        #expect(shown.folded == AttentionDisplay.cardFacts(item, now: now))

        let withChecklist = DecisionFactPlacement(
            item, includesCommitPlan: false, now: now, pools: [], locale: .current, timeZone: .current)
        #expect(withChecklist.visible.isEmpty)
    }

    /// The final review's checklist carries the commit-plan notice, so the
    /// card asks for its facts without it, as it does here.
    @Test func finalReviewKeepsItsDiffVisible() {
        let item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let placement = DecisionFactPlacement(
            item, includesCommitPlan: false, now: Date(timeIntervalSince1970: 0), pools: [], locale: .current,
            timeZone: .current)

        #expect(placement.visible.map(\.label) == ["Diff"])
        #expect(placement.folded.isEmpty)
    }

    /// R0: no type keeps a boxed Context section. The reason draws under
    /// the ask on every type but the three that lead with their own module
    /// and have a per-item test for folding it (visual audit D06, D07, D09;
    /// plan §9 revision 82). No other type folds.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func reasonFoldsOnlyOnTheCardsWithAPerItemTest(
        type: Components.Schemas.AttentionType
    ) {
        let expected: DecisionCardComposition.ReasonPlacement =
            switch type {
            case .agent_question, .ready_for_final_review, .finding_adjudication, .task_proposal,
                .effect_proposal:
                .recordedContext
            case .blocked: .detailsOnly
            default: .underAsk
            }
        #expect(DecisionCardComposition.reasonPlacement(for: type) == expected)
    }

    /// A finding adjudication's reason folds while the recommendation is on
    /// the card, but what accepting does never folds (plan §9): the first
    /// sentence of the daemon's "Accepting" line stays ahead of the actions,
    /// after any line about what changed since Discuss. Where the whole
    /// reason already draws under the ask there is no separate lead.
    @Test func whatAcceptingDoesStaysAheadOfTheActionsOnAFindingCard() {
        var item = AttentionFixtures.fixture(type: .finding_adjudication).item
        #expect(DecisionCardComposition.reasonPlacement(for: item) == .recordedContext)

        item.reason = [
            "Changed after Discuss: review-finding-17 moved from \"Decline\" to \"Fix in this PR\".",
            "Accepting starts a remediator that edits this PR. It may change only the "
                + "run's allowed paths (daemon/**), not just where a finding was reported.",
            "review-finding-17 (daemon/internal/signet/service.go:214-227): Fix in this PR.",
        ].joined(separator: "\n")
        #expect(
            DecisionCardComposition.reasonLead(for: item)
                == "Accepting starts a remediator that edits this PR.")

        item.reason = "Accepting parks the run: nothing is fixed or published."
        #expect(DecisionCardComposition.reasonLead(for: item) == item.reason)

        item.reason = "The reviewer and the agent disagree."
        #expect(DecisionCardComposition.reasonLead(for: item) == nil)

        item.reason = "Accepting parks the run: nothing is fixed or published."
        item.recommendation = nil
        #expect(DecisionCardComposition.reasonPlacement(for: item) == .underAsk)
        #expect(DecisionCardComposition.reasonLead(for: item) == nil)

        var other = AttentionFixtures.fixture(type: .task_proposal).item
        other.reason = item.reason
        #expect(DecisionCardComposition.reasonLead(for: other) == nil)
    }

    /// A proposal's reason folds only at its planned gate, where the daemon
    /// writes a sentence that restates the ask. The closure notice is the
    /// one proposal reason that says more (the issue could not be closed
    /// automatically, and the notice does not hold the pull request); the
    /// daemon opens it as exceptional, and it stays ahead of the actions
    /// (plan §9 revision 82).
    @Test(arguments: [Components.Schemas.AttentionType.task_proposal, .effect_proposal])
    func aProposalReasonFoldsOnlyAtItsPlannedGate(type: Components.Schemas.AttentionType) {
        var item = AttentionFixtures.fixture(type: type).item
        item.interruption_class = .planned_gate
        #expect(DecisionCardComposition.reasonPlacement(for: item) == .recordedContext)
        // Details still carry it in full.
        #expect(DecisionCardComposition.reason(for: item)?.text == item.reason)

        item.interruption_class = .exceptional
        item.reason =
            "The source issue could not be closed automatically; decide the fallback. "
            + "This notice does not hold the pull request."
        #expect(DecisionCardComposition.reasonPlacement(for: item) == .underAsk)
    }

    /// A blocked item's reason says what the run waits on and since when.
    /// It leaves the card's face only where the lead and the Waiting fact
    /// say both, which is the one wait the daemon writes today: a
    /// specification approval. An item with no typed wait, or a wait whose
    /// reason no writer defines yet, keeps the reason under the ask (plan
    /// §9 revision 82).
    @Test func aBlockedReasonLeavesTheFaceOnlyWhereTheLeadStatesTheWait() throws {
        let now = AttentionFixtures.createdInstant
        var item = AttentionFixtures.fixture(type: .blocked).item
        let wait = try #require(item.blocked_on?.value1)
        #expect(wait.kind == .spec_approval)
        #expect(DecisionCardComposition.reasonPlacement(for: item) == .detailsOnly)
        // What stands in for the reason: the lead and the duration.
        #expect(AttentionDisplay.ask(item) == "Waiting on specification approval.")
        #expect(AttentionDisplay.cardFacts(item, now: now).map(\.label).contains("Waiting"))
        // Details still carry it in full.
        #expect(DecisionCardComposition.reason(for: item)?.text == item.reason)

        for kind in [Components.Schemas.BlockedWaitKind.pr_checks, .external_review] {
            item.blocked_on = .init(
                value1: .init(kind: kind, since: wait.since, item_id: nil, pr_reference: nil))
            #expect(DecisionCardComposition.reasonPlacement(for: item) == .underAsk)
        }

        item.blocked_on = nil
        #expect(DecisionCardComposition.reasonPlacement(for: item) == .underAsk)
    }

    /// Survey frame 5.5: a system-health item's reason is the daemon's
    /// finding, so it reads as the card's statement and stays ahead of the
    /// actions. Every other reason the shell draws is the ask's second line.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func onlyTheSystemHealthReasonReadsAsTheCardsStatement(
        type: Components.Schemas.AttentionType
    ) {
        #expect(
            DecisionCardComposition.reasonFace(for: type)
                == (type == .system_health ? .statement : .secondLine))
        #expect(DecisionCardComposition.reasonPlacement(for: .system_health) == .underAsk)
    }

    /// The finding card's reason says what accepting does. It may fold only
    /// while the recommendation states that outcome beside the batch
    /// action: an action's consequence never folds (plan §9).
    @Test func aFindingReasonFoldsOnlyWhileTheRecommendationStatesTheOutcome() {
        let recommended = AttentionFixtures.fixture(type: .finding_adjudication).item
        var unrecommended = recommended
        unrecommended.recommendation = nil

        #expect(DecisionRecommendationPresentation.of(recommended) != nil)
        #expect(DecisionCardComposition.reasonPlacement(for: recommended) == .recordedContext)
        #expect(DecisionCardComposition.reasonPlacement(for: unrecommended) == .underAsk)
    }

    /// Visual audit D08: the dispute leads with the claim the snapshot
    /// supplies, and a supporting attachment stays below the actions.
    @Test func disputeLeadsWithItsReadableClaim() {
        let item = AttentionFixtures.fixture(type: .review_dispute).item
        let composition = DecisionCardComposition.forType(.review_dispute)

        #expect(
            composition.claims(from: item.agent_claims, at: 1, prominentClaimIndex: nil)
                .map(\.label) == ["Shadow finding review-fixture"])
        #expect(
            composition.claims(from: item.agent_claims, at: 5, prominentClaimIndex: nil)
                .map(\.label) == ["screenshot"])
        #expect(
            composition.cardLeadClaims(from: item.agent_claims, prominentClaimIndex: nil)
                .map(\.label) == ["Shadow finding review-fixture"])
    }

    /// Claims that arrived without inline text still lead, as their
    /// attachments: nothing marks one of them as the disputed finding, so
    /// none is held back, the lead is never empty while the snapshot has a
    /// claim, and nothing lists those claims a second time.
    @Test func disputeWithoutInlineTextLeadsWithTheAttachment() {
        let item = AttentionFixtures.disputeWithoutInlineText().item
        let composition = DecisionCardComposition.forType(.review_dispute)

        let lead = composition.claims(from: item.agent_claims, at: 1, prominentClaimIndex: nil)
        #expect(lead.map(\.label) == ["screenshot", "Shadow finding review-fixture"])
        #expect(lead.allSatisfy { $0.text == nil })
        #expect(
            composition.claims(from: item.agent_claims, at: 5, prominentClaimIndex: nil).isEmpty)
        #expect(
            composition.cardLeadClaims(from: item.agent_claims, prominentClaimIndex: nil) == lead)
        // The summary keeps its own layer either way.
        #expect(composition.summaries(from: item.agent_claims).count == 1)
    }

    /// With no claim the snapshot has no dissent to show, so neither claims
    /// module has anything to draw and no placeholder stands in for one.
    @Test func disputeWithoutAClaimDrawsNoClaimsSection() {
        var item = AttentionFixtures.fixture(type: .review_dispute).item
        item.agent_claims.removeAll { $0.label != AgentClaimLabels.summary }
        let composition = DecisionCardComposition.forType(.review_dispute)

        #expect(
            composition.claims(from: item.agent_claims, at: 1, prominentClaimIndex: nil).isEmpty)
        #expect(
            composition.claims(from: item.agent_claims, at: 5, prominentClaimIndex: nil).isEmpty)
        #expect(
            composition.cardLeadClaims(from: item.agent_claims, prominentClaimIndex: nil).isEmpty)
    }

    /// Only the dispute's claim is its own lead content. A failure card
    /// draws its readable claims in the card but its attachment claims stay
    /// supporting context when none is readable, and no other card draws a
    /// claim in the card that its platform lists elsewhere.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func onlyTheDisputeLeadsWithItsClaim(type: Components.Schemas.AttentionType) {
        let composition = DecisionCardComposition.forType(type)
        #expect(composition.leadsWithItsClaim == (type == .review_dispute))
        // The failure card reads its diagnostic in the card, between its
        // facts and its stages (frame 5.3), without leading with it.
        // The task proposal reads the proposal itself the same way (5.4).
        #expect(
            composition.placesReadableClaimsInCard
                == [.execution_failure, .task_proposal].contains(type))

        var attachment = AttentionFixtures.fixture(type: .execution_failure).item.agent_claims[0]
        attachment.label = "screenshot"
        attachment.text = nil
        if type != .review_dispute {
            #expect(
                composition.cardLeadClaims(from: [attachment], prominentClaimIndex: nil).isEmpty)
        }
        if type == .execution_failure {
            #expect(composition.claims(from: [attachment], at: 2, prominentClaimIndex: nil).isEmpty)
            #expect(
                composition.claims(from: [attachment], at: 6, prominentClaimIndex: nil)
                    == [attachment])
        }
    }

    private func ranking(
        _ item: Components.Schemas.AttentionItem,
        recommending recommended: Components.Schemas.Action? = nil
    ) -> DecisionActionRanking {
        DecisionActionRanking(
            requested: item.requested_decision, recommendedAction: recommended,
            alsoOverflowing: DecisionCardComposition.overflowActions(for: item._type))
    }

    /// Visual audit D07: View PR is the final review's filled button, and a
    /// card never shows two, so it yields to a recommendation block.
    @Test func viewPRIsFilledUnlessARecommendationHoldsTheFilledButton() {
        let item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let plain = ranking(item)
        #expect(plain.reviewing == .open_pr)
        #expect(DecisionCardComposition.filledAction(for: item, ranking: plain) == .open_pr)

        let recommended = ranking(item, recommending: .return_to_agent)
        #expect(recommended.reviewing == .open_pr)
        #expect(DecisionCardComposition.filledAction(for: item, ranking: recommended) == nil)

        // Frame 7.3: a stale review fills nothing, because the proof no
        // longer covers the head View PR would open.
        let stale = AttentionFixtures.staleReady().item
        #expect(DecisionCardComposition.isStale(stale))
        #expect(!DecisionCardComposition.isStale(item))
        #expect(DecisionCardComposition.filledAction(for: stale, ranking: ranking(stale)) == nil)
    }

    /// The question card fills Answer and Retry, and gives the fill up to a
    /// recommendation block. The fill names an action, and the row fills
    /// only its first button, so a repeated request never draws two.
    @Test func answerAndRetryTakesTheQuestionCardsOneFill() {
        var item = AttentionFixtures.fixture(type: .agent_question).item
        item.requested_decision = [.answer_without_retry, .answer_and_retry, .stop]
        let plain = ranking(item)
        #expect(plain.principal.contains(.answer_and_retry))
        #expect(DecisionCardComposition.filledAction(for: item, ranking: plain) == .answer_and_retry)

        #expect(
            DecisionCardComposition.filledAction(for: item, ranking: ranking(item, recommending: .stop))
                == nil)

        item.requested_decision = [.answer_without_retry, .stop]
        #expect(DecisionCardComposition.filledAction(for: item, ranking: ranking(item)) == nil)
    }

    /// R6: one control is filled, or none. Without a recommendation the fill
    /// goes to the type's one forward action; a type whose choices are peers,
    /// and the three with no frame, fill nothing, so View PR is an outline
    /// there.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func theFillGoesToTheTypesForwardActionOrToNothing(type: Components.Schemas.AttentionType) {
        let forward: [Components.Schemas.AttentionType: Components.Schemas.Action] = [
            .agent_question: .answer_and_retry, .ready_for_final_review: .open_pr,
            .execution_failure: .retry, .task_proposal: .start, .effect_proposal: .approve,
            .system_health: .acknowledge,
        ]
        var item = AttentionFixtures.fixture(type: type).item
        #expect(DecisionCardComposition.forwardAction(for: type) == forward[type])
        #expect(DecisionCardComposition.filledAction(for: item, ranking: ranking(item)) == forward[type])

        // A card that offers View PR beside peers leaves it an outline.
        item.requested_decision.append(.open_pr)
        let withPullRequest = DecisionCardComposition.filledAction(for: item, ranking: ranking(item))
        #expect(withPullRequest == forward[type])
    }

    /// A recommendation the card draws holds the fill in its own block, so
    /// the row under it fills nothing on any type: no card draws two.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func aDrawnRecommendationLeavesTheRowUnfilled(type: Components.Schemas.AttentionType) throws {
        let item = AttentionFixtures.fixture(type: type).item
        guard let recommended = item.requested_decision.first else { return }
        let recommending = ranking(item, recommending: recommended)
        #expect(recommending.recommended == recommended)
        #expect(DecisionCardComposition.filledAction(for: item, ranking: recommending) == nil)
    }

    /// R6's pick clause (#3011): the recommendation's action is filled
    /// unless the operator holds a pick, when the control that sends the
    /// picks is filled. Pressing a filled acceptance would send the proposed
    /// routes and drop the pick. The pick is held and cleared the way the
    /// card's route picker does it, and counted from the selections on
    /// findings the binding lists.
    @Test @MainActor func aHeldFindingPickMovesTheFillToChooseAnotherRoute() throws {
        let item = AttentionFixtures.fixture(type: .finding_adjudication).item
        let binding = try #require(item.finding_adjudication?.value1)
        let finding = try #require(binding.proposals.first)
        #expect(finding.route != .dispute)
        let picked = DecisionDetailView.selections([:], picking: .dispute, for: finding)
        let pickedBack = DecisionDetailView.selections(picked, picking: finding.route, for: finding)
        let held = !DecisionDetailView.selectedAlternatives(binding, selections: picked).isEmpty
        let cleared = !DecisionDetailView.selectedAlternatives(binding, selections: pickedBack).isEmpty
        #expect(held)
        #expect(!cleared)

        let recommending = ranking(item, recommending: .accept_recommended_route)
        #expect(recommending.principal.contains(.choose_alternative_route))
        let unrecommended = ranking(item)

        // With a pick: Choose Another Route is filled, the acceptance outlined.
        #expect(
            DecisionCardComposition.filledAction(for: item, ranking: recommending, holdsPick: held)
                == .choose_alternative_route)
        #expect(
            !DecisionCardComposition.recommendationIsFilled(
                for: item, ranking: recommending, holdsPick: held))

        // Without a pick, and once it is cleared: the acceptance is filled.
        for holdsPick in [false, cleared] {
            #expect(
                DecisionCardComposition.filledAction(
                    for: item, ranking: recommending, holdsPick: holdsPick) == nil)
            #expect(
                DecisionCardComposition.recommendationIsFilled(
                    for: item, ranking: recommending, holdsPick: holdsPick))
        }

        // No recommendation drawn: nothing is filled, with or without a pick.
        // A recommendation that did not revalidate reaches the ranking as
        // none (`unrecommended`); one the served surface no longer offers is
        // dropped by the ranking (`unserved`).
        let unserved = DecisionActionRanking(
            requested: item.requested_decision, recommendedAction: .accept_recommended_route,
            servedActions: item.requested_decision.filter { $0 != .accept_recommended_route })
        #expect(unserved.recommended == nil)
        for noRecommendation in [unrecommended, unserved] {
            for holdsPick in [false, true] {
                #expect(
                    DecisionCardComposition.filledAction(
                        for: item, ranking: noRecommendation, holdsPick: holdsPick) == nil)
                #expect(
                    !DecisionCardComposition.recommendationIsFilled(
                        for: item, ranking: noRecommendation, holdsPick: holdsPick))
            }
        }

        // One fill: the row and the recommendation never both take it.
        for candidate in [recommending, unrecommended, unserved] {
            for holdsPick in [false, true] {
                let rowFilled =
                    DecisionCardComposition.filledAction(
                        for: item, ranking: candidate, holdsPick: holdsPick) != nil
                let recommendationFilled = DecisionCardComposition.recommendationIsFilled(
                    for: item, ranking: candidate, holdsPick: holdsPick)
                #expect(!(rowFilled && recommendationFilled))
            }
        }
    }

    /// A held pick moves the fill on a finding adjudication only: every
    /// other type fills what it fills without one, recommended or not, and
    /// still draws at most one filled control.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func aHeldPickMovesNoFillOnOtherTypes(type: Components.Schemas.AttentionType) {
        guard type != .finding_adjudication else { return }
        let item = AttentionFixtures.fixture(type: type).item
        var rankings = [ranking(item)]
        if let recommended = item.requested_decision.first {
            rankings.append(ranking(item, recommending: recommended))
        }
        for candidate in rankings {
            let filled = DecisionCardComposition.filledAction(
                for: item, ranking: candidate, holdsPick: true)
            #expect(filled == DecisionCardComposition.filledAction(for: item, ranking: candidate))
            let recommendationFilled = DecisionCardComposition.recommendationIsFilled(
                for: item, ranking: candidate, holdsPick: true)
            #expect(recommendationFilled == (candidate.recommended != nil))
            #expect(!(filled != nil && recommendationFilled))
        }
    }

    /// The fill is never a destructive action, and never one the row does
    /// not draw: an action under More Actions cannot be the filled button.
    @Test func theFillIsNeverDestructiveOrOutOfTheRow() {
        var health = AttentionFixtures.fixture(type: .system_health).item
        health.requested_decision = [.run_doctor, .resume_unattended, .stop_unattended]
        #expect(DecisionCardComposition.filledAction(for: health, ranking: ranking(health)) == nil)

        for type in Components.Schemas.AttentionType.allCases {
            let item = AttentionFixtures.fixture(type: type).item
            if let filled = DecisionCardComposition.filledAction(for: item, ranking: ranking(item)) {
                #expect(AttentionDisplay.confirmationConsequence(filled, for: item) == nil)
                #expect(!ranking(item).overflow.contains(filled))
            }
        }
    }

    /// Discuss sits under More Actions on spec approval only, where the
    /// conversation is the place a reply starts (frame 5.1). The menu still
    /// draws when Discuss is its one entry.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func discussMovesUnderMoreActionsOnSpecApprovalOnly(type: Components.Schemas.AttentionType) {
        var item = AttentionFixtures.fixture(type: type).item
        item.requested_decision = [.approve, .discuss]
        let ranked = ranking(item)
        #expect(ranked.overflow.contains(.discuss) == (type == .spec_approval))
        #expect(ranked.principal.contains(.discuss) == (type != .spec_approval))
        #expect(DecisionCardComposition.overflowActions(for: type) == (type == .spec_approval ? [.discuss] : []))
    }

    /// Visual audit D06: the agent's own question leads, so the shell's
    /// generic ask is dropped only when a typed decision is there to replace
    /// it. No card is ever left without a lead.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func askIsDroppedOnlyWhereATypedQuestionLeads(type: Components.Schemas.AttentionType) {
        let item = AttentionFixtures.fixture(type: type).item
        #expect(DecisionCardComposition.rendersAsk(for: item) == (type != .agent_question))
    }

    @Test func questionWithoutTypedDecisionsKeepsTheAskAndItsContext() throws {
        let typed = AttentionFixtures.fixture(type: .agent_question).item
        #expect(!DecisionCardComposition.rendersAsk(for: typed))
        #expect(DecisionCardComposition.reasonPlacement(for: typed) == .recordedContext)

        var untyped = typed
        untyped.agent_question = nil
        #expect(DecisionCardComposition.rendersAsk(for: untyped))
        #expect(DecisionCardComposition.reasonPlacement(for: untyped) == .underAsk)

        var empty = typed
        var facts = try #require(empty.agent_question?.value1)
        facts.decisions = []
        empty.agent_question = .init(value1: facts)
        #expect(DecisionCardComposition.rendersAsk(for: empty))
        #expect(DecisionCardComposition.reasonPlacement(for: empty) == .underAsk)
    }

    /// Each fixture is its type's ordinary item, so its placement is the
    /// one its type names; the per-item tests cover the items that differ.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func reasonPlacementFollowsTheTypeForATypedItem(
        type: Components.Schemas.AttentionType
    ) {
        let item = AttentionFixtures.fixture(type: type).item
        #expect(
            DecisionCardComposition.reasonPlacement(for: item)
                == DecisionCardComposition.reasonPlacement(for: type))
    }

    @Test func severalDecisionsKeepTheirOrderAndTheirOwnOptions() throws {
        let item = AttentionFixtures.longQuestion().item
        let facts = try #require(item.agent_question?.value1)
        let presentation = try #require(AgentQuestionPresentation(item))

        #expect(facts.decisions.count == 2)
        #expect(presentation.decisions.map(\.question) == facts.decisions.map(\.question))
        #expect(
            presentation.decisions.map { $0.options.map(\.label) }
                == facts.decisions.map { $0.options.map(\.label) })
        #expect(
            presentation.decisions.map { $0.options.map(\.tradeoffs) }
                == facts.decisions.map { $0.options.map(\.tradeoffs) })
        // One recommended option per decision, the one the agent named.
        #expect(
            presentation.decisions.map { $0.options.filter(\.recommended).map(\.label) }
                == facts.decisions.map { [$0.recommendation] })
        #expect(!DecisionCardComposition.rendersAsk(for: item))
    }

    /// A quoted claim folds its identifiers; every one of them stays one
    /// disclosure away, exact.
    @Test func foldedClaimSourceKeepsEveryIdentifier() throws {
        let item = AttentionFixtures.fixture(type: .agent_question).item
        let claim = try #require(item.agent_claims.first { $0.text != nil })
        let text = try #require(claim.text)

        let rows = DecisionDetailView.claimSourceRows(claim, text: text)
        #expect(
            rows.map(\.label) == ["Label", "Media Type", "Agent Invocation", "Claim Digest"])
        #expect(rows.first?.value == claim.label)
        #expect(rows[1].value == text.media_type.rawValue)
        #expect(rows[2].value == "inv-agent-agent_question")
        #expect(rows.last?.value == claim.digest)
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

    /// Each type keeps its own lead: the final review's change summary and
    /// the disputed positions outrank the
    /// identifier-shaped facts that sit last before the actions. The final
    /// review's diff sits between its summary and its verdict (survey card
    /// 4b, R10).
    @Test func eachTypeLeadsWithItsOwnModuleNotWithItsFacts() {
        for (type, leading) in [
            (Components.Schemas.AttentionType.ready_for_final_review, DecisionCardModule.summary),
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

    /// R10: every decision type draws on its platform's ladder, and a card
    /// at phone width keeps the phone's padding on either platform.
    @Test func theCardScaleIsThePlatformLadder() {
        let scale = DecisionCardComposition.Scale.self
        let ladder = FreesideLadder.current
        #expect(scale.sectionGap == ladder.sectionGap)
        #expect(scale.moduleGap == ladder.moduleGap)
        #expect(scale.controlGap == ladder.controlGap)
        #expect(scale.foldLead == ladder.foldLead)
        #expect(scale.headGap == 10)
        #expect(scale.foldGap == 10)
        #expect(scale.padding(compact: false) == .init(top: 22, leading: 22, bottom: 20, trailing: 22))
        #expect(scale.padding(compact: true) == .init(top: 18, leading: 16, bottom: 16, trailing: 16))
    }

    /// The reviewing action opens the control group the action region
    /// closes, and the folds are the modules straight after it, so a
    /// composition can neither put the group's end ahead of its start nor
    /// fold a module it does not have.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func controlGroupAndFoldsStayInsideTheModuleList(type: Components.Schemas.AttentionType) {
        let composition = DecisionCardComposition.forType(type)
        if let reviewing = composition.reviewingActionInsertionIndex {
            #expect(reviewing <= composition.actionInsertionIndex)
        }
        #expect(
            composition.actionInsertionIndex + composition.foldedModuleCount
                <= composition.modules.count)
    }

    /// The foot lists attachment claims on both platforms, so an attachment
    /// claim renders once: never in the foot and a claims module both, and
    /// never in neither.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func anAttachmentClaimRendersOnce(type: Components.Schemas.AttentionType) {
        let composition = DecisionCardComposition.forType(type)
        let claims = AttentionFixtures.fixture(type: type).item.agent_claims
        let modules = composition.modules.indices.filter { composition.modules[$0] == .claims }
        let foot = composition.footClaims(from: claims, prominentClaimIndex: nil)
        for platform in DecisionCardComposition.UnverifiedContext.Platform.allCases {
            let inCard = modules.flatMap {
                composition.claimsInCard(from: claims, at: $0, prominentClaimIndex: nil, on: platform)
            }
            #expect(inCard.allSatisfy { !foot.contains($0) })
            for claim in claims where claim.text == nil && !AgentClaimLabels.isApprovalMaterial(claim.label) {
                #expect((inCard + foot).filter { $0 == claim }.count == 1)
            }
        }
    }

    /// The foot draws evidence and bindings, so no composition may place
    /// those two modules anywhere but the card's trailing supporting range,
    /// where dropping them leaves every other module where it was.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func evidenceAndDetailsOnlyTrailTheCard(type: Components.Schemas.AttentionType) {
        let modules = DecisionCardComposition.forType(type).modules
        #expect(Array(modules.suffix(2)) == [.evidence, .details])
        #expect(modules.dropLast(2).allSatisfy { $0 != .evidence && $0 != .details })
    }

    /// Survey card 4b: the review yield is the final review's one folded
    /// module. The question card folds its run and binding details the same
    /// way, and no other type folds a module yet.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func onlyTheComposedCardsFoldAModuleUnderTheirActions(type: Components.Schemas.AttentionType) {
        let composition = DecisionCardComposition.forType(type)
        let folded = composition.modules.dropFirst(composition.actionInsertionIndex)
            .prefix(composition.foldedModuleCount)
        let expected: [DecisionCardModule] =
            switch type {
            case .ready_for_final_review: [.yieldChart]
            case .agent_question: [.foldedFacts]
            default: []
            }
        #expect(Array(folded) == expected)
    }

    /// R28: a change row reads its counts aloud in words, since the plus
    /// and minus signs carry the meaning only on screen.
    @Test func changeRowSpeaksItsCountsInWords() throws {
        #expect(
            DecisionChangeRow(
                diff: .init(
                    files_changed: 9, additions: 412, deletions: 88, base_sha: "base",
                    head_sha: "head")
            ).spokenLabel == "Change: 412 added, 88 removed, 9 files")
        #expect(
            DecisionChangeRow(
                diff: .init(
                    files_changed: 1, additions: 3, deletions: 0, base_sha: "base",
                    head_sha: "head")
            ).spokenLabel == "Change: 3 added, 0 removed, 1 file")

        let revision = try #require(
            AttentionFixtures.revisedSpecification().item
                .spec_revision?.value1)
        let row = DecisionChangeRow(specification: revision.diff, sinceVersion: 1)
        #expect(row.keyword == "Change Since Version 1")
        #expect(row.counts.plain == "+2 \u{2212}1")
        #expect(row.spokenLabel == "Change Since Version 1: 2 lines added, 1 removed")
    }

    @Test func checklistUsesNeutralSuccessAndFailureOnlyWhereTheFactFails() throws {
        let clean = try #require(
            DecisionChecklistPresentation(
                AttentionFixtures.fixture(type: .ready_for_final_review).item))
        let degraded = try #require(
            DecisionChecklistPresentation(AttentionFixtures.degradedReady().item))

        #expect(clean.verdict?.result == .passed)
        #expect(clean.verdict?.value == "Clean")
        #expect(clean.rows.first(where: { $0.label == "Commit plan" })?.result == .note)
        #expect(clean.verdictLine == "Clean · 1 note · 5 passed")
        #expect(clean.summary == "Readiness checklist: Clean, 1 note, 5 passed.")
        #expect(
            clean.accessibilitySummary.contains(
                "Commit plan: Plan present, not honored, informational"))
        #expect(degraded.verdict?.result == .failed)
        #expect(degraded.verdict?.value == "Degraded")
        #expect(degraded.verdictLine == "Degraded · 1 waived · 1 advisory · 1 note · 5 passed")

        var invalidated = AttentionFixtures.fixture(type: .ready_for_final_review).item
        invalidated.status = .superseded
        invalidated.readiness_invalidation = .init(
            value1: .init(
                reason: .head_changed,
                bound: "cafebabe",
                observed: "feedface",
                observed_at: Date(timeIntervalSince1970: 0)))
        let invalidatedChecklist = try #require(DecisionChecklistPresentation(invalidated))
        // Frame 7.3: the chip reads Stale beside what the verdict was, and
        // the moved coordinate is a stale row, not a failed requirement, so
        // the line counts no failure.
        #expect(invalidatedChecklist.verdict?.result == .failed)
        #expect(invalidatedChecklist.verdict?.value == "Stale")
        #expect(invalidatedChecklist.priorVerdict == "was Clean")
        #expect(
            invalidatedChecklist.summary
                == "Readiness checklist: Stale, was Clean, 1 note, 4 passed.")
        #expect(
            invalidatedChecklist.accessibilitySummary.contains(
                "Verification verdict: Stale, needs attention"))
        #expect(
            invalidatedChecklist.accessibilitySummary.contains(
                "Bound to: Head cafebabe → feedface · Base main@deadbeef, stale"))

        var legacyInvalidated = invalidated
        legacyInvalidated.readiness = nil
        legacyInvalidated.readiness_detail = nil
        let legacyChecklist = try #require(DecisionChecklistPresentation(legacyInvalidated))
        #expect(legacyChecklist.verdict?.result == .failed)
        #expect(legacyChecklist.verdict?.value == "Stale")
        #expect(legacyChecklist.priorVerdict == nil)
        #expect(
            legacyChecklist.accessibilitySummary.contains(
                "Verification verdict: Stale, needs attention"))
        // With no bound coordinates to lead with, the invalidation keeps
        // its own row.
        #expect(
            legacyChecklist.rows.first
                == .init(
                    label: "Head changed", value: "cafebabe → feedface", result: .failed,
                    isStale: true))

        var informationalOnly = AttentionFixtures.fixture(type: .ready_for_final_review).item
        informationalOnly.readiness = nil
        informationalOnly.readiness_detail = nil
        informationalOnly.yield_history = nil
        let informationalChecklist = try #require(
            DecisionChecklistPresentation(informationalOnly))
        #expect(informationalChecklist.verdict?.result == .failed)
        #expect(informationalChecklist.rows.map(\.result) == [.note])
        #expect(informationalChecklist.verdict?.value == "Unavailable")
        #expect(informationalChecklist.summary == "Readiness checklist: Unavailable, 1 note.")
        #expect(
            informationalChecklist.accessibilitySummary.contains(
                "Verification verdict: Unavailable, needs attention"))

        informationalOnly.commit_plan_notice = nil
        let unavailableOnly = try #require(DecisionChecklistPresentation(informationalOnly))
        #expect(unavailableOnly.verdict?.result == .failed)
        #expect(unavailableOnly.rows.isEmpty)
        #expect(unavailableOnly.summary == "Readiness checklist: Unavailable.")

        var currentWithoutVerdict = AttentionFixtures.fixture(type: .ready_for_final_review).item
        currentWithoutVerdict.readiness = nil
        currentWithoutVerdict.readiness_detail = nil
        let currentChecklist = try #require(DecisionChecklistPresentation(currentWithoutVerdict))
        #expect(currentChecklist.verdict?.value == "Unavailable")
        // Without a readiness detail the terminal-review row is the only
        // check left, so the line counts it: the verdict word is
        // unavailable, not the rows it would have summarized.
        #expect(
            currentChecklist.verdictLine == "Unavailable · 1 note · 1 passed")
        #expect(
            currentChecklist.summary == "Readiness checklist: Unavailable, 1 note, 1 passed.")
    }

    @Test func checklistListsEveryRequirementWithItsWaiverAndBoundCoordinates() throws {
        let clean = try #require(
            DecisionChecklistPresentation(
                AttentionFixtures.fixture(type: .ready_for_final_review).item))
        #expect(clean.verdict?.label == "Verification verdict")
        // Severity first, the daemon's order inside each class.
        #expect(
            clean.rows.map(\.label) == [
                "Commit plan", "Bound to", "Checked at", "clean-verification", "independent-review",
                "Terminal review",
            ])
        #expect(
            clean.rows[1] == .init(label: "Bound to", value: "Head cafebabe · Base main@deadbeef", result: .passed))
        #expect(clean.rows[2] == .init(label: "Checked at", value: "Tip deadbeef", result: .passed))
        #expect(clean.rows[3] == .init(label: "clean-verification", value: "Passed", result: .passed))
        #expect(clean.passedRows.count == 5)
        #expect(clean.leadingRows.map(\.label) == ["Commit plan"])

        // A handoff at a moved tip says verification ran on the merge of the
        // head into it; a detail recorded before the tip existed shows no row.
        var movedItem = AttentionFixtures.fixture(type: .ready_for_final_review).item
        movedItem.readiness_detail = .init(
            value1: AttentionFixtures.movedTipReadinessDetail(head: movedItem.pr_head_sha))
        let moved = try #require(DecisionChecklistPresentation(movedItem))
        #expect(
            moved.rows.first(where: { $0.label == "Checked at" })
                == .init(
                    label: "Checked at",
                    value: "Tip facefeed · verified on merge 0ddba11 of the head into it",
                    result: .passed))
        var untippedItem = AttentionFixtures.fixture(type: .ready_for_final_review).item
        untippedItem.readiness_detail?.value1.handoff_tip = nil
        let untipped = try #require(DecisionChecklistPresentation(untippedItem))
        #expect(!untipped.rows.contains { $0.label == "Checked at" })

        let degraded = try #require(
            DecisionChecklistPresentation(AttentionFixtures.degradedReady().item))
        #expect(
            degraded.verdict == .init(label: "Verification verdict", value: "Degraded", result: .failed))
        #expect(
            degraded.rows.map(\.result) == [.waived, .advisory, .note, .passed, .passed, .passed, .passed, .passed])
        #expect(degraded.rows.first?.label == "repo-change-policy")
        #expect(
            degraded.rows.first(where: { $0.label == "license-headers (optional)" })
                == .init(
                    label: "license-headers (optional)", value: "Not run (advisory)",
                    result: .advisory))
        #expect(
            degraded.rows.first
                == .init(
                    label: "repo-change-policy",
                    value: "Failed, waived for repo_change_policy by explicit human approval, waiver waiver-1",
                    result: .waived))
        // The waiver sentence is the value the S6 fact-row rule stacks.
        #expect(try #require(degraded.rows.first).value.count > FactRow.stackThreshold)
        #expect(degraded.passedRows.count == 5)
        #expect(
            degraded.summary
                == "Readiness checklist: Degraded, 1 waived, 1 advisory, 1 note, 5 passed.")

        // The daemon's invalidation demotes the verdict, and the Bound-to
        // row leads with the coordinate that moved and both of its values
        // (frame 7.3), so no second row repeats the pair.
        let stale = try #require(DecisionChecklistPresentation(AttentionFixtures.staleReady().item))
        #expect(stale.verdict == .init(label: "Verification verdict", value: "Stale", result: .failed))
        #expect(
            stale.rows[0]
                == .init(
                    label: "Bound to", value: "Head cafebabe → feedface · Base main@deadbeef",
                    result: .failed, isStale: true))
        #expect(stale.rows.filter(\.isStale).count == 1)
        #expect(stale.rows[1].result == .note)

        // An item concluded before plan revision 110 may still carry a
        // base advance. It stays stale, the Bound-to row keeps the bound
        // coordinates, and the advance keeps its own row with both values.
        var advanced = AttentionFixtures.staleReady().item
        advanced.readiness_invalidation?.value1.reason = .base_advanced
        advanced.readiness_invalidation?.value1.bound = "deadbeef"
        advanced.readiness_invalidation?.value1.observed = "0badf00d"
        let advancedChecklist = try #require(DecisionChecklistPresentation(advanced))
        #expect(
            advancedChecklist.verdict
                == .init(label: "Verification verdict", value: "Stale", result: .failed))
        #expect(advancedChecklist.priorVerdict == "was Clean")
        #expect(
            advancedChecklist.rows[0]
                == .init(
                    label: "Bound to", value: "Head cafebabe · Base main@deadbeef",
                    result: .failed, isStale: true))
        #expect(
            advancedChecklist.rows[1]
                == .init(
                    label: "Base advanced", value: "deadbeef → 0badf00d", result: .failed,
                    isStale: true))

        // Frame 7.3's notice: one sentence, built from the same typed
        // facts. An item with no verdict to name, or an invalidation that
        // is not a head change, draws none.
        #expect(DecisionCardComposition.staleNotice(for: advanced) == nil)
        #expect(
            DecisionCardComposition.staleNotice(for: AttentionFixtures.staleReady().item)
                == "The head changed after verification. The verdict below was clean at head "
                + "cafebabe; the head is now at feedface.")
        var retargeted = AttentionFixtures.staleReady().item
        retargeted.readiness_invalidation?.value1.reason = .retargeted
        #expect(DecisionCardComposition.staleNotice(for: retargeted) == nil)
        var unverdicted = AttentionFixtures.staleReady().item
        unverdicted.readiness = nil
        #expect(DecisionCardComposition.staleNotice(for: unverdicted) == nil)
        #expect(
            DecisionCardComposition.staleNotice(
                for: AttentionFixtures.fixture(type: .ready_for_final_review).item) == nil)
        #expect(
            AttentionDisplay.shortRevision("0123456789abcdef0123456789abcdef01234567") == "01234567")
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

    /// The module is one accessibility element, and the passed rows are
    /// collapsed behind a disclosure VoiceOver cannot open, so the label has
    /// to name every requirement with its state, verdict included.
    @Test func checklistAccessibilityLabelNamesEveryRowIncludingCollapsedPassedOnes() throws {
        let degraded = try #require(
            DecisionChecklistPresentation(AttentionFixtures.degradedReady().item))

        #expect(degraded.passedRows.count == 5)
        #expect(
            degraded.accessibilitySummary.hasPrefix(
                "Readiness checklist: Degraded, 1 waived, 1 advisory, 1 note, 5 passed. "
                    + "Verification verdict: Degraded, needs attention;"))
        for row in degraded.rows {
            #expect(
                degraded.accessibilitySummary.contains(
                    "\(row.label): \(row.value), \(row.result.accessibilityState)"))
        }
        for row in degraded.passedRows {
            #expect(degraded.accessibilitySummary.contains("\(row.label): \(row.value), passed"))
        }
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

    /// The card lists the stages the run reached with the failure on top
    /// (frame 5.3). A stage never reached is not history, and the summary
    /// the rail speaks still counts it.
    @Test func theFailureCardListsReachedStagesNewestFirst() throws {
        let presentation = try #require(
            DecisionStageRailPresentation.failure(
                stages: ["Import", "Build", "Verify", "Publish"],
                failedStageIndex: 2))
        let listed = presentation.reachedNewestFirst

        #expect(listed.entries.map(\.title) == ["Verify", "Build", "Import"])
        #expect(listed.entries.map(\.state) == [.failed, .completed, .completed])
        #expect(listed.summary == presentation.summary)
    }

    /// Frame 5.3 names what the failure card's sections hold: the facts are
    /// the failure, and the agent's readable claim is its diagnostic, drawn
    /// in the card ahead of the stages on every platform.
    @Test func theFailureCardNamesItsFactsAndItsDiagnostic() {
        let item = AttentionFixtures.fixture(type: .execution_failure).item
        let composition = DecisionCardComposition.forType(.execution_failure)
        let claims = try? #require(composition.modules.firstIndex(of: .claims))
        let rail = try? #require(composition.modules.firstIndex(of: .stageRail))

        #expect(DecisionCardComposition.factsKeyword(for: .execution_failure) == "Failure")
        #expect(DecisionCardComposition.leadClaimsKeyword(for: .execution_failure) == "Diagnostic")
        #expect(DecisionCardComposition.factsKeyword(for: .system_health) == "Facts")
        if let claims, let rail {
            #expect(claims < rail)
            #expect(rail < composition.actionInsertionIndex)
            for platform in [DecisionCardComposition.UnverifiedContext.Platform.mac, .phone] {
                #expect(
                    !composition.claimsInCard(
                        from: item.agent_claims, at: claims, prominentClaimIndex: nil, on: platform
                    ).isEmpty)
            }
            let readable = composition.claims(
                from: item.agent_claims, at: claims, prominentClaimIndex: nil)
            #expect(!readable.isEmpty)
            #expect(readable.allSatisfy { $0.text != nil })
            #expect(
                composition.cardLeadClaims(from: item.agent_claims, prominentClaimIndex: nil)
                    == readable)
        }
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

    /// Frame 7.4: a dispute's positions each draw under an unverified
    /// keyword, so the first of them, the card's first such keyword in
    /// reading order, carries the one explanation control. A dispute with
    /// no positions keeps it on the supplied claim.
    @Test(arguments: DecisionCardComposition.UnverifiedContext.Platform.allCases)
    func theDisputesFirstPositionCarriesTheExplanation(
        platform: DecisionCardComposition.UnverifiedContext.Platform
    ) throws {
        let composition = DecisionCardComposition.forType(.review_dispute)
        let item = AttentionFixtures.fixture(type: .review_dispute).item
        let comparison = try #require(composition.modules.firstIndex(of: .comparison))
        let claims = try #require(composition.modules.firstIndex(of: .claims))

        #expect(
            composition.infoSlot(for: item, in: .init(platform: platform, hasComparison: true))
                == .module(comparison))
        #expect(
            composition.infoSlot(for: item, in: .init(platform: platform)) == .module(claims))
    }

    /// Frame 7.2: a prior comment and the agent's addressal read as
    /// conversation. A comment the agent claimed nothing for keeps its
    /// place with no response, never another comment's.
    @Test @MainActor func priorCommentsPairWithTheirOwnAddressal() throws {
        var item = AttentionFixtures.revisedSpecification().item
        var revision = try #require(item.spec_revision?.value1)
        let exchanges = DecisionDetailView.priorExchanges(in: item)

        #expect(exchanges.map(\.id) == revision.prior_comments.map(\.comment_id))
        #expect(exchanges.first?.marker == "on version \(revision.prior_comments[0].iteration)")
        #expect(exchanges.first?.response == revision.claimed_addressals.first?.response)

        revision.claimed_addressals = []
        item.spec_revision = .init(value1: revision)
        #expect(DecisionDetailView.priorExchanges(in: item).allSatisfy { $0.response == nil })

        item.spec_revision = nil
        #expect(DecisionDetailView.priorExchanges(in: item).isEmpty)
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
    /// that card draws no reason of its own; every other type keeps
    /// rendering the daemon's own sentence (#1098).
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
    /// move the reason into and keeps the reason rather than dropping the
    /// text entirely (#1098).
    @Test func legacySpecificationApprovalsKeepTheirReason() {
        let composition = DecisionCardComposition.forType(.spec_approval)
        var item = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(!composition.drawsReason(for: item))
        item.agent_claims.removeAll { $0.label == AgentClaimLabels.summary }
        #expect(composition.drawsReason(for: item))
    }

    /// Plan §7 "Routing": the diminishing-returns card leads with the verdict
    /// and the reversal list, so the stop cause renders above the actions.
    @Test func theStopCauseLeadsTheDiminishingCardAboveItsActions() throws {
        let composition = DecisionCardComposition.forType(.review_diminishing_returns)
        let stopCause = try #require(composition.modules.firstIndex(of: .stopCause))

        #expect(stopCause < composition.actionInsertionIndex)
        #expect(stopCause < (try #require(composition.modules.firstIndex(of: .yieldChart))))
        #expect(composition.actionInsertionIndex == composition.modules.firstIndex(of: .factBlock))
    }

    /// The daemon writes this item's reason as a summary line plus a
    /// `Binding: {…}` JSON line. With typed facts the card states the cause
    /// from them and draws no reason; an item without facts has no other
    /// statement of its cause and keeps it.
    @Test func aDiminishingCardWithTypedFactsDropsTheBindingReason() {
        let composition = DecisionCardComposition.forType(.review_diminishing_returns)
        var item = AttentionFixtures.fixture(type: .review_diminishing_returns).item
        item.reason =
            "Review yield has remained low under the resolved policy.\n"
            + #"Binding: {"run_id":"run-1","round":3}"#
        #expect(!composition.drawsReason(for: item))

        item.review_diminishing = nil
        #expect(composition.drawsReason(for: item))
    }

    /// A type only drops the reason because its summary layer renders the
    /// same text, so every such type has to compose that module and render
    /// it in the lead: dropping the reason took it out of the position §9
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

    /// Visual audit D09: the card emits one container per bound proposal, in
    /// the bound order, each keyed by its finding id and numbered by position.
    @Test func eachBoundProposalGetsItsOwnCard() throws {
        let binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let cards = FindingCardPresentation.cards(binding)

        #expect(cards.map(\.id) == binding.proposals.map(\.finding_id))
        #expect(cards.map(\.id) == ["review-finding-17", "review-finding-18"])
        #expect(cards.map(\.heading) == ["Finding 1", "Finding 2"])
    }

    /// Before anything is opened, a card shows the finding's exact message,
    /// the proposed route, who proposed it, and the reason (plan §9
    /// revision 78, #2608).
    @Test func aFindingCardShowsItsMessageRouteAndProducerBeforeAnythingOpens() throws {
        let binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let cards = FindingCardPresentation.cards(binding)
        let model = try #require(cards.first)
        let engineModel = try #require(cards.last)

        #expect(model.message == binding.proposals[0].finding_message)
        #expect(model.route == "Decline the finding")
        #expect(model.rationale == binding.proposals[0].rationale)
        // The word stays in the keyword; `UnverifiedLabel` draws it with the
        // button that explains it (visual audit D03).
        #expect(model.producer.spokenKeyword == "Proposed route (unverified)")
        #expect(model.producer.mark == "Agent · high")
        #expect(model.producer.sentence == "Model proposal (unverified)")

        #expect(engineModel.message == binding.proposals[1].finding_message)
        #expect(engineModel.route == "Fix in this PR")
        // A model judged the engine-authorized route, so it is unverified too.
        #expect(engineModel.producer.spokenKeyword == "Proposed route (unverified)")
        #expect(engineModel.producer.mark == "Agent · engine may fix")
        #expect(
            engineModel.producer.sentence == "Model judgment with engine-authorized remediation")
    }

    /// The head runs the message's title in with the heading, and the rest
    /// of the message draws under it (#2060). A realistic message is one
    /// long sentence, so it is all title and the card keeps its first
    /// viewport (#1141).
    @Test func aFindingCardSplitsItsMessageIntoTitleAndBody() throws {
        var binding = try #require(
            AttentionFixtures.findingAdjudicationRealisticMessages().item
                .finding_adjudication?.value1)
        for card in FindingCardPresentation.cards(binding) {
            #expect(card.title.map { String($0.characters) } == card.message)
            #expect(card.body.isEmpty)
        }

        binding.proposals[0].finding_message = "The retry is unbounded. Cap `attempts` at three."
        let card = try #require(FindingCardPresentation.cards(binding).first)
        #expect(card.title.map { String($0.characters) } == "The retry is unbounded.")
        #expect(card.body.count == 1)
        #expect(card.messageAccessibilityLabel == "Finding 1. \(binding.proposals[0].finding_message)")
    }

    /// Every other proposal field has a destination inside that finding's
    /// own disclosure, so removing the rendering below the actions lost
    /// nothing. The daemon's coordinates sit under their own title, apart
    /// from the producer's statements, and the binding digest stays with the
    /// binding in Identifiers. The route list leads with the proposed
    /// route, which states no consequence of its own.
    @Test func everyOtherFindingFieldHasADestinationInItsCardsDisclosure() throws {
        let binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let proposal = try #require(binding.proposals.first)
        let card = try #require(FindingCardPresentation.cards(binding).first)

        #expect(card.rationale == proposal.rationale)
        #expect(
            card.qualities == "Contradicts the goal · High confidence · compatibility not assessed")
        #expect(card.modelBacked)
        #expect(card.evidence == proposal.evidence)
        #expect(
            card.daemonFacts == [
                .init("Finding", "review-finding-17", monospaced: true),
                .init("Location", "daemon/internal/signet/service.go:214-227", monospaced: true),
                .init("Run", binding.run_id, monospaced: true),
                .init("Round", "3", monospaced: true),
            ])
        #expect(
            AttentionDisplay.findingAdjudicationRows(
                AttentionFixtures.fixture(type: .finding_adjudication).item
            ).contains(.init(label: "Adjudication Digest", value: binding.adjudication_digest)))
        #expect(card.assumptions == proposal.assumptions)
        #expect(card.citedRules == proposal.cited_rules)
        #expect(card.citedRulesKeyword == "Cited Rules")
        #expect(
            card.routeOptions == [
                .init(
                    route: proposal.route, label: card.route, consequence: nil,
                    isProposed: true),
                .init(
                    route: .dispute, label: AttentionDisplay.label(.dispute),
                    consequence: "Park the run: nothing is declined, fixed, or published.",
                    isProposed: false),
            ])
        #expect(card.gatingQuestions == proposal.open_questions)
        #expect(card.gatingQuestionsKeyword == "Open Question")
    }

    /// A finding with no location, no confidence, no alternatives, and empty
    /// lists keeps its card and its coordinates, and carries nothing for the
    /// view to draw an empty section from. A daemon-produced route is not a
    /// model's, so nothing of it draws as a quote, and with no alternative
    /// there is no route list: one route is not a choice.
    @Test func aFindingWithNothingOptionalStillHasItsCard() throws {
        var binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        var proposal = try #require(binding.proposals.last)
        proposal.producer = .engine
        proposal.finding_location = nil
        proposal.confidence = nil
        proposal.finding_message = ""
        proposal.evidence = []
        proposal.assumptions = []
        proposal.cited_rules = []
        proposal.offered_alternatives = []
        proposal.open_questions = []
        binding.proposals = [proposal]
        let card = try #require(FindingCardPresentation.cards(binding).first)

        #expect(card.heading == "Finding 1")
        #expect(card.message.isEmpty)
        #expect(card.messageAccessibilityLabel == "Finding 1")
        #expect(card.producer.spokenKeyword == "Proposed route")
        #expect(card.producer.mark == "Daemon")
        #expect(!card.modelBacked)
        #expect(!card.qualities.contains("confidence"))
        #expect(card.daemonFacts.map(\.label) == ["Finding", "Run", "Round"])
        #expect(card.evidence.isEmpty)
        #expect(card.assumptions.isEmpty)
        #expect(card.citedRules.isEmpty)
        #expect(card.routeOptions.isEmpty)
        #expect(card.gatingQuestions.isEmpty)
        #expect(card.disclosureSummary == nil)
    }

    /// The fold keeps what the operator checks: renamed now that the reason
    /// sits above it, with a summary counting its evidence, routes, and
    /// questions and naming only those present. Daemon Facts, cited rules,
    /// and assumptions are inside but not counted, which keeps the summary
    /// on its label's line within the first-viewport budget.
    @Test func theFindingFoldIsRenamedAndCountsWhatItHolds() throws {
        var binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let cards = FindingCardPresentation.cards(binding)

        #expect(FindingCardPresentation.disclosureTitle == "Evidence and Alternatives")
        #expect(cards.first?.disclosureSummary == "2 evidence · 2 routes · 1 question")
        #expect(cards.last?.disclosureSummary == "1 evidence")

        binding.proposals[0].offered_alternatives += [
            .init(route: .park_separate_work, consequence: "Park the run.")
        ]
        binding.proposals[0].evidence = ["service.go:214 re-derives the command id"]
        binding.proposals[0].open_questions = []
        #expect(
            FindingCardPresentation.cards(binding).first?.disclosureSummary
                == "1 evidence · 3 routes")
    }

    /// A finding's reason draws two lines on a card with one finding and
    /// one line once the card binds two or more, which keeps two realistic
    /// findings inside the first-viewport budget (#1141, #2608).
    @Test func aFindingsReasonClampsByHowManyFindingsTheCardBinds() {
        #expect(FindingCardPresentation.reasonLineLimit(findingCount: 1) == 2)
        #expect(FindingCardPresentation.reasonLineLimit(findingCount: 2) == 1)
        #expect(FindingCardPresentation.reasonLineLimit(findingCount: 6) == 1)
    }

    /// The clamp's control is More until it expands and Less after, and
    /// the same word names its accessibility action. Spoken, it says whose
    /// reason it opens, since every finding has one.
    @Test func theClampControlIsNamedForItsStateAndItsFinding() throws {
        let binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let card = try #require(FindingCardPresentation.cards(binding).first)

        #expect(ClampedText.controlTitle(isExpanded: false) == "More")
        #expect(ClampedText.controlTitle(isExpanded: true) == "Less")
        #expect(
            ClampedText.controlAccessibilityLabel(isExpanded: false, spokenName: card.reasonSpokenName)
                == "More, reason for Finding 1")
        #expect(
            ClampedText.controlAccessibilityLabel(isExpanded: true, spokenName: card.reasonSpokenName)
                == "Less, reason for Finding 1")
    }

    /// On `finding_adjudication` the recommendation's reason restates the
    /// finding items by id, so it leaves the face for the first Provenance
    /// row; every other card keeps it on the face (#2608).
    @Test func theRecommendationReasonIsTheFirstProvenanceRowOnlyOnFindingAdjudication() throws {
        let findings = AttentionFixtures.fixture(type: .finding_adjudication).item
        let findingRecommendation = try #require(DecisionRecommendationPresentation.of(findings))
        #expect(findingRecommendation.faceReason(for: findings) == nil)
        #expect(
            findingRecommendation.provenanceReason(for: findings)
                == "Decline review-finding-17. Fix review-finding-18 in this PR.")

        // The same recommendation on any other type keeps its reason on
        // the face and its Provenance as the source facts alone.
        for type in AttentionFixtures.phase1Types where type != .finding_adjudication {
            var item = AttentionFixtures.fixture(type: type).item
            item.recommendation = findings.recommendation
            let recommendation = try #require(DecisionRecommendationPresentation.of(item))
            #expect(recommendation.faceReason(for: item) == recommendation.reason)
            #expect(recommendation.provenanceReason(for: item) == nil)
        }
    }

    /// The spoken card names the finding, the route's keyword, who proposed
    /// it with the confidence its mark draws, and the route, so a model-proposed route is never heard as a
    /// daemon fact, and each card's identically titled disclosure says
    /// whose it is.
    @Test func aFindingCardIsSpokenWithItsFindingProducerAndRoute() throws {
        let binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let card = try #require(FindingCardPresentation.cards(binding).first)

        #expect(
            card.messageAccessibilityLabel
                == "Finding 1. Command handler retries without preserving the write-once "
                + "command identity.")
        #expect(
            card.routeAccessibilityLabel
                == "Finding 1 Proposed route (unverified), Model proposal (unverified), "
                + "high confidence: Decline the finding")
        #expect(
            card.disclosureAccessibilityLabel
                == "Evidence and Alternatives, Finding 1, 2 evidence · 2 routes · 1 question")
    }

    /// A held alternative is named on the card's face, where a closed
    /// disclosure cannot hide it, and says accepting leaves it unsent.
    @Test func aHeldAlternativeIsNamedOnItsFindingsCard() throws {
        let binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let card = try #require(FindingCardPresentation.cards(binding).first)

        #expect(
            FindingCardPresentation.selectionNotice(.dispute)
                == "Selected alternative: \(AttentionDisplay.label(.dispute)). "
                + "Accepting does not send it.")
        #expect(
            card.selectionAccessibilityLabel(.dispute)
                == "Finding 1. " + FindingCardPresentation.selectionNotice(.dispute))
    }

    /// The batch action says what it covers from the bound proposals: every
    /// one of them, at any count, and by number past two, where the stack
    /// of findings is longer than a glance takes in. One finding keeps the
    /// action's own label (the approved reference shows only the batch).
    @Test func acceptingStatesItsScopeFromTheBoundProposals() throws {
        let batch = AttentionFixtures.fixture(type: .finding_adjudication).item
        let single = AttentionFixtures.findingAdjudicationFixture(route: .remediate).item

        #expect(batch.finding_adjudication?.value1.proposals.count == 2)
        #expect(
            AttentionDisplay.label(.accept_recommended_route, for: batch)
                == "Accept All Dispositions")
        #expect(
            FindingCardPresentation.acceptanceScope(findingCount: 2)
                == "Accepting covers every proposed route above: all 2 findings.")

        var six = batch
        let proposals = try #require(batch.finding_adjudication?.value1.proposals)
        six.finding_adjudication?.value1.proposals = proposals + proposals + proposals
        #expect(
            AttentionDisplay.label(.accept_recommended_route, for: six)
                == "Accept All 6 Dispositions")
        var three = batch
        three.finding_adjudication?.value1.proposals = proposals + proposals.prefix(1)
        #expect(
            AttentionDisplay.label(.accept_recommended_route, for: three)
                == "Accept All 3 Dispositions")

        #expect(single.finding_adjudication?.value1.proposals.count == 1)
        #expect(
            AttentionDisplay.label(.accept_recommended_route, for: single)
                == "Accept Recommended Route")
        #expect(
            FindingCardPresentation.acceptanceScope(findingCount: 1)
                == "Accepting covers the proposed route for the one finding above.")

        // Only the batch acceptance is relabeled, and only where the item
        // binds findings.
        #expect(
            AttentionDisplay.label(.choose_alternative_route, for: batch)
                == AttentionDisplay.label(.choose_alternative_route))
        #expect(
            AttentionDisplay.label(.accept_recommended_route, for: nil)
                == "Accept Recommended Route")
    }

    /// The option's compact mark prints no register (R21), so the spoken
    /// label carries "unverified", on the recommended option alone and in
    /// the order the option is drawn.
    @Test func theRecommendedOptionStillReadsAsUnverified() {
        let recommended = AgentQuestionPresentation.Option(
            label: "Store first, then API", tradeoffs: "Existing rows migrate first.", recommended: true)
        let other = AgentQuestionPresentation.Option(
            label: "API first, then store", tradeoffs: "Clients move immediately.", recommended: false)
        #expect(
            DecisionDetailView.agentQuestionOptionAccessibilityLabel(recommended, number: 1)
                == "Option 1, Store first, then API, Agent recommends (unverified), Existing rows migrate first.")
        #expect(
            DecisionDetailView.agentQuestionOptionAccessibilityLabel(other, number: 2)
                == "Option 2, API first, then store, Clients move immediately.")
    }

    /// The closed foot's mono lines say what each fold holds in the
    /// operator's words (#2607): Evidence counts claims and attachments,
    /// and Identifiers names the item's version and head by value.
    @MainActor @Test func theFootSummariesCountWhatEachFoldHolds() {
        #expect(DecisionDetailView.evidenceSummary(claims: 1, attachments: 0) == "1 agent claim")
        #expect(DecisionDetailView.evidenceSummary(claims: 0, attachments: 2) == "2 attachments")
        #expect(
            DecisionDetailView.evidenceSummary(claims: 2, attachments: 1)
                == "2 agent claims · 1 attachment")

        var item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        item.item_version = 3
        item.pr_head_sha = "cafebabe1234567890"
        let rows: [AttentionDisplay.BindingRow] = [
            .init(label: "Item Version", value: "3"),
            .init(label: "Artifact Digest", value: "sha256:a"),
            .init(label: "Recipe Digest", value: "sha256:b"),
        ]
        #expect(
            DecisionDetailView.identifiersSummary(item, rows: rows)
                == "item v3 · pr head \(AttentionDisplay.shortRevision(item.pr_head_sha)) · 2 digests")
        item.pr_head_sha = ""
        #expect(DecisionDetailView.identifiersSummary(item, rows: Array(rows.prefix(2))) == "item v3 · 1 digest")
    }

    /// A sentence disclosure's label carries no unverified register. The
    /// Evidence fold draws the mark inside, so when it holds claims the
    /// closed disclosure says aloud that they are the agent's and
    /// unverified; attachments alone need no such word.
    @MainActor @Test func anEvidenceFoldWithClaimsSaysTheyAreUnverified() {
        #expect(
            DecisionDetailView.evidenceSpokenLabel(claims: 2, attachments: 1)
                == "Evidence, 2 unverified agent claims, 1 attachment")
        #expect(
            DecisionDetailView.evidenceSpokenLabel(claims: 1, attachments: 0)
                == "Evidence, 1 unverified agent claim")
        #expect(DecisionDetailView.evidenceSpokenLabel(claims: 0, attachments: 3) == nil)
    }

    /// The "Scope kept" row names the exact paths the operator left unchanged,
    /// so a long path prints whole rather than shortened: shortening a path a
    /// reader must recognize would lose meaning.
    @Test func scopeKeptRowKeepsTheWholePathUnshortened() throws {
        let longPath =
            "daemon/internal/store/production_attempt/migrations/0007_add_publish_eligible.go"
        var item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        item.scope_decision = .init(
            value1: .init(
                paths: [longPath], declared_paths: [longPath], head_sha: item.pr_head_sha,
                command_id: "scope-decision-long",
                answer: "Keep scope.", decided_at: Date(timeIntervalSince1970: 0)))

        let checklist = try #require(DecisionChecklistPresentation(item))

        let scopeRow = try #require(checklist.rows.first { $0.label == "Scope kept" })
        #expect(scopeRow.value.contains(longPath))
        #expect(longPath.count > FactRow.stackThreshold)
    }

    /// An accepted record-only command leaves its item open and unchanged,
    /// so its receipt names the action and says the item stays open (#1978).
    @Test(arguments: [
        (Components.Schemas.Action.acknowledge, "Acknowledge"),
        (.mark_seen, "Mark Seen"),
        (.open_pr, "View PR"),
        (.inspect_trust_failure, "Inspect Trust Failure"),
        (.run_doctor, "Run Doctor"),
    ])
    func recordOnlyReceiptNamesItsAction(
        action: Components.Schemas.Action, label: String
    ) {
        let item = AttentionFixtures.fixture(type: .system_health).item
        #expect(item.status == .open)
        #expect(
            DecisionCardComposition.recordOnlyReceipt(action: action, item: item, repeatCount: 1)
                == "\(label) recorded. This item stays open.")
    }

    /// The item looks the same after every accepted press, so the receipt
    /// has to change for a second one to be told from the first.
    @Test func recordOnlyReceiptCountsARepeat() {
        let item = AttentionFixtures.fixture(type: .system_health).item
        let first = DecisionCardComposition.recordOnlyReceipt(
            action: .acknowledge, item: item, repeatCount: 1)
        let second = DecisionCardComposition.recordOnlyReceipt(
            action: .acknowledge, item: item, repeatCount: 2)
        let third = DecisionCardComposition.recordOnlyReceipt(
            action: .acknowledge, item: item, repeatCount: 3)
        #expect(second == "Acknowledge recorded 2 times. This item stays open.")
        #expect(third == "Acknowledge recorded 3 times. This item stays open.")
        #expect(Set([first, second, third]).count == 3)
    }

    /// Every other accepted command keeps the head receipt: one that
    /// concludes or revises its item, `discuss`, and a record-only command
    /// whose item has closed since.
    @Test func recordOnlyReceiptIsOnlyForARecordOnlyCommandOnAnOpenItem() {
        let open = AttentionFixtures.fixture(type: .system_health).item
        for action in Components.Schemas.Action.allCases
        where ActionOutcome.of(action) != .records {
            #expect(
                DecisionCardComposition.recordOnlyReceipt(
                    action: action, item: open, repeatCount: 1) == nil,
                "\(action.rawValue) is not record-only")
        }
        for status in Components.Schemas.ItemStatus.allCases where status != .open {
            var closed = open
            closed.status = status
            #expect(
                DecisionCardComposition.recordOnlyReceipt(
                    action: .acknowledge, item: closed, repeatCount: 1) == nil,
                "a \(status.rawValue) item keeps the head receipt")
        }
    }

    /// The receipt draws in the control group, so a card that shows one must
    /// have a group: a record-only command is always one its item requested,
    /// and it leaves the item open.
    @Test func everyOpenItemRequestingARecordOnlyActionDrawsAControlGroup() {
        for snapshot in AttentionFixtures.defaultInbox() where snapshot.item.status == .open {
            let recordOnly = snapshot.item.requested_decision.filter {
                ActionOutcome.of($0) == .records
            }
            guard !recordOnly.isEmpty else { continue }
            #expect(DecisionDetailView.drawsControlGroup(snapshot.item), "\(snapshot.item.id)")
        }
    }

    /// A closed item takes no command, so its card draws no group of
    /// disabled actions, whatever it once requested (#2097).
    @Test func closedItemsDrawNoControlGroup() {
        for snapshot in AttentionFixtures.closedInbox() {
            #expect(!snapshot.item.requested_decision.isEmpty, "\(snapshot.item.id)")
            #expect(!DecisionDetailView.drawsControlGroup(snapshot.item), "\(snapshot.item.id)")
        }
    }

    // MARK: Review Guide (#2751)

    /// The guide fixture's item, and the same item with its guide removed.
    private static func guidedReady() -> (
        guided: Components.Schemas.AttentionItem, unguided: Components.Schemas.AttentionItem
    ) {
        let guided = AttentionFixtures.readyWithReviewGuide().item
        var unguided = guided
        unguided.review_guide = nil
        return (guided, unguided)
    }

    @Test func reviewGuideClaimIsListedOnlyWhenTheModuleDoesNotDrawIt() throws {
        let (guided, unguided) = Self.guidedReady()
        let composition = DecisionCardComposition.forType(.ready_for_final_review)
        let isGuideClaim = { (claim: Components.Schemas.AgentClaim) in
            claim.label == AgentClaimLabels.reviewGuide
        }
        // The fixture's guide claim has no inline text, so without the
        // module it is an attachment claim in the foot and the Evidence fold.
        #expect(guided.agent_claims.contains(where: isGuideClaim))
        for platform in DecisionCardComposition.UnverifiedContext.Platform.allCases {
            let drawn = DecisionCardComposition.listedClaims(guided)
            #expect(!composition.footClaims(from: drawn, prominentClaimIndex: nil).contains(where: isGuideClaim))
            #expect(
                !composition.evidenceClaims(from: drawn, prominentClaimIndex: nil, on: platform)
                    .contains(where: isGuideClaim))
            let listed = DecisionCardComposition.listedClaims(unguided)
            #expect(composition.footClaims(from: listed, prominentClaimIndex: nil).contains(where: isGuideClaim))
            #expect(
                composition.evidenceClaims(from: listed, prominentClaimIndex: nil, on: platform)
                    .contains(where: isGuideClaim))
        }
        // Only the claim the guide names leaves the lists: one with the
        // guide's label and another digest is still listed.
        var stray = guided
        var other = try #require(guided.agent_claims.first(where: isGuideClaim))
        other.digest = "sha256:another-guide"
        stray.agent_claims.append(other)
        #expect(DecisionCardComposition.listedClaims(stray).filter(isGuideClaim) == [other])
        // Its digest stays in Identifiers.
        let digest = try #require(guided.review_guide?.value1.claim_digest)
        #expect(
            DecisionDetailView.identifierRows(guided, proposalFacts: nil, effectProposalFacts: nil)
                .contains { $0.value == digest })
    }

    @Test func reviewGuideNamesItsProducerFromTheClaimItCites() throws {
        let (guided, unguided) = Self.guidedReady()
        let claim = try #require(DecisionCardComposition.reviewGuideClaim(guided))
        #expect(claim.digest == guided.review_guide?.value1.claim_digest)
        #expect(DecisionCardComposition.reviewGuideClaim(unguided) == nil)
    }

    @Test func reviewGuideKeywordCarriesTheUnverifiedRegisterOnlyWithAGuide() {
        let (guided, unguided) = Self.guidedReady()
        let composition = DecisionCardComposition.forType(.ready_for_final_review)
        let guideSlot = DecisionCardComposition.UnverifiedSlot.module(
            composition.modules.firstIndex(of: .reviewGuide)!)
        for platform in DecisionCardComposition.UnverifiedContext.Platform.allCases {
            let context = DecisionCardComposition.UnverifiedContext(platform: platform)
            #expect(composition.unverifiedSlots(for: guided, in: context).contains(guideSlot))
            #expect(!composition.unverifiedSlots(for: unguided, in: context).contains(guideSlot))
            // The summary comes first, so it keeps the card's one control.
            #expect(composition.infoSlot(for: guided, in: context) != guideSlot)
        }
    }

    @Test func reviewStopsOpenOnlyWhereOpenPRIsServedAndTheItemIsCurrent() throws {
        let guided = AttentionFixtures.readyWithReviewGuide().item
        let offered = DecisionActionRanking(
            requested: guided.requested_decision, servedActions: guided.requested_decision)
        #expect(guided.requested_decision.contains(.open_pr))
        #expect(DecisionCardComposition.opensReviewStops(guided, ranking: offered))
        let withheld = DecisionActionRanking(
            requested: guided.requested_decision,
            servedActions: guided.requested_decision.filter { $0 != .open_pr })
        #expect(!DecisionCardComposition.opensReviewStops(guided, ranking: withheld))
        // A recommended View PR leaves the ranking's other slots, and is
        // still served.
        let recommended = DecisionActionRanking(
            requested: guided.requested_decision, recommendedAction: .open_pr,
            servedActions: guided.requested_decision)
        #expect(recommended.recommended == .open_pr)
        #expect(DecisionCardComposition.opensReviewStops(guided, ranking: recommended))
        // A stale item's head no longer holds the code a stop points at.
        var stale = AttentionFixtures.staleReady().item
        stale.review_guide = guided.review_guide
        #expect(DecisionCardComposition.isStale(stale))
        #expect(
            !DecisionCardComposition.opensReviewStops(
                stale,
                ranking: DecisionActionRanking(
                    requested: stale.requested_decision, servedActions: stale.requested_decision)))
    }

    @Test func aStopOpensAtItsFirstResolvedHeadExcerpt() throws {
        let guide = try #require(AttentionFixtures.readyWithReviewGuide().item.review_guide?.value1)
        let resolved = try #require(DecisionCardComposition.openingExcerpt(guide.stops[0]))
        #expect(resolved.path == "daemon/internal/domain/review_guide.go")
        #expect(resolved.start_line == 10)
        // An unresolved base excerpt is nothing to open.
        #expect(DecisionCardComposition.openingExcerpt(guide.stops[1]) == nil)
        var unresolvedHead = guide.stops[0]
        unresolvedHead.excerpts[0].text = nil
        unresolvedHead.excerpts[0].unresolved_reason = .init(value1: .path_missing)
        #expect(DecisionCardComposition.openingExcerpt(unresolvedHead) == nil)
    }

    @Test func excerptTextSplitsIntoNumberableLines() {
        #expect(DecisionReviewGuideView.lines("a\n\tb\n") == ["a", "\tb"])
        #expect(DecisionReviewGuideView.lines("a\n\nb") == ["a", "", "b"])
        #expect(DecisionReviewGuideView.lines("only") == ["only"])
        #expect(DecisionReviewGuideView.lines("\n") == [""])
        // A CRLF file: Swift reads "\r\n" as one character.
        #expect(DecisionReviewGuideView.lines("a\r\nb\r\n") == ["a", "b"])
    }
}
