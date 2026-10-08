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
                .yieldChart, .summary, .claims, .evidence, .details,
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
        // then the diff and the verdict reached on it, and the review yield
        // follows the actions.
        #expect(
            DecisionCardComposition.forType(.ready_for_final_review).modules == [
                .recommendation, .summary, .facts, .checklist, .factBlock, .yieldChart, .claims,
                .evidence, .details,
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
                .comparison, .claims, .factBlock, .facts, .summary, .claims, .evidence,
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

        if type == .system_health || type == .blocked {
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
            [false, true].flatMap { accessibilityLayout in
                [false, true].map { recommended in
                    .init(
                        platform: platform, accessibilityLayout: accessibilityLayout,
                        drawsUnverifiedRecommendation: recommended)
                }
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
    /// attachment: macOS lists it in the inspector, and a phone folds the
    /// supporting claims at an accessibility size.
    @Test func aCardWithOnlyFoldedUnverifiedKeywordsNamesNoSlot() throws {
        let item = AttentionFixtures.fixture(type: .system_health).item
        let composition = DecisionCardComposition.forType(.system_health)
        let claims = try #require(composition.modules.firstIndex(of: .claims))

        #expect(composition.infoSlot(for: item, in: .init(platform: .phone)) == .module(claims))
        #expect(
            composition.infoSlot(
                for: item, in: .init(platform: .phone, accessibilityLayout: true)) == nil)
        #expect(composition.infoSlot(for: item, in: .init(platform: .mac)) == nil)
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
        let placement = DecisionFactPlacement(item, includesCommitPlan: false, now: now)
        let folds: [Components.Schemas.AttentionType] = [.agent_question, .review_dispute]

        #expect(DecisionCardComposition.foldsRoutineFacts(type) == folds.contains(type))
        #expect(placement.visible + placement.folded == facts)
        #expect(placement.folded == (folds.contains(type) ? facts : []))
    }

    @Test func foldedFactsAreTheRoutineCoordinates() {
        let now = Date(timeIntervalSince1970: 0)
        let question = DecisionFactPlacement(
            AttentionFixtures.fixture(type: .agent_question).item,
            includesCommitPlan: true, now: now)
        let dispute = DecisionFactPlacement(
            AttentionFixtures.fixture(type: .review_dispute).item,
            includesCommitPlan: true, now: now)

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

        let shown = DecisionFactPlacement(item, includesCommitPlan: true, now: now)
        #expect(shown.visible == [notice])
        #expect(shown.folded == AttentionDisplay.cardFacts(item, now: now))

        let withChecklist = DecisionFactPlacement(item, includesCommitPlan: false, now: now)
        #expect(withChecklist.visible.isEmpty)
    }

    /// The final review's checklist carries the commit-plan notice, so the
    /// card asks for its facts without it, as it does here.
    @Test func finalReviewKeepsItsDiffVisible() {
        let item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let placement = DecisionFactPlacement(
            item, includesCommitPlan: false, now: Date(timeIntervalSince1970: 0))

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

    /// Visual audit D06 to D08: the question card bounds only its options,
    /// the final review only the daemon's checklist, and the dispute reads
    /// its claim as prose, so all three quote their agent sections (R5);
    /// every other type keeps the dashed card around agent prose.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func agentSectionsAreQuotedOnlyOnTheApprovedCards(
        type: Components.Schemas.AttentionType
    ) {
        let quoted: [Components.Schemas.AttentionType] = [
            .agent_question, .ready_for_final_review, .review_dispute, .system_health, .blocked,
            .execution_failure, .task_proposal, .effect_proposal, .review_diminishing_returns,
            .spec_approval,
        ]
        #expect(
            DecisionCardComposition.agentSectionFrame(for: type)
                == (quoted.contains(type) ? .quoted : .dashedCard))
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

    /// R10: the ladder survey card 4b settled, on the cards composed on it
    /// so far. Every other type keeps the earlier scale until its sweep.
    @Test(arguments: Components.Schemas.AttentionType.allCases)
    func onlyTheComposedCardsTakeTheRefinedScale(type: Components.Schemas.AttentionType) {
        let refined: Set<Components.Schemas.AttentionType> = [
            .ready_for_final_review, .agent_question, .system_health, .blocked,
            .execution_failure, .task_proposal, .effect_proposal, .review_diminishing_returns,
            .review_dispute, .spec_approval,
        ]
        let scale = DecisionCardComposition.scale(for: type)
        #expect(scale == (refined.contains(type) ? .refined : .legacy))
    }

    @Test func refinedScaleIsTheLadderCard4bSettled() {
        let scale = DecisionCardComposition.Scale.refined
        #expect(scale.sectionGap == 22)
        #expect(scale.moduleGap == 11)
        #expect(scale.controlGap == 10)
        #expect(scale.foldLead == 18)
        #expect(scale.drawsFoldHairline)
        #expect(scale.padding(compact: false) == .init(top: 28, leading: 28, bottom: 24, trailing: 28))
        #expect(scale.padding(compact: true) == .init(top: 18, leading: 20, bottom: 18, trailing: 20))
        #expect(!scale.drawsReturnGlyph)
        #expect(scale.overflowLabel == "More Actions")
        #expect(DecisionCardComposition.Scale.legacy.overflowLabel == "More actions")
        #expect(!DecisionCardComposition.Scale.legacy.drawsFoldHairline)
        #expect(DecisionCardComposition.Scale.legacy.drawsReturnGlyph)
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

    /// The closed fold names what it holds: the folded rows' values in row
    /// order. A type that folds no routine fact has nothing to name.
    @Test func foldedFactsSummaryJoinsTheFoldedValues() {
        let question = AttentionFixtures.fixture(type: .agent_question).item
        let placement = DecisionFactPlacement(question, includesCommitPlan: false, now: .now)
        #expect(placement.foldedSummary == "Implementation \u{00B7} Owner decision")
        let review = AttentionFixtures.fixture(type: .ready_for_final_review).item
        #expect(DecisionFactPlacement(review, includesCommitPlan: false, now: .now).foldedSummary == nil)
    }

    @Test func reviewYieldFoldSaysHowManyRoundsRanAndHowTheLastEnded() throws {
        let item = AttentionFixtures.fixture(type: .ready_for_final_review).item
        let history = try #require(item.yield_history?.value1)
        let presentation = try #require(DecisionYieldPresentation(item))
        let rounds = history.rounds.count == 1 ? "1 round" : "\(history.rounds.count) rounds"
        let last =
            switch history.terminal_outcome {
            case .clean: "last clean"
            case .findings: "last had findings"
            }
        #expect(presentation.foldSummary == "\(rounds) · \(last)")
        #expect(DecisionYieldPresentation(rounds: []).foldSummary == "0 rounds")
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
        let row = DecisionChangeRow(specification: revision.diff, sinceRevision: 1)
        #expect(row.keyword == "Change Since Revision 1")
        #expect(row.counts.plain == "+2 \u{2212}1")
        #expect(row.spokenLabel == "Change Since Revision 1: 2 lines added, 1 removed")
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
        #expect(clean.verdictLine == "Clean · 1 note · 4 passed")
        #expect(clean.summary == "Readiness checklist: Clean, 1 note, 4 passed.")
        #expect(
            clean.accessibilitySummary.contains(
                "Commit plan: Plan present, not honored, informational"))
        #expect(degraded.verdict?.result == .failed)
        #expect(degraded.verdict?.value == "Degraded")
        #expect(degraded.verdictLine == "Degraded · 1 waived · 1 advisory · 1 note · 4 passed")

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
                == "Readiness checklist: Stale, was Clean, 1 note, 3 passed.")
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
        informationalOnly.base_freshness = nil
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
                "Commit plan", "Bound to", "clean-verification", "independent-review",
                "Terminal review",
            ])
        #expect(
            clean.rows[1] == .init(label: "Bound to", value: "Head cafebabe · Base main@deadbeef", result: .passed))
        #expect(clean.rows[2] == .init(label: "clean-verification", value: "Passed", result: .passed))
        #expect(clean.passedRows.count == 4)
        #expect(clean.leadingRows.map(\.label) == ["Commit plan"])

        let degraded = try #require(
            DecisionChecklistPresentation(AttentionFixtures.degradedReady().item))
        #expect(
            degraded.verdict == .init(label: "Verification verdict", value: "Degraded", result: .failed))
        #expect(
            degraded.rows.map(\.result) == [.waived, .advisory, .note, .passed, .passed, .passed, .passed])
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
        #expect(degraded.passedRows.count == 4)
        #expect(
            degraded.summary
                == "Readiness checklist: Degraded, 1 waived, 1 advisory, 1 note, 4 passed.")

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

        // A base advance the watch observed is the other staleness axis: the
        // verdict is still the daemon's, but it no longer describes the base.
        var advanced = AttentionFixtures.fixture(type: .ready_for_final_review).item
        advanced.base_freshness = .init(
            value1: .init(
                base_ref: "main", admitted_base_sha: "deadbeef", observed_base_sha: "0badf00d",
                advanced: true, observed_at: AttentionFixtures.createdInstant))
        let advancedChecklist = try #require(DecisionChecklistPresentation(advanced))
        #expect(
            advancedChecklist.verdict
                == .init(label: "Verification verdict", value: "Stale", result: .failed))
        #expect(advancedChecklist.priorVerdict == "was Clean")
        #expect(
            advancedChecklist.rows[0]
                == .init(
                    label: "Bound to", value: "Base main@deadbeef → 0badf00d · Head cafebabe",
                    result: .failed, isStale: true))
        // The Bound-to row carries the advance, so the freshness row does
        // not say it a second time.
        #expect(advancedChecklist.rows.first(where: { $0.label == "Base freshness" }) == nil)

        // Frame 7.3's notice: one sentence, built from the same typed
        // facts. An item with no verdict to name, or an invalidation whose
        // coordinates are not revisions, draws none.
        #expect(
            DecisionCardComposition.staleNotice(for: advanced)
                == "The base advanced after verification. The verdict below was clean at head "
                + "cafebabe against main@deadbeef; main is now at 0badf00d.")
        #expect(
            DecisionCardComposition.staleNotice(for: AttentionFixtures.staleReady().item)
                == "The head changed after verification. The verdict below was clean at head "
                + "cafebabe; the head is now at feedface.")
        var retargeted = AttentionFixtures.staleReady().item
        retargeted.readiness_invalidation?.value1.reason = .retargeted
        #expect(DecisionCardComposition.staleNotice(for: retargeted) == nil)
        var unverdicted = advanced
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

        #expect(degraded.passedRows.count == 4)
        #expect(
            degraded.accessibilitySummary.hasPrefix(
                "Readiness checklist: Degraded, 1 waived, 1 advisory, 1 note, 4 passed. "
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
                #expect(composition.drawsClaimsInCard(at: claims, on: platform))
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
        #expect(exchanges.first?.marker == "on revision \(revision.prior_comments[0].iteration)")
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
    /// the proposed route, and who proposed it (plan §9 revision 78).
    @Test func aFindingCardShowsItsMessageRouteAndProducerBeforeAnythingOpens() throws {
        let binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let cards = FindingCardPresentation.cards(binding)
        let model = try #require(cards.first)
        let engineModel = try #require(cards.last)

        #expect(model.message == binding.proposals[0].finding_message)
        #expect(model.route == "Decline the finding")
        #expect(model.producerLabel == "Model proposal (unverified)")
        // The word stays in the label; `UnverifiedLabel` draws it with the
        // button that explains it (visual audit D03).
        #expect(model.producerUnverifiedKeyword == "Model proposal")

        #expect(engineModel.message == binding.proposals[1].finding_message)
        #expect(engineModel.route == "Fix in this PR")
        #expect(
            engineModel.producerLabel == "Model judgment with engine-authorized remediation")
        #expect(engineModel.producerUnverifiedKeyword == nil)
    }

    /// Every other proposal and binding field has a destination inside that
    /// finding's own disclosure, so removing the rendering below the actions
    /// lost nothing. The daemon's coordinates sit under their own title,
    /// apart from the producer's rationale and evidence.
    @Test func everyOtherFindingFieldHasADestinationInItsCardsDisclosure() throws {
        let binding = try #require(
            AttentionFixtures.fixture(type: .finding_adjudication).item
                .finding_adjudication?.value1)
        let proposal = try #require(binding.proposals.first)
        let card = try #require(FindingCardPresentation.cards(binding).first)

        #expect(card.rationale == proposal.rationale)
        #expect(
            card.proposalRows == [
                .init("Goal relationship", "Contradictory"),
                .init("Work-unit compatibility", "Not assessed"),
                .init("Confidence", "High"),
            ])
        #expect(card.evidenceTitle == "Evidence (model-derived)")
        #expect(card.evidence == proposal.evidence)
        #expect(
            card.daemonFacts == [
                .init("Finding", "review-finding-17", monospaced: true),
                .init("Location", "daemon/internal/signet/service.go:214-227", monospaced: true),
                .init("Binding digest", binding.adjudication_digest, monospaced: true),
                .init("Run", binding.run_id, monospaced: true),
                .init("Round", "3", monospaced: true),
            ])
        #expect(card.assumptions == proposal.assumptions)
        #expect(card.citedRules == proposal.cited_rules)
        #expect(
            card.alternatives == [
                .init(
                    route: .dispute, label: AttentionDisplay.label(.dispute),
                    consequence: "Park the run: nothing is declined, fixed, or published.")
            ])
        #expect(card.gatingQuestions == proposal.open_questions)
    }

    /// A finding with no location, no confidence, no alternatives, and empty
    /// lists keeps its card and its coordinates, and carries nothing for the
    /// view to draw an empty section from. A daemon-produced route labels its
    /// evidence as the daemon's.
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
        #expect(card.producerLabel == "Daemon recommendation")
        #expect(card.producerUnverifiedKeyword == nil)
        #expect(card.proposalRows.map(\.label) == ["Goal relationship", "Work-unit compatibility"])
        #expect(card.evidenceTitle == "Evidence (daemon-derived)")
        #expect(card.daemonFacts.map(\.label) == ["Finding", "Binding digest", "Run", "Round"])
        #expect(card.evidence.isEmpty)
        #expect(card.assumptions.isEmpty)
        #expect(card.citedRules.isEmpty)
        #expect(card.alternatives.isEmpty)
        #expect(card.gatingQuestions.isEmpty)
    }

    /// The spoken card names the finding, who proposed its route, and the
    /// route, so a model-proposed route is never heard as a daemon fact, and
    /// each card's identically titled disclosure says whose it is.
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
                == "Finding 1 proposed route, Model proposal (unverified): Decline the finding")
        #expect(card.disclosureAccessibilityLabel == "Reason and alternatives, Finding 1")
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
    /// one of them, at any count. One finding keeps the action's own label
    /// (the approved reference shows only the batch).
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

    /// The card's Evidence module points at the open inspector rather than
    /// drawing the same attachments beside it, and the pointer counts them in
    /// the operator's words (#1107).
    @Test func theEvidencePointerCountsTheAttachmentsItStandsFor() {
        #expect(DecisionDetailView.evidencePointer(1) == "1 attachment → inspector")
        #expect(DecisionDetailView.evidencePointer(3) == "3 attachments → inspector")
        #expect(
            DecisionDetailView.evidencePointerAccessibilityLabel(1)
                == "1 attachment, shown in the inspector")
        #expect(
            DecisionDetailView.evidencePointerAccessibilityLabel(3)
                == "3 attachments, shown in the inspector")
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
}
