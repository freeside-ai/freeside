import Testing

@testable import FreesideCore

@MainActor
struct KeywordLabelTests {
    /// The card scale (R10): a keyword is a heading at 12.5pt tracked
    /// 0.08em, and a chip sits half a point under it tracked 0.04em.
    @Test func keywordAndChipSitAtTheCardScale() {
        #expect(FreesideFont.keywordSize == 12.5)
        #expect(FreesideFont.chipSize == 12)
        #expect(abs(FreesideFont.keywordTracking - 0.08 * FreesideFont.keywordSize) < 0.001)
        #expect(abs(FreesideFont.chipTracking - 0.04 * FreesideFont.chipSize) < 0.001)
    }

    #if canImport(AppKit)
        /// A fixed face has no platform size to read, so the screenshot
        /// bridge scales its base by the iOS ratio for its text style;
        /// otherwise the accessibility digests would render at the default.
        @Test func screenshotBridgeScalesAFixedFaceByTheIOSRatio() {
            FreesideFont.$screenshotDynamicTypeSize.withValue(.large) {
                #expect(
                    screenshotMetricBase(FreesideFont.keywordSize, relativeTo: .caption2) == 12.5)
            }
            FreesideFont.$screenshotDynamicTypeSize.withValue(.accessibility5) {
                #expect(
                    screenshotMetricBase(FreesideFont.keywordSize, relativeTo: .caption2)
                        == 12.5 * 26 / 11)
            }
        }
    #endif
}
