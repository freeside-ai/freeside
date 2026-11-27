import Foundation
import FreesideAPI
import Observation

/// The Devices screen's state: the daemon's paired devices, read through
/// `GET /devices`, and the two actions the screen offers, revocation and
/// each device's notifications switch.
///
/// The list is a partial read outside bootstrap, so the model owns it and
/// never caches it: a last-seen instant changes without moving the server
/// revision, and a stale list would misreport who can still act.
@MainActor
@Observable
package final class DevicesModel {
    package enum LoadState: Equatable {
        case loading
        case loaded
        case failed(String)
    }

    /// The state of sending a test notification to this device.
    package enum TestNotificationState: Equatable {
        case idle
        case sending
        case finished(TestNotificationOutcome)
    }

    /// What a test notification request answered, for the phone's setup step.
    /// `accepted` is ntfy's acceptance, never delivery to the phone.
    package enum TestNotificationOutcome: Equatable {
        case accepted(String)
        case failed(String)

        package var message: String {
            switch self {
            case .accepted(let message), .failed(let message): message
            }
        }

        package var isFailure: Bool {
            if case .failed = self { true } else { false }
        }
    }

    /// One listed device, flattened from the contract's active and revoked
    /// variants.
    package struct Row: Identifiable, Equatable {
        package let id: String
        package let name: String
        package let pairedAt: Date
        /// Nil when the daemon has recorded no authenticated request.
        package let lastSeenAt: Date?
        /// Non-nil exactly when the device is revoked.
        package let revokedAt: Date?
        package let isCurrent: Bool
        /// Whether the daemon notifies this device. Off until an operator
        /// turns it on; a revoked device reads off.
        package let notificationsEnabled: Bool

        package var isRevoked: Bool { revokedAt != nil }
    }

    private(set) var rows: [Row] = []
    package private(set) var loadState: LoadState = .loading
    package private(set) var revokingID: String?
    package private(set) var revokeFailure: String?
    /// The device whose notifications switch is being set. One change runs
    /// at a time, and the switch shows the requested value meanwhile.
    package private(set) var notificationsChangingID: String?
    package private(set) var notificationsFailure: String?
    /// State of the phone's test-notification control.
    package private(set) var testNotificationState: TestNotificationState = .idle

    static let loadFailureMessage = "Couldn't load devices. Check the connection to the daemon and try again."
    static let unauthenticatedMessage = "This device is no longer paired with the daemon."
    static let revokeFailureMessage =
        "The daemon didn't confirm the revocation. Refresh to see whether the device is still paired."
    static let notificationsFailureMessage =
        "The daemon didn't confirm the change to notifications. Refresh to see the current setting."
    static let signOutFailureMessage =
        "This device was revoked, but its stored credential couldn't be removed. Close this sheet and choose Pair Again."
    static let testNotificationAcceptedMessage = "Accepted by ntfy. Tap the notification when it arrives."
    static let testNotificationUnavailableMessage = "This daemon doesn't have notifications configured."
    static let testNotificationUnknownOutcomeMessage =
        "The daemon returned a test-notification result this app doesn't recognize."
    static let testNotificationTooRecentMessage = "A test notification was just sent. Wait a moment and try again."
    static let testNotificationInactiveMessage = "This device is no longer active on the daemon."
    static let testNotificationNotFoundMessage = "The daemon doesn't recognize this device."
    static let testNotificationUnreachableMessage = "Couldn't reach the daemon to send a test notification."

    private let coordinator: SyncCoordinator
    private let signOut: @MainActor () throws -> Void
    private var loadGeneration = 0
    /// The observed revision the rows were read at, so a heartbeat that
    /// reports a newer one reloads and the model's own read does not.
    private var loadedAtRevision: Int64?

    /// Where this device's notifications are published, shown on its own
    /// card so the operator can subscribe to it in the ntfy app. Nil on a
    /// platform that takes no notification link (the Mac). The topic is a
    /// secret: whoever holds it reads this device's notifications.
    package let notificationSubscription: DeviceNtfySubscription?
    /// Where this device's card reads when a notification was last opened
    /// here. Read when the card draws, so a test notification tapped while
    /// the sheet is open shows at once.
    package let notificationOpens: NotificationOpenLog?

    /// `signOut` deletes this device's credential and returns to pairing. It
    /// runs only after the daemon confirms this device's own revocation.
    package init(
        coordinator: SyncCoordinator, notificationSubscription: DeviceNtfySubscription? = nil,
        notificationOpens: NotificationOpenLog? = nil,
        signOut: @escaping @MainActor () throws -> Void
    ) {
        self.coordinator = coordinator
        self.notificationSubscription = notificationSubscription
        self.notificationOpens = notificationOpens
        self.signOut = signOut
    }

    var currentDeviceID: String { coordinator.store.device.deviceID }
    package var observedRevision: Int64? { coordinator.cursors?.highestObservedServerRevision }

    package var currentDevice: Row? { rows.first { $0.isCurrent && !$0.isRevoked } }
    package var otherDevices: [Row] { rows.filter { !$0.isCurrent && !$0.isRevoked } }
    package var revokedDevices: [Row] { rows.filter(\.isRevoked) }
    /// Off until the list has loaded: the daemon's default.
    package var currentDeviceNotificationsEnabled: Bool { currentDevice?.notificationsEnabled ?? false }
    /// Whether the daemon serves `POST /devices/{device_id}/test-notification`
    /// (`GET /health`'s `features` lists `device_test_notification`).
    package var testNotificationServed: Bool { coordinator.daemonFeatures.contains(.device_test_notification) }

    /// Loads on first appearance and again whenever the observed server
    /// revision has moved since the rows were read: pairing and revocation
    /// are synchronized writes, so the heartbeat reports them.
    package func loadIfStale() async {
        guard loadState != .loaded || loadedAtRevision != observedRevision else { return }
        await load()
    }

    package func load() async {
        loadGeneration += 1
        let generation = loadGeneration
        if rows.isEmpty { loadState = .loading }
        do {
            let output = try await coordinator.store.client.listDevices()
            guard generation == loadGeneration else { return }
            switch output {
            case .ok(let ok):
                let entries = try ok.body.json
                rows = entries.map { Self.row($0, currentDeviceID: currentDeviceID) }
                if let revision = entries.map(\.as_of_revision).max() {
                    coordinator.observe(revision: revision)
                }
                loadedAtRevision = observedRevision
                loadState = .loaded
                // The failures ask for this read; the rows now answer them.
                revokeFailure = nil
                notificationsFailure = nil
            case .undocumented(let status, _):
                fail(status: status)
            }
        } catch {
            guard generation == loadGeneration, !(error is CancellationError), !Task.isCancelled else { return }
            rows = []
            loadState = .failed(Self.loadFailureMessage)
        }
    }

    /// Revokes a device. Revoking this device signs the app out, and only
    /// once the daemon's answer names this device as revoked: deleting the
    /// credential cannot be undone, so no other answer reaches `signOut`.
    package func revoke(_ id: String) async {
        guard revokingID == nil else { return }
        revokingID = id
        revokeFailure = nil
        defer { revokingID = nil }
        do {
            let output = try await coordinator.store.client.revokeDevice(path: .init(device_id: id))
            switch output {
            case .ok(let ok):
                let snapshot = try ok.body.json
                guard case .revoked(let revoked) = snapshot.device, revoked.id == id else {
                    revokeFailure = Self.revokeFailureMessage
                    return
                }
                if id == currentDeviceID {
                    do {
                        try signOut()
                    } catch {
                        // The daemon now refuses this credential, so the
                        // banner's Pair Again is the way to retry removal.
                        coordinator.store.publish(.unauthenticated)
                        revokeFailure = Self.signOutFailureMessage
                    }
                    return
                }
                // Revocation is a synchronized write this client made, so
                // the cache catches up before the list is read again.
                _ = await coordinator.refreshAfterCommit()
                await load()
            case .notFound:
                await load()
                revokeFailure = Self.revokeFailureMessage
            case .undocumented(let status, _):
                if status == 401 {
                    fail(status: status)
                } else {
                    revokeFailure = Self.revokeFailureMessage
                }
            }
        } catch {
            revokeFailure = Self.revokeFailureMessage
        }
    }

    /// Turns a device's notifications on or off. The daemon's answer is the
    /// row; a change moves the server revision, so the cache catches up and
    /// the list is read again, as after a revocation.
    package func setNotifications(_ id: String, enabled: Bool) async {
        guard notificationsChangingID == nil else { return }
        notificationsChangingID = id
        notificationsFailure = nil
        // Released early on success, after which another change may start;
        // the defer must not clear that one's marker.
        var holdsMarker = true
        defer { if holdsMarker { notificationsChangingID = nil } }
        do {
            let output = try await coordinator.store.client.setDeviceNotifications(
                path: .init(device_id: id), body: .json(.init(enabled: enabled)))
            switch output {
            case .ok(let ok):
                let entry = try ok.body.json
                let row = Self.row(entry, currentDeviceID: currentDeviceID)
                if let index = rows.firstIndex(where: { $0.id == row.id }) {
                    rows[index] = row
                }
                // The row now holds the answer, so the switch reads it rather
                // than the requested value through the reload below.
                notificationsChangingID = nil
                holdsMarker = false
                _ = await coordinator.refreshAfterCommit()
                await load()
            case .notFound:
                // The device was revoked or forgotten since the list was read.
                await load()
                notificationsFailure = Self.notificationsFailureMessage
            case .undocumented(let status, _) where status == 401:
                fail(status: status)
            case .badRequest, .forbidden, .undocumented:
                notificationsFailure = Self.notificationsFailureMessage
            }
        } catch {
            notificationsFailure = Self.notificationsFailureMessage
        }
    }

    /// Asks the daemon to publish one test notification to this device's topic
    /// (`POST /devices/{device_id}/test-notification`). Ignores overlapping
    /// calls while one is in flight, and never touches `notificationOpens`:
    /// that row records only when a notification's link is actually tapped.
    package func sendTestNotification() async {
        guard testNotificationState != .sending else { return }
        testNotificationState = .sending
        do {
            let output = try await coordinator.store.client.sendDeviceTestNotification(
                path: .init(device_id: currentDeviceID))
            switch output {
            case .ok(let ok):
                let result = try ok.body.json
                testNotificationState = .finished(Self.testNotificationOutcome(from: result))
            case .tooManyRequests(let limited):
                testNotificationState = .finished(
                    .failed(Self.tooRecentMessage(retryAfter: limited.headers.Retry_hyphen_After)))
            case .forbidden:
                testNotificationState = .finished(.failed(Self.testNotificationInactiveMessage))
            case .notFound:
                testNotificationState = .finished(.failed(Self.testNotificationNotFoundMessage))
            case .undocumented(let status, _) where status == 401:
                testNotificationState = .idle
                fail(status: status)
            case .undocumented(let status, _):
                testNotificationState = .finished(
                    .failed("The daemon didn't answer the test notification request (HTTP \(status))."))
            }
        } catch {
            if error is CancellationError || Task.isCancelled {
                testNotificationState = .idle
                return
            }
            testNotificationState = .finished(.failed(Self.testNotificationUnreachableMessage))
        }
    }

    private static func testNotificationOutcome(
        from result: Components.Schemas.DeviceTestNotification
    ) -> TestNotificationOutcome {
        switch result.outcome {
        case .accepted:
            return .accepted(testNotificationAcceptedMessage)
        case .provider_rejected:
            if let status = result.provider_status {
                return .failed("ntfy refused the test notification (HTTP \(status)).")
            }
            return .failed("ntfy refused the test notification.")
        case .transport_failed:
            if let failure = result.transport_failure {
                return .failed("Couldn't reach ntfy: \(failure).")
            }
            return .failed("Couldn't reach ntfy.")
        case .notifier_unavailable:
            return .failed(testNotificationUnavailableMessage)
        case .unknown:
            return .failed(testNotificationUnknownOutcomeMessage)
        }
    }

    private static func tooRecentMessage(retryAfter: Int?) -> String {
        guard let seconds = retryAfter, seconds > 0 else { return testNotificationTooRecentMessage }
        let unit = seconds == 1 ? "second" : "seconds"
        return "A test notification was just sent. Try again in \(seconds) \(unit)."
    }

    /// A failed read shows no rows: a list that may be out of date would
    /// misreport which devices can still act.
    private func fail(status: Int) {
        rows = []
        if status == 401 {
            // The daemon refused the credential: the same state the
            // heartbeat would report, surfaced without waiting for it.
            coordinator.store.publish(.unauthenticated)
            loadState = .failed(Self.unauthenticatedMessage)
        } else {
            loadState = .failed(Self.loadFailureMessage)
        }
    }

    private static func row(_ entry: Components.Schemas.DeviceListEntry, currentDeviceID: String) -> Row {
        switch entry.device {
        case .active(let device):
            Row(
                id: device.id, name: device.display_name, pairedAt: device.paired_at,
                lastSeenAt: entry.last_seen_at, revokedAt: nil, isCurrent: device.id == currentDeviceID,
                notificationsEnabled: entry.notifications_enabled)
        case .revoked(let device):
            Row(
                id: device.id, name: device.display_name, pairedAt: device.paired_at,
                lastSeenAt: entry.last_seen_at, revokedAt: device.revoked_at,
                isCurrent: device.id == currentDeviceID, notificationsEnabled: entry.notifications_enabled)
        }
    }
}
