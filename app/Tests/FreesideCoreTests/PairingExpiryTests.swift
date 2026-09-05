import FreesideAPI
import SwiftUI
import Testing

@testable import FreesideCore

#if os(macOS)
    import AppKit
#endif

@MainActor
struct PairingExpiryTests {
    private let now = Date(timeIntervalSinceReferenceDate: 10_000)

    @Test(arguments: [
        (840.0, "in 14 min"), (61.0, "in 1 min"), (60.0, "in 1 min"),
        (30.0, "in under a minute"), (0.0, "expired"), (-1.0, "expired"),
    ])
    func expiryBuckets(remaining: TimeInterval, expected: String) {
        #expect(PairingView.expiryText(until: now.addingTimeInterval(remaining), at: now) == expected)
    }

    @Test func urgencyStartsUnderFiveMinutes() {
        #expect(!PairingView.expiryIsUrgent(until: now.addingTimeInterval(300), at: now))
        #expect(PairingView.expiryIsUrgent(until: now.addingTimeInterval(299), at: now))
        #expect(PairingView.expiryIsUrgent(until: now, at: now))
    }

    @Test func expiryRowKeepsExactTimestamp() {
        let facts = MockServer.pairingFacts
        let row = PairingView.detailRows(facts, now: facts.code_expires_at)[1]
        #expect(row.label == "Code expires")
        #expect(row.exact == facts.code_expires_at.formatted(.iso8601))
        #expect(row.valueColor == Color.accentText)
    }

    @Test func pairingDemoStartsWithFutureExpiry() async throws {
        // The API's date encoding preserves whole seconds.
        let launchedAt = Date(timeIntervalSince1970: Date().timeIntervalSince1970.rounded(.down))
        let session = AppSession.pairingDemo()
        guard case .needsPairing(let model) = session.phase else {
            Issue.record("expected the interactive pairing demo")
            return
        }
        model.pairingCode = "483911"
        await model.refreshFacts()
        let facts = try #require(model.facts)
        #expect(facts.code_expires_at >= launchedAt.addingTimeInterval(600))
        #expect(facts.code_expires_at <= Date().addingTimeInterval(600))
        #expect(await model.pair() != nil)
    }
}

#if os(macOS)
    extension ScreenshotRegressionTests {
        func mountedTimelineMatchesWallClock(remaining: TimeInterval) throws {
            _ = FreesideFont.registration
            var facts = MockServer.pairingFacts
            facts.code_expires_at = Date().addingTimeInterval(remaining)
            let live = try renderPairingDetails(PairingView.detailsContent(facts))
            let fixed = try renderPairingDetails(PairingView.detailsContent(facts, now: Date()))
            #expect(live == fixed)
        }

        private func renderPairingDetails(_ content: some View) throws -> Data {
            let renderer = ImageRenderer(
                content: VStack(alignment: .leading, spacing: 6) { content }
                    .font(FreesideFont.body)
                    .foregroundStyle(Color.ink)
                    .environment(\.colorScheme, .light)
                    .environment(\.dynamicTypeSize, .large)
                    .frame(width: 480)
                    .fixedSize(horizontal: false, vertical: true)
                    .background(Color.ground))
            let image = try #require(renderer.cgImage)
            return try #require(NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]))
        }
    }
#endif
