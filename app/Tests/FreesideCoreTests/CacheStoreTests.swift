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
        attentionItems: [AttentionFixtures.fixture(type: .spec_approval)],
        conversations: AttentionFixtures.defaultConversations(),
        tasks: TaskFixtures.defaultTasks(),
        taskTimelines: TaskFixtures.defaultTimelines()
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

    @Test func aPreConversationsFormatThreeCachePreservesOnlyTheCommandLedger() throws {
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
        legacyState.removeValue(forKey: "conversations")
        object["format"] = 3
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.conversations.isEmpty)
        #expect(migrated.runs.isEmpty)
        #expect(migrated.schedules.isEmpty)
        #expect(migrated.runTimelines.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    @Test func aLegacyLedgerWithoutPayloadKindDecodesAsDecision() throws {
        // Before the client-command payload became a kind-discriminated union
        // (submit_task), a persisted decision command encoded a bare, kind-less
        // payload. The generated decoder now requires the discriminator, so the
        // load path injects it (defaulting to decision) rather than dropping the
        // whole ledger and its retryable command IDs (plan §5.14 sync test 4).
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
        var persistedState = try #require(object["state"] as? [String: Any])
        var pending = try #require(persistedState["pendingCommands"] as? [String: Any])
        var entry = try #require(pending["item-a"] as? [String: Any])
        var command = try #require(entry["command"] as? [String: Any])
        var payload = try #require(command["payload"] as? [String: Any])
        payload.removeValue(forKey: "kind")
        command["payload"] = payload
        entry["command"] = command
        pending["item-a"] = entry
        persistedState["pendingCommands"] = pending
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.pendingCommands == state.pendingCommands)
        guard case .decision(let decoded)? = migrated.pendingCommands?["item-a"]?.command.payload
        else {
            Issue.record("expected a decision command")
            return
        }
        #expect(decoded.kind == .decision)
        #expect(decoded.item_id == "item-a")
    }

    @Test func aLegacyRunProposalSnapshotDecodesAsTaskProposal() throws {
        // The run_proposal -> task_proposal attention-vocabulary rename (#1210)
        // changed a persisted AttentionType value. AttentionType decodes
        // strictly, so a format-5 cache written before the rename would throw
        // on decode and discard the whole cache, including the retryable
        // command ledger. The load path translates the stored type in place.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.attentionItems = [AttentionFixtures.fixture(type: .task_proposal)]
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var snapshots = try #require(persistedState["attentionItems"] as? [[String: Any]])
        var item = try #require(snapshots[0]["item"] as? [String: Any])
        item["type"] = "run_proposal"
        snapshots[0]["item"] = item
        persistedState["attentionItems"] = snapshots
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.attentionItems.count == 1)
        #expect(migrated.attentionItems[0].item._type == .task_proposal)
        #expect(migrated.pendingCommands == state.pendingCommands)
    }

    @Test func aLegacyRunProposalRevisionKeyReloadsAsTaskProposalRevision() throws {
        // The same rename changed the run_proposal_revision decision-payload
        // key. The generated decoder ignores the unknown old key rather than
        // rejecting it, so a committed start_with_changes command persisted
        // before the rename would silently reload without its revision and fail
        // its verbatim retry. The load path renames the key in place.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var command = makeCommand(itemID: "item-a")
        command.payload.asDecision.action = .start_with_changes
        command.payload.asDecision.task_proposal_revision = .init(
            value1: .init(
                intent: .implement_subject, expected_cost_units: 25,
                scope: .init(
                    component_count: 2, declared_path_count: 3, touches_control_plane: true)))
        var state = sampleState()
        state.pendingCommands = ["item-a": .init(command: command, state: .unresolved)]
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var persistedState = try #require(object["state"] as? [String: Any])
        var pending = try #require(persistedState["pendingCommands"] as? [String: Any])
        var entry = try #require(pending["item-a"] as? [String: Any])
        var commandJSON = try #require(entry["command"] as? [String: Any])
        var payload = try #require(commandJSON["payload"] as? [String: Any])
        let revision = try #require(payload["task_proposal_revision"])
        payload.removeValue(forKey: "task_proposal_revision")
        payload["run_proposal_revision"] = revision
        commandJSON["payload"] = payload
        entry["command"] = commandJSON
        pending["item-a"] = entry
        persistedState["pendingCommands"] = pending
        object["state"] = persistedState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.pendingCommands == state.pendingCommands)
        guard case .decision(let decoded)? = migrated.pendingCommands?["item-a"]?.command.payload
        else {
            Issue.record("expected a decision command")
            return
        }
        #expect(decoded.action == .start_with_changes)
        #expect(decoded.task_proposal_revision?.value1.expected_cost_units == 25)
    }

    @Test func aPreTasksFormatFourCacheKeepsTheLedgerAndTelemetrySections() throws {
        // Format 4 predates task snapshots and task timelines. Its cursors
        // must not make an upgraded client consider an empty task list
        // current; the ledger and the telemetry sections are epoch-independent
        // client state and survive, so a queued event drains rather than
        // vanishing with the upgrade.
        let (store, directory) = temporaryStore()
        defer { try? FileManager.default.removeItem(at: directory) }
        var state = sampleState()
        state.pendingCommands = [
            "item-a": .init(command: makeCommand(itemID: "item-a"), state: .unresolved)
        ]
        state.comprehensionQueue = [
            .init(
                eventID: "event-1",
                input: .init(
                    item_id: "item-a", kind: .card_opened,
                    item_decision_surface_digest: "sha256:" + String(repeating: "a", count: 64),
                    occurred_at: Date(timeIntervalSince1970: 1_700_000_000), sequence: 1))
        ]
        state.comprehensionSequence = 1
        state.registeredCapabilityFingerprint = "fingerprint-1"
        state.comprehensionDeviceID = "device-1"
        try store.save(state)

        let file = directory.appendingPathComponent("cache.json")
        var object = try #require(
            try JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        var legacyState = try #require(object["state"] as? [String: Any])
        legacyState.removeValue(forKey: "tasks")
        legacyState.removeValue(forKey: "taskTimelines")
        object["format"] = 4
        object["state"] = legacyState
        try JSONSerialization.data(withJSONObject: object).write(to: file)

        let migrated = try #require(store.load())
        #expect(migrated.cursors == nil)
        #expect(migrated.attentionItems.isEmpty)
        #expect(migrated.conversations.isEmpty)
        #expect(migrated.runs.isEmpty)
        #expect(migrated.runTimelines.isEmpty)
        #expect(migrated.tasks.isEmpty)
        #expect(migrated.taskTimelines.isEmpty)
        #expect(migrated.pendingCommands == state.pendingCommands)
        #expect(migrated.comprehensionQueue == state.comprehensionQueue)
        #expect(migrated.comprehensionSequence == 1)
        #expect(migrated.registeredCapabilityFingerprint == "fingerprint-1")
        #expect(migrated.comprehensionDeviceID == "device-1")
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
                // Mirrors SecItemMergeResults: the Data Protection result wins
                // when it succeeds, otherwise the file-based result stands.
                var merged: (OSStatus, Any?) = (errSecItemNotFound, nil)
                for backend in targets(for: query) {
                    calls.append(call(.copy, backend: backend, query: query))
                    let result: (OSStatus, Any?)
                    if var overrides = copyOverrides[backend], !overrides.isEmpty {
                        result = overrides.removeFirst()
                        copyOverrides[backend] = overrides
                    } else if let item = items[backend] {
                        result = (errSecSuccess, item)
                    } else {
                        result = (errSecItemNotFound, nil)
                    }
                    if result.0 == errSecSuccess { return result }
                    if result.0 != errSecItemNotFound { merged = result }
                }
                return merged
            },
            add: { [self] attributes in
                // SecItemAdd never targets both: only an explicit `true` adds
                // to the Data Protection Keychain.
                let backend: FakeKeychainBackend =
                    attributes[kSecUseDataProtectionKeychain as String] as? Bool == true
                    ? .dataProtection : .legacy
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
                // A delete runs against every targeted backend; one success is
                // a success, otherwise the first real error is reported.
                var merged = errSecItemNotFound
                for backend in targets(for: query) {
                    calls.append(call(.delete, backend: backend, query: query))
                    var status = errSecSuccess
                    if var statuses = deleteStatuses[backend], !statuses.isEmpty {
                        status = statuses.removeFirst()
                        deleteStatuses[backend] = statuses
                    }
                    if status == errSecSuccess || status == errSecItemNotFound {
                        status =
                            items.removeValue(forKey: backend) != nil
                            ? errSecSuccess : errSecItemNotFound
                    }
                    if status == errSecSuccess {
                        merged = errSecSuccess
                    } else if status != errSecItemNotFound && merged != errSecSuccess {
                        merged = status
                    }
                }
                return merged
            })
    }

    // Mirrors SecItemCategorizeQuery on macOS: an explicit
    // kSecUseDataProtectionKeychain selects exactly one backend, and a query
    // that omits it targets both. The store must never issue the latter (a
    // flag-less delete removed the live credential, #997).
    private func targets(for query: [String: Any]) -> [FakeKeychainBackend] {
        switch query[kSecUseDataProtectionKeychain as String] as? Bool {
        case true?: return [.dataProtection]
        case false?: return [.legacy]
        case nil: return [.dataProtection, .legacy]
        }
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

    @Test func dataProtectionSaveAndLoadUseTheScopedBackend() throws {
        let fake = FakeKeychain()
        let store = KeychainCredentialStore(
            service: "ai.freeside.tests.deployment-a", operations: fake.operations)
        let value = try credential()

        try store.save(value)
        #expect(try store.load() == value)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.items[.dataProtection] != nil)
        #expect(
            fake.calls == [
                FakeKeychainCall(
                    kind: .delete, backend: .legacy,
                    service: "ai.freeside.tests.deployment-a"),
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
                FakeKeychainCall(
                    kind: .delete, backend: .legacy,
                    service: "ai.freeside.tests.deployment-a"),
            ])
        let added = try #require(fake.addedAttributes.first)
        #expect(added[kSecUseDataProtectionKeychain as String] as? Bool == true)
        #expect(
            added[kSecAttrAccessible as String] as? String
                == kSecAttrAccessibleAfterFirstUnlock as String)
    }

    // Regression for the #997 symptom: the legacy cleanup on a successful
    // read must target only the file-based Keychain. A flag-less delete also
    // reaches the Data Protection Keychain, so the first load after pairing
    // deleted the credential it had just returned.
    @Test func repeatedLoadsKeepTheAuthoritativeItem() throws {
        let fake = FakeKeychain()
        let value = try credential()
        fake.items[.dataProtection] = try storedItem(value)
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        #expect(try store.load() == value)
        #expect(try store.load() == value)
        #expect(fake.items[.dataProtection] != nil)
        #expect(fake.calls.map(\.kind) == [.copy, .delete, .copy, .delete])
        #expect(fake.calls.map(\.backend) == [.dataProtection, .legacy, .dataProtection, .legacy])
    }

    @Test func failedLegacyCleanupBeforeReplacementPreservesAuthoritativeItem() throws {
        let fake = FakeKeychain()
        let existing = try credential()
        let existingItem = try storedItem(existing)
        fake.items[.dataProtection] = existingItem
        fake.items[.legacy] = existingItem
        fake.deleteStatuses[.legacy] = [errSecInteractionNotAllowed]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            try store.save(try credential(deviceID: "device-2", secretByte: 2))
            Issue.record("expected legacy cleanup to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecInteractionNotAllowed)
        }

        #expect(
            fake.items[.dataProtection]?[kSecValueData as String] as? Data
                == existingItem[kSecValueData as String] as? Data)
        #expect(fake.items[.legacy] != nil)
        #expect(fake.calls.map(\.kind) == [.delete])
        #expect(fake.calls.map(\.backend) == [.legacy])
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

    @Test func exactServiceLegacyItemMigratesCopyVerifyDelete() throws {
        let fake = FakeKeychain()
        let value = try credential()
        let legacy = try storedItem(value)
        fake.items[.legacy] = legacy
        let store = KeychainCredentialStore(
            service: "ai.freeside.tests.encoded%2Fpath", operations: fake.operations)

        #expect(try store.load() == value)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.items[.dataProtection] != nil)
        #expect(fake.calls.map(\.kind) == [.copy, .copy, .add, .copy, .delete])
        #expect(
            fake.calls.map(\.backend) == [
                .dataProtection, .legacy, .dataProtection, .dataProtection, .legacy,
            ])
        #expect(fake.calls.allSatisfy { $0.service == "ai.freeside.tests.encoded%2Fpath" })
        #expect(
            fake.addedAttributes[0][kSecValueData as String] as? Data
                == legacy[kSecValueData as String] as? Data)
    }

    @Test func failedLegacyCopyLeavesTheLegacyItemUntouched() throws {
        let fake = FakeKeychain()
        fake.items[.legacy] = try storedItem(credential())
        fake.addStatuses = [errSecAuthFailed]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        #expect(throws: KeychainCredentialStore.KeychainError.self) {
            try store.load()
        }
        #expect(fake.items[.legacy] != nil)
        #expect(fake.items[.dataProtection] == nil)
        #expect(fake.calls.map(\.kind) == [.copy, .copy, .add])
    }

    @Test func failedLegacyVerificationLeavesTheLegacyItemUntouched() throws {
        let fake = FakeKeychain()
        let legacy = try credential()
        fake.items[.legacy] = try storedItem(legacy)
        fake.copyOverrides[.dataProtection] = [
            (errSecItemNotFound, nil),
            (errSecSuccess, try storedItem(credential(deviceID: "device-2", secretByte: 2))),
        ]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            _ = try store.load()
            Issue.record("expected read-back mismatch to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecDecode)
        }
        #expect(fake.items[.legacy] != nil)
        #expect(fake.calls.map(\.kind) == [.copy, .copy, .add, .copy])
    }

    @Test func authoritativeCorruptionFailsClosedWithoutLegacyFallback() throws {
        let fake = FakeKeychain()
        fake.items[.dataProtection] = [
            kSecAttrAccount as String: "device-1",
            kSecValueData as String: Data("not-json".utf8),
        ]
        fake.items[.legacy] = try storedItem(credential())
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            _ = try store.load()
            Issue.record("expected corrupt authoritative item to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecDecode)
        }
        #expect(fake.calls.map(\.kind) == [.copy])
        #expect(fake.calls.map(\.backend) == [.dataProtection])
        #expect(fake.items[.legacy] != nil)
    }

    @Test func authoritativeErrorFailsClosedWithoutLegacyFallback() throws {
        let fake = FakeKeychain()
        fake.items[.legacy] = try storedItem(credential())
        fake.copyOverrides[.dataProtection] = [(errSecInteractionNotAllowed, nil)]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        do {
            _ = try store.load()
            Issue.record("expected authoritative Keychain error to fail")
        } catch let error as KeychainCredentialStore.KeychainError {
            #expect(error.status == errSecInteractionNotAllowed)
        }
        #expect(fake.calls.map(\.kind) == [.copy])
        #expect(fake.calls.map(\.backend) == [.dataProtection])
    }

    @Test func authoritativeDuplicateWinsAndCleansLegacy() throws {
        let fake = FakeKeychain()
        let authoritative = try credential()
        let conflictingLegacy = try credential(
            deviceID: "device-2", secretByte: 2,
            serverURL: "https://other-ntfy.example",
            topic: "fs-11111111111111111111111111111111")
        fake.items[.dataProtection] = try storedItem(authoritative)
        fake.items[.legacy] = try storedItem(conflictingLegacy)
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        #expect(try store.load() == authoritative)
        #expect(fake.items[.legacy] == nil)
        #expect(fake.calls.map(\.kind) == [.copy, .delete])
        #expect(fake.calls.map(\.backend) == [.dataProtection, .legacy])
    }

    @Test func interruptedCleanupFailsLoudAndRetries() throws {
        let fake = FakeKeychain()
        let value = try credential()
        fake.items[.dataProtection] = try storedItem(value)
        fake.items[.legacy] = try storedItem(value)
        fake.deleteStatuses[.legacy] = [errSecInteractionNotAllowed, errSecSuccess]
        let store = KeychainCredentialStore(service: "service", operations: fake.operations)

        #expect(throws: KeychainCredentialStore.KeychainError.self) {
            try store.load()
        }
        #expect(fake.items[.legacy] != nil)
        #expect(try store.load() == value)
        #expect(fake.items[.legacy] == nil)
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
