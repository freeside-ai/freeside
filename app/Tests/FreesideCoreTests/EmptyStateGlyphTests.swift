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

        /// The key mark is the menu-bar status item's alone, which draws it
        /// from its own image. No view draws the `KeyMark` shape: the only
        /// source file that names it is the one that declares it, and
        /// outside comments that file names it once, in the declaration.
        @Test func noViewDrawsTheKeyMark() throws {
            let app = URL(fileURLWithPath: #filePath)
                .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            // Simple boundaries: the default ones read `KeyMark.member` as
            // one word and would miss it.
            let name = try Regex(#"\bKeyMark\b"#).wordBoundaryKind(.simple)
            var uses: [String: Int] = [:]
            var scanned = 0
            for directory in ["Sources", "Apps"] {
                let root = app.appendingPathComponent(directory)
                let files = try #require(FileManager.default.enumerator(at: root, includingPropertiesForKeys: nil))
                for case let file as URL in files where file.pathExtension == "swift" {
                    scanned += 1
                    let count = try String(contentsOf: file, encoding: .utf8)
                        .split(separator: "\n", omittingEmptySubsequences: false)
                        .filter { !$0.drop(while: \.isWhitespace).hasPrefix("//") }
                        .reduce(0) { $0 + $1.matches(of: name).count }
                    if count > 0 { uses[file.lastPathComponent] = count }
                }
            }
            #expect(scanned > 50)
            #expect(uses == ["DesignLanguage.swift": 1])
        }
    #endif
}
