import Foundation
import FreesideAPI
import Security
import Testing

@testable import FreesideCore

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
        try store.save(state)
        #expect(store.load() == state)

        // A later save replaces the earlier state wholesale, as a
        // bootstrap rebuild does.
        let newer = sampleState(revision: 9)
        try store.save(newer)
        #expect(store.load() == newer)
    }

    @Test func preCreationTimestampCacheDecodesAsLegacyNil() throws {
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        try store.save(sampleState())

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var state = try #require(object["state"] as? [String: Any])
        var snapshots = try #require(state["attentionItems"] as? [[String: Any]])
        var snapshot = snapshots[0]
        var item = try #require(snapshot["item"] as? [String: Any])
        item.removeValue(forKey: "created_at")
        snapshot["item"] = item
        snapshots[0] = snapshot
        state["attentionItems"] = snapshots
        object["state"] = state
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let legacy = try #require(store.load())
        #expect(legacy.attentionItems[0].item.created_at == nil)
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

    @Test func aPreRunsFormatTwoCachePreservesOnlyTheCommandLedger() throws {
        // Format 2 predates runs and schedules.  Its valid cached cursors
        // must not make an upgraded client consider empty default arrays
        // current, or completed durable runs would stay invisible until a
        // later server revision happens to force a bootstrap.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        legacyState.removeValue(forKey: "runs")
        legacyState.removeValue(forKey: "schedules")
        legacyState.removeValue(forKey: "runTimelines")
        object["format"] = 2
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.runs.isEmpty)
        #expect(migrated.schedules.isEmpty)
        #expect(migrated.runTimelines.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
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
        try store.save(state)
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
        try store.save(state)
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
        try store.save(state)

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
        let subscription = try #require(
            DeviceNtfySubscription(
                serverURL: grant.ntfy_subscription.server_url,
                topic: grant.ntfy_subscription.topic))
        let credential = try #require(
            DeviceCredential(
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
        try store.save(sampleState())
        #expect(store.load() != nil)

        store.discard()

        #expect(store.load() == nil)
        #expect(
            !FileManager.default.fileExists(
                atPath: directory.appendingPathComponent("cache.json").path))
    }
}

private enum FakeKeychainBackend: Hashable {
    case dataProtection
    case legacy
}

private struct FakeKeychainCall: Equatable {
    enum Kind: Equatable {
        case copy
        case add
        case delete
    }

    let kind: Kind
    let backend: FakeKeychainBackend
    let service: String
}

private final class FakeKeychain: @unchecked Sendable {
    var items: [FakeKeychainBackend: [String: Any]] = [:]
    var calls: [FakeKeychainCall] = []
    var addedAttributes: [[String: Any]] = []
    var copyOverrides: [FakeKeychainBackend: [(OSStatus, Any?)]] = [:]
    var addStatuses: [OSStatus] = []
    var deleteStatuses: [FakeKeychainBackend: [OSStatus]] = [:]

    var operations: KeychainSecurityOperations {
        KeychainSecurityOperations(
            copyMatching: { [self] query in
                let backend = backend(for: query)
                calls.append(call(.copy, backend: backend, query: query))
                if var overrides = copyOverrides[backend], !overrides.isEmpty {
                    let result = overrides.removeFirst()
                    copyOverrides[backend] = overrides
                    return result
                }
                guard let item = items[backend] else {
                    return (errSecItemNotFound, nil)
                }
                return (errSecSuccess, item)
            },
            add: { [self] attributes in
                let backend = backend(for: attributes)
                calls.append(call(.add, backend: backend, query: attributes))
                addedAttributes.append(attributes)
                if !addStatuses.isEmpty {
                    let status = addStatuses.removeFirst()
                    if status != errSecSuccess { return status }
                }
                if items[backend] != nil { return errSecDuplicateItem }
                items[backend] = [
                    kSecAttrAccount as String: attributes[kSecAttrAccount as String] as Any,
                    kSecValueData as String: attributes[kSecValueData as String] as Any,
                ]
                return errSecSuccess
            },
            delete: { [self] query in
                let backend = backend(for: query)
                calls.append(call(.delete, backend: backend, query: query))
                if var statuses = deleteStatuses[backend], !statuses.isEmpty {
                    let status = statuses.removeFirst()
                    deleteStatuses[backend] = statuses
                    if status != errSecSuccess && status != errSecItemNotFound {
                        return status
                    }
                }
                guard items.removeValue(forKey: backend) != nil else {
                    return errSecItemNotFound
                }
                return errSecSuccess
            })
    }

