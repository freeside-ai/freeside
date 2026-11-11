#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// Reading a ready card's accessibility tree, as VoiceOver and other
    /// assistive clients do, must finish (#2993). The walk hosts each ready
    /// fixture in a window with interactive controls on and the window's
    /// selectable-text default, and reads what a client asks each element
    /// for.
    ///
    /// This walk does not reproduce the crash #2993 reports: the card is
    /// built before the first read, and only a view inserted after it
    /// recurses (`AccessibilityLabelRecursionTests` covers that). It guards
    /// the tree's shape (bounded, and labelled) on every ready fixture.
    @Suite(.serialized) @MainActor struct ReadyCardAccessibilityTests {
        nonisolated private static let readyFixtures: [Components.Schemas.AttentionItemSnapshot] = [
            AttentionFixtures.fixture(type: .ready_for_final_review),
            AttentionFixtures.readyWithReviewGuide(),
            AttentionFixtures.degradedReady(),
            AttentionFixtures.staleReady(),
            AttentionFixtures.scopeKeptReady(),
            AttentionFixtures.readyWithElapsedChecksDeadline(),
            AttentionFixtures.publishedTaskReady(),
        ]

        /// Bounds that turn a tree that never ends into a failure instead of
        /// a hang; an element met twice is read once. A ready card in the app holds a few hundred elements, and
        /// a test host exposes a few dozen of them.
        nonisolated private static let maxDepth = 64
        nonisolated private static let maxElements = 5_000

        @Test(arguments: readyFixtures)
        func walkingTheWholeTreeCompletes(
            _ snapshot: Components.Schemas.AttentionItemSnapshot
        ) throws {
            let window = host(snapshot)
            defer {
                window.close()
                setAssistiveClientAttributes(false)
            }
            let root = try #require(window.contentView)

            var walk = Walk()
            try walk.visit(root, depth: 0)

            // A walk that saw nothing would pass vacuously.
            #expect(walk.labels > 0, "the walk read no labels on \(snapshot.item.id)")
        }

        private func host(_ snapshot: Components.Schemas.AttentionItemSnapshot) -> NSWindow {
            _ = FreesideFont.registration
            setAssistiveClientAttributes(true)
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            store.replaceAll(with: [snapshot])
            let detail = DecisionDetailView(store: store, itemID: snapshot.item.id)
                .frame(width: 560, height: 900)
                // The default `FreesideRootView` sets for every window.
                .textSelection(.enabled)

            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: 560, height: 900),
                styleMask: [.titled], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            window.contentView = NSHostingView(rootView: detail)
            window.contentView?.layoutSubtreeIfNeeded()
            // A short spin of the run loop lets the card's `.task` work settle.
            RunLoop.main.run(until: Date().addingTimeInterval(0.2))
            window.contentView?.layoutSubtreeIfNeeded()
            return window
        }

        /// SwiftUI builds its accessibility tree only once a client asks for
        /// it; these are the attributes an assistive client sets. They are
        /// process-wide, so the test turns them back off.
        private func setAssistiveClientAttributes(_ on: Bool) {
            for name in ["AXEnhancedUserInterface", "AXManualAccessibility"] {
                NSApplication.shared.accessibilitySetValue(
                    on, forAttribute: NSAccessibility.Attribute(rawValue: name))
            }
        }

        private struct Walk {
            var seen = Set<ObjectIdentifier>()
            var labels = 0

            mutating func visit(_ object: AnyObject, depth: Int) throws {
                guard seen.insert(ObjectIdentifier(object)).inserted else { return }
                try #require(depth <= maxDepth, "the accessibility tree is deeper than \(maxDepth)")
                try #require(
                    seen.count <= maxElements, "the accessibility tree has over \(maxElements) elements")
                guard let element = object as? NSObject else { return }

                // An assistive client's reads enter through this method, the
                // first AppKit frame in the crash; SwiftUI's elements answer
                // nothing useful through the public protocol in a test host.
                let read = NSSelectorFromString("_accessibilityValueForAttribute:clientError:")
                guard element.responds(to: read), let implementation = element.method(for: read)
                else { return }
                typealias ClientRead =
                    @convention(c) (
                        AnyObject, Selector, NSString, UnsafeMutablePointer<Int32>?
                    ) -> Unmanaged<AnyObject>?
                let clientRead = unsafeBitCast(implementation, to: ClientRead.self)
                func value(_ attribute: NSAccessibility.Attribute) -> Any? {
                    var clientError: Int32 = 0
                    return clientRead(element, read, attribute.rawValue as NSString, &clientError)?
                        .takeUnretainedValue()
                }

                // The description is the element's label, the read the crash
                // recursed in.
                if let label = value(.description) as? String, !label.isEmpty { labels += 1 }
                for attribute: NSAccessibility.Attribute in [.title, .value, .help, .role] {
                    _ = value(attribute)
                }
                for child in value(.children) as? [AnyObject] ?? [] {
                    try visit(child, depth: depth + 1)
                }
            }
        }
    }
#endif
