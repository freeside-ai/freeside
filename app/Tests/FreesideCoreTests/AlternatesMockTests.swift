import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideModel

/// #1722: `-FreesideAlternates YES` seeds the mock with a quota failure and a
/// quota-parked task, so the retry dialog and the move menu can be captured
/// with offers that read current, stale, and unsupported.
@Suite @MainActor struct AlternatesMockTests {
    /// A launch in November 2026, months past the fixtures' August readings.
    private static let now = Date(timeIntervalSince1970: 1_794_000_000)
    private static let utc = TimeZone(identifier: "UTC")!
    private static let locale = Locale(identifier: "en_US_POSIX")

    private func coordinator() async -> SyncCoordinator {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: .alternates(now: Self.now)),
            device: DeviceIdentity(deviceID: "device-mock"), cache: InMemoryCacheStore(), submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    private func parkedTaskID(_ coordinator: SyncCoordinator) throws -> String {
        try #require(
            coordinator.tasks.first { $0.task.current_position?.value1.run_id == RunFixtures.activeRunID }?.task.id)
    }

    @Test func theCardAndTheParkedTaskOfferEnrollmentsThatReadThreeWays() async throws {
        let coordinator = await coordinator()
        let offers = UsageFixtures.alternateAgentOffers()
        let card = try #require(coordinator.store.orderedSnapshots.first { $0.item.id == "item-execution_failure" })
        #expect(card.item.execution_failure?.value1.cause == .quota)
        #expect(card.item.alternate_agents == offers)

        let taskID = try parkedTaskID(coordinator)
        await coordinator.refreshTaskTimeline(for: taskID)
        let task = try #require(coordinator.tasks.first { $0.task.id == taskID })
        let position = TaskDisplay.position(
            task.task, runs: coordinator.runs, history: coordinator.taskTimelinesByTaskID[taskID])
        #expect(TaskDisplay.moveOffers(position) == offers)

        let labels = AgentChoiceDisplay.offerOptions(
            offers, pools: coordinator.usagePools, now: Self.now, locale: Self.locale, timeZone: Self.utc
        ).map(\.label)
        let readAt = FreesideFormat.shortTime(
            Self.now.addingTimeInterval(-300), now: Self.now, locale: Self.locale, timeZone: Self.utc)
        #expect(
            labels[0]
                == "\(UsageFixtures.readingSecondEnrollmentID) · claude-opus · claude-team · 5-hour: 65% left · read \(readAt)"
        )
        #expect(labels[1].hasPrefix("\(UsageFixtures.staleEnrollmentID) · claude-opus · claude-b · "))
        #expect(labels[1].contains(" · stale since "))
        #expect(labels[2].hasPrefix("\(UsageFixtures.unsupportedCollectorID) · gpt-codex · codex-b · unsupported "))
    }

    @Test func aMoveFromTheParkedTaskIsRecorded() async throws {
        let coordinator = await coordinator()
        let taskID = try parkedTaskID(coordinator)
        let model = ParkedTaskMoveModel(coordinator: coordinator)

        await model.move(taskID: taskID, to: .init(enrollment_id: UsageFixtures.readingSecondEnrollmentID))

        #expect(model.failure == nil)
        let line = try #require(
            coordinator.tasks.first { $0.task.id == taskID }?.task.task_lines.first { $0.role == .implementer })
        #expect(line.selection?.value1.enrollment_id == UsageFixtures.readingSecondEnrollmentID)
        #expect(line.provenance?.value1.pool == UsageFixtures.currentPoolID)
    }
}
