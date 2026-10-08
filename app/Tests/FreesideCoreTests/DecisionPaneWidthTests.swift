#if os(macOS)
    import AppKit
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The decision detail picks one column or two from the width of its
    /// pane. The card caps its own width until that choice is made, so the
    /// measurement must not read the card back.
    @Suite(.serialized) @MainActor struct DecisionPaneWidthTests {
        @Test func aScrollViewNarrowerThanItsPaneReportsThePane() {
            final class Measurement: @unchecked Sendable {
                var width: CGFloat?
            }
            let measurement = Measurement()
            let paneWidth: CGFloat = 1_200
            let pane = ScrollView {
                Color.clear.frame(width: 560, height: 100)
            }
            .onPaneWidthChange { measurement.width = $0 }
            .frame(width: paneWidth, height: 600)

            let host = NSHostingView(rootView: pane)
            host.frame = CGRect(x: 0, y: 0, width: paneWidth, height: 600)
            host.layoutSubtreeIfNeeded()
            // The geometry callback lands on a later main-queue turn.
            let deadline = Date().addingTimeInterval(5)
            while measurement.width == nil, Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                host.layoutSubtreeIfNeeded()
            }

            #expect(measurement.width == paneWidth)
        }
    }
#endif
