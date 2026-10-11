import SwiftUI

/// How this iPhone gets its notifications: the steps that subscribe the ntfy
/// app to this device's topic. The Devices sheet shows it on this device's
/// own card. The last line says whether a notification's link has been
/// opened on this phone since it paired. That is the one sign on the phone
/// that setup worked, and it is an observation: any app can open such a
/// link, and so can a notification sent before the phone paired again.
///
/// The ntfy iPhone app takes no link that would subscribe it, so the
/// operator pastes the topic there by hand. The topic is a secret (whoever
/// holds it reads this phone's notifications) and nobody needs to read it,
/// so it is copied and never drawn. A server other than ntfy's own has to be
/// entered in the ntfy app too, so only then is it shown.
struct PhoneNotificationSetup: View {
    // swift-format-ignore: NeverForceUnwrap
    static let ntfyAppStoreURL = URL(string: "https://apps.apple.com/app/ntfy/id1625396347")!

    let subscription: DeviceNtfySubscription
    /// When a notification's link was last opened under this pairing
    /// (`NotificationOpenLog`), or nil when none has been.
    let openedAt: Date?
    @Environment(\.locale) private var locale
    @Environment(\.timeZone) private var timeZone
    @Environment(\.pinnedNow) private var pinnedNow
    /// False where a sheet's own header already names the steps.
    var showsHeading = true
    @State private var copiedTopic = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if showsHeading {
                KeywordLabel(text: "Phone notifications")
                    .accessibilityAddTraits(.isHeader)
            }
            step(Self.installStep) {
                Link("Get ntfy on the App Store", destination: Self.ntfyAppStoreURL)
                    .buttonStyle(FreesideActionButtonStyle(tone: .tertiary))
            }
            step(Self.subscribeStep(for: subscription)) {
                VStack(alignment: .leading, spacing: 8) {
                    if !subscription.usesHostedServer {
                        // The copy control draws in a screenshot too: a touch
                        // platform has no hover to reveal it.
                        TechnicalDetailRow(
                            row: .init(label: "Server", value: subscription.serverURL), stacksAlways: true)
                    }
                    Button(copiedTopic ? "Topic Copied" : "Copy Topic") {
                        Clipboard.copy(subscription.topic)
                        copiedTopic = true
                    }
                    .buttonStyle(FreesideActionButtonStyle(tone: .secondary))
                }
            }
            step(Self.testStep) {
                if let openedAt {
                    FactRow(
                        label: Self.openedLabel,
                        value: FreesideFormat.shortTime(
                            openedAt, now: pinnedNow ?? .now, locale: locale, timeZone: timeZone)
                    )
                    .exactInstant(openedAt)
                } else {
                    FactRow(label: Self.openedLabel, value: "Never")
                }
            }
        }
    }

    static let installStep = "1. Install the ntfy app and allow its notifications."
    static let testStep =
        "3. On the daemon's host, run freesided notify-test, then tap the notification it sends."

    /// Says what was observed, not that notifications work: any app can
    /// open the link a notification carries.
    static let openedLabel = "Notification opened"

    static func subscribeStep(for subscription: DeviceNtfySubscription) -> String {
        subscription.usesHostedServer
            ? "2. In ntfy, add a subscription and paste this phone's topic."
            : "2. In ntfy, add a subscription, paste this phone's topic, turn on Use another server, and enter this server."
    }

    private func step(_ sentence: String, @ViewBuilder control: () -> some View) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(sentence)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            control()
        }
    }
}

/// The setup steps as their own sheet, which the iPhone offers once after a
/// pairing (`AppSession.offersNotificationSetup`): the operator has just
/// been given a topic, and nothing else would lead them to the Devices card
/// that holds it.
struct PhoneNotificationSetupSheet: View {
    @Environment(\.dismiss) private var dismiss
    let subscription: DeviceNtfySubscription
    let opens: NotificationOpenLog
    let deviceID: String
    var rendersInteractiveControls = true

    var body: some View {
        VStack(spacing: 0) {
            if rendersInteractiveControls {
                ScrollView { content }
            } else {
                content
            }
            FreesideSheetActionRow.done { dismiss() }
        }
        .background(Color.ground2)
        .freesideSheetPresentation()
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 0) {
            FreesideSheetHeader(
                eyebrow: "Notifications",
                ask: "Get notified on this phone",
                consequence:
                    "Freeside notifies this phone through the ntfy app when an item needs you. These steps stay on this device's card in Devices.",
                askLineLimit: nil)
            // Read here, when the sheet draws, so a test notification tapped
            // while it is open shows at once.
            PhoneNotificationSetup(
                subscription: subscription, openedAt: opens.openedAt(deviceID: deviceID), showsHeading: false
            )
            .padding(.horizontal, 16)
            .padding(.bottom, 16)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}
