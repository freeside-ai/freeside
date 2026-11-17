import Foundation
import Testing

@testable import FreesideCore

@Suite struct PairDeviceViewTests {
    private let now = Date(timeIntervalSince1970: 1_794_872_116)

    @Test func theCountdownReadsMinutesAndSecondsUntilItExpires() {
        #expect(PairDeviceView.expiryText(until: now.addingTimeInterval(9 * 60 + 41), at: now) == "Expires in 9:41")
        #expect(PairDeviceView.expiryText(until: now.addingTimeInterval(60), at: now) == "Expires in 1:00")
        // A part-second left still reads as a second, not as expired.
        #expect(PairDeviceView.expiryText(until: now.addingTimeInterval(0.2), at: now) == "Expires in 0:01")
        #expect(PairDeviceView.expiryText(until: now, at: now) == nil)
        #expect(PairDeviceView.expiryText(until: now.addingTimeInterval(-1), at: now) == nil)
    }
}
