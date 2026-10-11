import Foundation
import Testing

@testable import FreesideCore

@Suite struct PhoneNotificationSetupTests {
    private static let topic = "fs-00000000000000000000000000000000"

    /// The ntfy app subscribes on ntfy's own server unless told otherwise, so
    /// only another server has to be shown and entered.
    @Test(arguments: [
        ("https://ntfy.sh", true),
        ("https://ntfy.sh/", true),
        ("HTTPS://NTFY.SH", true),
        ("https://ntfy.sh:443", true),
        ("https://ntfy.sh:8443", false),
        ("https://ntfy.sh/relay", false),
        ("https://ntfy.sh.example", false),
        ("https://push.ntfy.sh", false),
        ("https://ntfy.example", false),
        ("http://127.0.0.1:8080", false),
    ])
    func onlyNtfysOwnServerIsTheHostedOne(_ serverURL: String, _ hosted: Bool) throws {
        let subscription = try #require(DeviceNtfySubscription(serverURL: serverURL, topic: Self.topic))
        #expect(subscription.usesHostedServer == hosted)
        // The subscribe step names the server only when the operator has to
        // enter one.
        #expect(PhoneNotificationSetup.subscribeStep(for: subscription).contains("server") == !hosted)
    }
}
