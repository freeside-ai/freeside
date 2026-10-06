import Foundation
import FreesideAPI

/// The submitting device's identity, carried on every ClientCommand.
/// Pairing (plan §5.14; the cache-and-pairing unit) will mint a real one;
/// until then the composition root supplies the fixed mock identity.
public struct DeviceIdentity: Sendable {
    public let deviceID: String

    public init(deviceID: String) {
        self.deviceID = deviceID
    }

    /// The permissive mock seeds a device under this id, so the mock app's
    /// device list contains the device reading it.
    public static let mock = DeviceIdentity(deviceID: DeviceFixtures.currentDeviceID)
}
