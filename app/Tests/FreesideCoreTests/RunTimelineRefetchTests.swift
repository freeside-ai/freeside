#if os(macOS)
    import AppKit
    import FreesideAPI
    import Synchronization
    import SwiftUI
    import Testing

    @testable import FreesideCore
    @testable import FreesideModel

    /// The Mac's run page in a window, refreshed the way sync refreshes a
    /// live run each round: the daemon's revision moves, the coordinator
    /// bootstraps, and the page refetches a timeline that draws the same
    /// rows (F56, #3455). The read is held so the page spends a visible
    /// while in `.loading`, as it does against a real daemon.
    @Suite(.serialized, .needsWindowServerSession) @MainActor struct RunTimelineRefetchTests {
        /// The page as the root view builds it: the run looked up from the
        /// coordinator on every pass, so a bootstrap hands it a new snapshot.
        struct Root: View {
            let coordinator: SyncCoordinator
            let runID: String

            var body: some View {
                if let run = coordinator.runs.first(where: { $0.run.id == runID }) {
                    RunTimelineView(coordinator: coordinator, snapshot: run)
                }
            }
        }

        /// A live run with no review round yet, whose Review section says
        /// so: the line the page used to drop while a refetch was in flight,
        /// which moved everything under it.
        @Test func aRefetchThatDrawsTheSameRowsLeavesThePageStill() async throws {
            _ = FreesideFont.registration
            let runID = "run-freeside-specification"
            let server = MockServer()
            let coordinator = SyncCoordinator(
                client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
            await coordinator.bootstrap()
            let host = NSHostingView(
                rootView: Root(coordinator: coordinator, runID: runID)
                    // A pinned clock keeps the strip and times from moving
                    // on their own while the test watches the pixels.
                    .environment(\.pinnedNow, RunFixtures.timeStripNow))
            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: 820, height: 300), styleMask: [.titled],
                backing: .buffered, defer: false)
            window.contentView = host
            window.orderFront(nil)
            defer { window.orderOut(nil) }

            func pageScroll() throws -> NSScrollView {
                func all(_ view: NSView) -> [NSScrollView] {
                    ((view as? NSScrollView).map { [$0] } ?? []) + view.subviews.flatMap(all)
                }
                return try #require(
                    all(host).max { ($0.documentView?.frame.height ?? 0) < ($1.documentView?.frame.height ?? 0) })
            }
            func pixels() throws -> Data {
                let rep = try #require(host.bitmapImageRepForCachingDisplay(in: host.bounds))
                host.cacheDisplay(in: host.bounds, to: rep)
                return Data(bytes: try #require(rep.bitmapData), count: rep.bytesPerPlane)
            }
            func settle(until done: () -> Bool, each: () throws -> Void = {}) async throws {
                let deadline = Date().addingTimeInterval(30)
                while !done() {
                    try #require(Date() < deadline, "timed out after 30 s waiting")
                    try await Task.sleep(for: .milliseconds(10))
                    host.layoutSubtreeIfNeeded()
                    try each()
                }
            }

            let taskID = try #require(coordinator.runs.first { $0.run.id == runID }?.run.task_id)
            func state() -> String {
                "run \(String(describing: coordinator.timelineLoadStates[runID])), "
                    + "task \(String(describing: coordinator.taskTimelineLoadStates[taskID])), "
                    + "freshness \(coordinator.store.freshness)"
            }
            /// Until the page has drawn the same frame for half a second
            /// with both of its reads answered: a fixed wait reads a slow
            /// machine's late layout as movement.
            func settleStill() async throws -> (height: CGFloat, pixels: Data) {
                var last: (height: CGFloat, pixels: Data)?
                var since = Date()
                let deadline = Date().addingTimeInterval(30)
                while true {
                    try #require(Date() < deadline, "timed out after 30 s waiting for the page to settle")
                    try await Task.sleep(for: .milliseconds(50))
                    host.layoutSubtreeIfNeeded()
                    let now = (height: try pageScroll().documentView?.frame.height ?? 0, pixels: try pixels())
                    let loaded =
                        coordinator.timelineLoadStates[runID] == .loaded
                        && coordinator.taskTimelineLoadStates[taskID] == .loaded
                    if !loaded || last?.height != now.height || last?.pixels != now.pixels {
                        last = now
                        since = Date()
                    } else if Date().timeIntervalSince(since) >= 0.5 {
                        return now
                    }
                }
            }

            _ = try await settleStill()
            let scroll = try pageScroll()
            scroll.contentView.scroll(to: NSPoint(x: 0, y: 150))
            scroll.reflectScrolledClipView(scroll.contentView)
            let (height, still) = try await settleStill()
            let offset = scroll.contentView.bounds.minY

            // The refetch's read is held until the test has watched the
            // page in `.loading`, so a busy machine can't answer it
            // between two looks.
            let reads = Atomic(0)
            let released = Atomic(false)
            await server.setBeforeRespond { operation in
                guard operation == "getRunTimeline" else { return }
                let deadline = Date().addingTimeInterval(30)
                while !released.load(ordering: .acquiring), Date() < deadline {
                    try await Task.sleep(for: .milliseconds(10))
                }
            }
            await server.setAfterRespond { operation in
                if operation == "getRunTimeline" { reads.add(1, ordering: .relaxed) }
            }
            var moved: [String] = []
            func look() throws {
                let sampled = scroll.documentView?.frame.height
                let drawn = try pixels()
                if sampled != height || scroll.contentView.bounds.minY != offset || drawn != still {
                    moved.append(
                        "\(state()): height \(String(describing: sampled)) from \(height), "
                            + "offset \(scroll.contentView.bounds.minY) from \(offset)")
                }
            }
            await server.advanceRun(id: runID)
            let bootstrap = Task { await coordinator.bootstrap() }
            try await settle(until: { coordinator.timelineLoadStates[runID] == .loading }, each: look)
            for _ in 0..<20 {
                try await Task.sleep(for: .milliseconds(10))
                host.layoutSubtreeIfNeeded()
                try look()
            }
            released.store(true, ordering: .releasing)
            try await settle(
                until: { reads.load(ordering: .relaxed) > 0 && coordinator.timelineLoadStates[runID] == .loaded },
                each: look)
            await bootstrap.value
            let (after, _) = try await settleStill()

            #expect(moved.isEmpty, "the page moved: \(moved.prefix(3).joined(separator: "; "))")
            #expect(after == height, "the page ends \(after) tall, from \(height): \(state())")
            #expect(try pixels() == still, "the page ends drawn differently: \(state())")
        }
    }
#endif