    private func backend(for query: [String: Any]) -> FakeKeychainBackend {
        query[kSecUseDataProtectionKeychain as String] as? Bool == true
            ? .dataProtection : .legacy
    }

    private func call(
        _ kind: FakeKeychainCall.Kind,
        backend: FakeKeychainBackend,
        query: [String: Any]
    ) -> FakeKeychainCall {
        FakeKeychainCall(
            kind: kind,
            backend: backend,
            service: query[kSecAttrService as String] as? String ?? "")
    }
}

private func credential(
    deviceID: String = "device-1",
    secretByte: UInt8 = 1,
    serverURL: String = "https://ntfy.example",
    topic: String = "fs-00000000000000000000000000000000"
) throws -> DeviceCredential {
    let subscription = try #require(
        DeviceNtfySubscription(serverURL: serverURL, topic: topic))
    return try #require(
        DeviceCredential(
            deviceID: deviceID,
            token: testDeviceToken(for: deviceID, secretByte: secretByte),
            ntfySubscription: subscription))
}

private func storedItem(_ credential: DeviceCredential) throws -> [String: Any] {
    let data = try JSONSerialization.data(withJSONObject: [
        "formatVersion": 1,
        "deviceID": credential.deviceID,
        "token": credential.token,
        "ntfyServerURL": credential.ntfySubscription.serverURL,
        "ntfyTopic": credential.ntfySubscription.topic,
    ])
    return [
        kSecAttrAccount as String: credential.deviceID,
        kSecValueData as String: data,
    ]
}

@Suite(.serialized) struct CredentialStoreTests {
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

