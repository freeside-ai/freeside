import Foundation
import FreesideAPI
import Observation

/// The Devices screen's state: the daemon's paired devices, read through
/// `GET /devices`, and the one action the screen offers, revocation.
///
/// The list is a partial read outside bootstrap, so the model owns it and
/// never caches it: a last-seen instant changes without moving the server
/// revision, and a stale list would misreport who can still act.
@MainActor
@Observable
final class DevicesModel {
    enum LoadState: Equatable {
        case loading
        case loaded
        case failed(String)
    }

    /// One listed device, flattened from the contract's active and revoked
    /// variants.
    struct Row: Identifiable, Equatable {
        let id: String
        let name: String
        let pairedAt: Date
        /// Nil when the daemon has recorded no authenticated request.
        let lastSeenAt: Date?
        /// Non-nil exactly when the device is revoked.
        let revokedAt: Date?
        let isCurrent: Bool

        var isRevoked: Bool { revokedAt != nil }
    }

    private(set) var rows: [Row] = []
    private(set) var loadState: LoadState = .loading
    private(set) var revokingID: String?
    private(set) var revokeFailure: String?

    static let loadFailureMessage = "Couldn't load devices. Check the connection to the daemon and try again."
    static let unauthenticatedMessage = "This device is no longer paired with the daemon."
    static let revokeFailureMessage =
        "The daemon didn't confirm the revocation. Refresh to see whether the device is still paired."
    static let signOutFailureMessage =
        "This device was revoked, but its stored credential couldn't be removed. Close this sheet and choose Pair Again."

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
    let notificationSubscription: DeviceNtfySubscription?

    /// `signOut` deletes this device's credential and returns to pairing. It
    /// runs only after the daemon confirms this device's own revocation.
    init(
        coordinator: SyncCoordinator, notificationSubscription: DeviceNtfySubscription? = nil,
        signOut: @escaping @MainActor () throws -> Void
    ) {
        self.coordinator = coordinator
        self.notificationSubscription = notificationSubscription
        self.signOut = signOut
    }

    var currentDeviceID: String { coordinator.store.device.deviceID }
    var observedRevision: Int64? { coordinator.cursors?.highestObservedServerRevision }

    var currentDevice: Row? { rows.first { $0.isCurrent && !$0.isRevoked } }
    var otherDevices: [Row] { rows.filter { !$0.isCurrent && !$0.isRevoked } }
    var revokedDevices: [Row] { rows.filter(\.isRevoked) }

    /// Loads on first appearance and again whenever the observed server
    /// revision has moved since the rows were read: pairing and revocation
    /// are synchronized writes, so the heartbeat reports them.
    func loadIfStale() async {
        guard loadState != .loaded || loadedAtRevision != observedRevision else { return }
        await load()
    }

    func load() async {
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
                // The failure asks for this read; the rows now answer it.
                revokeFailure = nil
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
    func revoke(_ id: String) async {
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
                        coordinator.store.freshness = .unauthenticated
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

    /// A failed read shows no rows: a list that may be out of date would
    /// misreport which devices can still act.
    private func fail(status: Int) {
        rows = []
        if status == 401 {
            // The daemon refused the credential: the same state the
            // heartbeat would report, surfaced without waiting for it.
            coordinator.store.freshness = .unauthenticated
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
                lastSeenAt: entry.last_seen_at, revokedAt: nil, isCurrent: device.id == currentDeviceID)
        case .revoked(let device):
            Row(
                id: device.id, name: device.display_name, pairedAt: device.paired_at,
                lastSeenAt: entry.last_seen_at, revokedAt: device.revoked_at,
                isCurrent: device.id == currentDeviceID)
        }
    }
}
