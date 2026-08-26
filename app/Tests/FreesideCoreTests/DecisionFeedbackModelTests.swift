import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct DecisionFeedbackModelTests {
    private struct ScheduledAction {
        let delay: Duration
        let action: @MainActor () -> Void
    }

    @Test func announcementPrecedesDelayedAdvanceAndReceiptDismissal() {
        var events: [String] = []
        var scheduled: [ScheduledAction] = []
        let model = DecisionFeedbackModel(
            announce: { events.append("announce:\($0)") },
            schedule: { delay, action in
                scheduled.append(.init(delay: delay, action: action))
                return Task {}
            })
        let conclusion = DecisionConclusion(
            itemID: "item-spec_approval",
            actionLabel: "Approve",
            resultingStatus: .resolved,
            at: Date(timeIntervalSince1970: 1_700_000_000))

        model.present(conclusion, advancesAutomatically: true) {
            events.append("advance")
        }

        #expect(model.conclusion == conclusion)
        #expect(events == ["announce:Approve applied."])
        #expect(scheduled.map(\.delay) == [.milliseconds(800), .seconds(6)])

        scheduled[0].action()
        #expect(events == ["announce:Approve applied.", "advance"])
        #expect(model.conclusion == conclusion)

        scheduled[1].action()
        #expect(model.conclusion == nil)
    }

    @Test func automaticAdvanceCanBeDisabledWithoutHidingTheReceipt() {
        var scheduled: [ScheduledAction] = []
        let model = DecisionFeedbackModel(
            announce: { _ in },
            schedule: { delay, action in
                scheduled.append(.init(delay: delay, action: action))
                return Task {}
            })

        model.present(
            .init(
                itemID: "item-spec_approval",
                actionLabel: "Approve",
                resultingStatus: .resolved,
                at: .now),
            advancesAutomatically: false,
            advance: { Issue.record("automatic advance ran while disabled") })

        #expect(model.conclusion != nil)
        #expect(scheduled.map(\.delay) == [.seconds(6)])
    }

    @Test func snoozeReceiptHasNoUnavailableViewDestination() {
        let feedback = DecisionFeedbackModel(
            announce: { _ in },
            schedule: { _, _ in Task {} })
        feedback.present(
            .init(
                itemID: "item-run_proposal",
                actionLabel: "Snooze",
                resultingStatus: nil,
                at: .now),
            advancesAutomatically: false,
            advance: {})

        #expect(feedback.conclusion?.hasQueryableItem == false)
    }

    @Test func receiptSurvivesTheResolvedItemLeavingTheOpenRebuild() async throws {
        let server = MockServer()
        let store = await makeStore(server: server)
        let feedback = DecisionFeedbackModel(
            announce: { _ in },
            schedule: { _, _ in Task {} })
        let model = DecisionModel(
            store: store,
            itemID: "item-spec_approval",
            onConclusion: { conclusion in
                feedback.present(
                    conclusion,
                    advancesAutomatically: false,
                    advance: {})
            })
        await model.validate()

        await model.submit(.approve)
        let conclusion = try #require(feedback.conclusion)
        store.replaceAll(
            with: store.openSnapshots.filter { $0.item.id != conclusion.itemID })

        #expect(store.snapshotsByID[conclusion.itemID] == nil)
        #expect(feedback.conclusion == conclusion)
    }
}