    // macOS: the file-based login keychain is the persistent backend (#960); a
    // save writes there, best-effort clears any stale Data Protection copy, and
    // never sets kSecAttrAccessible (a Data Protection attribute the file backend
    // rejects).
    @Test func fileBackendSaveAndLoadUseTheScopedBackend() throws {
        let fake = FakeKeychain()
        let store = KeychainCredentialStore(
            service: "ai.freeside.tests.deployment-a",
            operations: fake.operations,
            legacyBackendEnabled: true)
        let value = try credential()

        try store.save(value)
        #expect(try store.load() == value)
        #expect(fake.items[.dataProtection] == nil)
        #expect(fake.items[.legacy] != nil)
        #expect(
            fake.calls == [
                FakeKeychainCall(
                    kind: .delete, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .delete, backend: .legacy,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .add, backend: .legacy,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .copy, backend: .legacy,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .copy, backend: .legacy,
                    service: "ai.freeside.tests.deployment-a"),
            ])
        let added = try #require(fake.addedAttributes.first)
        #expect(added[kSecUseDataProtectionKeychain as String] as? Bool != true)
        #expect(added[kSecAttrAccessible as String] == nil)
    }

    // iOS: the Data Protection Keychain is the persistent backend, and the add
    // scopes kSecAttrAccessible to it.
    @Test func dataProtectionSaveAndLoadUseTheScopedBackend() throws {
        let fake = FakeKeychain()
        let store = KeychainCredentialStore(
            service: "ai.freeside.tests.deployment-a",
            operations: fake.operations,
            legacyBackendEnabled: false)
        let value = try credential()

        try store.save(value)
        #expect(try store.load() == value)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.items[.dataProtection] != nil)
        #expect(
            fake.calls == [
                FakeKeychainCall(
                    kind: .delete, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .add, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .copy, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
                FakeKeychainCall(
                    kind: .copy, backend: .dataProtection,
                    service: "ai.freeside.tests.deployment-a"),
            ])
        let added = try #require(fake.addedAttributes.first)
        #expect(added[kSecUseDataProtectionKeychain as String] as? Bool == true)
        #expect(
            added[kSecAttrAccessible as String] as? String
                == kSecAttrAccessibleAfterFirstUnlock as String)
    }

    // A Data Protection copy left by an earlier build is cleared on save so it
    // can never shadow the authoritative file-keychain credential.
    @Test func macOSSaveClearsAStaleDataProtectionCopy() throws {
        let fake = FakeKeychain()
        fake.items[.dataProtection] = try storedItem(
            credential(deviceID: "device-stale", secretByte: 9))
        let store = KeychainCredentialStore(
            service: "service", operations: fake.operations, legacyBackendEnabled: true)
        let value = try credential()

        try store.save(value)

        #expect(fake.items[.dataProtection] == nil)
        #expect(fake.items[.legacy] != nil)
        #expect(try store.load() == value)
        #expect(
            Array(fake.calls.prefix(2)) == [
                FakeKeychainCall(kind: .delete, backend: .dataProtection, service: "service"),
                FakeKeychainCall(kind: .delete, backend: .legacy, service: "service"),
            ])
    }

    // Clearing the non-authoritative Data Protection backend is best-effort: its
    // failure must not block the real save into the persistent file backend.
    @Test func macOSSaveSurvivesADataProtectionClearFailure() throws {
        let fake = FakeKeychain()
        fake.items[.dataProtection] = try storedItem(
            credential(deviceID: "device-stale", secretByte: 9))
        fake.deleteStatuses[.dataProtection] = [errSecInteractionNotAllowed]
        let store = KeychainCredentialStore(
            service: "service", operations: fake.operations, legacyBackendEnabled: true)
        let value = try credential()

        try store.save(value)

        #expect(try store.load() == value)
        #expect(fake.items[.legacy] != nil)
        #expect(fake.calls.first?.kind == .delete)
        #expect(fake.calls.first?.backend == .dataProtection)
    }

    @Test func platformWithoutDistinctLegacyBackendNeverConsultsIt() throws {
        let fake = FakeKeychain()
        let value = try credential()
        fake.items[.dataProtection] = try storedItem(value)
        let store = KeychainCredentialStore(
            service: "service",
            operations: fake.operations,
            legacyBackendEnabled: false)

        #expect(try store.load() == value)
        #expect(try store.load() == value)
        try store.delete()

        #expect(fake.items[.dataProtection] == nil)
        #expect(fake.calls.map(\.kind) == [.copy, .copy, .delete])
        #expect(fake.calls.allSatisfy { $0.backend == .dataProtection })
    }

    // A corrupt authoritative item fails closed; the store never falls back to
    // the non-authoritative backend.
    @Test func loadFailsClosedOnCorruptPrimaryItem() throws {
        let fake = FakeKeychain()
        fake.items[.legacy] = [
            kSecAttrAccount as String: "device-1",
            kSecValueData as String: Data("not-json".utf8),
        ]
        fake.items[.dataProtection] = try storedItem(credential())
        let store = KeychainCredentialStore(
            service: "service", operations: fake.operations, legacyBackendEnabled: true)

        do {
            _ = try store.load()
            Issue.record("expected corrupt authoritative item to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecDecode)
        }
        #expect(fake.calls.map(\.kind) == [.copy])
        #expect(fake.calls.map(\.backend) == [.legacy])
        #expect(fake.items[.dataProtection] != nil)
    }

    // A Keychain error on the authoritative read propagates; it is never masked
    // by consulting the other backend.
    @Test func loadPropagatesAPrimaryKeychainError() throws {
        let fake = FakeKeychain()
        fake.copyOverrides[.legacy] = [(errSecInteractionNotAllowed, nil)]
        let store = KeychainCredentialStore(
            service: "service", operations: fake.operations, legacyBackendEnabled: true)

        do {
            _ = try store.load()
            Issue.record("expected authoritative Keychain error to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecInteractionNotAllowed)
        }
        #expect(fake.calls.map(\.kind) == [.copy])
        #expect(fake.calls.map(\.backend) == [.legacy])
    }

    @Test func deleteClearsBothBackendsAndPreservesTheFirstError() throws {
        let fake = FakeKeychain()
        let value = try credential()
        fake.items[.dataProtection] = try storedItem(value)
        fake.items[.legacy] = try storedItem(value)
        fake.deleteStatuses[.dataProtection] = [errSecAuthFailed]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            try store.delete()
            Issue.record("expected the first backend error")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecAuthFailed)
        }
        #expect(fake.items[.dataProtection] != nil)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.calls.map(\.kind) == [.delete, .delete])
        #expect(fake.calls.map(\.backend) == [.dataProtection, .legacy])

        fake.deleteStatuses[.dataProtection] = [errSecSuccess]
        try store.delete()
        #expect(fake.items.isEmpty)
    }
}
