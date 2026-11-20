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
}
