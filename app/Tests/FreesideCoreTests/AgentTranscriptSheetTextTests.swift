import Foundation
import Testing

@testable import FreesideCore

struct AgentTranscriptSheetTextTests {
    /// Agent prose keeps its formatting but no live link, and a label
    /// never hides its destination.
    @Test func agentLinksAreInertAndShowTheirDestination() {
        let text = AgentTranscriptSheetText.inertMarkdown(
            "See [the fix](https://example.com/a) and <https://example.com/b>, in `rig/`.")
        #expect(
            String(text.characters)
                == "See the fix (https://example.com/a) and https://example.com/b, in rig/.")
        #expect(text.runs.allSatisfy { $0.link == nil })
        #expect(text.runs.contains { $0.inlinePresentationIntent?.contains(.code) == true })
    }

    /// The running clock counts to the second and drops a time it can't
    /// know rather than guess one (#1926).
    @Test func theRunningClockCountsToTheSecond() {
        let locale = Locale(identifier: "en_US")
        #expect(AgentTranscriptSheetText.working(4 * 60 + 12.7, locale: locale) == "Working for 4m 12s")
        #expect(AgentTranscriptSheetText.working(-3, locale: locale) == "Working for 0s")
        #expect(AgentTranscriptSheetText.working(nil, locale: locale) == "Working")
    }
}
