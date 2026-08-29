import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct DecisionActionRankingTests {
    @Test func everyFixtureFiltersExactlyThePendingActionSet() {
        for type in AttentionFixtures.phase1Types {
            let requested = AttentionFixtures.phase1ActionSets[type] ?? []
            let ranking = DecisionActionRanking(requested: requested)
            let expectedUnavailable = requested.filter { ActionOutcome.of($0) == .pending }
            let visible =
                ranking.principal
                + [ranking.reviewing].compactMap { $0 }
                + ranking.overflow

            #expect(ranking.unavailable == expectedUnavailable)
            #expect(visible.allSatisfy { ActionOutcome.of($0) != .pending })
            #expect(Set(visible + ranking.unavailable) == Set(requested))
        }
    }

    @Test func recommendationIsExplicitNeverOfferOrder() {
        let requested: [Components.Schemas.Action] = [.approve, .retry]

        let absent = DecisionActionRanking(requested: requested)
        let explicit = DecisionActionRanking(
            requested: requested,
            recommendedAction: .retry)

        #expect(absent.recommended == nil)
        #expect(absent.principal == requested)
        #expect(explicit.recommended == .retry)
        #expect(explicit.principal == [.approve])
    }

    @Test func invalidOrUnavailableRecommendationDoesNotAcquireAuthority() {
        let requested: [Components.Schemas.Action] = [.approve, .answer_and_retry]

        #expect(
            DecisionActionRanking(requested: requested, recommendedAction: .retry)
                .recommended == nil)
        #expect(
            DecisionActionRanking(requested: requested, recommendedAction: .answer_and_retry)
                .recommended == nil)
    }

    @Test func reviewingAndOverflowActionsAreRankedByJob() {
        let ranking = DecisionActionRanking(
            requested: [.stop, .dismiss, .open_pr, .mark_seen, .snooze, .approve])

        #expect(ranking.principal == [.approve])
        #expect(ranking.reviewing == .open_pr)
        #expect(ranking.overflow == [.snooze, .mark_seen, .dismiss, .stop])
    }

    @Test func recommendationIsExcludedFromEverySecondaryActionBucket() {
        let reviewing = DecisionActionRanking(
            requested: [.approve, .open_pr],
            recommendedAction: .open_pr)
        let overflow = DecisionActionRanking(
            requested: [.approve, .stop],
            recommendedAction: .stop)

        #expect(reviewing.recommended == .open_pr)
        #expect(reviewing.principal == [.approve])
        #expect(reviewing.reviewing == nil)
        #expect(reviewing.overflow.isEmpty)
        #expect(overflow.recommended == .stop)
        #expect(overflow.principal == [.approve])
        #expect(overflow.reviewing == nil)
        #expect(overflow.overflow.isEmpty)
    }

    @Test func undisplayedRecommendationRemainsADecidingAction() {
        let ranking = DecisionActionRanking(
            requested: [.approve, .retry],
            recommendedAction: .approve,
            reservesRecommendedAction: false)

        #expect(ranking.recommended == nil)
        #expect(ranking.principal == [.approve, .retry])
    }

    @Test func allFilteredDecisionRendersCapabilityMismatch() {
        let ranking = DecisionActionRanking(
            requested: [.answer_and_retry])

        #expect(ranking.notDecidableHere)
        #expect(ranking.principal.isEmpty)
        #expect(ranking.reviewing == nil)
        #expect(ranking.overflow.isEmpty)
    }

    @Test func emptyInformationalItemIsNotCapabilityMismatch() {
        #expect(!DecisionActionRanking(requested: []).notDecidableHere)
    }

    @Test func confirmationsAreLimitedToConsequentialActions() {
        let item = AttentionFixtures.fixture(type: .execution_failure).item
        let confirmed: [Components.Schemas.Action] = [
            .stop, .stop_unattended, .decline, .dismiss,
        ]
        for action in AttentionFixtures.phase1Actions {
            #expect(
                (AttentionDisplay.confirmationConsequence(action, for: item) != nil)
                    == confirmed.contains(action))
        }
        #expect(AttentionDisplay.confirmationConsequence(.approve, for: item) == nil)
        #expect(AttentionDisplay.confirmationConsequence(.snooze, for: item) == nil)
    }

    @Test func iconsAreLimitedToNavigationRetryAndLossRisk() {
        let iconActions: [Components.Schemas.Action] = [
            .open_pr, .retry, .snooze, .stop, .stop_unattended, .return_to_agent,
        ]
        for action in AttentionFixtures.phase1Actions {
            #expect((AttentionDisplay.systemImage(action) != nil) == iconActions.contains(action))
        }
    }
}
