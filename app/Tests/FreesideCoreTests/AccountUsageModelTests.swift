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

    @Test func theSettingsRowSummarizesThePoolsOrAnUnansweredCommand() async throws {
        let server = MockServer()
        let model = AccountUsageModel(coordinator: await coordinator(server))
        #expect(SettingsSheet.usageSummary(model) == "3 pools")
        let empty = AccountUsageModel(coordinator: await coordinator(MockServer(usagePools: [])))
        #expect(SettingsSheet.usageSummary(empty) == "No pools")
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        await model.refresh(pool: UsageFixtures.readingPoolID)
        #expect(SettingsSheet.usageSummary(model) == "Unconfirmed")
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
        // After a reassignment the refusal outlives the readings.
        pool.windows = []
        #expect(UsageDisplay.windows(pool) == ["seven_day_opus"])
    }

    @Test func aNoticeNamesTheCommandRetryResends() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = AccountUsageModel(coordinator: coordinator)
        await server.setBeforeRespond { operation in
            if operation == "submitCommand" { throw MockServer.ForcedStatus(503) }
        }
        await model.reassign(pool: UsageFixtures.readingPoolID, to: UsageFixtures.readingSecondEnrollmentID)
        let pending = try #require(model.pending)
        #expect(
            AccountUsageView.describe(pending)
                == "making \(UsageFixtures.readingSecondEnrollmentID) the collector for \(UsageFixtures.readingPoolID)")
    }

    /// Each provider keyword heads its pools in the daemon's pool order,
    /// providers in the order each first appears.
    @Test func thePoolsGroupUnderTheirProviderInFirstAppearanceOrder() {
        let groups = AccountUsageView.providerGroups(UsageFixtures.defaultUsagePools())
        #expect(groups.map(\.provider) == ["anthropic", "openai"])
        #expect(
            groups.map { $0.pools.map(\.id) }
                == [
                    [UsageFixtures.readingPoolID, UsageFixtures.unrecordedPoolID], [UsageFixtures.unsupportedPoolID],
                ])
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

    // MARK: Capacity (R32)

    private func mark(_ window: String, resetsAt: Date?) -> Components.Schemas.UsageExhaustedMark {
        .init(
            window: window, source: .provider_refusal, invocation_id: "invocation", resets_at: resetsAt,
            set_at: Self.base)
    }

    private func emptyPool() throws -> Components.Schemas.UsagePoolUsage {
        var pool = try #require(UsageFixtures.defaultUsagePools().first).pool
        pool.windows = []
        pool.recorded_exhausted_marks = []
        return pool
    }

    @Test func whatIsLeftIsWhatTheReadingsPercentLeaves() {
        #expect(UsageDisplay.left(reading("seven_day", percent: 42)) == 58)
        #expect(UsageDisplay.left(reading("seven_day", percent: 100)) == 0)
        // The schema sets no ceiling, and nothing reads as less than none left.
        #expect(UsageDisplay.left(reading("seven_day", percent: 120)) == 0)
        #expect(UsageDisplay.left(reading("credits", quantity: 1_200, unit: "credits")) == nil)
        #expect(UsageDisplay.left(reading("five_hour", status: .exhausted)) == nil)
    }

    /// The fixture's two windows: `five_hour` has room, and `seven_day_opus`
    /// is used up until its refusal lapses, a day after its reading resets.
    @Test func theFixturesWindowsReadAsWhatIsLeft() throws {
        let pool = try #require(UsageFixtures.defaultUsagePools().first).pool
        let now = Self.base.addingTimeInterval(3_000)
        #expect(
            UsageDisplay.capacity(window: "five_hour", in: pool, now: now)
                == .init(
                    window: "five_hour", name: "5-hour", value: "58% left", left: 58, level: .fine,
                    resetsAt: Self.base.addingTimeInterval(9_000)))
        #expect(
            UsageDisplay.capacity(window: "seven_day_opus", in: pool, now: now)
                == .init(
                    window: "seven_day_opus", name: "Weekly Opus", value: "Used up", left: 0, level: .usedUp,
                    resetsAt: Self.base.addingTimeInterval(172_800)))
    }

    @Test func aWindowIsLowAtFifteenLeftAndNotAtSixteen() throws {
        var pool = try emptyPool()
        pool.windows = [reading("seven_day", percent: 85), reading("five_hour", percent: 84)]
        let low = UsageDisplay.capacity(window: "seven_day", in: pool, now: Self.base)
        #expect(low.level == .low)
        #expect(low.value == "15% left")
        let fine = UsageDisplay.capacity(window: "five_hour", in: pool, now: Self.base)
        #expect(fine.level == .fine)
        #expect(fine.left == 16)
    }

    /// Nothing is left in any unit once a window is used up, so it takes the
    /// empty meter whether its reading has a percent, a quantity, or nothing.
    @Test func anExhaustedStatusOrALiveMarkIsUsedUpWithNothingLeft() throws {
        var pool = try emptyPool()
        pool.windows = [reading("five_hour", status: .exhausted)]
        pool.recorded_exhausted_marks = [mark("seven_day_opus", resetsAt: Self.base.addingTimeInterval(600))]
        for window in ["five_hour", "seven_day_opus"] {
            let capacity = UsageDisplay.capacity(window: window, in: pool, now: Self.base)
            #expect(capacity.level == .usedUp, "\(window)")
            #expect(capacity.left == 0, "\(window)")
            #expect(capacity.value == "Used up", "\(window)")
        }
        // The mark-only window resets when its mark does.
        #expect(
            UsageDisplay.capacity(window: "seven_day_opus", in: pool, now: Self.base).resetsAt
                == Self.base.addingTimeInterval(600))
        // A quantity reading with a live mark is used up too.
        pool.windows = [reading("credits", quantity: 1_200, unit: "credits")]
        pool.recorded_exhausted_marks = [mark("credits", resetsAt: nil)]
        #expect(UsageDisplay.capacity(window: "credits", in: pool, now: Self.base).left == 0)
    }

    @Test func aReadingWithNoPercentDrawsItsValueAndNoMeter() throws {
        var pool = try emptyPool()
        pool.windows = [reading("credits", quantity: 1_200, unit: "credits"), reading("five_hour")]
        let quantity = UsageDisplay.capacity(window: "credits", in: pool, now: Self.base)
        #expect(quantity.value == "1200 credits")
        #expect(quantity.left == nil)
        #expect(quantity.level == .fine)
        #expect(quantity.nameIsWindowID)
        let status = UsageDisplay.capacity(window: "five_hour", in: pool, now: Self.base)
        #expect(status.value == "Allowed")
        #expect(status.left == nil)
        #expect(!status.nameIsWindowID)
    }

    @Test func aWindowWhoseMarksHaveAllLapsedHasNoReading() throws {
        var pool = try emptyPool()
        let reset = Self.base.addingTimeInterval(600)
        pool.recorded_exhausted_marks = [mark("seven_day_opus", resetsAt: reset)]
        #expect(
            UsageDisplay.capacity(window: "seven_day_opus", in: pool, now: reset)
                == .init(
                    window: "seven_day_opus", name: "Weekly Opus", value: "No reading", left: nil,
                    level: .noReading, resetsAt: nil))
    }

    /// A used-up window has room again only once its last live cause lapses.
    @Test func aUsedUpWindowResetsWithItsLastLiveCause() throws {
        var pool = try emptyPool()
        // A live mark makes the reading used up, and a reading with no reset
        // of its own takes the mark's.
        pool.windows = [reading("seven_day", percent: 40)]
        pool.recorded_exhausted_marks = [mark("seven_day", resetsAt: Self.base.addingTimeInterval(1_200))]
        #expect(
            UsageDisplay.capacity(window: "seven_day", in: pool, now: Self.base).resetsAt
                == Self.base.addingTimeInterval(1_200))
        // An exhausted reading that outlasts the mark sets the reset.
        pool.windows[0].status = .exhausted
        pool.windows[0].resets_at = Self.base.addingTimeInterval(3_600)
        #expect(
            UsageDisplay.capacity(window: "seven_day", in: pool, now: Self.base).resetsAt
                == Self.base.addingTimeInterval(3_600))
        // A lapsed mark is no cause.
        pool.recorded_exhausted_marks.append(mark("seven_day", resetsAt: Self.base.addingTimeInterval(7_200)))
        #expect(
            UsageDisplay.capacity(window: "seven_day", in: pool, now: Self.base.addingTimeInterval(7_200)).resetsAt
                == Self.base.addingTimeInterval(3_600))
        // A live cause with no reset leaves none to state.
        pool.recorded_exhausted_marks.append(mark("seven_day", resetsAt: nil))
        #expect(UsageDisplay.capacity(window: "seven_day", in: pool, now: Self.base).resetsAt == nil)
    }

    @Test func theResetLineCountsDownThenNamesTheWeekdayThenTheDate() {
        let now = Self.base  // Wed Aug 12 2026, 02:40 UTC
        func line(_ offset: TimeInterval) -> String {
            UsageDisplay.resetLine(
                to: now.addingTimeInterval(offset), now: now, locale: Locale(identifier: "en_US"), timeZone: .gmt
            ).replacing("\u{202F}", with: " ")
        }
        #expect(line(41 * 60) == "Resets in 41m")
        #expect(line(86_400 - 60) == "Resets in 23h 59m")
        #expect(line(86_400) == "Resets Thu 2:40 AM")
        #expect(line(6 * 86_400) == "Resets Tue 2:40 AM")
        // Today's weekday a week on would read as earlier today.
        #expect(line(7 * 86_400 - 60) == "Resets Aug 19, 2:39 AM")
        #expect(line(7 * 86_400) == "Resets Aug 19, 2:40 AM")
        #expect(line(0) == "Resets passed")
    }

    @Test func staleSinceIsTheEarliestStaleAtTheClockHasPassed() throws {
        var pool = try emptyPool()
        pool.windows = [
            reading("seven_day", percent: 10, staleAt: Self.base.addingTimeInterval(120)),
            reading("five_hour", percent: 10, staleAt: Self.base.addingTimeInterval(60)),
            reading("credits", quantity: 5, unit: "credits"),
        ]
        #expect(UsageDisplay.staleSince(pool, now: Self.base) == nil)
        #expect(
            UsageDisplay.staleSince(pool, now: Self.base.addingTimeInterval(60)) == Self.base.addingTimeInterval(60))
        #expect(UsageDisplay.staleSince(pool, now: .distantFuture) == Self.base.addingTimeInterval(60))
    }

    @Test func theAttemptLineSaysWhatTheCollectorLastDidUnlessTheReadingsAreStale() throws {
        var pool = try emptyPool()
        let at = Self.base.addingTimeInterval(-3_600)  // Wed Aug 12, 1:40 AM UTC
        func line(now: Date = Self.base) -> UsageDisplay.AttemptLine {
            let line = UsageDisplay.attemptLine(pool, now: now, locale: Locale(identifier: "en_US"), timeZone: .gmt)
            return .init(text: line.text.replacing("\u{202F}", with: " "), isWax: line.isWax, instant: line.instant)
        }
        pool.last_attempt = nil
        #expect(line() == .init(text: "no reading", isWax: false, instant: nil))
        pool.last_attempt = .init(value1: .init(status: .succeeded, at: at, detail: ""))
        #expect(line() == .init(text: "read Aug 12, 1:40 AM", isWax: false, instant: at))
        pool.last_attempt = .init(value1: .init(status: .failed, at: at, detail: "timed out"))
        #expect(line() == .init(text: "refresh failed Aug 12, 1:40 AM", isWax: true, instant: at))
        pool.last_attempt = .init(value1: .init(status: .unsupported, at: at, detail: ""))
        #expect(line() == .init(text: "unsupported Aug 12, 1:40 AM", isWax: true, instant: at))
        // A stale reading outranks the attempt.
        pool.windows = [reading("seven_day", percent: 10, staleAt: Self.base)]
        #expect(line() == .init(text: "stale since Aug 12, 2:40 AM", isWax: true, instant: Self.base))
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
