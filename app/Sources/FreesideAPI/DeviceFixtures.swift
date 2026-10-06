import Foundation
import OpenAPIRuntime

/// The paired devices the permissive mock lists. The mock app's session is
/// the fixed identity `currentDeviceID`, which never pairs with the mock, so
/// without a seed the device list could not show the device reading it.
/// Three rows cover every state the Devices screen renders: the current
/// device, another active one, and a revoked one that was never seen.
public enum DeviceFixtures {
    public static let currentDeviceID = "device-mock"
    public static let otherDeviceID = "device-mock-phone"
    public static let revokedDeviceID = "device-mock-retired"

    /// Fixed instants, like every fixture's, so a rendered list never
    /// depends on the clock.
    static let referenceInstant = AttentionFixtures.createdInstant
    private static let day: TimeInterval = 86_400

    public static func defaultDevices() -> [Components.Schemas.DeviceListEntry] {
        [
            .init(
                as_of_revision: 1,
                entity_version: 1,
                device: active(
                    id: currentDeviceID, name: "Studio Mac",
                    pairedAt: referenceInstant.addingTimeInterval(-21 * day)),
                last_seen_at: referenceInstant.addingTimeInterval(-120)),
            .init(
                as_of_revision: 1,
                entity_version: 1,
                device: active(
                    id: otherDeviceID, name: "Ben's iPhone",
                    pairedAt: referenceInstant.addingTimeInterval(-9 * day)),
                last_seen_at: referenceInstant.addingTimeInterval(-5 * 3600)),
            // Revoked before the daemon recorded activity: no last-seen
            // instant, and a second entity version from the revoking write.
            .init(
                as_of_revision: 1,
                entity_version: 2,
                device: .revoked(
                    .init(
                        id: revokedDeviceID,
                        display_name: "Old iPad",
                        status: .revoked,
                        paired_at: referenceInstant.addingTimeInterval(-120 * day),
                        revoked_at: referenceInstant.addingTimeInterval(-45 * day))),
                last_seen_at: nil),
        ]
    }

    private static func active(id: String, name: String, pairedAt: Date) -> Components.Schemas.Device {
        // The contract requires revoked_at with an explicit null on an
        // active device; an empty container encodes as JSON null.
        .active(
            .init(
                id: id, display_name: name, status: .active, paired_at: pairedAt,
                revoked_at: OpenAPIValueContainer(nilLiteral: ())))
    }
}
