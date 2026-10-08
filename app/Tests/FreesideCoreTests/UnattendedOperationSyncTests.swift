import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// #980: the standing stopped indicator's state reaches the client through
/// sync alone, survives relaunch and rebuild, clears only when the daemon
/// reports admission open, and is never claimed current from a cache.
@MainActor
struct UnattendedOperationSyncTests {
    private static let noticeID = "system-health-unattended-stopped-cmd-stop"

    private func backupWaiverServer() -> MockServer {
        var item = AttentionFixtures.fixture(type: .system_health)
        item.item.posture = .init(value1: .blocking)
        item.item.blocking_supersession = .init(
            value1: .init(kind: .backup_encryption_waiver, repository_id: 424_242))
        return MockServer(items: [item])
    }

    @Test func heartbeatBootstrapsBothBackupHealthTransitionsAtTheSameRevision() async throws {
        let server = backupWaiverServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        let initial = try #require(coordinator.cursors)
        #expect(coordinator.unattendedOperation == .init(admission: .open, stops: []))

        for healthy in [false, true] {
            await server.setBackupHealthy(healthy)
            await coordinator.heartbeat()
            let expected = try await coordinator.store.client.getSyncRevision().ok.body.json
            #expect(coordinator.unattendedOperation == expected.unattended_operation)
            #expect(coordinator.unattendedOperation?.admission == (healthy ? .open : .stopped))
            #expect(coordinator.cursors == initial)
            #expect(coordinator.store.freshness == .fresh)
            // Heartbeats never write this cache: its new verdict proves a
            // canonical bootstrap was adopted at the unchanged revision.
            #expect(cache.load()?.unattendedOperation == expected.unattended_operation)

            if !healthy {
                let adoptedVerdict = coordinator.unattendedOperation
                let adoptedCursors = coordinator.cursors
                let operations = OperationLog()
                await server.setBeforeRespond { await operations.append($0) }

                await coordinator.heartbeat()

                let issued = await operations.ids
                #expect(!issued.contains("getSyncBootstrap"))
                #expect(coordinator.unattendedOperation == adoptedVerdict)
                #expect(coordinator.cursors == adoptedCursors)
                #expect(coordinator.store.freshness == .fresh)
                await server.setBeforeRespond(nil)
            }
        }
    }

    @Test func failedBootstrapAfterGateChangeDoesNotLeaveOldVerdictFresh() async throws {
        let server = backupWaiverServer()
        let coordinator = makeCoordinator(server: server)
        await coordinator.bootstrap()
        let initial = try #require(coordinator.cursors)
        await server.setBackupHealthy(false)
        await server.setBeforeRespond { operationID in
            if operationID == "getSyncBootstrap" { throw MockOutage() }
        }

        await coordinator.heartbeat()

        #expect(coordinator.unattendedOperation == .init(admission: .open, stops: []))
        #expect(coordinator.cursors == initial)
        #expect(coordinator.store.freshness != .fresh)
    }

