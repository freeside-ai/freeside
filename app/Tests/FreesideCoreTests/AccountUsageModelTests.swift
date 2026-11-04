import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// #1721: the usage pools read from sync, and refresh and reassignment are
/// saved commands whose results arrive through sync.
@Suite @MainActor struct AccountUsageModelTests {
    private func coordinator(
        _ server: MockServer = MockServer(), cache: any CacheStore = InMemoryCacheStore()
    ) async -> SyncCoordinator {
        let result = SyncCoordinator(
            client: APIClientFactory.mock(server: server), device: DeviceIdentity(deviceID: "device-mock"),
            cache: cache, submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    private func pool(_ coordinator: SyncCoordinator, _ id: String) throws -> Components.Schemas.UsagePoolSnapshot {
        try #require(coordinator.usagePools.first { $0.pool.id == id })
    }

    // MARK: Reading

    @Test func syncAdoptsThePoolsAndARelaunchRestoresThem() async throws {
        let cache = InMemoryCacheStore()
        let first = await coordinator(cache: cache)
        #expect(first.usagePools.map(\.pool.id) == UsageFixtures.defaultUsagePools().map(\.pool.id).sorted())
        #expect(cache.load()?.usagePools == first.usagePools)
        let restored = SyncCoordinator(client: first.store.client, cache: cache)
        #expect(restored.usagePools == first.usagePools)
    }

    @Test func aRecordWriteReachesTheClientThroughTheRevision() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let before = try pool(coordinator, UsageFixtures.readingPoolID)
        await server.advanceUsagePool(id: UsageFixtures.readingPoolID)
        await coordinator.refresh()
        #expect(try pool(coordinator, UsageFixtures.readingPoolID).entity_version == before.entity_version + 1)
    }

    // MARK: Refresh

    @Test func anAcceptedRefreshSaysItWasRequestedAndReadingsArriveThroughSync() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = await coordinator(server, cache: cache)
        let model = AccountUsageModel(coordinator: coordinator)
        let before = try pool(coordinator, UsageFixtures.readingPoolID)
        await model.refresh(pool: UsageFixtures.readingPoolID)
        #expect(coordinator.pendingUsageCommand == nil)
        #expect(cache.load()?.pendingUsageCommand == nil)
        guard case .refresh_usage(let record) = model.settled?.record else {
            Issue.record("no settled refresh")
            return
        }
        #expect(record.pool == UsageFixtures.readingPoolID)
        // The receipt changes no record; the collection's write does.
        #expect(try pool(coordinator, UsageFixtures.readingPoolID).entity_version == before.entity_version)
        await server.advanceUsagePool(id: UsageFixtures.readingPoolID)
        await coordinator.refresh()
        #expect(try pool(coordinator, UsageFixtures.readingPoolID).entity_version == before.entity_version + 1)
    }

