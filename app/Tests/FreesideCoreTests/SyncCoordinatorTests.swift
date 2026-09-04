import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@MainActor
private func makeCoordinator(
    server: MockServer, cache: CacheStore = InMemoryCacheStore()
) -> SyncCoordinator {
    SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache)
}

/// A cache whose saves fail on demand, to drive the durable-persistence
/// submission precondition (#163). Loads and discards delegate to an
/// in-memory backing, so whatever did persist is still visible to a
/// relaunch; `failSaves` can be flipped mid-test to model a disk that
/// recovers.
private final class FailingCacheStore: CacheStore, @unchecked Sendable {
    struct SaveRefused: Error {}

    private let backing = InMemoryCacheStore()
    private let lock = NSLock()
    private var _failSaves: Bool

    init(failSaves: Bool) { _failSaves = failSaves }

    var failSaves: Bool {
        get { lock.withLock { _failSaves } }
        set { lock.withLock { _failSaves = newValue } }
    }

    func load() -> CachedState? { backing.load() }

    func save(_ state: CachedState) throws {
        if failSaves { throw SaveRefused() }
        try backing.save(state)
    }

    func discard() { backing.discard() }
}

private final class CountingCacheStore: CacheStore, @unchecked Sendable {
    private let backing = InMemoryCacheStore()
    private let lock = NSLock()
    private var _saveCount = 0

    var saveCount: Int { lock.withLock { _saveCount } }

    func load() -> CachedState? { backing.load() }

    func save(_ state: CachedState) throws {
        lock.withLock { _saveCount += 1 }
        try backing.save(state)
    }