    private func makeCoordinator(
        server: MockServer, cache: CacheStore = InMemoryCacheStore()
    ) -> SyncCoordinator {
        SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache)
    }

    private func submit(
        _ action: Components.Schemas.Action, id: String, on itemID: String,
        through coordinator: SyncCoordinator, server: MockServer
    ) async throws {
        let snapshot = try #require(await server.snapshot(itemID: itemID))
        let command = Components.Schemas.ClientCommand(
            command_id: id,
            device_id: coordinator.store.device.deviceID,
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .decision(
                .init(
                    kind: .decision, item_id: itemID, action: action,
                    item_version: snapshot.item.item_version,
                    pr_head_sha: snapshot.item.pr_head_sha,
                    artifact_digests: snapshot.item.artifact_digests)))
        _ = try await coordinator.store.client.submitCommand(body: .json(command)).ok.body.json
    }

    @Test func stopRebuildAcknowledgeResume() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        #expect(coordinator.unattendedOperation == .init(admission: .open, stops: []))
        #expect(coordinator.unattendedStoppedMessage() == nil)

        // A second device stops unattended operation. This client learns of
        // it from the heartbeat's revision gap, with no manual refresh.
        try await submit(
            .stop_unattended, id: "cmd-stop", on: "item-system_health",
            through: coordinator, server: server)
        await coordinator.heartbeat()
        let stopped = try #require(coordinator.unattendedOperation)
        #expect(stopped.admission == .stopped)
        #expect(stopped.stops.map(\.kind) == [.operator_stop])
        #expect(stopped.stops.first?.item_id == Self.noticeID)
        #expect(stopped.stops.first?.command_id == "cmd-stop")
        #expect(coordinator.unattendedStoppedMessage() != nil)
        #expect(cache.load()?.unattendedOperation == stopped)

        // Sync rebuild: a client with no cache bootstraps into the same
        // state.
        let rebuilt = makeCoordinator(server: server)
        await rebuilt.bootstrap()
        #expect(rebuilt.unattendedOperation == stopped)

        // Acknowledge is seen, never resolved (plan §4): the stop stands.
        try await submit(
            .acknowledge, id: "cmd-ack", on: Self.noticeID,
            through: coordinator, server: server)
        await coordinator.heartbeat()
        #expect(coordinator.unattendedOperation?.admission == .stopped)

        try await submit(
            .resume_unattended, id: "cmd-resume", on: Self.noticeID,
            through: coordinator, server: server)
        await coordinator.heartbeat()
        #expect(coordinator.unattendedOperation == .init(admission: .open, stops: []))
        #expect(coordinator.unattendedStoppedMessage() == nil)
        #expect(cache.load()?.unattendedOperation?.admission == .open)
    }

    @Test func relaunchRestoresTheStopWithoutClaimingItIsCurrent() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        try await submit(
            .stop_unattended, id: "cmd-stop", on: "item-system_health",
            through: coordinator, server: server)
        await coordinator.heartbeat()
        let stopped = try #require(coordinator.unattendedOperation)

        let relaunched = makeCoordinator(server: server, cache: cache)

        #expect(relaunched.unattendedOperation == stopped)
        #expect(relaunched.store.freshness == .unvalidated)
        // The indicator stands on the cached state, worded as the last known
        // state rather than the present one.
        let presentation = try #require(
            UnattendedStoppedPresentation.make(
                operation: relaunched.unattendedOperation,
                freshness: relaunched.store.freshness,
                lastUpdatedAt: relaunched.lastUpdatedAt, now: .now,
                reason: { _ in nil }))
        #expect(!presentation.isCurrent)
        #expect(presentation.message.hasPrefix("At the last successful refresh"))
        #expect(presentation.message.hasSuffix("The current state is unknown."))
    }

    @Test func aNewEpochDiscardsTheOldEpochsStop() async throws {
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        try await submit(
            .stop_unattended, id: "cmd-stop", on: "item-system_health",
            through: coordinator, server: server)
        await coordinator.heartbeat()
        #expect(coordinator.unattendedOperation?.admission == .stopped)

        // A restore issues a new epoch whose store holds no stop. The dead
        // epoch's state goes with its cache.
        await server.restoreAttentionState(items: [], revision: 1)
        await coordinator.heartbeat()

        #expect(coordinator.unattendedOperation == .init(admission: .open, stops: []))
        #expect(cache.load()?.unattendedOperation?.admission == .open)
    }

    @Test func aCacheWithoutCursorsCarriesNoOperatingState() async throws {
        // The state is scoped by the cursors like every other row: an epoch
        // discard that fails to re-bootstrap leaves no state to restore.
        let server = MockServer()
        let cache = InMemoryCacheStore()
        let coordinator = makeCoordinator(server: server, cache: cache)
        await coordinator.bootstrap()
        try await submit(
            .stop_unattended, id: "cmd-stop", on: "item-system_health",
            through: coordinator, server: server)
        await coordinator.heartbeat()

        await server.rotateEpoch()
        await server.setBeforeRespond { operationID in
            if operationID == "getSyncBootstrap" { throw MockOutage() }
        }
        await coordinator.heartbeat()

        #expect(coordinator.cursors == nil)
        #expect(coordinator.unattendedOperation == nil)
        #expect(makeCoordinator(server: server, cache: cache).unattendedOperation == nil)
    }
}

