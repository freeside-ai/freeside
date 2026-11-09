import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// #2494: the committed agent tree's catalog reaches the client through the
/// bootstrap alone, follows a daemon restart on a changed tree through the
/// heartbeat's lineup revision, survives relaunch, and dies with its epoch.
@MainActor
struct AgentCatalogSyncTests {
    private func makeCoordinator(
        server: MockServer, cache: CacheStore = InMemoryCacheStore()
    ) -> SyncCoordinator {
        SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache)
    }

    /// The default catalog as a daemon restarted on a changed tree serves it:
    /// a new revision and a reviewer moved to another selection.
    private static func changedCatalog() -> Components.Schemas.AgentCatalog {
        var catalog = UsageFixtures.defaultAgentCatalog()
        catalog.lineup_revision = .init(value1: "sha256:" + String(repeating: "e", count: 64))
        catalog.lineup = catalog.lineup.map {
            $0.role == .reviewer
                ? .init(role: .reviewer, selection: UsageFixtures.agentSelection("claude-secondary"))
                : $0
        }
        return catalog
    }

    @Test func bootstrapAdoptsTheCatalog() async throws {
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        await coordinator.bootstrap()
        #expect(coordinator.agentCatalog == UsageFixtures.defaultAgentCatalog())
        #expect(cache.load()?.agentCatalog == UsageFixtures.defaultAgentCatalog())
    }

    @Test func aDaemonWithoutATreeAdoptsATreelessCatalogAndNoBootstrapLoop() async throws {
        let server = MockServer(agentCatalog: UsageFixtures.treelessAgentCatalog())
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        // The catalog still lists the enrollments; it has no tree half.
        #expect(coordinator.agentCatalog == UsageFixtures.treelessAgentCatalog())
        #expect(coordinator.agentCatalog?.lineup_revision == nil)

        let operations = OperationLog()
        await server.setBeforeRespond { await operations.append($0) }
        await coordinator.heartbeat()
        #expect(await !operations.ids.contains("getSyncBootstrap"))
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func aRestartOnAChangedTreeIsAdoptedAtTheSameRevision() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        let initial = try #require(coordinator.cursors)

        // An unchanged tree: the heartbeat does not bootstrap.
        let operations = OperationLog()
        await server.setBeforeRespond { await operations.append($0) }
        await coordinator.heartbeat()
        #expect(await !operations.ids.contains("getSyncBootstrap"))
        await server.setBeforeRespond(nil)

        let changed = Self.changedCatalog()
        await server.setAgentCatalog(changed)
        await coordinator.heartbeat()

        #expect(coordinator.agentCatalog == changed)
        #expect(coordinator.cursors == initial)
        #expect(coordinator.store.freshness == .fresh)
        // Heartbeats never write the cache: the new catalog there proves a
        // bootstrap was adopted at the unchanged revision.
        #expect(cache.load()?.agentCatalog == changed)
    }

    @Test func relaunchRestoresTheCatalog() async throws {
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        await coordinator.bootstrap()

        let relaunched = makeCoordinator(server: MockServer(), cache: cache)
        #expect(relaunched.agentCatalog == UsageFixtures.defaultAgentCatalog())
        #expect(relaunched.store.freshness == .unvalidated)
    }

    @Test func aCacheFromBeforeTheCatalogBootstrapsAtTheSameRevision() async throws {
        // A cache written before the catalog joined restores its cursors and
        // rows with no catalog; the heartbeat's lineup revision differs from
        // the absent one, which triggers the bootstrap that fills it.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        await makeCoordinator(server: server, cache: cache).bootstrap()
        var legacy = try #require(cache.load())
        legacy.agentCatalog = nil
        try cache.save(legacy)

        let relaunched = makeCoordinator(server: server, cache: cache)
        #expect(relaunched.agentCatalog == nil)
        let cursors = try #require(relaunched.cursors)
        await relaunched.heartbeat()

        #expect(relaunched.agentCatalog == UsageFixtures.defaultAgentCatalog())
        #expect(relaunched.cursors == cursors)
        #expect(relaunched.store.freshness == .fresh)
    }

    @Test func aNewEpochDropsTheCatalog() async throws {
        // The catalog is scoped by the cursors like every other row: an epoch
        // discard that fails to re-bootstrap leaves no catalog to restore.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        #expect(coordinator.agentCatalog != nil)

        await server.rotateEpoch()
        await server.setBeforeRespond { operationID in
            if operationID == "getSyncBootstrap" { throw MockOutage() }
        }
        await coordinator.heartbeat()

        #expect(coordinator.cursors == nil)
        #expect(coordinator.agentCatalog == nil)
        #expect(makeCoordinator(server: server, cache: cache).agentCatalog == nil)
    }
}

private actor OperationLog {
    private(set) var ids: [String] = []

    func append(_ id: String) {
        ids.append(id)
    }
}

private struct MockOutage: Error {}
