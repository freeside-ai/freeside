import Foundation
import FreesideAPI
import FreesideCore
import Testing

@MainActor
private func makeCoordinator(
    server: MockServer, cache: CacheStore = InMemoryCacheStore()
) -> SyncCoordinator {
    SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache)
}

/// The client half of plan §5.14's cursor and freshness semantics,
/// against the mock daemon.
@Suite @MainActor struct SyncCoordinatorTests {
    @Test func bootstrapSetsBothCursorsAndPersistsTheCache() async throws {
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        #expect(coordinator.store.freshness == .unvalidated)

        await coordinator.bootstrap()

        let cursors = try #require(coordinator.cursors)
        #expect(cursors.lastFullSnapshotRevision == cursors.highestObservedServerRevision)
        #expect(coordinator.store.rows.count == AttentionFixtures.phase1Types.count)
        #expect(coordinator.store.freshness == .fresh)
        let persisted = try #require(cache.load())
        #expect(persisted.cursors == cursors)
        #expect(persisted.attentionItems.count == coordinator.store.rows.count)
    }

    @Test func partialRefetchAdvancesOnlyTheObservedCursor() async throws {
        // Test 11, client half: a concurrent write refetched item-by-item
        // must not mark the whole cache current; the heartbeat then finds
        // the gap and only the bootstrap closes it.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)

        await server.advance(itemID: "item-spec_approval")
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()

        let partial = try #require(coordinator.cursors)
        #expect(partial.lastFullSnapshotRevision == before.lastFullSnapshotRevision)
        #expect(partial.highestObservedServerRevision > partial.lastFullSnapshotRevision)

        await coordinator.heartbeat()

        let converged = try #require(coordinator.cursors)
        #expect(converged.lastFullSnapshotRevision == converged.highestObservedServerRevision)
        #expect(converged.lastFullSnapshotRevision > before.lastFullSnapshotRevision)
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func epochChangeDiscardsTheCacheAndBootstraps() async throws {
        // Test 8, client half: a restored daemon issues a new epoch; the
        // client discards its cache and cursors — even though they sit
        // ahead of the restored revision — and bootstraps fresh.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        await server.advance(itemID: "item-spec_approval")
        await coordinator.heartbeat()
        let before = try #require(coordinator.cursors)

        await server.rotateEpoch(revision: 1)
        await coordinator.heartbeat()

        let after = try #require(coordinator.cursors)
        #expect(after.syncEpoch != before.syncEpoch)
        // The dead epoch's cursors are gone, not compared: the new pair
        // adopts the restored revision even though it runs behind.
        #expect(after.lastFullSnapshotRevision < before.lastFullSnapshotRevision)
        #expect(after.highestObservedServerRevision == after.lastFullSnapshotRevision)
        #expect(coordinator.store.freshness == .fresh)
        #expect(cache.load()?.cursors.syncEpoch == after.syncEpoch)
    }

    @Test func aDeadEpochIsEvictedEvenWhenTheRebootstrapFails() async throws {
        // §5.14 cache eviction on epoch change: the rows are dead the
        // moment the heartbeat reports a new epoch, so an outage during
        // the re-bootstrap must not keep rendering (or persisting)
        // pre-restore state.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        #expect(!coordinator.store.rows.isEmpty)

        await server.rotateEpoch()
        await server.setBeforeRespond { operationID in
            if operationID == "getSyncBootstrap" { throw MockOutage() }
        }
        await coordinator.heartbeat()

        #expect(coordinator.store.rows.isEmpty)
        #expect(coordinator.cursors == nil)
        #expect(cache.load() == nil)
        #expect(coordinator.store.freshness == .unreachable)

        await server.setBeforeRespond(nil)
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .fresh)
        #expect(!coordinator.store.rows.isEmpty)
    }

    @Test func launchingFromTheCacheRendersRowsWithoutClaimingFreshness() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let first = makeCoordinator(server: server, cache: cache)
        await first.bootstrap()

        // A new session over the same cache: rows render before any
        // network call, but nothing claims they are current.
        let second = makeCoordinator(server: server, cache: cache)
        #expect(second.store.rows.count == first.store.rows.count)
        #expect(second.cursors == first.cursors)
        #expect(second.store.freshness == .unvalidated)

