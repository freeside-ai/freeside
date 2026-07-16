import Foundation
import FreesideAPI
import FreesideCore
import Testing

/// A paired device against an enforcing server: the real credential
/// flow, minus UI.
@MainActor
private func pairedStore(server: MockServer) async throws -> InboxStore {
    await server.seedPairingCode("483911")
    let grant = try await APIClientFactory.mock(server: server).pairDevice(
        body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
    ).created.body.json
    guard case .active(let active) = grant.device.device else {
        Issue.record("expected an active device")
        throw MockOutage()
    }
    let client = APIClientFactory.mock(server: server) { grant.device_token }
    let store = InboxStore(
        client: client, device: DeviceIdentity(deviceID: active.id))
    await store.refresh()
    return store
}

private struct MockOutage: Error {}

/// Revocation honesty, client halves of plan §5.14 sync tests 15-16:
/// a revoked device stops acting, says so, and never fabricates or
/// destroys a command outcome to smooth it over.
@Suite @MainActor struct DecisionRevocationTests {
    @Test func revokedDeviceCannotSubmitAPreparedCommand() async throws {
        // Test 15: revocation lands between preparing the decision and
        // submitting it. The fresh submission dies at the credential
        // gate before any acceptance, so nothing renders applied,
        // nothing stays pending, and the device state surfaces.
        let server = MockServer(authMode: .enforcing)
        let store = try await pairedStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)
        let before = await server.snapshot(itemID: "item-spec_approval")

        _ = try await store.client.revokeDevice(path: .init(device_id: store.device.deviceID)).ok

        await model.submit(.approve)

        #expect(model.phase == .idle)
        #expect(model.appliedRecord == nil)
        #expect(model.submissionError != nil)
        #expect(store.pendingCommandsByItemID["item-spec_approval"] == nil)
        #expect(store.freshness == .unauthenticated)
        #expect(!model.actionsEnabled)
        // No side effect on the daemon.
        #expect(await server.snapshot(itemID: "item-spec_approval") == before)
    }

    @Test func revokedRetryRendersTheRecordedResultWithoutNewSideEffect() async throws {
        // Test 16, recorded-result branch: the command committed but its
        // response was lost, revocation lands while it is unresolved,
        // and the verbatim retry is served the recorded result. The
        // client renders that record; the daemon state does not move
        // again.
        let server = MockServer(authMode: .enforcing)
        let store = try await pairedStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()

        // Every submission response is lost after the handler applies,
        // so the command commits server-side while the client stays
        // ambiguous through the automatic resend too.
        await server.setAfterRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        await model.submit(.approve)
        #expect(store.pendingCommandsByItemID["item-spec_approval"]?.state == .unresolved)
        await server.setAfterRespond(nil)
        let committed = await server.snapshot(itemID: "item-spec_approval")
        #expect(committed?.item.status == .resolved)

        _ = try await store.client.revokeDevice(path: .init(device_id: store.device.deviceID)).ok

        #expect(model.canRetryLostResponse)
        await model.retryLostResponse()

        // The recorded result renders; the slot settles; nothing new
        // committed (the item is exactly as the original acceptance
        // left it).
        #expect(model.appliedRecord?.action == .approve)
        #expect(store.pendingCommandsByItemID["item-spec_approval"] == nil)
        #expect(await server.snapshot(itemID: "item-spec_approval") == committed)
        #expect(!model.actionsEnabled)
    }

    @Test func revokedRetryOfAnUncommittedCommandStaysAmbiguousNotFalselyRejected() async throws {
        // Test 16's other face: the original attempt never reached the
        // daemon, so the revoked retry gets the credential gate's 401 -
        // which proves nothing about commitment. The slot must stay
        // held rather than settle as "not recorded" on the strength of
        // an auth failure.
        let server = MockServer(authMode: .enforcing)
        let store = try await pairedStore(server: server)
        let model = DecisionModel(store: store, itemID: "item-spec_approval")
        await model.validate()
        let before = await server.snapshot(itemID: "item-spec_approval")

        await server.setBeforeRespond { operationID in
            if operationID == "submitCommand" { throw MockOutage() }
        }
        await model.submit(.approve)
        #expect(store.pendingCommandsByItemID["item-spec_approval"]?.state == .unresolved)
        await server.setBeforeRespond(nil)

        _ = try await store.client.revokeDevice(path: .init(device_id: store.device.deviceID)).ok

        #expect(model.canRetryLostResponse)
        await model.retryLostResponse()

        #expect(store.pendingCommandsByItemID["item-spec_approval"]?.state == .unresolved)
        #expect(model.appliedRecord == nil)
        #expect(store.freshness == .unauthenticated)
        #expect(!model.actionsEnabled)
        #expect(await server.snapshot(itemID: "item-spec_approval") == before)
    }

    @Test func actionsDisableWhileUnreachableAndRecover() async throws {
        // Acceptance 4: a validated card goes read-only the moment the
        // sync layer reports the daemon unreachable, and recovers with
        // it, with no per-card revalidation required either way.
        let server = MockServer()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        let model = DecisionModel(store: coordinator.store, itemID: "item-spec_approval")
        await model.validate()
        #expect(model.actionsEnabled)

        await server.setBeforeRespond { _ in throw MockOutage() }
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .unreachable)
        #expect(!model.actionsEnabled)

        await server.setBeforeRespond(nil)
        await coordinator.heartbeat()
        #expect(coordinator.store.freshness == .fresh)
        #expect(model.actionsEnabled)
    }
}
