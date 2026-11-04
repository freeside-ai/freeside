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
}
