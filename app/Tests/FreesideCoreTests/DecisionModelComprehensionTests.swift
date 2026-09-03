import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

@testable import FreesideCore

/// A transport-level outage (no HTTP response), so a read fails the way an
/// unreachable daemon does rather than returning an authoritative status.
private struct MockOutage: Error {}

/// Holds the model a transport hook inspects mid-request; set after the client
/// (and thus the model) is built, read only on the main actor.
private final class ModelBox: @unchecked Sendable {
    var model: DecisionModel?
    var sawNonNilSurfaceDuringRefetch = false
}

/// Runs a hook while a chosen operation is in flight, to observe model state at
/// the network suspension point. Delegates everything else to the mock.
private struct HookOnOperationTransport: ClientTransport {
    let base: MockServerTransport
    let operation: String
    let onInFlight: @Sendable () async -> Void

    func send(
        _ request: HTTPRequest, body: HTTPBody?, baseURL: URL, operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        if operationID == operation {
            await onInFlight()
        }
        return try await base.send(
            request, body: body, baseURL: baseURL, operationID: operationID)
    }
}

@Suite @MainActor struct DecisionModelComprehensionTests {
    /// A store bound to a paired, enforcing device with its capability contract
    /// already registered, so the action surface derives for the same device
    /// the submitted command carries.
    private func pairedStore() async throws -> (store: InboxStore, itemID: String) {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let anon = APIClientFactory.mock(server: server)
        let grant = try await anon.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "d"))
        ).created.body.json
        let client = APIClientFactory.mock(server: server, token: { grant.device_token })
        let actions = Components.Schemas.Action.allCases.filter { ActionOutcome.of($0) != .pending }
        _ = try await client.registerCapabilityContract(
            path: .init(device_id: "device-1"), body: .json(.init(actions: actions))
        ).ok
        let store = InboxStore(client: client, device: DeviceIdentity(deviceID: "device-1"))
        await store.refresh()
        return (store, "item-ready_for_final_review")
    }

    @Test func cardOpenRecordsBeforeFetchingTheSurface() async throws {
        let (store, itemID) = try await pairedStore()
        let model = DecisionModel(store: store, itemID: itemID)
        // card_opened is enqueued synchronously at appearance, before any fetch.
        model.emitCardOpened()
        #expect(store.comprehensionQueue.contains { $0.input.kind == .card_opened })
        #expect(model.actionSurface == nil)
        // The surface fetch is a separate, later step.
        await model.refreshActionSurface()
        #expect(model.actionSurface != nil)
        await store.drainComprehensionEvents()
        #expect(store.comprehensionQueue.isEmpty)
    }

    @Test func cardOpenRecordsFromTheCachedDigestWithoutASurface() async throws {
        // A fast resolve-and-leave records the open even if the surface fetch
        // never runs: the event uses the item's cached decision-surface digest.
        let (store, itemID) = try await pairedStore()
        let model = DecisionModel(store: store, itemID: itemID)
        let cachedDigest = try #require(
            store.snapshotsByID[itemID]?.item.decision_surface.digest)
        model.emitCardOpened()
        let event = try #require(
            store.comprehensionQueue.first { $0.input.kind == .card_opened })
        #expect(event.input.item_decision_surface_digest == cachedDigest)
        // A second appearance does not double-record.
        model.emitCardOpened()
        #expect(store.comprehensionQueue.filter { $0.input.kind == .card_opened }.count == 1)
    }

    @Test func openCardRefetchesTheSurfaceWhenTheItemSurfaceChanges() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let anon = APIClientFactory.mock(server: server)
        let grant = try await anon.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "d"))
        ).created.body.json
        let client = APIClientFactory.mock(server: server, token: { grant.device_token })
        let actions = Components.Schemas.Action.allCases.filter { ActionOutcome.of($0) != .pending }
        _ = try await client.registerCapabilityContract(
            path: .init(device_id: "device-1"), body: .json(.init(actions: actions))
        ).ok
        let coordinator = SyncCoordinator(
            client: client, device: DeviceIdentity(deviceID: "device-1"),
            cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        let store = coordinator.store
        let itemID = "item-ready_for_final_review"
        let model = DecisionModel(store: store, itemID: itemID)
        await model.refreshActionSurface()
        let original = try #require(model.actionSurface)
        let originalItemDigest = try #require(
            store.snapshotsByID[itemID]?.item.decision_surface.digest)
        #expect(original.item_decision_surface_digest == originalItemDigest)

        // A daemon restore re-derives the item's decision surface while the card
        // stays open, returning the item under a new epoch with a new digest.
        var restored = try #require(store.snapshotsByID[itemID])
        let newDigest = "sha256:" + String(repeating: "e", count: 64)
        restored.item.decision_surface.digest = newDigest
        await server.restoreAttentionState(items: [restored], revision: 9000)
        await coordinator.heartbeat()
        #expect(store.snapshotsByID[itemID]?.item.decision_surface.digest == newDigest)

        // Reopening the card (its revalidationID reran) must refetch a surface
        // bound to the new digest, not reuse the stale one the daemon rejects.
        await model.refreshActionSurface()
        let refreshed = try #require(model.actionSurface)
        #expect(refreshed.item_decision_surface_digest == newDigest)
        #expect(refreshed.digest != original.digest)
    }

    @Test func actionTelemetryEmitsEvenWhenTheReadYourWriteRefetchFails() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let anon = APIClientFactory.mock(server: server)
        let grant = try await anon.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "d"))
        ).created.body.json
        let client = APIClientFactory.mock(server: server, token: { grant.device_token })
        let actions = Components.Schemas.Action.allCases.filter { ActionOutcome.of($0) != .pending }
        _ = try await client.registerCapabilityContract(
            path: .init(device_id: "device-1"), body: .json(.init(actions: actions))
        ).ok
        let store = InboxStore(client: client, device: DeviceIdentity(deviceID: "device-1"))
        await store.refresh()
        let itemID = "item-ready_for_final_review"
        let model = DecisionModel(store: store, itemID: itemID, openURL: { _ in true })
        model.emitCardOpened()
        await model.validate()
        await model.refreshActionSurface()
        await store.drainComprehensionEvents()
        #expect(store.comprehensionQueue.isEmpty)

        // The command is accepted, but the read-your-write refetch and the
        // telemetry drain both fail as transport outages. The action event must
        // still be enqueued (emitted from the validated result before the
        // refetch), so it survives in the queue for the next round instead of
        // being dropped on the failed-refetch settle path.
        await server.setBeforeRespond { op in
            if op == "getAttentionItem" || op == "recordComprehensionEvent" {
                throw MockOutage()
            }
        }
        await model.submit(.open_pr)
        await server.setBeforeRespond(nil)
        #expect(store.comprehensionQueue.contains { $0.input.kind == .action_taken })
    }

    @Test func refreshActionSurfaceClearsTheStaleSurfaceBeforeRefetching() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let anon = APIClientFactory.mock(server: server)
        let grant = try await anon.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "d"))
        ).created.body.json
        let box = ModelBox()
        let transport = HookOnOperationTransport(
            base: MockServerTransport(server: server), operation: "getActionSurface",
            onInFlight: {
                await MainActor.run {
                    if box.model?.actionSurface != nil {
                        box.sawNonNilSurfaceDuringRefetch = true
                    }
                }
            })
        // swift-format-ignore: NeverForceUnwrap
        let client = APIClientFactory.live(
            serverURL: URL(string: "https://freeside.invalid")!, transport: transport,
            token: { grant.device_token })
        let actions = Components.Schemas.Action.allCases.filter { ActionOutcome.of($0) != .pending }
        _ = try await client.registerCapabilityContract(
            path: .init(device_id: "device-1"), body: .json(.init(actions: actions))
        ).ok
        let coordinator = SyncCoordinator(
            client: client, device: DeviceIdentity(deviceID: "device-1"),
            cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        let store = coordinator.store
        let itemID = "item-ready_for_final_review"
        let model = DecisionModel(store: store, itemID: itemID)
        box.model = model
        await model.refreshActionSurface()
        #expect(model.actionSurface != nil)

        // The item's decision surface changes under a new epoch (a daemon
        // restore), so the next refresh must refetch.
        var restored = try #require(store.snapshotsByID[itemID])
        restored.item.decision_surface.digest = "sha256:" + String(repeating: "e", count: 64)
        await server.restoreAttentionState(items: [restored], revision: 9000)
        await coordinator.heartbeat()

        await model.refreshActionSurface()
        // Throughout the stale refetch the installed surface was nil, so a submit
        // racing it could never carry the superseded digest.
        #expect(box.sawNonNilSurfaceDuringRefetch == false)
        #expect(
            model.actionSurface?.item_decision_surface_digest
                == "sha256:" + String(repeating: "e", count: 64))
    }

    @Test func submitAttachesTheSurfaceDigestAndStampsEvidence() async throws {
        let (store, itemID) = try await pairedStore()
        let model = DecisionModel(store: store, itemID: itemID, openURL: { _ in true })
        model.emitCardOpened()
        await model.validate()
        await model.refreshActionSurface()
        let surface = try #require(model.actionSurface)
        await model.submit(.open_pr)
        let evidence = model.appliedRecord?.decision_evidence?.value1
        #expect(evidence?.action_surface_digest == surface.digest)
        // The card_opened and action_taken events drained.
        await store.drainComprehensionEvents()
        #expect(store.comprehensionQueue.isEmpty)
    }
}
