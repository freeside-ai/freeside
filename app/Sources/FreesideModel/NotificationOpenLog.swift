import Foundation
import Observation

/// When a notification's link was last opened on this device, per paired
/// device ID, kept in this device's preferences. It is how setup can say
/// that a notification arrived: a test notice writes no delivery row, so
/// nothing on the daemon records that one reached the phone.
///
/// An entry is an observation, not proof. Any app on the phone can open a
/// `freeside://` link, so it says a link was opened here, not that ntfy
/// delivered a notification. It is keyed by device ID because pairing again
/// mints a new device and a new topic, which nothing has been opened on yet.
@MainActor
@Observable
public final class NotificationOpenLog {
    private static let key = "FreesideNotificationOpenedAt"
    private let defaults: UserDefaults?
    private var openedAtByDevice: [String: Date] {
        didSet { defaults?.set(openedAtByDevice, forKey: Self.key) }
    }

    /// `defaults` nil keeps the log in memory only, for a test or a
    /// screenshot that must neither read this device's log nor add to it.
    public init(defaults: UserDefaults? = .standard) {
        self.defaults = defaults
        openedAtByDevice = defaults?.dictionary(forKey: Self.key) as? [String: Date] ?? [:]
    }

    public func openedAt(deviceID: String) -> Date? {
        openedAtByDevice[deviceID]
    }

    public func record(deviceID: String, at date: Date = .now) {
        openedAtByDevice[deviceID] = date
    }
}
