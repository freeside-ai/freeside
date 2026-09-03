import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

@testable import FreesideCore

/// Holds the store the hook mutates: the transport is built before the store
/// exists, so the store is set afterward and read only on the main actor.
private final class StoreBox: @unchecked Sendable {
    var store: InboxStore?
}

/// Enqueues one telemetry event the moment a record request is in flight,
/// modelling a concurrent enqueue that lands while a drain is suspended at the
/// network await. Everything but that one seam delegates to the mock transport.
private struct EnqueueDuringRecordTransport: ClientTransport {
    let base: MockServerTransport
    let onRecord: @Sendable () async -> Void

    func send(
        _ request: HTTPRequest, body: HTTPBody?, baseURL: URL, operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        if operationID == "recordComprehensionEvent" {
            await onRecord()
        }
        return try await base.send(
            request, body: body, baseURL: baseURL, operationID: operationID)
    }
}

/// Answers every recordComprehensionEvent with a fixed HTTP response, to drive
/// the drain's poison-vs-retry classification. Any other operation gets a bare
/// 200 (the tests below never issue one).
private struct RecordStatusTransport: ClientTransport {
    let status: HTTPResponse.Status
    let body: HTTPBody?

    func send(
        _ request: HTTPRequest, body: HTTPBody?, baseURL: URL, operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        if operationID == "recordComprehensionEvent" {
            let response = HTTPResponse(
                status: status,
                headerFields: self.body == nil ? [:] : [.contentType: "application/json"])
            return (response, self.body)
        }
        return (HTTPResponse(status: .ok), nil)
    }
}

@Suite @MainActor struct ComprehensionTelemetryTests {
    private let surfaceDigest = "sha256:" + String(repeating: "a", count: 64)

    private func storeWithRecordStatus(
        _ status: HTTPResponse.Status, body: HTTPBody? = nil
    ) -> InboxStore {
        // swift-format-ignore: NeverForceUnwrap
        let client = APIClientFactory.live(
            serverURL: URL(string: "https://freeside.invalid")!,
            transport: RecordStatusTransport(status: status, body: body))
        return InboxStore(client: client)
    }

    @Test func drainRetainsAnEventAfterATransient5xx() async {
        // A 5xx is retryable: the event must survive the drain for the next
        // round rather than being dropped as poison.
        let store = storeWithRecordStatus(.internalServerError)
        store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: "item-1", itemDecisionSurfaceDigest: surfaceDigest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        await store.drainComprehensionEvents()
        #expect(store.comprehensionQueue.count == 1)
    }