private actor OperationLog {
    private(set) var ids: [String] = []

    func append(_ id: String) {
        ids.append(id)
    }
}

@MainActor
struct UnattendedStoppedPresentationTests {
    private static let now = Date(timeIntervalSince1970: 1_786_502_645)

    private func make(
        _ stops: [Components.Schemas.UnattendedStop],
        admission: Components.Schemas.UnattendedAdmission = .stopped,
        freshness: InboxStore.Freshness = .fresh,
        age: TimeInterval = 5,
        reason: String? = "An open item"
    ) -> UnattendedStoppedPresentation? {
        .make(
            operation: .init(admission: admission, stops: stops), freshness: freshness,
            lastUpdatedAt: Self.now.addingTimeInterval(-age), now: Self.now,
            reason: { _ in reason })
    }

    @Test func openOrUnknownShowsNothing() {
        #expect(make([], admission: .open) == nil)
        #expect(
            UnattendedStoppedPresentation.make(
                operation: nil, freshness: .fresh, lastUpdatedAt: Self.now, now: Self.now,
                reason: { _ in nil }) == nil)
    }

    @Test func anOperatorStopIsWordedAsADecisionAndLinksItsNotice() throws {
        let presentation = try #require(
            make([
                .init(kind: .operator_stop, item_id: "notice", command_id: "cmd", since: Self.now),
                .init(kind: .blocking_system_health, item_id: "finding", command_id: nil, since: nil),
            ]))
        #expect(presentation.cause == .operatorStop)
        #expect(presentation.itemID == "notice")
        #expect(presentation.isCurrent)
        #expect(
            presentation.message
                == "Unattended operation is stopped by operator decision. "
                + "No new unattended work starts until it is resumed. 1 other stop is in force.")
        #expect(presentation.actionTitle == "Review to Resume")
    }

    @Test func aBlockingFindingNeverClaimsAnOperatorStoppedIt() throws {
        let presentation = try #require(
            make(
                [.init(kind: .blocking_system_health, item_id: "finding", command_id: nil, since: nil)],
                reason: "The daemon stopped after a restart loop"))
        #expect(presentation.cause == .blockingFinding)
        #expect(
            presentation.message
                == "Unattended operation is stopped by a system health finding: "
                + "The daemon stopped after a restart loop.")
        #expect(!presentation.message.contains("operator"))
        #expect(presentation.actionTitle == "Open Finding")
    }

    @Test func aStopWhoseItemIsNotOpenHereOffersNoLink() throws {
        let presentation = try #require(
            make(
                [.init(kind: .operator_stop, item_id: "notice", command_id: "cmd", since: Self.now)],
                reason: nil))
        #expect(presentation.cause == .operatorStop)
        #expect(presentation.itemID == nil)
    }

    @Test func anOperatorStopWithNoOpenNoticeStillStands() throws {
        let presentation = try #require(
            make([.init(kind: .operator_stop, item_id: nil, command_id: "cmd", since: Self.now)]))
        #expect(presentation.cause == .operatorStop)
        #expect(presentation.itemID == nil)
    }

    /// Each case is a fresh-looking snapshot made not current one way: by
    /// age alone, or by a freshness state that cannot vouch for it.
    @Test(arguments: [
        (InboxStore.Freshness.fresh, true),
        (.unvalidated, false), (.unreachable, false), (.syncFailing, false),
        (.unauthenticated, false),
        (.contractMismatch(daemonContract: "sha256:other"), false),
    ])
    func aSnapshotThatIsNotCurrentIsNeverWordedAsCurrent(
        freshness: InboxStore.Freshness, pastThreshold: Bool
    ) throws {
        let presentation = try #require(
            make(
                [.init(kind: .operator_stop, item_id: "notice", command_id: "cmd", since: Self.now)],
                freshness: freshness,
                age: pastThreshold ? SyncCoordinator.stalenessThreshold : 5))
        #expect(!presentation.isCurrent)
        #expect(
            presentation.message
                == "At the last successful refresh, unattended operation was stopped by "
                + "operator decision. The current state is unknown.")
    }
}

private struct MockOutage: Error {}
