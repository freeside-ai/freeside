import CoreGraphics
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
}
