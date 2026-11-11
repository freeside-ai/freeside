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
        @Test func nothingRoutedDrawsTheNothingOpenPlaceholder() async throws {
            let lines = try await mountedLines(showsInboxClearResult: false) { $0.contains("Nothing open") }
            #expect(lines.contains("Nothing open"))
            #expect(lines.contains("Choose an item in the sidebar."))
            #expect(!lines.contains("Nothing needs you."))
        }

        @Test func theInboxClearResultKeepsItsOwnLines() async throws {
            // An empty inbox, as after the last item closes: an open item
            // arriving by sync would clear the result before it is read.
            let lines = try await mountedLines(showsInboxClearResult: true, server: MockServer(items: [])) {
                $0.contains("Nothing needs you.")
            }
            #expect(lines.contains("Nothing needs you."))
            #expect(!lines.contains("Nothing open"))
        }

        @Test func aBlockingFindingLeavesTheInboxClearResultStanding() async throws {
            // A finding is not a Needs you row (#2920): when sync brings one
            // open and blocking, with the gate it closes, after the result
            // is up, the inbox still reads clear; the standing notice above
            // it carries the stop. An open Needs You item would clear it.
            let finding = AttentionFixtures.blockingSystemHealth()
            var synced = false
            let lines = try await mountedLines(
                showsInboxClearResult: true, server: MockServer(items: [finding]),
                afterMount: { session in
                    guard case .ready(let coordinator) = session.phase else { return }
                    await coordinator.bootstrap()
                    synced =
                        coordinator.store.snapshotsByID[finding.item.id] != nil
                        && coordinator.unattendedOperation?.admission == .stopped
                },
                until: { $0.contains("Nothing needs you.") })
            #expect(synced)
            #expect(lines.contains("Nothing needs you."))
            #expect(!lines.contains("Nothing open"))
        }

        @Test func aNeedsYouRowArrivingClearsTheInboxClearResult() async throws {
            // The counterpart of the finding case above: an open decision
            // about work is a Needs You row, and its arrival by sync ends
            // the result for the placeholder.
            let question = AttentionFixtures.fixture(type: .agent_question)
            let lines = try await mountedLines(
                showsInboxClearResult: true, server: MockServer(items: [question]),
                afterMount: { session in
                    guard case .ready(let coordinator) = session.phase else { return }
                    await coordinator.bootstrap()
                },
                until: { $0.contains("Nothing open") })
            #expect(lines.contains("Nothing open"))
            #expect(!lines.contains("Nothing needs you."))
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
        }

        /// The text mounted in a Mac window holding a root paired with
        /// `server` with nothing routed, once `done` holds for it or five
        /// seconds pass. `afterMount` runs once the root is in its window;
        /// the root doesn't sync on its own in this harness.
        private func mountedLines(
            showsInboxClearResult: Bool, server: MockServer = MockServer(),
            afterMount: (AppSession) async -> Void = { _ in }, until done: ([String]) -> Bool
        ) async throws -> [String] {
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
            host.layoutSubtreeIfNeeded()
            await afterMount(session)
            return withExtendedLifetime(window) { settledLines(in: host, until: done) }
        }

        /// The text mounted under `host` once `done` holds for it or five
        /// seconds pass. Synchronous, since the run loop turns only outside
        /// an async context.
        private func settledLines(in host: NSView, until done: ([String]) -> Bool) -> [String] {
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
