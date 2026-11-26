import Foundation
import FreesideAPI
import FreesideTestSupport
import HTTPTypes
import OpenAPIRuntime
import Testing

@testable import FreesideCore
@testable import FreesideModel

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

/// Replaces one operation's response with raw JSON bytes so tests can exercise
/// forward-compatible `unknown` enum decoding on the wire.
private struct RawJSONTransport: ClientTransport {
    let base: MockServerTransport
    let operationID: String
    let json: String

    func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        if operationID == self.operationID {
            let response = HTTPResponse(
                status: .ok, headerFields: [.contentType: "application/json"])
            return (response, HTTPBody(Data(json.utf8)))
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

private actor InFlightGate {
    private var entered = false
    private var released = false
    private var enteredWaiter: CheckedContinuation<Void, Never>?
    private var releaseWaiter: CheckedContinuation<Void, Never>?

    func hold() async {
        entered = true
        enteredWaiter?.resume()
        enteredWaiter = nil
        if released { return }
        await withCheckedContinuation { releaseWaiter = $0 }
    }

    func waitUntilHeld() async {
        if entered { return }
        await withCheckedContinuation { enteredWaiter = $0 }
    }

    func release() {
        released = true
        releaseWaiter?.resume()
        releaseWaiter = nil
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

    @Test func turningNotificationsOnAndOffFollowsTheDaemon() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = DevicesModel(coordinator: coordinator, signOut: {})
        await model.load()
        // The daemon's default: this device is off until someone turns it on.
        #expect(!model.currentDeviceNotificationsEnabled)

        await model.setNotifications(DeviceFixtures.currentDeviceID, enabled: true)

        #expect(model.notificationsFailure == nil)
        #expect(model.notificationsChangingID == nil)
        #expect(model.currentDeviceNotificationsEnabled)
        // The change moved the server revision; the cache caught up.
        #expect(coordinator.store.freshness == .fresh)

        await model.setNotifications(DeviceFixtures.otherDeviceID, enabled: false)

        #expect(model.notificationsFailure == nil)
        #expect(model.otherDevices.map(\.notificationsEnabled) == [false])
        #expect(model.currentDeviceNotificationsEnabled)
    }

    @Test func aRefusedCredentialWhileSettingNotificationsFlagsThePairing() async throws {
        let server = MockServer()
        let transport = StatusOverrideTransport(
            base: MockServerTransport(server: server), operationID: "setDeviceNotifications", status: 401,
            once: OneShot())
        let coordinator = await coordinator(server, transport: transport)
        let model = DevicesModel(coordinator: coordinator, signOut: {})
        await model.load()

        await model.setNotifications(DeviceFixtures.currentDeviceID, enabled: true)

        #expect(model.loadState == .failed(DevicesModel.unauthenticatedMessage))
        #expect(model.rows.isEmpty)
        #expect(coordinator.store.freshness == .unauthenticated)
    }

    @Test func aFailedNotificationsChangeKeepsTheStoredSwitchAndSaysSo() async throws {
        let server = MockServer()
        let coordinator = await coordinator(server)
        let model = DevicesModel(coordinator: coordinator, signOut: {})
        await model.load()
        await server.setBeforeRespond { operationID in
            if operationID == "setDeviceNotifications" { throw MockServer.ForcedStatus(503) }
        }

        await model.setNotifications(DeviceFixtures.currentDeviceID, enabled: true)

        #expect(model.notificationsFailure == DevicesModel.notificationsFailureMessage)
        #expect(model.notificationsChangingID == nil)
        #expect(!model.currentDeviceNotificationsEnabled)

        // A request that never reaches the daemon reads the same way.
        await model.load()
        await server.setBeforeRespond { operationID in
            if operationID == "setDeviceNotifications" { throw URLError(.notConnectedToInternet) }
        }
        await model.setNotifications(DeviceFixtures.currentDeviceID, enabled: true)
        #expect(model.notificationsFailure == DevicesModel.notificationsFailureMessage)
        #expect(!model.currentDeviceNotificationsEnabled)

        // Reading the list again answers the failure's ask.
        await server.setBeforeRespond(nil)
        await model.load()
        #expect(model.notificationsFailure == nil)
    }

    @Test func aRevokedDeviceHasNoSwitchToSet() async throws {
        let server = MockServer()
        let model = DevicesModel(coordinator: await coordinator(server), signOut: {})
        await model.load()

        await model.setNotifications(DeviceFixtures.revokedDeviceID, enabled: true)

        #expect(model.notificationsFailure == DevicesModel.notificationsFailureMessage)
        #expect(model.revokedDevices.map(\.notificationsEnabled) == [false])
    }

    @Test func confirmationCopyWarnsBeforeRevokingThisDevice() {
        let paired = Date(timeIntervalSince1970: 1_767_323_045)
        let current = DevicesModel.Row(
            id: "device-1", name: "Studio Mac", pairedAt: paired, lastSeenAt: nil, revokedAt: nil,
            isCurrent: true, notificationsEnabled: false)
        let other = DevicesModel.Row(
            id: "device-2", name: "Ben's iPhone", pairedAt: paired, lastSeenAt: nil, revokedAt: nil,
            isCurrent: false, notificationsEnabled: false)

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

    @Test func testNotificationServedFollowsDaemonHealthFeatures() async {
        let server = MockServer()
        let served = DevicesModel(coordinator: await coordinator(server), signOut: {})
        #expect(served.testNotificationServed)

        let older = MockServer()
        await older.setHealthFeatures(
            MockServer.servedFeatures.filter { $0 != .device_test_notification })
        let unserved = DevicesModel(coordinator: await coordinator(older), signOut: {})
        #expect(!unserved.testNotificationServed)
    }

    @Test func testNotificationMapsEveryOutcomeAndLeavesNotificationOpenedUnchanged() async {
        let server = MockServer()
        let opens = NotificationOpenLog(defaults: nil)
        let model = DevicesModel(
            coordinator: await coordinator(server), notificationOpens: opens, signOut: {})
        await model.load()

        // Accepted: shows accepted message and never records an opened notification.
        await model.sendTestNotification()
        #expect(model.testNotificationState == .finished(.accepted(DevicesModel.testNotificationAcceptedMessage)))
        #expect(opens.openedAt(deviceID: DeviceFixtures.currentDeviceID) == nil)

        // Provider rejected with status.
        await server.setTestNotificationAnswer(
            .outcome(.init(outcome: .provider_rejected, provider_status: 403, transport_failure: nil)))
        await model.sendTestNotification()
        #expect(
            model.testNotificationState
                == .finished(.failed("ntfy refused the test notification (HTTP 403).")))

        // Transport failed with failure class.
        await server.setTestNotificationAnswer(
            .outcome(.init(outcome: .transport_failed, provider_status: nil, transport_failure: "request timed out")))
        await model.sendTestNotification()
        #expect(
            model.testNotificationState
                == .finished(.failed("Couldn't reach ntfy: request timed out.")))

        // No notifier configured.
        await server.setTestNotificationAnswer(
            .outcome(.init(outcome: .notifier_unavailable, provider_status: nil, transport_failure: nil)))
        await model.sendTestNotification()
        #expect(model.testNotificationState == .finished(.failed(DevicesModel.testNotificationUnavailableMessage)))

        // Too recent (429 with Retry-After).
        await server.setTestNotificationAnswer(.tooRecent(retryAfter: 25))
        await model.sendTestNotification()
        #expect(
            model.testNotificationState
                == .finished(.failed("A test notification was just sent. Try again in 25 seconds.")))

        // 403 (revoked after auth) and 404 (unrecognized device).
        await server.setBeforeRespond { operationID in
            if operationID == "sendDeviceTestNotification" { throw MockServer.ForcedStatus(403) }
        }
        await model.sendTestNotification()
        #expect(model.testNotificationState == .finished(.failed(DevicesModel.testNotificationInactiveMessage)))

        await server.setBeforeRespond { operationID in
            if operationID == "sendDeviceTestNotification" { throw MockServer.ForcedStatus(404) }
        }
        await model.sendTestNotification()
        #expect(model.testNotificationState == .finished(.failed(DevicesModel.testNotificationNotFoundMessage)))

        // Thrown transport error (daemon unreachable).
        await server.setBeforeRespond { operationID in
            if operationID == "sendDeviceTestNotification" { throw URLError(.notConnectedToInternet) }
        }
        await model.sendTestNotification()
        #expect(model.testNotificationState == .finished(.failed(DevicesModel.testNotificationUnreachableMessage)))

        // Unknown outcome from a newer daemon decodes via tolerant enum.
        let futureServer = MockServer()
        let rawTransport = RawJSONTransport(
            base: MockServerTransport(server: futureServer), operationID: "sendDeviceTestNotification",
            json: #"{"outcome":"future_outcome","provider_status":null,"transport_failure":null}"#)
        let futureModel = DevicesModel(
            coordinator: await coordinator(futureServer, transport: rawTransport), signOut: {})
        await futureModel.sendTestNotification()
        #expect(
            futureModel.testNotificationState
                == .finished(.failed(DevicesModel.testNotificationUnknownOutcomeMessage)))
    }

