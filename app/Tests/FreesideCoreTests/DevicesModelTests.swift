import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

@testable import FreesideCore

/// Answers `revokeDevice` with the snapshot of a different device than the
/// one asked for: a daemon answer the client must not act on.
private struct MisdirectedRevokeTransport: ClientTransport {
    let base: MockServerTransport
    let answeredDeviceID: String

    func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        var request = request
        if operationID == "revokeDevice" {
            request.path = "/devices/\(answeredDeviceID)/revoke"
        }
        return try await base.send(request, body: body, baseURL: baseURL, operationID: operationID)
    }
}

private actor CallCounter {
    private(set) var counts: [String: Int] = [:]

    func record(_ operationID: String) {
        counts[operationID, default: 0] += 1
    }
}

@MainActor
private final class SignOutSpy {
    struct Refused: Error {}

    private(set) var calls = 0
    var fails = false

    func signOut() throws {
        calls += 1
        if fails { throw Refused() }
    }
}

@Suite @MainActor struct DevicesModelTests {
    private func coordinator(
        _ server: MockServer, transport: (any ClientTransport)? = nil,
        device: String = DeviceIdentity.mock.deviceID
    ) async -> SyncCoordinator {
        // swift-format-ignore: NeverForceUnwrap
        let client = Client(
            serverURL: URL(string: "https://freeside.invalid")!,
            transport: transport ?? MockServerTransport(server: server))
        let result = SyncCoordinator(
            client: client, device: DeviceIdentity(deviceID: device),
            cache: InMemoryCacheStore(), submissionDaemonID: "mock")
        await result.refresh()
        return result
    }

    @Test func listsEveryDeviceAndMarksTheCurrentOne() async throws {
        let spy = SignOutSpy()
        let model = DevicesModel(coordinator: await coordinator(MockServer()), signOut: spy.signOut)
        #expect(model.loadState == .loading)

        await model.loadIfStale()

        #expect(model.loadState == .loaded)
        let current = try #require(model.currentDevice)
        #expect(current.id == DeviceFixtures.currentDeviceID)
        #expect(current.isCurrent)
        #expect(current.lastSeenAt != nil)
        #expect(model.otherDevices.map(\.id) == [DeviceFixtures.otherDeviceID])
        #expect(model.otherDevices.allSatisfy { !$0.isCurrent })
        // A revoked device is grouped apart, and one the daemon never
        // recorded a request from has no last-seen instant.
        let revoked = try #require(model.revokedDevices.first)
        #expect(model.revokedDevices.count == 1)
        #expect(revoked.id == DeviceFixtures.revokedDeviceID)
        #expect(revoked.revokedAt != nil)
        #expect(revoked.lastSeenAt == nil)
        #expect(spy.calls == 0)
    }