    /// A refresh's readings may never come, so its receipt alone settles it,
    /// even when the follow-up sync fails.
    @Test func aRefreshSettlesOnItsReceipt() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "getSyncBootstrap" { throw MockServer.ForcedStatus(503) }
        }
        await model.refresh(pool: UsageFixtures.readingPoolID)
        #expect(coordinator.pendingUsageCommand == nil)
        #expect(model.settled != nil)
    }

    // MARK: Reassignment

    @Test func anAcceptedReassignmentSettlesOnceSyncShowsTheNewCollector() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        // The daemon accepts the reassignment, but the follow-up bootstrap fails.
        await server.setBeforeRespond { operation in
            if operation == "getSyncBootstrap" { throw MockServer.ForcedStatus(503) }
        }
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        #expect(coordinator.pendingUsageCommand?.receipt != nil)
        #expect(try pool(coordinator, UsageFixtures.readingPoolID).pool.collector == UsageFixtures.readingCollectorID)
        #expect(model.settled == nil)
        #expect(!model.canSend)

        await server.setBeforeRespond(nil)
        await coordinator.refresh()
        #expect(coordinator.pendingUsageCommand == nil)
        let synced = try pool(coordinator, UsageFixtures.readingPoolID)
        #expect(synced.pool.collector == UsageFixtures.readingSecondEnrollmentID)
        #expect(synced.pool.windows.isEmpty)
        guard case .reassign_collector(let record) = model.settled?.record else {
            Issue.record("no settled reassignment")
            return
        }
        #expect(record.collector == UsageFixtures.readingSecondEnrollmentID)
        #expect(model.canSend)
    }

    /// The daemon records nothing for the collector the pool already has,
    /// so the snapshot's revision, not the record's, settles it.
    @Test func reassigningTheCurrentCollectorSettles() async throws {
        let coordinator = await coordinator()
        let model = AccountUsageModel(coordinator: coordinator)
        let before = try pool(coordinator, UsageFixtures.readingPoolID)
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingCollectorID)
        #expect(coordinator.pendingUsageCommand == nil)
        #expect(model.settled != nil)
        #expect(try pool(coordinator, UsageFixtures.readingPoolID) == before)
    }

    /// Another device's later reassignment replaces this one's notice.
    @Test func aLaterReassignmentFromAnotherDeviceReplacesTheNotice() async throws {
        let coordinator = await coordinator()
        let model = AccountUsageModel(coordinator: coordinator)
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        #expect(model.settled != nil)
        let other = Components.Schemas.ClientCommand(
            command_id: "other-reassign", device_id: DeviceFixtures.otherDeviceID,
            payload: .reassign_collector(
                .init(
                    kind: .reassign_collector, pool: UsageFixtures.readingPoolID,
                    enrollment_id: UsageFixtures.readingCollectorID)))
        _ = try await coordinator.store.client.submitCommand(body: .json(other)).ok
        await coordinator.refresh()
        #expect(model.settled == nil)
    }

    // MARK: Delivery

    @Test func theCommandIsSavedBeforeItIsSentAndNotSentWhenSavingFails() async throws {
        let coordinator = await coordinator(cache: RefusingCache())
        let model = AccountUsageModel(coordinator: coordinator)
        let before = try await revision(coordinator)
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        #expect(model.failure?.contains("Nothing was sent") == true)
        #expect(coordinator.pendingUsageCommand == nil)
        #expect(try await revision(coordinator) == before)
    }

    @Test func oneUsageCommandIsPendingAtATime() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        await model.refresh(pool: UsageFixtures.readingPoolID)
        let first = try #require(coordinator.pendingUsageCommand)
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        #expect(coordinator.pendingUsageCommand == first)
    }

    @Test func aLostAnswerSurvivesRelaunchAndRetryResendsTheSameCommand() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let cache = DiskCacheStore(directory: directory)
        let server = MockServer()
        let first = await coordinator(server, cache: cache)
        let lost = InjectedFailures(times: 1)
        await server.setAfterRespond { operation in
            if operation == "submitCommand" { try await lost.consume() }
        }
        let firstModel = AccountUsageModel(coordinator: first)
        await firstModel.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        let saved = try #require(first.pendingUsageCommand)
        #expect(saved.receipt == nil)
        let applied = try await revision(first)

        let restored = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-mock"), cache: cache,
            submissionDaemonID: "mock")
        #expect(restored.pendingUsageCommand == saved)
        await restored.refresh()
        // A relaunch never resends on its own.
        #expect(restored.pendingUsageCommand == saved)
        let restoredModel = AccountUsageModel(coordinator: restored)
        await restoredModel.retry()
        // The replay returns the recorded receipt and applies nothing twice.
        #expect(restored.pendingUsageCommand == nil)
        #expect(try await revision(restored) == applied)
        #expect(restoredModel.settled?.revision == applied)
        #expect(
            try pool(restored, UsageFixtures.readingPoolID).pool.collector
                == UsageFixtures.readingSecondEnrollmentID)
    }

    @Test func aSavedCommandFromAnotherDeviceOrDaemonIsNotRestored() async throws {
        let cache = InMemoryCacheStore()
        let server = MockServer()
        let first = await coordinator(server, cache: cache)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        await AccountUsageModel(coordinator: first).refresh(pool: UsageFixtures.readingPoolID)
        #expect(first.pendingUsageCommand != nil)
        let otherDevice = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-other"), cache: cache,
            submissionDaemonID: "mock")
        #expect(otherDevice.pendingUsageCommand == nil)
        let otherDaemon = SyncCoordinator(
            client: first.store.client, device: DeviceIdentity(deviceID: "device-mock"), cache: cache,
            submissionDaemonID: "other")
        #expect(otherDaemon.pendingUsageCommand == nil)
    }

    @Test func aForgedReceiptLeavesTheCommandUnresolved() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        await server.setCommandResultTransform { result in
            guard case .reassign_collector(var record) = result.record else { return result }
            record.collector = UsageFixtures.readingCollectorID
            return .init(record: .reassign_collector(record), revision: result.revision)
        }
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        let pending = try #require(coordinator.pendingUsageCommand)
        #expect(pending.receipt == nil)
        #expect(model.settled == nil)
    }

    @Test func anUnknownPoolFailsAndChangesNothing() async throws {
        let coordinator = await coordinator()
        let model = AccountUsageModel(coordinator: coordinator)
        let before = coordinator.usagePools
        await model.refresh(pool: "missing")
        #expect(coordinator.pendingUsageCommand == nil)
        #expect(model.failure?.contains("status 404") == true)
        #expect(model.failure?.contains("Nothing was changed") == true)
        #expect(coordinator.usagePools == before)
    }

    @Test func anEnrollmentNotOnThePoolFailsAndChangesNothing() async throws {
        let coordinator = await coordinator()
        let model = AccountUsageModel(coordinator: coordinator)
        let before = coordinator.usagePools
        await model.reassign(pool: UsageFixtures.unsupportedPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        #expect(coordinator.pendingUsageCommand == nil)
        #expect(model.failure?.contains("status 400") == true)
        #expect(coordinator.usagePools == before)
        // A sheet reopened after the refusal still shows it.
        #expect(AccountUsageModel(coordinator: coordinator).failure == model.failure)
    }

    @Test(arguments: [401, 403, 500, 503])
    func anUnresolvedAnswerKeepsTheSavedCommand(status: Int) async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(status) }
        }
        await model.refresh(pool: UsageFixtures.readingPoolID)
        let pending = try #require(coordinator.pendingUsageCommand)
        #expect(pending.receipt == nil)
        #expect(!model.canSend)
        guard status == 401 || status == 403 else { return }
        #expect(coordinator.store.freshness == .unauthenticated)
        // An unpaired device sends nothing, Retry included.
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.record($0) }
        await model.retry()
        #expect(await !operations.operations.contains("submitCommand"))
        #expect(coordinator.pendingUsageCommand == pending)
    }

    @Test func anUnansweredCommandIsNotResentInANewEpoch() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        #expect(coordinator.pendingUsageCommand?.receipt == nil)
        await server.setBeforeRespond(nil)
        await server.rotateEpoch()
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.record($0) }
        await coordinator.refresh()
        await model.retry()
        #expect(coordinator.pendingUsageCommand == nil)
        #expect(await !operations.operations.contains("submitCommand"))
        #expect(try pool(coordinator, UsageFixtures.readingPoolID).pool.collector == UsageFixtures.readingCollectorID)
    }

    /// A receipt from the old epoch can't be compared with the new record,
    /// so the command finishes without claiming the reassignment.
    @Test func anAcceptedReassignmentFinishesWithoutANoticeInANewEpoch() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "getSyncBootstrap" { throw MockServer.ForcedStatus(503) }
        }
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        #expect(coordinator.pendingUsageCommand?.receipt != nil)
        await server.setBeforeRespond(nil)
        await server.rotateEpoch()
        await coordinator.refresh()
        #expect(coordinator.pendingUsageCommand == nil)
        #expect(model.settled == nil)
        #expect(model.canSend)
    }

    // MARK: Display

    private static let base = Date(timeIntervalSince1970: 1_786_502_400)

    private func reading(
        _ window: String, percent: Int? = nil, quantity: Int64? = nil, unit: String? = nil,
        status: Components.Schemas.WindowStatus = .allowed, staleAt: Date? = nil
    ) -> Components.Schemas.UsageWindowReading {
        .init(
            window: window, enrollment_id: "enrollment", generation: 1, status: status, used_percent: percent,
            quantity: quantity, unit: unit, observed_at: Self.base, stale_at: staleAt)
    }

    @Test func aShareIsTheReadingsOwnNumberOrNone() {
        #expect(UsageDisplay.share(reading("seven_day", percent: 42)) == "42%")
        #expect(UsageDisplay.share(reading("credits", quantity: 1_200, unit: "credits")) == "1200 credits")
        #expect(UsageDisplay.share(reading("five_hour", status: .exhausted)) == nil)
        #expect(UsageDisplay.value(reading("five_hour", status: .exhausted)) == "Exhausted")
        #expect(UsageDisplay.value(reading("five_hour")) == "Allowed")
    }

    @Test func aReadingIsStaleOnceTheClockReachesItsStaleAt() {
        let stale = reading("seven_day", percent: 10, staleAt: Self.base.addingTimeInterval(60))
        #expect(!UsageDisplay.isStale(stale, now: Self.base))
        #expect(UsageDisplay.isStale(stale, now: Self.base.addingTimeInterval(60)))
        #expect(!UsageDisplay.isStale(reading("seven_day"), now: .distantFuture))
    }

    @Test func aWindowIsExhaustedWhileAnyMarkHasNotLapsed() {
        let lapsing = Components.Schemas.UsageExhaustedMark(
            window: "seven_day", source: .usage_reading, reading_observed_at: Self.base,
            resets_at: Self.base.addingTimeInterval(60), set_at: Self.base)
        let holding = Components.Schemas.UsageExhaustedMark(
            window: "five_hour", source: .provider_refusal, invocation_id: "invocation", set_at: Self.base)
        let marks = [lapsing, holding]
        #expect(UsageDisplay.isExhausted(window: "seven_day", marks: marks, now: Self.base))
        #expect(!UsageDisplay.isExhausted(window: "seven_day", marks: marks, now: Self.base.addingTimeInterval(60)))
        // A mark with no reset holds until the record changes.
        #expect(UsageDisplay.isExhausted(window: "five_hour", marks: marks, now: .distantFuture))
        #expect(!UsageDisplay.isExhausted(window: "seven_day_opus", marks: marks, now: Self.base))
    }

    @Test func theWindowsAreEveryReadingAndMarkInWindowOrder() throws {
        var pool = try #require(UsageFixtures.defaultUsagePools().first).pool
        #expect(UsageDisplay.windows(pool) == ["five_hour", "seven_day_opus"])
        #expect(UsageDisplay.models(limitedBy: "seven_day_opus", in: pool) == ["claude-opus"])
        #expect(UsageDisplay.models(limitedBy: "five_hour", in: pool).isEmpty)
        // After a reassignment the refusal outlives the readings.
        pool.windows = []
        #expect(UsageDisplay.windows(pool) == ["seven_day_opus"])
    }

    @Test func knownWindowsHaveWordsAndOthersShowTheirId() {
        #expect(UsageDisplay.label("seven_day") == "Weekly")
        #expect(UsageDisplay.label("five_hour") == "5-hour")
        #expect(UsageDisplay.label("seven_day_cowork") == nil)
    }

    @Test func theCountdownIsToTheMinute() {
        let now = Self.base
        #expect(UsageDisplay.countdown(to: now.addingTimeInterval(30), now: now) == "in 1m")
        #expect(UsageDisplay.countdown(to: now.addingTimeInterval(45 * 60), now: now) == "in 45m")
        #expect(UsageDisplay.countdown(to: now.addingTimeInterval(2 * 3_600), now: now) == "in 2h")
        #expect(UsageDisplay.countdown(to: now.addingTimeInterval(9_000), now: now) == "in 2h 30m")
        #expect(UsageDisplay.countdown(to: now.addingTimeInterval(3 * 86_400 + 4 * 3_600), now: now) == "in 3d 4h")
        #expect(UsageDisplay.countdown(to: now, now: now) == "passed")
    }

    // MARK: Helpers

    private func revision(_ coordinator: SyncCoordinator) async throws -> Int64 {
        try await coordinator.store.client.getSyncRevision().ok.body.json.revision
    }
}

private struct RefusingCache: CacheStore {
    struct Unavailable: Error {}
    func load() -> CachedState? { nil }
    func save(_ state: CachedState) throws { throw Unavailable() }
    func discard() {}
}

private actor OperationLog {
    private(set) var operations: [String] = []
    func record(_ operation: String) { operations.append(operation) }
}
