#if os(macOS)
    import AppKit
    import FreesideAPI
    import FreesideModel
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// A card's action buttons stay where they drew while it validates
    /// (#1460): the progress row that reports validation sits after the
    /// buttons, so its coming and going moves nothing under the pointer.
    @Suite(.serialized) @MainActor struct CardActionPlacementTests {
        @Test func theButtonsHoldStillAsValidationEnds() async throws {
            _ = FreesideFont.registration
            let snapshot = AttentionFixtures.fixture(type: .ready_for_final_review)
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer(items: [snapshot])))
            store.replaceAll(with: [snapshot])
            let pending = DecisionModel(store: store, itemID: snapshot.item.id)
            let validated = DecisionModel(store: store, itemID: snapshot.item.id)
            await validated.validate()
            try #require(pending.validation == .pending)
            try #require(validated.validation == .validated)

            let whilePending = elements(of: snapshot, store: store, model: pending)
            let afterward = elements(of: snapshot, store: store, model: validated)

            // The row draws only while pending, so the comparison is real.
            let row = try #require(whilePending.labels[Self.validating])
            #expect(afterward.labels[Self.validating] == nil)
            // Folds after the row move down by its height; everything drawn
            // above it, the action buttons among them, holds still.
            let above = whilePending.buttons.filter { $0.value.maxY <= row.minY }
            #expect(Set(["Return to Agent", "View PR"]).isSubset(of: above.keys))
            for (label, frame) in above {
                #expect(afterward.buttons[label] == frame, "\(label)")
            }
        }

        private static let validating = "Validating current state…"

        private struct Elements {
            var buttons: [String: CGRect] = [:]
            var labels: [String: CGRect] = [:]
        }

        /// The frames of the labelled buttons and other labelled elements of
        /// the card for `snapshot`, drawn over `model` in a window, as an
        /// assistive client reads them, measured down from the card's top.
        private func elements(
            of snapshot: Components.Schemas.AttentionItemSnapshot, store: InboxStore, model: DecisionModel
        ) -> Elements {
            setAssistiveClientAttributes(true)
            defer { setAssistiveClientAttributes(false) }
            let card = DecisionDetailView(
                store: store, itemID: snapshot.item.id, loadsAttachments: false,
                now: AttentionFixtures.createdInstant, model: model
            )
            .screenshotCard(snapshot.item, at: .large)
            .environment(\.dynamicTypeSize, .large)
            .frame(width: 900, alignment: .topLeading)
            .fixedSize(horizontal: false, vertical: true)

            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: 900, height: 3_000), styleMask: [.borderless],
                backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            defer { window.close() }
            let host = NSHostingView(rootView: card)
            window.contentView = host
            for _ in 0..<20 {
                RunLoop.main.run(until: Date().addingTimeInterval(0.02))
                host.layoutSubtreeIfNeeded()
            }
            // Screen space rises from the bottom, and the window fits its
            // height to the card, so measure from the card's top.
            let top = window.convertToScreen(host.convert(host.bounds, to: nil)).maxY
            var found = Elements()
            var seen = Set<ObjectIdentifier>()
            visit(host, top: top, found: &found, seen: &seen)
            return found
        }

        private func visit(
            _ object: AnyObject, top: CGFloat, found: inout Elements, seen: inout Set<ObjectIdentifier>
        ) {
            guard seen.insert(ObjectIdentifier(object)).inserted, seen.count < 5_000,
                let element = object as? NSObject
            else { return }
            // SwiftUI's elements answer through the client entry point in a
            // test host, not the public protocol (ReadyCardAccessibilityTests).
            let read = NSSelectorFromString("_accessibilityValueForAttribute:clientError:")
            guard element.responds(to: read), let implementation = element.method(for: read) else { return }
            typealias ClientRead =
                @convention(c) (AnyObject, Selector, NSString, UnsafeMutablePointer<Int32>?) -> Unmanaged<
                    AnyObject
                >?
            let clientRead = unsafeBitCast(implementation, to: ClientRead.self)
            func value(_ attribute: String) -> Any? {
                var clientError: Int32 = 0
                return clientRead(element, read, attribute as NSString, &clientError)?.takeUnretainedValue()
            }
            let role = value(NSAccessibility.Attribute.role.rawValue) as? String
            let label =
                (value(NSAccessibility.Attribute.description.rawValue) as? String)
                ?? (value(NSAccessibility.Attribute.value.rawValue) as? String)
                ?? (value(NSAccessibility.Attribute.title.rawValue) as? String)
            if let label, !label.isEmpty, let screen = (value("AXFrame") as? NSValue)?.rectValue {
                let frame = CGRect(
                    x: screen.minX, y: top - screen.maxY, width: screen.width, height: screen.height)
                found.labels[label] = frame
                if role == NSAccessibility.Role.button.rawValue { found.buttons[label] = frame }
            }
            for child in value(NSAccessibility.Attribute.children.rawValue) as? [AnyObject] ?? [] {
                visit(child, top: top, found: &found, seen: &seen)
            }
        }

        private func setAssistiveClientAttributes(_ on: Bool) {
            for name in ["AXEnhancedUserInterface", "AXManualAccessibility"] {
                NSApplication.shared.accessibilitySetValue(
                    on, forAttribute: NSAccessibility.Attribute(rawValue: name))
            }
        }
    }
#endif