    @Test func revokingAnotherDeviceMovesItToRevokedWithoutSigningOut() async throws {
        let spy = SignOutSpy()
        let coordinator = await coordinator(MockServer())
        let model = DevicesModel(coordinator: coordinator, signOut: spy.signOut)
        await model.load()

        await model.revoke(DeviceFixtures.otherDeviceID)

        #expect(model.revokeFailure == nil)
        #expect(model.revokingID == nil)
        #expect(model.otherDevices.isEmpty)
        #expect(
            model.revokedDevices.map(\.id) == [DeviceFixtures.otherDeviceID, DeviceFixtures.revokedDeviceID])
        #expect(model.currentDevice?.id == DeviceFixtures.currentDeviceID)
        #expect(spy.calls == 0)
        // The revoking write moved the server revision; the cache caught up,
        // so the client is not left reporting itself unvalidated.
        #expect(coordinator.store.freshness == .fresh)
    }

    @Test func revokingThisDeviceSignsOutAfterTheDaemonConfirms() async throws {
        let spy = SignOutSpy()
        let server = MockServer()
        let model = DevicesModel(coordinator: await coordinator(server), signOut: spy.signOut)
        await model.load()

        await model.revoke(DeviceFixtures.currentDeviceID)

        #expect(spy.calls == 1)
        #expect(model.revokeFailure == nil)
        guard case .revoked = await server.device(id: DeviceFixtures.currentDeviceID)?.device else {
            Issue.record("the daemon did not record the revocation")
            return
        }
    }

    @Test func aFailedRevocationOfThisDeviceNeverSignsOut() async throws {
        // Signing out deletes the credential, so anything short of the
        // daemon's 200 keeps it: here the request fails before commit.
        let spy = SignOutSpy()
        let server = MockServer()
        let transport = StatusOverrideTransport(
            base: MockServerTransport(server: server), operationID: "revokeDevice", status: 503,
            once: OneShot())
        let model = DevicesModel(
            coordinator: await coordinator(server, transport: transport), signOut: spy.signOut)
        await model.load()

        await model.revoke(DeviceFixtures.currentDeviceID)

        #expect(spy.calls == 0)
        #expect(model.revokeFailure == DevicesModel.revokeFailureMessage)
        guard case .active = await server.device(id: DeviceFixtures.currentDeviceID)?.device else {
            Issue.record("the failed request must not have revoked the device")
            return
        }
    }

    @Test func aSuccessfulReadClearsTheRevocationFailure() async throws {
        // The failure tells the operator to refresh; once the list has been
        // read again it shows the outcome, and the message would be stale.
        let server = MockServer()
        let transport = StatusOverrideTransport(
            base: MockServerTransport(server: server), operationID: "revokeDevice", status: 503,
            once: OneShot())
        let model = DevicesModel(coordinator: await coordinator(server, transport: transport), signOut: {})
        await model.load()
        await model.revoke(DeviceFixtures.otherDeviceID)
        #expect(model.revokeFailure == DevicesModel.revokeFailureMessage)

        await model.load()

        #expect(model.revokeFailure == nil)
        #expect(model.otherDevices.map(\.id) == [DeviceFixtures.otherDeviceID])
    }

    @Test func anAnswerNamingAnotherDeviceNeverSignsOut() async throws {
        // The returned snapshot is the authority for the destructive step:
        // a 200 that names a different device confirms nothing about this one.
        let spy = SignOutSpy()
        let server = MockServer()
        let transport = MisdirectedRevokeTransport(
            base: MockServerTransport(server: server), answeredDeviceID: DeviceFixtures.otherDeviceID)
        let model = DevicesModel(
            coordinator: await coordinator(server, transport: transport), signOut: spy.signOut)
        await model.load()

        await model.revoke(DeviceFixtures.currentDeviceID)

        #expect(spy.calls == 0)
        #expect(model.revokeFailure == DevicesModel.revokeFailureMessage)
    }

    @Test func aFailedSignOutIsSurfacedAndOffersRePairing() async throws {
        let spy = SignOutSpy()
        spy.fails = true
        let coordinator = await coordinator(MockServer())
        let model = DevicesModel(coordinator: coordinator, signOut: spy.signOut)
        await model.load()

        await model.revoke(DeviceFixtures.currentDeviceID)

        #expect(spy.calls == 1)
        #expect(model.revokeFailure == DevicesModel.signOutFailureMessage)
        // The daemon now refuses the credential that could not be removed,
        // so the banner's Pair Again is the retry.
        #expect(coordinator.store.freshness == .unauthenticated)
    }

    @Test func reloadsOnlyWhenTheHeartbeatObservesANewRevision() async throws {
        let server = MockServer()
        let counter = CallCounter()
        await server.setBeforeRespond { await counter.record($0) }
        let coordinator = await coordinator(server)
        let model = DevicesModel(coordinator: coordinator, signOut: {})
        await model.loadIfStale()
        #expect(await counter.counts["listDevices"] == 1)

        // Nothing moved: appearing again, or a sync round whose heartbeat
        // confirms the cache current, reads nothing.
        await coordinator.refresh()
        await model.loadIfStale()
        #expect(await counter.counts["listDevices"] == 1)

        // Another client pairs a device. The heartbeat observes the new
        // revision, and the list follows it.
        await server.seedPairingCode("483911")
        _ = try await coordinator.store.client.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPad"))
        ).created
        let before = model.observedRevision
        await coordinator.refresh()
        #expect(model.observedRevision != before)
        await model.loadIfStale()

        #expect(await counter.counts["listDevices"] == 2)
        #expect(model.otherDevices.map(\.name).contains("Ben's iPad"))
    }

    @Test func aRefusedCredentialClearsTheListAndFlagsThePairing() async throws {
        let server = MockServer()
        let transport = StatusOverrideTransport(
            base: MockServerTransport(server: server), operationID: "listDevices", status: 401,
            once: OneShot())
        let coordinator = await coordinator(server, transport: transport)
        let model = DevicesModel(coordinator: coordinator, signOut: {})

        await model.load()

        #expect(model.loadState == .failed(DevicesModel.unauthenticatedMessage))
        #expect(model.rows.isEmpty)
        #expect(coordinator.store.freshness == .unauthenticated)
    }

    @Test func aFailedReadShowsNoRows() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = DevicesModel(coordinator: coordinator, signOut: {})
        await model.load()
        #expect(!model.rows.isEmpty)

        await server.setBeforeRespond { operationID in
            if operationID == "listDevices" { throw MockServer.ForcedStatus(500) }
        }
        await model.load()

        #expect(model.loadState == .failed(DevicesModel.loadFailureMessage))
        #expect(model.rows.isEmpty)
    }

    @Test func confirmationCopyWarnsBeforeRevokingThisDevice() {
        let paired = Date(timeIntervalSince1970: 1_767_323_045)
        let current = DevicesModel.Row(
            id: "device-1", name: "Studio Mac", pairedAt: paired, lastSeenAt: nil, revokedAt: nil,
            isCurrent: true)
        let other = DevicesModel.Row(
            id: "device-2", name: "Ben's iPhone", pairedAt: paired, lastSeenAt: nil, revokedAt: nil,
            isCurrent: false)

        #expect(DevicesView.confirmationTitle(for: current) == "Revoke this device?")
        #expect(DevicesView.confirmationTitle(for: other) == "Revoke Ben's iPhone?")
        let own = DevicesView.confirmationMessage(for: current, unresolvedActionCount: 0)
        #expect(own.contains("signs out"))
        #expect(!own.contains("unresolved"))
        #expect(
            DevicesView.confirmationMessage(for: current, unresolvedActionCount: 2)
                .hasSuffix("2 unresolved actions made under this pairing won't be retried."))
        #expect(
            DevicesView.confirmationMessage(for: current, unresolvedActionCount: 1)
                .contains("1 unresolved action made"))
        // Revoking another device signs nothing out here.
        #expect(!DevicesView.confirmationMessage(for: other, unresolvedActionCount: 3).contains("signs out"))
    }
}
