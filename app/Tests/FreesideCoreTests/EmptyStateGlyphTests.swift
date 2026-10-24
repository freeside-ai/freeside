import Foundation
import Testing

@testable import FreesideCore

#if os(macOS)
    import AppKit
#endif

struct EmptyStateGlyphTests {
    @Test func eachSectionNamesItsSystemGlyph() {
        #expect(EmptyStateGlyph.inbox.symbolName == "archivebox")
        #expect(EmptyStateGlyph.tasks.symbolName == "checklist.checked")
        #expect(EmptyStateGlyph.revoked.symbolName == "lock.slash")
    }

    #if os(macOS)
        /// A name the system does not carry draws nothing, silently.
        @Test(arguments: [EmptyStateGlyph.inbox, .tasks, .revoked])
        func theSystemCarriesEachGlyph(_ glyph: EmptyStateGlyph) {
            #expect(NSImage(systemSymbolName: glyph.symbolName, accessibilityDescription: nil) != nil)
        }
    #endif
}