    @Test func testNotificationIgnoresOverlappingCallsWhileInFlight() async {
        let server = MockServer()
        let counter = CallCounter()
        let gate = InFlightGate()
        await server.setBeforeRespond { operationID in
            guard operationID == "sendDeviceTestNotification" else { return }
            await counter.record(operationID)
            await gate.hold()
        }
        let model = DevicesModel(coordinator: await coordinator(server), signOut: {})
        await model.load()

        let first = Task { await model.sendTestNotification() }
        await gate.waitUntilHeld()
        #expect(model.testNotificationState == .sending)

        // Second call while the first is running does nothing.
        await model.sendTestNotification()
        #expect(await counter.counts["sendDeviceTestNotification"] == 1)

        await gate.release()
        await first.value
        #expect(model.testNotificationState == .finished(.accepted(DevicesModel.testNotificationAcceptedMessage)))
        #expect(await counter.counts["sendDeviceTestNotification"] == 1)
    }

    @Test func aRefusedCredentialWhileSendingTestNotificationFlagsThePairing() async {
        let server = MockServer()
        let transport = StatusOverrideTransport(
            base: MockServerTransport(server: server), operationID: "sendDeviceTestNotification", status: 401,
            once: OneShot())
        let coordinator = await coordinator(server, transport: transport)
        let model = DevicesModel(coordinator: coordinator, signOut: {})
        await model.load()

        await model.sendTestNotification()

        #expect(model.loadState == .failed(DevicesModel.unauthenticatedMessage))
        #expect(model.rows.isEmpty)
        #expect(coordinator.store.freshness == .unauthenticated)
    }
}
