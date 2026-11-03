#if os(macOS)
    import AppKit
    import FreesideAPI
    import FreesideTestSupport
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// What the Mac's detail column draws with nothing open (#2515): a
    /// placeholder that says where to choose, not a summary of the sidebar
    /// beside it, and the inbox-clear result after the last item closes.
    @Suite(.serialized) @MainActor struct DetailPlaceholderTests {
        @Test func nothingRoutedDrawsTheNothingOpenPlaceholder() throws {
            let lines = try mountedLines(showsInboxClearResult: false) { $0.contains("Nothing open") }
            #expect(lines.contains("Nothing open"))
            #expect(lines.contains("Choose an item in the sidebar."))
            #expect(!lines.contains("Nothing needs you."))
        }

        @Test func theInboxClearResultKeepsItsOwnLines() throws {
            // An empty inbox, as after the last item closes: an open item
            // arriving by sync would clear the result before it is read.
            let lines = try mountedLines(showsInboxClearResult: true, server: MockServer(items: [])) {
                $0.contains("Nothing needs you.")
            }
            #expect(lines.contains("Nothing needs you."))
            #expect(!lines.contains("Nothing open"))
        }

        /// A window can't be shorter than its content's minimum, which
        /// SwiftUI measures by proposing no space at all. These panes sit
        /// outside a scroll view, so a line that insists on its full wrapped
        /// height would report it one character wide, hundreds of points
        /// tall, and the window would open taller than the display.
        @Test func panesOutsideAScrollViewKeepAWindowShort() throws {
            func minimumHeight(_ view: some View) -> CGFloat {
                NSHostingController(rootView: view).sizeThatFits(in: .zero).height
            }
            for seat in [UnavailableStateView.Seat.pane, .detailColumn] {
                #expect(
                    minimumHeight(
                        UnavailableStateView(
                            glyph: .tasks, title: "Tasks", description: "Select a task to inspect its history.",
                            seat: seat)) < 200)
            }
            #expect(
                minimumHeight(
                    SidebarEmptyState(
                        glyph: .inbox, title: "No resolved items",
                        description: "Attention items in this scope will appear here."))
                    < 200)
        }

        /// The text mounted in a Mac window holding a root paired with
        /// `server` with nothing routed, once `done` holds for it or five
        /// seconds pass.
        private func mountedLines(
            showsInboxClearResult: Bool, server: MockServer = MockServer(), until done: ([String]) -> Bool
        ) throws -> [String] {
            let credentials = InMemoryCredentialStore()
            try credentials.save(
                try #require(
                    DeviceCredential(
                        deviceID: "device-1", token: testDeviceToken(for: "device-1"),
                        ntfySubscription: .mock)))
            let session = AppSession(
                client: APIClientFactory.mock(server: server), credentials: credentials,
                cache: InMemoryCacheStore(), cacheRoot: nil)
            let root = FreesideRootView(
                session: session, navigation: NavigationModel(launchInputs: .standard()),
                showsInboxClearResult: showsInboxClearResult)
            let host = NSHostingView(rootView: root)
            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: 1_440, height: 900), styleMask: [.borderless],
                backing: .buffered, defer: false)
            window.contentView = host
            var lines: [String] = []
            let deadline = Date().addingTimeInterval(5)
            repeat {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                host.layoutSubtreeIfNeeded()
                lines = mountedText(in: host)
            } while !done(lines) && Date() < deadline
            return lines
        }

        /// The strings of the text mounted under `view`. Selectable text is
        /// an `NSTextField` on macOS 26 and reports its string only through
        /// the accessibility API on macOS 27.
        private func mountedText(in view: NSView) -> [String] {
            let own: [String]
            if let field = view as? NSTextField {
                own = [field.stringValue]
            } else if view.accessibilityRole() == .staticText,
                let string = view.accessibilityValue() as? String
            {
                own = [string]
            } else {
                own = []
            }
            return own + view.subviews.flatMap { mountedText(in: $0) }
        }
    }
#endif