        await second.heartbeat()
        #expect(second.store.freshness == .fresh)
    }

    @Test func unreachableDaemonDegradesToTheBannerAndRecovers() async throws {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let rows = coordinator.store.rows

        await server.setBeforeRespond { _ in throw MockOutage() }
        await coordinator.heartbeat()

        // The cached view survives; only the freshness claim changes.
        #expect(coordinator.store.freshness == .unreachable)
        #expect(coordinator.store.rows == rows)

        await server.setBeforeRespond(nil)
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func rejectedCredentialSurfacesAsUnauthenticated() async throws {
        // An enforcing server with no credential: every sync read is
        // 401, which is a distinct honest state (revoked or unpaired),
        // never a generic outage.
        let coordinator = makeCoordinator(server: MockServer(authMode: .enforcing))
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .unauthenticated)
    }

    @Test func aStaleBootstrapResponseNeverWinsOverANewerRound() async throws {
        // Refute-first finding: a bootstrap response held open across a
        // restore must not land late and win the cache back for the
        // dead epoch. Only the newest sync round may adopt.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)

        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setAfterRespond { operationID in
            if operationID == "getSyncBootstrap" {
                await reached.open()
                await release.wait()
            }
        }
        // The stale round: its epoch-1 snapshot is computed, its
        // response held open.
        let stale = Task { await coordinator.bootstrap() }
        await reached.wait()
        await server.setAfterRespond(nil)

        // The restore lands and a newer round adopts the new epoch.
        await server.rotateEpoch()
        await coordinator.bootstrap()
        let adopted = try #require(coordinator.cursors)

        await release.open()
        await stale.value

        #expect(coordinator.cursors == adopted)
        #expect(cache.load()?.cursors == adopted)
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func resolveOnOneDeviceConvergesTheOther() async throws {
        // Test 1, client half: device A resolves; device B's heartbeat
        // finds the gap and its bootstrap converges on the same state.
        let server = MockServer()
        let deviceA = makeCoordinator(server: server)
        let deviceB = makeCoordinator(server: server)
        await deviceA.bootstrap()
        await deviceB.bootstrap()

        let model = DecisionModel(store: deviceA.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        #expect(deviceA.store.snapshotsByID["item-spec_approval"]?.item.status == .resolved)
        #expect(deviceB.store.snapshotsByID["item-spec_approval"]?.item.status == .open)

        await deviceB.heartbeat()

        let converged = try #require(deviceB.store.snapshotsByID["item-spec_approval"])
        #expect(converged == deviceA.store.snapshotsByID["item-spec_approval"])
        // B is fully current again; A's own full-snapshot cursor lags by
        // design until its next heartbeat, its partial read having
        // advanced only the observed cursor.
        let cursorsB = try #require(deviceB.cursors)
        #expect(cursorsB.lastFullSnapshotRevision == cursorsB.highestObservedServerRevision)
        #expect(
            cursorsB.highestObservedServerRevision
                == deviceA.cursors?.highestObservedServerRevision)
    }

    @Test func staleSecondDeviceSubmissionRendersTheReplacement() async throws {
        // Test 2, client half: device B validated while the item was
        // open, device A then resolved it, and B's submission against
        // the superseded version is rejected with the replacement item
        // rendered — never applied, never an error dead-end.
        let server = MockServer()
        let deviceA = makeCoordinator(server: server)
        let deviceB = makeCoordinator(server: server)
        await deviceA.bootstrap()
        await deviceB.bootstrap()

        let modelB = DecisionModel(store: deviceB.store, itemID: "item-spec_approval")
        await modelB.validate()
        #expect(modelB.actionsEnabled)

        let modelA = DecisionModel(store: deviceA.store, itemID: "item-spec_approval")
        await modelA.validate()
        await modelA.submit(.approve)

        // stop is a concluding action this unit can submit; the point is
        // the version binding, not which decision B picked.
        await modelB.submit(.stop)

        #expect(modelB.phase == .superseded)
        let replacement = try #require(deviceB.store.snapshotsByID["item-spec_approval"])
        #expect(replacement.item.status == .resolved)
        #expect(replacement == deviceA.store.snapshotsByID["item-spec_approval"])
    }
}

private struct MockOutage: Error {}
