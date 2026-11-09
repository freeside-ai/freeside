import SwiftUI

/// The Notifications pane of Settings: this device's notifications switch.
/// The daemon notifies a device only while its switch is on, and only of
/// items opened after it was turned on; every device starts off. Delivery
/// is ntfy's, and only an iPhone holds a subscription (its own credential),
/// so the iPhone shows the setup steps once its switch is on, and the Mac
/// says nothing listens there yet.
struct NotificationsPane: View {
    let model: DevicesModel
    /// When a notification's link was last opened under this pairing.
    let openedAt: Date?
    var client: SettingsClient = .current
    var rendersInteractiveControls = true

    var body: some View {
        VStack(alignment: .leading, spacing: SettingsPaneInsets.sectionSpacing(client)) {
            SettingsPaneHead(
                title: "Notifications", line: "Delivered by ntfy. The daemon publishes; each iPhone subscribes.")
            if let failure = model.notificationsFailure {
                Notice(tone: .wax, keyword: "Failed", sentence: failure)
                    .accessibilityElement(children: .combine)
            }
            switch model.loadState {
            case .loading:
                Text("Loading…")
                    .font(FreesideFont.cardBody)
                    .foregroundStyle(Color.inkDim)
            case .failed(let reason):
                Notice(tone: .wax, keyword: "Failed", sentence: reason)
                    .accessibilityElement(children: .combine)
            case .loaded:
                if let device = model.currentDevice {
                    SettingsGroup([device]) { row in
                        SettingsRow(label: client.name, line: line(row)) {
                            DeviceNotificationsSwitch(
                                model: model, row: row, rendersInteractiveControls: rendersInteractiveControls)
                        }
                    }
                    if client != .mac, device.notificationsEnabled, let subscription = model.notificationSubscription {
                        PhoneNotificationSetup(subscription: subscription, openedAt: openedAt, showsHeading: false)
                    }
                }
            }
        }
    }

    private func line(_ row: DevicesModel.Row) -> String {
        switch client {
        case .mac: "Nothing on this Mac listens for notifications yet."
        case .iPhone, .iPad: row.notificationsEnabled ? "On" : "Off"
        }
    }
}
