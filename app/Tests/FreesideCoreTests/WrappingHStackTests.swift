import CoreGraphics
import SwiftUI
import Testing

@testable import FreesideCore

@MainActor
struct WrappingHStackTests {
    private let chipSizes = Array(repeating: CGSize(width: 40, height: 10), count: 3)
    private let layout = WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6)

    @Test func wideProposalUsesContentWidth() {
        #expect(layout.fittingSize(for: chipSizes, proposedWidth: 500) == CGSize(width: 132, height: 10))
    }

    @Test func narrowProposalWrapsAtContentWidth() {
        #expect(layout.fittingSize(for: chipSizes, proposedWidth: 90) == CGSize(width: 86, height: 26))
    }

    @Test func unspecifiedProposalUsesSingleRowWidth() {
        #expect(layout.fittingSize(for: chipSizes, proposedWidth: nil) == CGSize(width: 132, height: 10))
    }

    #if canImport(AppKit)
        /// A round title set in an accessibility size is wider than a phone
        /// by itself; left at its intrinsic width it carried the whole card
        /// past the edge.
        @Test func childWiderThanTheLineWrapsInsideIt() {
            let row = WrappingHStack(horizontalSpacing: 6, verticalSpacing: 6) {
                Text(String(repeating: "Remediation ", count: 12))
                Text("Running")
            }
            let single = NSHostingController(rootView: Text("Remediation")).sizeThatFits(
                in: CGSize(width: 200, height: 10_000))
            let size = NSHostingController(rootView: row).sizeThatFits(in: CGSize(width: 200, height: 10_000))
            #expect(size.width <= 200)
            #expect(size.height > single.height * 2)
        }
    #endif
}