    func discard() { backing.discard() }
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
        #expect(
            coordinator.store.orderedConversations == AttentionFixtures.defaultConversations())
        #expect(coordinator.store.freshness == .fresh)
        let persisted = try #require(cache.load())
        #expect(persisted.cursors == cursors)
        #expect(persisted.attentionItems.count == coordinator.store.rows.count)
        #expect(persisted.conversations == coordinator.store.orderedConversations)
        #expect(persisted.runs == coordinator.runs)
        #expect(persisted.schedules == coordinator.schedules)
    }

    @Test func malformedBootstrapConversationFailsClosedBeforeCacheAdoption() async {
        var malformed = AttentionFixtures.defaultConversations()[0]
        malformed.conversation.messages[0].conversation_id = "conv-other"
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(
            server: MockServer(conversations: [malformed]), cache: cache)

        await coordinator.bootstrap()

        #expect(coordinator.cursors == nil)
        #expect(coordinator.store.rows.isEmpty)
        #expect(coordinator.store.conversationsByID.isEmpty)
        #expect(coordinator.store.freshness == .syncFailing)
        #expect(cache.load() == nil)
    }

    @Test func bootstrapConversationBeyondItsRevisionFailsClosed() async {
        let server = MockServer()
        await server.setBootstrapTransform { snapshot in
            var malformed = snapshot
            malformed.conversations[0].as_of_revision = snapshot.revision + 1
            return malformed
        }
        let coordinator = makeCoordinator(server: server)

        await coordinator.bootstrap()

        #expect(coordinator.cursors == nil)
        #expect(coordinator.store.rows.isEmpty)
        #expect(coordinator.store.conversationsByID.isEmpty)
        #expect(coordinator.store.freshness == .syncFailing)
    }

    @Test func malformedCachedConversationIsNotRestoredOnRelaunch() throws {
        var malformed = AttentionFixtures.defaultConversations()[0]
        malformed.conversation.messages[1].sequence = 4
        let cache = InMemoryCacheStore()
        try cache.save(
            CachedState(
                cursors: .init(
                    syncEpoch: "mock-epoch", lastFullSnapshotRevision: 1,
                    highestObservedServerRevision: 1),
                attentionItems: AttentionFixtures.defaultInbox(),
                conversations: [malformed]))

        let coordinator = makeCoordinator(server: MockServer(), cache: cache)

        #expect(coordinator.cursors == nil)
        #expect(coordinator.store.rows.isEmpty)
        #expect(coordinator.store.conversationsByID.isEmpty)
        #expect(coordinator.store.freshness == .unvalidated)
    }

    @Test func cachedConversationBeyondItsCursorIsNotRestoredOnRelaunch() throws {
        var future = AttentionFixtures.defaultConversations()[0]
        future.as_of_revision = 2
        let cache = InMemoryCacheStore()
        try cache.save(
            CachedState(
                cursors: .init(
                    syncEpoch: "mock-epoch", lastFullSnapshotRevision: 1,
                    highestObservedServerRevision: 1),
                attentionItems: AttentionFixtures.defaultInbox(),
                conversations: [future]))

        let coordinator = makeCoordinator(server: MockServer(), cache: cache)

        #expect(coordinator.cursors == nil)
        #expect(coordinator.store.rows.isEmpty)
        #expect(coordinator.store.conversationsByID.isEmpty)
        #expect(coordinator.store.freshness == .unvalidated)
    }

    @Test func sameRevisionConversationUpdatePersistsForRelaunch() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        let model = DecisionModel(
            store: coordinator.store, itemID: "item-spec_approval", onConclusion: { _ in })
        await model.validate()

        await model.submitDiscuss(message: "Preserve this acknowledged message.")

        let relaunched = makeCoordinator(server: server, cache: cache)
        let item = try #require(relaunched.store.snapshotsByID["item-spec_approval"])
        let conversation = try #require(relaunched.store.conversation(for: item.item))
        #expect(conversation.conversation.status == .awaiting_agent)
        #expect(conversation.conversation.messages.last?.author == .user)
        #expect(conversation.conversation.messages.last?.body == "Preserve this acknowledged message.")
    }

    @Test func unchangedHeartbeatDoesNotRewriteTheCache() async {
        let cache = CountingCacheStore()
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        await coordinator.bootstrap()
        let savesAfterBootstrap = cache.saveCount

        await coordinator.heartbeat()

        #expect(savesAfterBootstrap == 1)
        #expect(cache.saveCount == savesAfterBootstrap)
    }

    @Test func runAndTimelinePartialReadsDoNotMarkTheCacheCurrent() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)

        await server.advanceRun(id: RunFixtures.activeRunID)
        await coordinator.refreshRuns()
        await coordinator.refreshTimeline(for: RunFixtures.activeRunID)

        let partial = try #require(coordinator.cursors)
        #expect(partial.lastFullSnapshotRevision == before.lastFullSnapshotRevision)
        #expect(partial.highestObservedServerRevision > partial.lastFullSnapshotRevision)
        #expect(coordinator.store.freshness == .unvalidated)
        #expect(coordinator.timelinesByRunID[RunFixtures.activeRunID] != nil)
        #expect(coordinator.timelineLoadStates[RunFixtures.activeRunID] == .loaded)
        #expect(cache.load()?.runTimelines.map(\.run_id) == [RunFixtures.activeRunID])

        await coordinator.heartbeat()
        let converged = try #require(coordinator.cursors)
        #expect(converged.lastFullSnapshotRevision == converged.highestObservedServerRevision)
    }

    @Test func emptyRunListBootstrapsAfterTheServerRevisionAdvances() async throws {
        let server = MockServer(runs: [])
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)

        await server.advance(itemID: AttentionFixtures.defaultInbox()[0].item.id)
        await coordinator.refreshRuns()

        let after = try #require(coordinator.cursors)
        #expect(after.lastFullSnapshotRevision > before.lastFullSnapshotRevision)
        #expect(after.highestObservedServerRevision > before.highestObservedServerRevision)
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func timelineNotFoundStopsLoading() async {
        let server = MockServer(timelines: [])
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()

        await coordinator.refreshTimeline(for: RunFixtures.activeRunID)

        #expect(coordinator.timelinesByRunID[RunFixtures.activeRunID] == nil)
        #expect(coordinator.timelineLoadStates[RunFixtures.activeRunID] == .unavailable)
    }

    @Test func timelineResponseInFlightAcrossEpochChangeIsDiscarded() async {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "getRunTimeline" {
                await reached.open()
                await release.wait()
            }
        }

        let refresh = Task { await coordinator.refreshTimeline(for: RunFixtures.activeRunID) }
        await reached.wait()
        await server.rotateEpoch(revision: 1)
        await coordinator.heartbeat()
        await release.open()
        await refresh.value

        #expect(coordinator.timelinesByRunID.isEmpty)
    }

    @Test func timelineResponseMayFinishAcrossOrdinaryHeartbeat() async {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "getRunTimeline" {
                await reached.open()
                await release.wait()
            }
        }

        let refresh = Task { await coordinator.refreshTimeline(for: RunFixtures.activeRunID) }
        await reached.wait()
        await coordinator.heartbeat()
        await release.open()
        await refresh.value

        #expect(coordinator.timelinesByRunID[RunFixtures.activeRunID] != nil)
        #expect(coordinator.timelineLoadStates[RunFixtures.activeRunID] == .loaded)
    }

    @Test func staleRunFailureCannotOverwriteReplacementFreshness() async {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "listRuns" {
                await reached.open()
                await release.wait()
                throw InjectedFailure()
            }
        }

        let refresh = Task { await coordinator.refreshRuns() }
        await reached.wait()
        await server.rotateEpoch(revision: 1)
        await coordinator.heartbeat()
        await release.open()
        await refresh.value

        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func canceledTimelineRequestIsNotAnOutage() async {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let reached = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "getRunTimeline" {
                await reached.open()
                try await Task.sleep(for: .seconds(30))
            }
        }

        let refresh = Task { await coordinator.refreshTimeline(for: RunFixtures.activeRunID) }
        await reached.wait()
        refresh.cancel()
        await refresh.value

        #expect(coordinator.store.freshness == .fresh)
        #expect(coordinator.timelineLoadStates[RunFixtures.activeRunID] == .idle)
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
        #expect(cache.load()?.cursors?.syncEpoch == after.syncEpoch)
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
        #expect(coordinator.runs.isEmpty)
        #expect(coordinator.schedules.isEmpty)
        #expect(coordinator.timelinesByRunID.isEmpty)
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

    @Test func reachableDaemonWithFailingReadsSurfacesSyncFailing() async throws {
        // A non-401 answered failure on any of the four sync reads is a
        // reachable-but-failing daemon: a state distinct from unreachable
        // (which is a transport outage, still asserted separately above),
        // keeping the cache readable and recovering to fresh on the next
        // good round.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let rows = coordinator.store.rows

        func force(_ operationID: String) async {
            await server.setBeforeRespond { op in
                if op == operationID { throw MockServer.ForcedStatus(500) }
            }
        }
        func recover() async {
            await server.setBeforeRespond(nil)
            await coordinator.heartbeat()
            #expect(coordinator.store.freshness == .fresh)
        }

        await force("getSyncBootstrap")
        await coordinator.bootstrap()
        #expect(coordinator.store.freshness == .syncFailing)
        #expect(coordinator.store.rows == rows)
        await recover()

        await force("getSyncRevision")
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .syncFailing)
        await recover()

        await force("listRuns")
        await coordinator.refreshRuns()
        #expect(coordinator.store.freshness == .syncFailing)
        await recover()

        await force("getRunTimeline")
        await coordinator.refreshTimeline(for: RunFixtures.activeRunID)
        #expect(coordinator.store.freshness == .syncFailing)
        #expect(coordinator.timelineLoadStates[RunFixtures.activeRunID] == .unavailable)
        await recover()
    }

    @Test func answered200WithUndecodableBodySurfacesSyncFailing() async throws {
        // Schema skew: the daemon answers 200 but the body does not match
        // this client's schema, so `ok.body.json` throws. That is an
        // answered-but-failing read, not a transport outage, and must
        // reach syncFailing on every path rather than falling into the
        // transport catch. ForcedStatus(200) pairs a 200 with an
        // error-shaped body the sync/run schemas cannot decode.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let rows = coordinator.store.rows

        func force(_ operationID: String) async {
            await server.setBeforeRespond { op in
                if op == operationID { throw MockServer.ForcedStatus(200) }
            }
        }
        func recover() async {
            await server.setBeforeRespond(nil)
            await coordinator.heartbeat()
            #expect(coordinator.store.freshness == .fresh)
        }

        await force("getSyncBootstrap")
        await coordinator.bootstrap()
        #expect(coordinator.store.freshness == .syncFailing)
        #expect(coordinator.store.rows == rows)
        await recover()

        await force("getSyncRevision")
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .syncFailing)
        await recover()

        await force("listRuns")
        await coordinator.refreshRuns()
        #expect(coordinator.store.freshness == .syncFailing)
        await recover()

        await force("getRunTimeline")
        await coordinator.refreshTimeline(for: RunFixtures.activeRunID)
        #expect(coordinator.store.freshness == .syncFailing)
        #expect(coordinator.timelineLoadStates[RunFixtures.activeRunID] == .unavailable)
        await recover()
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

    @Test func aBootstrapOlderThanAPartialReadRefetchesBeforeAdopting() async throws {
        // A bootstrap is canonical only at the revision it read. If a
        // same-epoch partial run read reaches a later revision while that
        // response is delayed, the old bootstrap must not replace it and call
        // the cache fresh.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)

        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setAfterRespond { operationID in
            if operationID == "getSyncBootstrap" {
                await reached.open()
                await release.wait()
            }
        }
        let stale = Task { await coordinator.bootstrap() }
        await reached.wait()
        await server.setAfterRespond(nil)

        await server.advanceRun(id: RunFixtures.activeRunID)
        await coordinator.refreshRuns()
        let partial = try #require(coordinator.cursors)
        #expect(partial.highestObservedServerRevision > before.lastFullSnapshotRevision)

        await release.open()
        await stale.value

        let adopted = try #require(coordinator.cursors)
        #expect(adopted.lastFullSnapshotRevision == adopted.highestObservedServerRevision)
        #expect(adopted.lastFullSnapshotRevision == partial.highestObservedServerRevision)
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func aHeartbeatOlderThanAPartialReadBootstrapsBeforeFreshness() async throws {
        // A heartbeat's server revision is only the value it captured. If a
        // partial read reaches a newer revision before that response returns,
        // the heartbeat must inspect the live cursor and bootstrap instead of
        // restoring a false .fresh state.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)

        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setAfterRespond { operationID in
            if operationID == "getSyncRevision" {
                await reached.open()
                await release.wait()
            }
        }
        let heartbeat = Task { await coordinator.heartbeat() }
        await reached.wait()
        await server.setAfterRespond(nil)

        await server.advanceRun(id: RunFixtures.activeRunID)
        await coordinator.refreshRuns()
        let partial = try #require(coordinator.cursors)
        #expect(partial.highestObservedServerRevision > before.lastFullSnapshotRevision)

        await release.open()
        await heartbeat.value

        let converged = try #require(coordinator.cursors)
        #expect(converged.lastFullSnapshotRevision == converged.highestObservedServerRevision)
        #expect(converged.lastFullSnapshotRevision == partial.highestObservedServerRevision)
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

    @Test func anUnresolvedCommandSurvivesRelaunch() async throws {
        // #115, §5.14 test 4 across a restart: a command whose response
        // was lost keeps its retry affordance through a relaunch, and
        // the restored slot still blocks a second command for the item.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let first = makeCoordinator(server: server, cache: cache)
        await first.bootstrap()

        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        let model = DecisionModel(store: first.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        let entry = try #require(first.store.pendingCommandsByItemID["item-spec_approval"])
        #expect(entry.state == .unresolved)

        let second = makeCoordinator(server: server, cache: cache)
        let restored = try #require(
            second.store.pendingCommandsByItemID["item-spec_approval"])
        #expect(restored.command == entry.command)
        #expect(restored.state == .unresolved)
        #expect(
            second.store.registerPendingCommand(
                makeCommand(itemID: "item-spec_approval", commandID: "cmd-duplicate"))
                == .slotOccupied)
    }

    @Test func anInFlightEntryRestoresAsUnresolved() async throws {
        // A command persisted mid-flight has failed ambiguously by the
        // time a relaunch reads it: no task awaits its response, so only
        // the unresolved state (the retry affordance) is honest.
        let cache = InMemoryCacheStore()
        let first = makeCoordinator(server: MockServer(), cache: cache)
        #expect(first.store.registerPendingCommand(makeCommand(itemID: "item-x")) == .registered)
        #expect(cache.load()?.pendingCommands?["item-x"]?.state == .inFlight)

        let second = makeCoordinator(server: MockServer(), cache: cache)
        #expect(second.store.pendingCommandsByItemID["item-x"]?.state == .unresolved)
    }

    @Test func restoreDropsEntriesTheReGateRejects() async throws {
        // Decoded ledger fields are re-gated, never trusted (Codex P2 on
        // #125): another device's command must not occupy this device's
        // slots — after a re-pair its verbatim resend would die at the
        // daemon's device gate and clear a possibly committed outcome —
        // and a key naming a different item than its command must not
        // block that item. Only the consistent same-device entry lands.
        let cache = InMemoryCacheStore()
        try cache.save(
            CachedState(
                cursors: nil,
                attentionItems: [],
                pendingCommands: [
                    "item-mine": .init(
                        command: makeCommand(itemID: "item-mine"), state: .unresolved),
                    "item-foreign": .init(
                        command: makeCommand(
                            itemID: "item-foreign", commandID: "cmd-foreign",
                            deviceID: "device-old"),
                        state: .unresolved),
                    "item-mismatched": .init(
                        command: makeCommand(
                            itemID: "item-other", commandID: "cmd-mismatched"),
                        state: .unresolved),
                ]))
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)

        #expect(coordinator.store.pendingCommandsByItemID.count == 1)
        #expect(coordinator.store.pendingCommandsByItemID["item-mine"] != nil)
    }

    @Test func aRestoredRetryReplaysTheRecordedResult() async throws {
        // #115 acceptance 2, recorded-result branch: the command
        // committed, its response was lost, the app restarted. The
        // restored verbatim resend is served the recorded result — no
        // second side effect — and the slot clears.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let first = makeCoordinator(server: server, cache: cache)
        await first.bootstrap()

        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        let model = DecisionModel(store: first.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        let lost = try #require(first.store.pendingCommandsByItemID["item-spec_approval"])
        await server.setAfterRespond(nil)

        let second = makeCoordinator(server: server, cache: cache)
        await second.bootstrap()
        let restored = DecisionModel(store: second.store, itemID: "item-spec_approval")
        await restored.validate()
        #expect(restored.canRetryLostResponse)

        await restored.retryLostResponse()

        #expect(restored.appliedRecord?.command_id == lost.command.command_id)
        #expect(second.store.pendingCommandsByItemID["item-spec_approval"] == nil)
        #expect(
            second.store.snapshotsByID["item-spec_approval"]?.item.status == .resolved)
    }

    @Test func aRestoredRetryAcceptsAuthoritativeRejection() async throws {
        // #115 acceptance 2, rejection branch: a restored command the
        // daemon never recorded, for an item it does not know, draws an
        // authoritative rejection on resend and the slot clears — the
        // decision was definitively not recorded, nothing to recover.
        let cache = InMemoryCacheStore()
        try cache.save(
            CachedState(
                cursors: nil,
                attentionItems: [],
                pendingCommands: [
                    "item-ghost": .init(
                        command: makeCommand(itemID: "item-ghost"), state: .unresolved)
                ]))
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        let model = DecisionModel(store: coordinator.store, itemID: "item-ghost")
        #expect(model.canRetryLostResponse)

        await model.retryLostResponse()

        #expect(coordinator.store.pendingCommandsByItemID["item-ghost"] == nil)
        #expect(model.submissionError != nil)
    }

    @Test func aHeartbeatEpochDiscardPreservesTheLedger() async throws {
        // #115 acceptance 4 on the eager path: the heartbeat's epoch
        // mismatch evicts rows and cursors immediately, but commitment
        // is epoch-independent — the ledger survives the eviction, the
        // persisted file, and a relaunch inside the outage window.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()

        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        #expect(coordinator.store.pendingCommandsByItemID["item-spec_approval"] != nil)
        await server.setAfterRespond(nil)

        await server.rotateEpoch()
        await server.setBeforeRespond { operationID in
            if operationID == "getSyncBootstrap" { throw MockOutage() }
        }
        await coordinator.heartbeat()

        #expect(coordinator.store.rows.isEmpty)
        #expect(coordinator.cursors == nil)
        let persisted = try #require(cache.load())
        #expect(persisted.cursors == nil)
        #expect(persisted.attentionItems.isEmpty)
        #expect(persisted.pendingCommands?["item-spec_approval"] != nil)

        let second = makeCoordinator(server: server, cache: cache)
        #expect(
            second.store.pendingCommandsByItemID["item-spec_approval"]?.state
                == .unresolved)
    }

    @Test func aBootstrapEpochDiscardPreservesTheLedger() async throws {
        // #115 acceptance 4 on the backstop path: an epoch change first
        // observed by a direct bootstrap discards and re-adopts in one
        // motion; the re-persisted cache carries the new cursors and the
        // surviving ledger together.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()

        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        await server.setAfterRespond(nil)

        await server.rotateEpoch()
        await coordinator.bootstrap()

        let persisted = try #require(cache.load())
        #expect(persisted.cursors?.syncEpoch == coordinator.cursors?.syncEpoch)
        #expect(persisted.pendingCommands?["item-spec_approval"] != nil)
        #expect(
            coordinator.store.pendingCommandsByItemID["item-spec_approval"] != nil)
    }

    @Test func clearingTheLastLedgerEntryAfterDiscardRemovesTheFile() async throws {
        // Once the surviving ledger settles with no cursors to keep, the
        // file goes too: keeping one would undo the epoch eviction.
        let cache = InMemoryCacheStore()
        let command = makeCommand(itemID: "item-x")
        try cache.save(
            CachedState(
                cursors: nil,
                attentionItems: [],
                pendingCommands: [
                    "item-x": .init(command: command, state: .unresolved)
                ]))
        let coordinator = makeCoordinator(server: MockServer(), cache: cache)
        #expect(coordinator.store.pendingCommandsByItemID["item-x"] != nil)

        coordinator.store.clearPendingCommand(
            itemID: "item-x", commandID: command.command_id)

        #expect(cache.load() == nil)
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

    @Test func anEpochEvictionInvalidatesAPriorValidation() async {
        // A card validated before a daemon restore must not stay enabled
        // once the heartbeat evicts the dead epoch and re-bootstraps the
        // rows: the pre-restore validation certified an epoch this card
        // never revalidated, so actions fail closed until it does (#162;
        // plan §5.14 cache eviction on epoch change).
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        // The daemon restores under a new epoch; the heartbeat discards
        // the cache and re-bootstraps the same open item.
        await server.rotateEpoch(revision: 1)
        await coordinator.heartbeat()

        #expect(coordinator.store.cacheGeneration > 0)
        #expect(model.snapshot?.item.status == .open)  // the row is back...
        #expect(!model.actionsEnabled)  // ...but not certified

        // A fresh validation against the new epoch re-enables actions.
        await model.validate()
        #expect(model.actionsEnabled)
    }

    @Test func aValidationEvictedMidFetchRefetchesInsteadOfCertifying() async {
        // Everything is @MainActor, so a heartbeat eviction can land while
        // validate()'s getAttentionItem is suspended at its await. The
        // response is then from a possibly dead epoch, so validate must
        // drop it and re-fetch against the current epoch rather than
        // applying and certifying it under the new generation (#162).
        //
        // The in-process mock computes each response at delivery time, so
        // it cannot hand back a genuinely stale-epoch body; what is
        // asserted here is that the eviction is detected and forces the
        // re-fetch (a second getAttentionItem), which is the mechanism that
        // protects against the stale-epoch response a real daemon can send.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")

        let reached = AsyncGate()
        let release = AsyncGate()
        let firstGet = OneShot()
        let getCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" {
                await getCalls.increment()
                if await firstGet.fire() {
                    await reached.open()
                    await release.wait()
                }
            }
        }
        let validation = Task { await model.validate() }
        await reached.wait()

        // Evict the cache for a new epoch while the fetch is in flight.
        coordinator.store.discardSnapshots()

        await release.open()
        await validation.value

        // The guard fired: a second fetch happened rather than certifying
        // the in-flight response, then the conversation-bearing item was
        // confirmed once more around its thread read before certification.
        #expect(await getCalls.count == 3)
        #expect(model.validation == .validated)
    }

    @Test func aPostCommitRefetchEvictedMidFetchRefetchesInsteadOfCertifying() async {
        // The read-your-write refetch after a successful submit is another
        // @MainActor await an eviction can land inside; like validate() it
        // must not apply/certify a response that resumed under a changed
        // cache generation (#162). Same mock caveat as the validate case:
        // the assertion is that the eviction forces a re-validate rather
        // than certifying the in-flight refetch.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()  // getAttentionItem #1
        #expect(model.actionsEnabled)

        // Installed after the initial validate(), so the first fetch this
        // hook sees is the post-commit read-your-write refetch; hold it.
        let reached = AsyncGate()
        let release = AsyncGate()
        let getCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "getAttentionItem" {
                if await getCalls.incrementAndGet() == 1 {
                    await reached.open()
                    await release.wait()
                }
            }
        }
        let submission = Task { await model.submit(.approve) }
        await reached.wait()

        // Evict the cache for a new epoch while the refetch is in flight.
        coordinator.store.discardSnapshots()

        await release.open()
        await submission.value

        // The guard fired: the .ok path recovered against the current
        // epoch (at least one further getAttentionItem) instead of
        // certifying the single in-flight refetch. (The exact count depends
        // on the recovery path — settleAmbiguousOutcome re-validates and
        // replays — so this pins the property, not a brittle count.)
        #expect(await getCalls.count >= 2)
    }

    @Test func aSuccessResultEvictedMidFlightIsTreatedAsAmbiguousNotCleared() async {
        // A 200 that resumes after a mid-flight eviction is from a possibly
        // rolled-back pre-restore epoch. Clearing the ledger then would drop
        // the retry state discardSnapshots() preserves, so the .ok arm must
        // treat it as ambiguous (keep the slot, replay) rather than applied
        // (#162). Observable via the replay: a second submitCommand runs.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        let reached = AsyncGate()
        let release = AsyncGate()
        let firstSubmit = OneShot()
        let submitCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" {
                await submitCalls.increment()
                if await firstSubmit.fire() {
                    await reached.open()
                    await release.wait()
                }
            }
        }
        let submission = Task { await model.submit(.approve) }
        await reached.wait()

        // Evict the cache for a new epoch while the command is in flight.
        coordinator.store.discardSnapshots()

        await release.open()
        await submission.value

        // Treated as ambiguous (settleAmbiguousOutcome replays), not cleared
        // as applied: a second submitCommand ran.
        #expect(await submitCalls.count == 2)
    }

    @Test func aRefetchEvictedMidFlightIsSettledAmbiguousNotFalselyApplied() async {
        // The 200 was valid (no eviction during submitCommand), but a
        // restore lands during the read-your-write refetch. Because the
        // record and slot are settled only after the refetch confirms the
        // generation, this is handled as ambiguous (keep the slot, replay)
        // rather than left showing a false "applied" with the retry slot
        // dropped (#162). Observable via the replay: a second submitCommand.
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        // Installed after the initial validate(), so the first fetch this
        // hook sees is the post-commit refetch; hold it.
        let reached = AsyncGate()
        let release = AsyncGate()
        let getCalls = Counter()
        let submitCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { await submitCalls.increment() }
            if operationID == "getAttentionItem", await getCalls.incrementAndGet() == 1 {
                await reached.open()
                await release.wait()
            }
        }
        let submission = Task { await model.submit(.approve) }
        await reached.wait()

        // Restore during the read-your-write refetch.
        coordinator.store.discardSnapshots()

        await release.open()
        await submission.value

        // Settled ambiguous (a replay ran), not the refetch-eviction branch
        // just revalidating and leaving the optimistic record in place.
        #expect(await submitCalls.count == 2)
    }

    @Test func aFailedLedgerPersistBlocksTheSendAndSurfacesIt() async throws {
        // #163: the pending-command ledger must reach disk before the
        // first byte leaves. If the durable write fails, no command is
        // sent (its reusable command_id would be lost on relaunch), the
        // in-memory slot is rolled back, and the failure surfaces on the
        // card instead of passing silently as disposable-cache loss.
        let server = MockServer()
        let cache = FailingCacheStore(failSaves: true)
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()

        let submitCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { await submitCalls.increment() }
        }

        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        await model.submit(.approve)

        #expect(await submitCalls.count == 0)  // nothing left the client
        #expect(model.submissionError != nil)  // surfaced, not swallowed
        #expect(model.phase == .idle)
        #expect(coordinator.store.pendingCommandsByItemID["item-spec_approval"] == nil)
    }

    @Test func aRecoveredLedgerPersistLetsTheSendProceed() async throws {
        // The precondition fails closed, it does not wedge: once the
        // device can persist again, the same decision submits normally
        // (#163). Guards against the gate over-blocking a healthy write.
        let server = MockServer()
        let cache = FailingCacheStore(failSaves: true)
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()

        let submitCalls = Counter()
        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { await submitCalls.increment() }
        }

        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        #expect(await submitCalls.count == 0)  // blocked while the write fails

        // The disk recovers; resubmitting the same decision now persists
        // its ledger entry and sends.
        cache.failSaves = false
        await model.submit(.approve)

        #expect(await submitCalls.count == 1)  // the command left this time
        #expect(model.submissionError == nil)
        #expect(model.phase == .applied)
        #expect(coordinator.store.pendingCommandsByItemID.isEmpty)
    }

    @Test func concurrentManualRefreshesShareOneDaemonRound() async {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        let heartbeatCalls = Counter()
        let bootstrapCalls = Counter()
        let runCalls = Counter()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            switch operationID {
            case "getSyncRevision":
                await heartbeatCalls.increment()
                await reached.open()
                await release.wait()
            case "getSyncBootstrap":
                await bootstrapCalls.increment()
            case "listRuns":
                await runCalls.increment()
            default:
                break
            }
        }

        let first = Task { await coordinator.refresh() }
        await reached.wait()
        let second = Task { await coordinator.refresh() }
        await release.open()
        await first.value
        await second.value

        #expect(await heartbeatCalls.count == 1)
        #expect(await bootstrapCalls.count == 1)
        #expect(await runCalls.count == 1)
        #expect(coordinator.lastUpdatedAt != nil)
    }

    @Test func periodicAndManualRefreshesShareOneDaemonRound() async {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        let heartbeatCalls = Counter()
        let bootstrapCalls = Counter()
        let runCalls = Counter()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            switch operationID {
            case "getSyncRevision":
                await heartbeatCalls.increment()
                await reached.open()
                await release.wait()
            case "getSyncBootstrap":
                await bootstrapCalls.increment()
            case "listRuns":
                await runCalls.increment()
            default:
                break
            }
        }

        let periodic = Task { await coordinator.periodicRefresh() }
        await reached.wait()
        let manual = Task { await coordinator.refresh() }
        await release.open()
        await periodic.value
        await manual.value

        #expect(await heartbeatCalls.count == 1)
        #expect(await bootstrapCalls.count == 1)
        #expect(await runCalls.count == 1)
    }

    @Test func refreshClosesAGapObservedByTheRunList() async throws {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let before = try #require(coordinator.cursors)
        let mutation = OneShot()
        await server.setBeforeRespond { operationID in
            if operationID == "listRuns", await mutation.fire() {
                await server.advanceRun(id: RunFixtures.activeRunID)
            }
        }

        await coordinator.refresh()

        let converged = try #require(coordinator.cursors)
        #expect(converged.lastFullSnapshotRevision > before.lastFullSnapshotRevision)
        #expect(converged.lastFullSnapshotRevision == converged.highestObservedServerRevision)
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func rapidAutomaticRefreshesShareOneDaemonRound() async {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        let heartbeatCalls = Counter()
        let bootstrapCalls = Counter()
        let runCalls = Counter()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            switch operationID {
            case "getSyncRevision":
                await heartbeatCalls.increment()
                await reached.open()
                await release.wait()
            case "getSyncBootstrap":
                await bootstrapCalls.increment()
            case "listRuns":
                await runCalls.increment()
            default:
                break
            }
        }

        let first = Task { await coordinator.automaticRefresh() }
        await reached.wait()
        let second = Task { await coordinator.automaticRefresh() }
        await release.open()
        await first.value
        await second.value

        #expect(await heartbeatCalls.count == 1)
        #expect(await bootstrapCalls.count == 1)
        #expect(await runCalls.count == 1)
    }

    @Test func recentFailedRefreshDoesNotSuppressReachabilityRecovery() async throws {
        let server = MockServer()
        let coordinator = makeCoordinator(server: server)
        await server.setBeforeRespond { operationID in
            if operationID == "listRuns" { throw MockOutage() }
        }

        await coordinator.refresh()
        #expect(coordinator.store.freshness == .unreachable)
        #expect(coordinator.lastUpdatedAt != nil)

        await server.setBeforeRespond(nil)
        await coordinator.automaticRefresh()

        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func lastUpdatedTurnsStaleAtTheNamedThreshold() async throws {
        let coordinator = makeCoordinator(server: MockServer())
        await coordinator.bootstrap()
        let updatedAt = try #require(coordinator.lastUpdatedAt)

        #expect(!coordinator.isStale(at: updatedAt.addingTimeInterval(59)))
        #expect(coordinator.isStale(at: updatedAt.addingTimeInterval(60)))
    }

    @Test func heartbeatCadenceStaysUnderTheStalenessThreshold() {
        // The banner (and every app's heartbeat loop) trusts that a single
        // missed beat cannot cross the staleness threshold: the cadence must
        // divide well into it, so recovery keeps the banner clear (#1130).
        #expect(
            SyncCoordinator.heartbeatInterval < .seconds(SyncCoordinator.stalenessThreshold))
    }
}

private struct MockOutage: Error {}
