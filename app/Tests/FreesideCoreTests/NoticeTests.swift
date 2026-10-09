import SwiftUI
import Testing

@testable import FreesideCore

/// R14 as the 8 Oct 2026 interfaces handoff revises it: a notice states,
/// and its act is a button, never text trailing the sentence.
@MainActor
struct NoticeTests {
    /// The act is cut as a button in every tone: the wax outline under a
    /// wax notice, the plain outline otherwise, and never the text cut.
    @Test func aNoticesActIsCutAsAButtonInEveryTone() {
        #expect(Notice.Tone.wax.actionTone == .destructive)
        #expect(Notice.Tone.accent.actionTone == .secondary)
        #expect(Notice.Tone.neutral.actionTone == .secondary)
        #expect(Notice.Tone.allCases.allSatisfy { $0.actionTone != .tertiary })
    }

    #if canImport(AppKit)
        /// At a width where the keyword, the sentence, and a trailing label
        /// would share one line, the act still takes a line of its own, a
        /// control's height under the sentence.
        @Test(arguments: Notice.Tone.allCases)
        func theActTakesItsOwnLineUnderTheSentence(tone: Notice.Tone) {
            _ = FreesideFont.registration
            let stating = height(Notice(tone: tone, keyword: "Stopped", sentence: "Nothing runs."))
            let acting = height(
                Notice(
                    tone: tone, keyword: "Stopped", sentence: "Nothing runs.",
                    action: .init(label: "Review") {}))
            #expect(acting - stating >= FreesideLadder.current.actionHeight)
        }

        private func height(_ notice: Notice) -> CGFloat {
            let host = NSHostingView(rootView: notice.frame(width: 600))
            host.setFrameSize(NSSize(width: 600, height: 4_000))
            host.layoutSubtreeIfNeeded()
            return host.fittingSize.height
        }
    #endif
}
