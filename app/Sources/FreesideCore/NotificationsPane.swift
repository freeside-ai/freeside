import SwiftUI

/// The Notifications pane of Settings: whether this device is subscribed.
/// Delivery is ntfy's, and only an iPhone holds a subscription (its own
/// credential), so the iPhone shows its state and the setup steps, and the
/// Mac says in one row that setup lives on the iPhone, a departure from the
/// canvas's two-row frame (the 4 Nov 2026 handoff's "Values sync lacks").
struct NotificationsPane: View {
    let subscription: DeviceNtfySubscription?
    /// When a notification's link was last opened under this pairing.
    let openedAt: Date?
    var client: SettingsClient = .current

    var body: some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            SettingsPaneHead(
                title: "Notifications", line: "Delivered by ntfy. The daemon publishes; each iPhone subscribes.")
            switch client {
            case .mac:
                SettingsGroup(["mac"]) { _ in
                    SettingsRow(label: client.name, line: "Notifications are set up on the iPhone.")
                }
            case .iPhone, .iPad:
                SettingsGroup(["device"]) { _ in
                    SettingsRow(
                        label: client.name, line: subscription == nil ? "Not set up" : "Subscribed · topic set"
                    ) {
                        if subscription != nil {
                            StateChip(label: "On", cut: .ink)
                        }
                    }
                }
                if let subscription {
                    PhoneNotificationSetup(subscription: subscription, openedAt: openedAt, showsHeading: false)
                }
            }
        }
    }
}
