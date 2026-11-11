#if os(macOS)
    import AppKit
    import FreesideAPI
    import ObjectiveC
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// A view that enters the hierarchy while an assistive client is reading
    /// must not send SwiftUI's label lookup into itself (#2993).
    ///
    /// The crash: under the window's selectable-text default, a `Text` is
    /// backed by an AppKit text view. An `.accessibilityLabel` that reaches
    /// that `Text` through a wrapper (any layout or style modifier, a
    /// `Group`, a `Label`) makes the text view answer for the label. When
    /// such a view is inserted after a client has turned accessibility on,
    /// a read of its description asks the SwiftUI node, the node asks the
    /// text view, and the text view asks the node again, until the stack
    /// overflows. A view that existed before the client attached does not
    /// recurse, which is why a walk of an already-built card passes.
    ///
    /// The fix at each site is to make the view its own accessibility
    /// element before labelling it (app/README.md, Copyable Text). A
    /// labelled control (a button, a menu, a text field) does not recurse.
    @Suite(.serialized) @MainActor struct AccessibilityLabelRecursionTests {
        nonisolated private static let cardFixtures: [Components.Schemas.AttentionItemSnapshot] =
            AttentionFixtures.defaultInbox() + [
                AttentionFixtures.readyWithReviewGuide(),
                AttentionFixtures.degradedReady(),
                AttentionFixtures.staleReady(),
                AttentionFixtures.scopeKeptReady(),
                AttentionFixtures.readyWithElapsedChecksDeadline(),
                AttentionFixtures.publishedTaskReady(),
            ]

        /// Keeps the probe honest: the bare construct has to recurse, or a
        /// clean result from the other tests means nothing. If this fails
        /// on a newer macOS, SwiftUI no longer has the defect.
        @Test func theProbeCatchesALabelOnAWrappedSelectableText() throws {
            let recursing = try AccessibilityInsertionProbe.recursingElements(
                in: Text("shown").padding(8).accessibilityLabel("spoken"))
            #expect(!recursing.isEmpty, "the probe no longer reproduces the #2993 recursion")
        }

        @Test func aLabelledElementDoesNotRecurse() throws {
            let recursing = try AccessibilityInsertionProbe.recursingElements(
                in: Text("shown").padding(8)
                    .accessibilityElement(children: .ignore)
                    .accessibilityAddTraits(.isStaticText)
                    .accessibilityLabel("spoken"))
            #expect(recursing.isEmpty)
        }

        /// The view the reported crashes ran through: the review round's
        /// verdict chip on a task's current run.
        @Test func aStateChipDoesNotRecurse() throws {
            let recursing = try AccessibilityInsertionProbe.recursingElements(
                in: VStack {
                    StateChip(label: "Pending", color: .inkDim)
                    StateChip(label: "Running", color: .accentText, glyph: "●")
                })
            #expect(recursing.isEmpty, "recursing: \(recursing)")
        }

        /// The task page's title, which no screenshot surface inserts as a
        /// selectable text.
        @Test func aTaskNameDoesNotRecurse() throws {
            let recursing = try AccessibilityInsertionProbe.recursingElements(
                in: VStack {
                    TaskNameLabel(name: .init(text: "Summarize review rounds", source: .agent))
                    TaskNameLabel(name: .init(text: "task-0123", source: .identifier), lineLimit: nil)
                })
            #expect(recursing.isEmpty, "recursing: \(recursing)")
        }

        /// Every decision card, with its interactive controls on.
        @Test(arguments: cardFixtures)
        func insertingADecisionCardDoesNotRecurse(
            _ snapshot: Components.Schemas.AttentionItemSnapshot
        ) throws {
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            store.replaceAll(with: [snapshot])
            let recursing = try AccessibilityInsertionProbe.recursingElements(
                in: DecisionDetailView(store: store, itemID: snapshot.item.id),
                settling: 0.2)
            #expect(recursing.isEmpty, "\(snapshot.item.id) recursing: \(recursing)")
        }
    }

    /// Inserts a view under the window's selectable-text default after an
    /// assistive client's first read, then reads every element the way a
    /// client does and names the ones whose label lookup re-entered itself.
    @MainActor enum AccessibilityInsertionProbe {
        static func recursingElements(
            in view: some View, width: CGFloat = 560, settling: TimeInterval = 0.05
        ) throws -> [String] {
            _ = FreesideFont.registration
            let detector = try #require(
                LabelReentryDetector.install(),
                "SwiftUI's accessibility node class or its label method moved")
            setAssistiveClientAttributes(true)
            let insertion = Insertion()
            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: width, height: 900),
                styleMask: [.titled], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            defer {
                window.close()
                setAssistiveClientAttributes(false)
                detector.remove()
            }
            window.contentView = NSHostingView(
                rootView: InsertedLater(insertion: insertion, width: width) { view })
            let root = try #require(window.contentView)
            func settle() {
                root.layoutSubtreeIfNeeded()
                RunLoop.main.run(until: Date().addingTimeInterval(settling))
                root.layoutSubtreeIfNeeded()
            }

            settle()
            // The first read is what makes SwiftUI build its accessibility
            // tree; only views inserted after it can recurse.
            var activation = Walk(detector: detector)
            activation.visit(root, depth: 0)
            insertion.inserted = true
            settle()
            var walk = Walk(detector: detector)
            walk.visit(root, depth: 0)
            // A walk that never reached the inserted view would pass
            // vacuously.
            try #require(
                walk.seen.count > activation.seen.count,
                "the walk after insertion read no more than the walk before it")
            try #require(!walk.truncated, "the accessibility tree outran the walk's bounds")
            return walk.recursing
        }

        /// The attributes an assistive client sets. They are process-wide,
        /// so the probe turns them back off.
        private static func setAssistiveClientAttributes(_ on: Bool) {
            for name in ["AXEnhancedUserInterface", "AXManualAccessibility"] {
                NSApplication.shared.accessibilitySetValue(
                    on, forAttribute: NSAccessibility.Attribute(rawValue: name))
            }
        }

        @MainActor @Observable fileprivate final class Insertion {
            var inserted = false
        }

        private struct InsertedLater<Content: View>: View {
            let insertion: Insertion
            let width: CGFloat
            @ViewBuilder let content: () -> Content

            var body: some View {
                VStack {
                    Text("Anchor")
                    if insertion.inserted { content() }
                }
                .frame(width: width, height: 900, alignment: .topLeading)
                // The default `FreesideRootView` sets for every window.
                .textSelection(.enabled)
            }
        }

        @MainActor private struct Walk {
            let detector: LabelReentryDetector
            var seen = Set<ObjectIdentifier>()
            var recursing: [String] = []
            /// The tree was deeper or larger than the bounds that keep a
            /// walk from hanging.
            var truncated = false

            mutating func visit(_ object: AnyObject, depth: Int) {
                guard depth <= 64, seen.count < 5_000 else {
                    truncated = true
                    return
                }
                guard seen.insert(ObjectIdentifier(object)).inserted,
                    let element = object as? NSObject
                else { return }
                // An assistive client's reads enter through this method, the
                // first AppKit frame in the crash reports.
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

                // The description and the title are the two reads the crash
                // reports recursed in.
                let before = detector.reentries
                _ = value(.description)
                _ = value(.title)
                if detector.reentries > before {
                    recursing.append(value(.value) as? String ?? String(describing: type(of: element)))
                }
                for child in value(.children) as? [AnyObject] ?? [] {
                    visit(child, depth: depth + 1)
                }
            }
        }
    }

    /// Counts the times SwiftUI's accessibility node is asked for its label
    /// while it is already answering for it, and answers nil the second
    /// time. Without it the recursion overflows the stack and takes the
    /// test process down with no failure to read.
    ///
    /// The node's label method is the one Objective-C dispatch in the loop
    /// the crash reports show, so it is the one place a test can interpose.
    @MainActor final class LabelReentryDetector {
        private(set) var reentries = 0
        private var answering = Set<ObjectIdentifier>()
        private let method: Method
        private let original: IMP

        private init(method: Method, original: IMP) {
            self.method = method
            self.original = original
        }

        static func install() -> LabelReentryDetector? {
            let selector = NSSelectorFromString("accessibilityLabel")
            guard let nodeClass = NSClassFromString("SwiftUI.AccessibilityNode"),
                let method = class_getInstanceMethod(nodeClass, selector),
                // An inherited method would be replaced for every subclass
                // of the class that owns it.
                method != class_getInstanceMethod(class_getSuperclass(nodeClass), selector)
            else { return nil }
            let original = method_getImplementation(method)
            let detector = LabelReentryDetector(method: method, original: original)
            typealias Label = @convention(c) (AnyObject, Selector) -> Unmanaged<NSString>?
            let label = unsafeBitCast(original, to: Label.self)
            let block: @convention(block) (AnyObject) -> NSString? = { node in
                let answer: String? = MainActor.assumeIsolated {
                    let id = ObjectIdentifier(node)
                    guard detector.answering.insert(id).inserted else {
                        detector.reentries += 1
                        return nil
                    }
                    defer { detector.answering.remove(id) }
                    return label(node, selector)?.takeUnretainedValue() as String?
                }
                return answer as NSString?
            }
            method_setImplementation(method, imp_implementationWithBlock(block))
            return detector
        }

        func remove() {
            method_setImplementation(method, original)
        }
    }
#endif
