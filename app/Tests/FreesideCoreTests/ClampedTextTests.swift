#if os(macOS)
    import AppKit
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The shared clamp drawn in a real host (#2608): how many lines it
    /// draws clamped, whole, and expanded. The test process has no
    /// accessibility client, so SwiftUI builds no accessibility tree here;
    /// `DecisionCardCompositionTests` pins the spoken names the view
    /// applies instead.
    @Suite(.serialized) @MainActor struct ClampedTextTests {
        private let width: CGFloat = 480
        private let long = String(
            repeating: "The approved specification makes command identity write-once at the daemon boundary. ",
            count: 6)
        private let short = "No test covers the retry path."

        @Test func aLongReasonClampsToTwoLines() {
            let oneLine = height(short, lineLimit: 2, expanded: false)
            let clamped = height(long, lineLimit: 2, expanded: false)

            #expect(clamped > oneLine * 1.5)
            #expect(clamped < oneLine * 2.5)
        }

        @Test func aClampOfOneLineDrawsOneLine() {
            let oneLine = height(short, lineLimit: 1, expanded: false)
            let clamped = height(long, lineLimit: 1, expanded: false)

            #expect(abs(clamped - oneLine) < 1)
        }

        /// A reason that fits draws at its own height whether or not it may
        /// expand: there is nothing for More to reveal.
        @Test func aReasonThatFitsDrawsWhole() {
            #expect(
                height(short, lineLimit: 2, expanded: false)
                    == height(short, lineLimit: 2, expanded: true))
        }

        @Test func anExpandedReasonDrawsWhole() {
            let clamped = height(long, lineLimit: 2, expanded: false)
            let expanded = height(long, lineLimit: 2, expanded: true)

            #expect(expanded > clamped * 2)
        }

        private func height(_ text: String, lineLimit: Int, expanded: Bool) -> CGFloat {
            _ = FreesideFont.registration
            let view = ClampedText(
                text: text, font: FreesideFont.cardBody, lineLimit: lineLimit,
                isExpanded: .constant(expanded), spokenName: "reason for Finding 1"
            )
            .environment(\.dynamicTypeSize, .large)
            .frame(width: width, alignment: .topLeading)
            .fixedSize(horizontal: false, vertical: true)

            let host = NSHostingView(rootView: view)
            host.frame = CGRect(x: 0, y: 0, width: width, height: 2_000)
            host.layoutSubtreeIfNeeded()
            return host.fittingSize.height
        }
    }
#endif
