import SwiftUI

/// How this iPhone gets its notifications: the steps that subscribe the ntfy
/// app to this device's topic. The Devices sheet shows it on this device's
/// own card.
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
    @State private var copiedTopic = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            KeywordLabel(text: "Phone notifications")
                .accessibilityAddTraits(.isHeader)
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
            step(Self.testStep) { EmptyView() }
        }
    }

    static let installStep = "1. Install the ntfy app and allow its notifications."
    static let testStep =
        "3. On the daemon's host, run freesided notify-test, then tap the notification it sends."

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