    @Test func drainDropsAnEventAfterADefinitive4xx() async {
        // A documented 400 is a definitive client rejection (malformed or
        // unbacked): drop it so the queue stops looping on it.
        let store = storeWithRecordStatus(
            .badRequest, body: HTTPBody(#"{"message":"unbacked event"}"#))
        store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: "item-1", itemDecisionSurfaceDigest: surfaceDigest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        await store.drainComprehensionEvents()
        #expect(store.comprehensionQueue.isEmpty)
    }

    @Test func enqueueIncrementsTheSequenceAndQueues() async {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: "item-1", itemDecisionSurfaceDigest: surfaceDigest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        store.enqueueComprehensionEvent(
            kind: .not_decidable_here_shown, itemID: "item-1",
            itemDecisionSurfaceDigest: surfaceDigest, decisionActionSurfaceDigest: nil,
            commandID: nil)
        #expect(store.comprehensionQueue.count == 2)
        #expect(store.comprehensionQueue.map(\.input.sequence) == [1, 2])
        // Every event carries a distinct client idempotency key.
        #expect(Set(store.comprehensionQueue.map(\.eventID)).count == 2)
    }

    @Test func drainClearsTheQueueOnSuccess() async {
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        await store.refresh()
        // A seeded inbox item the event can reference.
        let itemID = store.rows.first?.item.id ?? "item-spec_approval"
        store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: itemID, itemDecisionSurfaceDigest: surfaceDigest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        await store.drainComprehensionEvents()
        #expect(store.comprehensionQueue.isEmpty)
    }

    @Test func queueAndFingerprintSurviveRelaunchThroughTheCache() async {
        let cache = InMemoryCacheStore()
        let first = SyncCoordinator(
            client: APIClientFactory.mock(server: MockServer()), cache: cache)
        first.store.registeredCapabilityFingerprint = "approve,discuss"
        first.store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: "item-1", itemDecisionSurfaceDigest: surfaceDigest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        #expect(cache.load()?.comprehensionQueue?.isEmpty == false)

        let second = SyncCoordinator(
            client: APIClientFactory.mock(server: MockServer()), cache: cache)
        #expect(second.store.comprehensionQueue.count == 1)
        #expect(second.store.registeredCapabilityFingerprint == "approve,discuss")
    }

    @Test func repairDropsAnotherDevicesQueueAndFingerprint() async {
        // A deployment cache keyed by server URL is reused across a re-pair. The
        // prior device's telemetry carries no device id of its own, so the
        // owning-device stamp is the only thing that tells it apart.
        let cache = InMemoryCacheStore()
        let first = SyncCoordinator(
            client: APIClientFactory.mock(server: MockServer()),
            device: DeviceIdentity(deviceID: "device-A"), cache: cache)
        first.store.registeredCapabilityFingerprint = "approve,discuss"
        first.store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: "item-1", itemDecisionSurfaceDigest: surfaceDigest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        #expect(cache.load()?.comprehensionQueue?.isEmpty == false)
        #expect(cache.load()?.comprehensionDeviceID == "device-A")

        let second = SyncCoordinator(
            client: APIClientFactory.mock(server: MockServer()),
            device: DeviceIdentity(deviceID: "device-B"), cache: cache)
        // Neither the foreign queue nor the foreign fingerprint is adopted, so
        // device-B resends nothing under its credential and re-registers its own
        // contract instead of assuming device-A's still holds.
        #expect(second.store.comprehensionQueue.isEmpty)
        #expect(second.store.registeredCapabilityFingerprint == nil)
    }

    @Test func sameDeviceRelaunchStillRestoresItsOwnState() async {
        // The owning-device guard must not reject the ordinary same-device
        // relaunch that the queue-survives-relaunch contract depends on.
        let cache = InMemoryCacheStore()
        let device = DeviceIdentity(deviceID: "device-1")
        let first = SyncCoordinator(
            client: APIClientFactory.mock(server: MockServer()), device: device, cache: cache)
        first.store.registeredCapabilityFingerprint = "approve,discuss"
        first.store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: "item-1", itemDecisionSurfaceDigest: surfaceDigest,
            decisionActionSurfaceDigest: nil, commandID: nil)

        let second = SyncCoordinator(
            client: APIClientFactory.mock(server: MockServer()), device: device, cache: cache)
        #expect(second.store.comprehensionQueue.count == 1)
        #expect(second.store.registeredCapabilityFingerprint == "approve,discuss")
    }

    @Test func epochEvictionClearsTheRegisteredFingerprint() async {
        // A sync-epoch change can be a daemon restore that rolled the capability
        // row back, so a surviving fingerprint would suppress the re-registration
        // the restored daemon needs before any action surface derives.
        let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
        store.registeredCapabilityFingerprint = "approve,discuss"
        store.discardSnapshots()
        #expect(store.registeredCapabilityFingerprint == nil)
    }

    @Test func drainKeepsAnEventEnqueuedWhileAnotherWasInFlight() async {
        // Two drains overlap across the network await: an event enqueued while
        // the first drain is suspended must survive that drain settling its own
        // snapshot, which a whole-queue replacement would clobber.
        let itemID = "item-spec_approval"
        let digest = surfaceDigest
        let box = StoreBox()
        let transport = EnqueueDuringRecordTransport(
            base: MockServerTransport(server: MockServer()),
            onRecord: {
                await MainActor.run {
                    box.store?.enqueueComprehensionEvent(
                        kind: .card_opened, itemID: itemID, itemDecisionSurfaceDigest: digest,
                        decisionActionSurfaceDigest: nil, commandID: nil)
                }
            })
        // swift-format-ignore: NeverForceUnwrap
        let client = APIClientFactory.live(
            serverURL: URL(string: "https://freeside.invalid")!, transport: transport)
        let store = InboxStore(client: client)
        box.store = store
        await store.refresh()
        store.enqueueComprehensionEvent(
            kind: .card_opened, itemID: itemID, itemDecisionSurfaceDigest: digest,
            decisionActionSurfaceDigest: nil, commandID: nil)
        #expect(store.comprehensionQueue.count == 1)

        await store.drainComprehensionEvents()
        // The first event settled and left; the second, enqueued mid-flight,
        // remains (its per-device sequence is 2).
        #expect(store.comprehensionQueue.map(\.input.sequence) == [2])
    }

    @Test func capabilityRegistersAtSessionStart() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let anon = APIClientFactory.mock(server: server)
        let grant = try await anon.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "d"))
        ).created.body.json
        let client = APIClientFactory.mock(server: server, token: { grant.device_token })
        let coordinator = SyncCoordinator(
            client: client, device: DeviceIdentity(deviceID: "device-1"),
            cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        // Session start registered the contract and cached its fingerprint, so a
        // later action surface derives instead of returning 409.
        #expect(coordinator.store.registeredCapabilityFingerprint != nil)
        let itemID = coordinator.store.rows.first?.item.id ?? "item-spec_approval"
        let surface = try await client.getActionSurface(path: .init(item_id: itemID))
        if case .ok = surface {
        } else {
            Issue.record("action surface did not derive after session-start registration")
        }
    }
}
