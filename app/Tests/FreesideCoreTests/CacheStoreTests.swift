import Foundation
import FreesideAPI
import FreesideCore
import Testing

private func temporaryStore() -> (DiskCacheStore, URL) {
    let directory = FileManager.default.temporaryDirectory
        .appendingPathComponent("freeside-cache-tests-\(UUID().uuidString)")
    return (DiskCacheStore(directory: directory), directory)
}

private func sampleState(revision: Int64 = 5) -> CachedState {
    CachedState(
        cursors: SyncCursors(
            syncEpoch: "epoch-1",
            lastFullSnapshotRevision: revision,
            highestObservedServerRevision: revision
        ),
        attentionItems: [AttentionFixtures.fixture(type: .spec_approval)]
    )
}

@Suite struct DiskCacheStoreTests {
    @Test func roundTripsTheCachedState() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        #expect(store.load() == nil)
        let state = sampleState()
        store.save(state)
        #expect(store.load() == state)

        // A later save replaces the earlier state wholesale, as a
        // bootstrap rebuild does.
        let newer = sampleState(revision: 9)
        store.save(newer)
        #expect(store.load() == newer)
    }

    @Test func anythingUnreadableLoadsAsAbsent() throws {
        // The cache is disposable by design: corruption, a foreign
        // format, or a future incompatible version all mean "bootstrap",
        // never a decode error surfaced to the user.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        try FileManager.default.createDirectory(
            at: directory, withIntermediateDirectories: true)
        let file = directory.appendingPathComponent("cache.json")

        try Data("not json {".utf8).write(to: file)
        #expect(store.load() == nil)

        try Data(#"{"format": 999, "state": {}}"#.utf8).write(to: file)
        #expect(store.load() == nil)

        // A pre-ledger format-1 file is one such foreign format: it
        // loads as absent (one bootstrap; a pre-upgrade unresolved
        // ledger did not exist to lose).
        try Data(#"{"format": 1, "state": {}}"#.utf8).write(to: file)
        #expect(store.load() == nil)
    }

    @Test func roundTripsThePendingCommandLedger() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .inFlight),
            "item-b": .init(
                command: makeCommand(itemID: "item-b", commandID: "cmd-b"),
                state: .unresolved),
        ]
        store.save(state)
        #expect(store.load() == state)
    }

    @Test func aLedgerOnlyStateRoundTrips() throws {
        // The post-epoch-discard shape: cursors and rows are dead while
        // an unresolved command still needs its verbatim resend (#115).
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        let state = CachedState(
            cursors: nil,
            attentionItems: [],
            pendingCommands: [
                "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
            ])
        store.save(state)
        #expect(store.load() == state)
    }

    @Test func aCorruptLedgerSectionLoadsAsAbsentWithoutDroppingTheRest() throws {
        // The ledger degrades independently: garbling only the
        // pendingCommands section costs the retry affordance, never the
        // cursors and rows saved beside it.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        let file = directory.appendingPathComponent("cache.json")

        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        store.save(state)

        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var inner = try #require(object["state"] as? [String: Any])
        inner["pendingCommands"] = ["item-a": 42]
        object["state"] = inner
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let loaded = try #require(store.load())
        #expect(loaded.pendingCommands == nil)
        #expect(loaded.cursors == state.cursors)
        #expect(loaded.attentionItems == state.attentionItems)
    }

    @Test @MainActor func thePersistedLedgerCarriesNoCredentialMaterial() async throws {
        // #115 acceptance 3: the ledger persists whole ClientCommands,
        // so prove at the byte level that a command minted through the
        // real paired, bearer-authenticated submit path writes no token
        // material to disk — the credential's only sink stays the
        // per-request Authorization header.
        let (cache, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }

        let server = MockServer(authMode: .enforcing)
        await server.seedPairingCode("483911")
        let grant = try await APIClientFactory.mock(server: server).pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        guard case .active(let active) = grant.device.device else {
            Issue.record("expected an active device")
            return
        }
        let subscription = try #require(DeviceNtfySubscription(
            serverURL: grant.ntfy_subscription.server_url,
            topic: grant.ntfy_subscription.topic))
        let credential = try #require(DeviceCredential(
            deviceID: active.id,
            token: grant.device_token,
            ntfySubscription: subscription))
        let client = APIClientFactory.mock(server: server) { credential.token }
        let coordinator = SyncCoordinator(
            client: client, device: DeviceIdentity(deviceID: active.id), cache: cache)
        await coordinator.bootstrap()

        // Lose the response after the mock records it, so the ledger
        // holds the submitted command when it persists.
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw InjectedFailure() }
        }
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        await model.submit(.approve)
        #expect(
            coordinator.store.pendingCommandsByItemID["item-spec_approval"]?.state
                == .unresolved)

        let data = try Data(contentsOf: directory.appendingPathComponent("cache.json"))
        let text = try #require(String(data: data, encoding: .utf8)).lowercased()
        #expect(text.contains("pendingcommands"))
        #expect(!text.contains("authorization"))
        #expect(!text.contains("bearer"))
        #expect(!text.contains(credential.token.lowercased()))
        #expect(!text.contains(credential.ntfySubscription.topic.lowercased()))
        // The token scheme prefix and the token's base64 form: no
        // token-shaped fragment reaches disk.
        #expect(!text.contains("fsd1"))
        #expect(
            !text.contains(
                Data(credential.token.utf8).base64EncodedString().lowercased()))
        #expect(
            !text.contains(
                Data(credential.ntfySubscription.topic.utf8).base64EncodedString().lowercased()))
    }

    @Test func discardDeletesTheFile() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        store.save(sampleState())
        #expect(store.load() != nil)

        store.discard()

        #expect(store.load() == nil)
        #expect(
            !FileManager.default.fileExists(
                atPath: directory.appendingPathComponent("cache.json").path))
    }
}

@Suite struct CredentialStoreTests {
    @Test func inMemoryStoreRoundTrips() throws {
        let store = InMemoryCredentialStore()
        #expect(try store.load() == nil)

        let credential = DeviceCredential(
            deviceID: "device-1", token: testDeviceToken(for: "device-1"),
            ntfySubscription: .mock)!
        try store.save(credential)
        #expect(try store.load() == credential)

        try store.delete()
        #expect(try store.load() == nil)
    }

    @Test func keychainStoreRoundTripsUnderAScopedService() throws {
        // Runs against the real Keychain (FreesideCoreTests are
        // Apple-platform, developer-machine tests; CI's Linux job never
        // sees them). The service name is unique per run and the item
        // is removed either way.
        let store = KeychainCredentialStore(
            service: "ai.freeside.tests.\(UUID().uuidString)")
        defer { try? store.delete() }

        #expect(try store.load() == nil)
        let first = DeviceCredential(
            deviceID: "device-1", token: testDeviceToken(for: "device-1"),
            ntfySubscription: .mock)!
        try store.save(first)
        #expect(try store.load() == first)

        // A re-pair replaces the whole identity.
        let secondSubscription = try #require(DeviceNtfySubscription(
            serverURL: "https://other-ntfy.example",
            topic: "fs-11111111111111111111111111111111"))
        let second = DeviceCredential(
            deviceID: "device-2", token: testDeviceToken(for: "device-2", secretByte: 2),
            ntfySubscription: secondSubscription)!
        try store.save(second)
        #expect(try store.load() == second)

        try store.delete()
        #expect(try store.load() == nil)
    }
}
