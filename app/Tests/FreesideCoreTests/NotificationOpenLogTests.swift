import Foundation
import Testing

@testable import FreesideCore

@Suite @MainActor struct NotificationOpenLogTests {
    private static let opened = Date(timeIntervalSince1970: 1_752_000_000)

    @Test func anEntryBelongsToOnePairedDevice() {
        let log = NotificationOpenLog(defaults: nil)
        log.record(deviceID: "device-7", at: Self.opened)

        #expect(log.openedAt(deviceID: "device-7") == Self.opened)
        // Pairing again mints a new device, which has opened nothing yet.
        #expect(log.openedAt(deviceID: "device-8") == nil)
    }

    @Test func aLaterOpenReplacesTheEarlierOne() {
        let log = NotificationOpenLog(defaults: nil)
        log.record(deviceID: "device-7", at: Self.opened)
        log.record(deviceID: "device-7", at: Self.opened.addingTimeInterval(60))

        #expect(log.openedAt(deviceID: "device-7") == Self.opened.addingTimeInterval(60))
    }

    @Test func theLogSurvivesARelaunch() throws {
        let suiteName = "NotificationOpenLogTests-\(UUID().uuidString)"
        let defaults = try #require(UserDefaults(suiteName: suiteName))
        defer { defaults.removePersistentDomain(forName: suiteName) }

        NotificationOpenLog(defaults: defaults).record(deviceID: "device-7", at: Self.opened)

        #expect(NotificationOpenLog(defaults: defaults).openedAt(deviceID: "device-7") == Self.opened)
    }
}
