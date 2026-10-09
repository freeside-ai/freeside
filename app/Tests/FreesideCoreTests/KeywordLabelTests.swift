import CoreGraphics
import Testing

@testable import FreesideCore

@MainActor
struct KeywordLabelTests {
    /// The ladder (R10): a keyword and a chip share one size, the keyword
    /// tracked 0.08em and the chip 0.04em.
    @Test func keywordAndChipSitOnTheLadder() {
        #expect(FreesideFont.keywordSize == FreesideLadder.current.keyword)
        #expect(FreesideFont.chipSize == FreesideLadder.current.chip)
        #expect(abs(FreesideFont.keywordTracking - 0.08 * FreesideFont.keywordSize) < 0.001)
        #expect(abs(FreesideFont.chipTracking - 0.04 * FreesideFont.chipSize) < 0.001)
    }

    #if canImport(AppKit)
        /// A fixed face has no platform size to read, so the screenshot
        /// bridge scales its base by the iOS ratio for its text style;
        /// otherwise the accessibility digests would render at the default.
        @Test func screenshotBridgeScalesAFixedFaceByTheIOSRatio() {
            let keyword = FreesideFont.keywordSize
            let enlarged: CGFloat = keyword * 26 / 11
            FreesideFont.$screenshotDynamicTypeSize.withValue(.large) {
                #expect(screenshotMetricBase(keyword, relativeTo: .caption2) == keyword)
            }
            FreesideFont.$screenshotDynamicTypeSize.withValue(.accessibility5) {
                #expect(screenshotMetricBase(keyword, relativeTo: .caption2) == enlarged)
            }
        }
    #endif
}
