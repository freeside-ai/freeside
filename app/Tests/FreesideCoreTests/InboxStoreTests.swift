import FreesideAPI
import Testing

@testable import FreesideCore

@MainActor
@Suite struct InboxStoreTests {
    @Test func refreshReconstructsTheInboxFromTheCanonicalList() async {
        let store = await makeStore(server: MockServer())
        #expect(store.loadState == .loaded)
        #expect(store.rows.count == AttentionFixtures.phase1Types.count)
        #expect(Set(store.rows.map(\.item._type)) == Set(AttentionFixtures.phase1Types))
    }

    @Test func refreshFailureIsSurfacedNotMasked() async {
        let server = MockServer()
        await server.setBeforeRespond { operationID in
            if operationID == "listAttentionItems" { throw InjectedFailure() }
        }
        let store = await makeStore(server: server)
        #expect(store.rows.isEmpty)
        guard case .failed = store.loadState else {
            Issue.record("expected .failed, got \(store.loadState)")
            return
        }
    }

    @Test func rowsSortOpenItemsFirstThenPriority() async {
        let store = await makeStore(server: MockServer())
        guard var resolved = store.snapshotsByID["item-execution_failure"] else {
            Issue.record("missing seeded snapshot")
            return
        }
        resolved.item.status = .resolved
        store.apply(resolved)

        let statuses = store.rows.map(\.item.status)
        let firstNonOpen = statuses.firstIndex { $0 != .open } ?? statuses.count
        #expect(!statuses[..<firstNonOpen].contains { $0 != .open })
        #expect(!statuses[firstNonOpen...].contains(.open))
        // The urgent item left the open set, so the high-priority one leads.
        #expect(store.rows.first?.item.priority == .high)
        #expect(store.rows.last?.item.id == "item-execution_failure")
    }

    @Test func clearReleasesOnlyTheSettledCommand() async {
        // A late completion from an older replay must never release a
        // newer command's slot: the clear is conditional on the stored
        // command_id matching the one that settled.
        let store = await makeStore(server: MockServer())
        guard let snapshot = store.snapshotsByID["item-spec_approval"] else {
            Issue.record("missing seeded snapshot")
            return
        }
        var older = Components.Schemas.ClientCommand(
            command_id: "cmd-older",
            device_id: "device-mock",
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .init(
                item_id: "item-spec_approval",
                action: .approve,
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests
            )
        )
        #expect(store.registerPendingCommand(older))
        store.clearPendingCommand(itemID: "item-spec_approval", commandID: "cmd-older")
        older.command_id = "cmd-newer"
        #expect(store.registerPendingCommand(older))

        // The older command's late completion must not release the slot.
        store.clearPendingCommand(itemID: "item-spec_approval", commandID: "cmd-older")
        #expect(store.pendingCommandsByItemID["item-spec_approval"]?.command.command_id == "cmd-newer")
        store.clearPendingCommand(itemID: "item-spec_approval", commandID: "cmd-newer")
        #expect(store.pendingCommandsByItemID["item-spec_approval"] == nil)
    }

    @Test func staleRefreshFailureNeverClobbersANewerSuccess() async {
        // An older refresh that fails late must not overwrite the load
        // state of a newer one that already succeeded.
        let server = MockServer()
        let store = InboxStore(client: APIClientFactory.mock(server: server))

        let firstCall = OneShot()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operationID in
            if operationID == "listAttentionItems", await firstCall.fire() {
                await reached.open()
                await release.wait()
                throw InjectedFailure()
            }
        }
        let first = Task { await store.refresh() }
        await reached.wait()

        await store.refresh()
        #expect(store.loadState == .loaded)

        await release.open()
        await first.value
        #expect(store.loadState == .loaded)
        #expect(!store.rows.isEmpty)
    }

    @Test func refreshNeverHidesSnapshotsTheListDoesNotCarry() async {
        // An older or lagging list response must not drop an item the
        // store already knows (e.g. one applied from a conflict
        // replacement before the list caught up): rows would hide it
        // while its snapshot stayed cached.
        let store = await makeStore(server: MockServer())
        var extra = AttentionFixtures.fixture(type: .blocked)
        extra.item.id = "item-new"
        store.apply(extra)
        #expect(store.rows.contains { $0.item.id == "item-new" })

        // The mock's list does not carry item-new.
        await store.refresh()
        #expect(store.loadState == .loaded)
        #expect(store.rows.contains { $0.item.id == "item-new" })
    }

    @Test func applyNeverDowngradesToAnOlderSnapshot() async {
        // Two reads of one item can complete out of order; the store
        // keeps the newest entity_version, so a late older response
        // cannot re-open an item a card already saw as advanced.
        let server = MockServer()
        let store = await makeStore(server: server)
        guard let older = store.snapshotsByID["item-spec_approval"] else {
            Issue.record("missing seeded snapshot")
            return
        }
        await server.advance(itemID: "item-spec_approval")
        guard let newer = await server.snapshot(itemID: "item-spec_approval") else {
            Issue.record("missing server snapshot")
            return
        }

        store.apply(newer)
        store.apply(older)
        #expect(store.snapshotsByID["item-spec_approval"] == newer)
    }

    @Test func applyUpsertsAReplacementSnapshotInPlace() async {
        let server = MockServer()
        let store = await makeStore(server: server)
        await server.advance(itemID: "item-spec_approval")
        guard let replacement = await server.snapshot(itemID: "item-spec_approval") else {
            Issue.record("missing server snapshot")
            return
        }

        store.apply(replacement)
        #expect(store.snapshotsByID["item-spec_approval"] == replacement)
        #expect(store.rows.count == AttentionFixtures.phase1Types.count)
    }
}
