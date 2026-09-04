import Testing

@testable import FreesideCore

@MainActor
struct KeywordLabelTests {
    #if canImport(AppKit)
        @Test func macOSEyebrowUsesTheLiftedBaseSize() {
            #expect(FreesideFont.eyebrowPointSize() == 10.5)
        }

        @Test func screenshotBridgeKeepsTheEyebrowOnIOSMetrics() {
            FreesideFont.$screenshotDynamicTypeSize.withValue(.large) {
                #expect(FreesideFont.eyebrowPointSize() == FreesideFont.size(of: .caption2))
            }
        }
    #endif
}
