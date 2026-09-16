#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    @Suite(.serialized) @MainActor struct SpecApprovalReaderLayoutTests {
        private let longText =
            (1...35).map {
                "## Section \($0)\n\nPreserve existing sessions during migration and verify the new reader.\n\n"
            }.joined() + "## Final paragraph\n\nThe complete specification ends here."

        @Test func byteLimitSpecificationMountsOnlyVisibleBlocks() {
            let host = mount(text: String(repeating: "# task\n\n", count: 8192), width: 480, height: 900)
            defer { host.close() }
            #expect((1..<256).contains(textFields(in: host).count))
        }

        @Test func denseDiffUsesBoundedChunksAndMeasuresTheFinalLine() throws {
            let host = mount(
                SpecApprovalReaderViewport {
                    UnifiedDiffView(
                        unified: "@@ -0,0 +1,20001 @@\n" + String(repeating: "+a\n-b\n", count: 10_000)
                            + "+" + String(repeating: "long offscreen row ", count: 100),
                        linesAdded: 20_001, linesRemoved: 0, truncated: false
                    )
                    .id("reader-content")
                }, width: 480, height: 900)
            defer { host.close() }
            #expect((1..<400).contains(textFields(in: host).count))
            try reachesEnd(verticalScroll(in: host))
            let scroll = try #require(scrolls(in: host).first { $0.hasHorizontalScroller })
            #expect(try #require(scroll.documentView).frame.width > scroll.contentSize.width)
            #expect(
                textFields(in: host).contains {
                    $0.stringValue.hasSuffix(String(repeating: "long offscreen row ", count: 100))
                })
        }

        @Test func longSpecificationUsesTheWholeTallViewport() throws {
            let short = mount(text: longText, width: 480, height: 400)
            let tall = mount(text: longText, width: 480, height: 900)
            defer {
                short.close()
                tall.close()
            }
            let shortScroll = try verticalScroll(in: short)
            let tallScroll = try verticalScroll(in: tall)

            #expect(tallScroll.contentSize.height > 800)
            #expect(tallScroll.contentSize.height - shortScroll.contentSize.height > 450)
            try reachesEnd(tallScroll)
        }

        @Test func shortSpecificationHasNoMinimumHeightInnerViewport() throws {
            let host = mount(text: "# Short specification\n\nFinal paragraph.", width: 480, height: 900)
            defer { host.close() }
            let scroll = try verticalScroll(in: host)
            let document = try #require(scroll.documentView)

            #expect(document.frame.height < 280)
            #expect(scroll.contentView.bounds.minY == 0)
        }

        @Test func narrowLargeTextKeepsDigestAndCodeReachable() throws {
            let host = mount(
                text: longText + "\n\n```swift\n" + String(repeating: "longCodeLine", count: 30) + "\n```",
                width: 320, height: 500, size: .accessibility3)
            defer { host.close() }
            let scroll = try verticalScroll(in: host)
            let document = try #require(scroll.documentView)

            #expect(document.frame.width <= 320)
            try reachesEnd(scroll)
            settle(host)
            let horizontal = try #require(scrolls(in: host).first { $0.hasHorizontalScroller })
            let codeWidth = try #require(horizontal.documentView).frame.width
            #expect(codeWidth > horizontal.contentSize.width)
        }

        @Test(arguments: [DynamicTypeSize.large, .accessibility3], [30, 160])
        func diffHasOneVerticalOwnerAndKeepsHorizontalOverflow(size: DynamicTypeSize, lineCount: Int) throws {
            let lastLine = "+line \(lineCount) " + String(repeating: "漢🙂é wide ", count: 30)
            let host = FreesideFont.$screenshotDynamicTypeSize.withValue(size) {
                mount(
                    SpecApprovalReaderViewport {
                        UnifiedDiffView(
                            unified: "@@ -1,1 +1,\(lineCount) @@\n"
                                + (1..<lineCount).map {
                                    "\($0.isMultiple(of: 2) ? "+" : "-")line \($0) "
                                        + String(repeating: "wide ", count: 30)
                                }.joined(separator: "\n") + "\n" + lastLine,
                            linesAdded: lineCount, linesRemoved: 1, truncated: true
                        )
                        .id("reader-content")
                    }.environment(\.dynamicTypeSize, size), width: 320, height: 500)
            }
            defer { host.close() }
            let vertical = try verticalScroll(in: host)
            try reachesEnd(vertical)
            settle(host)
            let horizontal = try #require(scrolls(in: host).first { $0.hasHorizontalScroller })
            let lines = try #require(horizontal.documentView)
            #expect(lines.frame.width > horizontal.contentSize.width)
            #expect(lines.frame.height <= horizontal.contentSize.height + 1)
            let lastRow = try #require(textFields(in: host).first { $0.stringValue.hasSuffix(lastLine) })
            let lastRowFrame = lastRow.convert(lastRow.bounds, to: vertical.documentView)
            #expect(lastRowFrame.maxY <= vertical.documentVisibleRect.maxY + 1)
        }

        @Test func laterDenseHunkStartsCollapsed() {
            let host = mount(
                SpecApprovalReaderViewport {
                    UnifiedDiffView(
                        unified: "@@ -1 +1 @@\n-old\n+new\n@@ -10 +10,200 @@\n"
                            + String(repeating: "+hidden row\n", count: 200),
                        linesAdded: 201, linesRemoved: 1, truncated: false)
                }, width: 480, height: 900)
            defer { host.close() }
            #expect(textFields(in: host).contains { $0.stringValue == "+new" })
            #expect(!textFields(in: host).contains { $0.stringValue.contains("hidden row") })
        }

        private func mount(
            text: String, width: CGFloat, height: CGFloat, size: DynamicTypeSize = .large
        ) -> ReaderHost {
            FreesideFont.$screenshotDynamicTypeSize.withValue(size) {
                mount(
                    SpecApprovalReaderViewport {
                        SpecificationReaderView(
                            text: text, mediaType: .text_sol_markdown,
                            digest: "sha256:" + String(repeating: "a", count: 64)
                        )
                        .id("reader-content")
                    }.environment(\.dynamicTypeSize, size), width: width, height: height)
            }
        }

        private func mount(_ view: some View, width: CGFloat, height: CGFloat) -> ReaderHost {
            _ = FreesideFont.registration
            let driver = ScrollDriver()
            let host = ReaderHost(
                rootView: AnyView(
                    ScrollViewReader { proxy in
                        view.onAppear { driver.toEnd = { proxy.scrollTo("reader-content", anchor: .bottom) } }
                    }))
            host.driver = driver
            host.testTypeSize = FreesideFont.screenshotDynamicTypeSize
            host.frame = CGRect(x: 0, y: 0, width: width, height: height)
            let window = NSWindow(
                contentRect: host.frame, styleMask: [.borderless], backing: .buffered, defer: false)
            window.isReleasedWhenClosed = false
            window.contentView = host
            host.testWindow = window
            window.orderFront(nil)
            settle(host)
            return host
        }

        private final class ReaderHost: NSHostingView<AnyView> {
            var testWindow: NSWindow?
            var driver: ScrollDriver?
            var testTypeSize: DynamicTypeSize?

            func close() {
                testWindow?.close()
                testWindow = nil
            }
        }

        private final class ScrollDriver {
            var toEnd: (() -> Void)?
        }

        private func verticalScroll(in host: NSView) throws -> NSScrollView {
            let vertical = scrolls(in: host).filter { $0.hasVerticalScroller }
            #expect(vertical.count == 1, "The production reader must have one vertical scroll owner")
            return try #require(vertical.first)
        }

        private func reachesEnd(_ scroll: NSScrollView) throws {
            let document = try #require(scroll.documentView)
            var ancestor: NSView? = scroll
            while ancestor != nil && !(ancestor is ReaderHost) { ancestor = ancestor?.superview }
            let host = try #require(ancestor as? ReaderHost)
            FreesideFont.$screenshotDynamicTypeSize.withValue(host.testTypeSize) {
                // Let SwiftUI realize the lazy end rows, then include the
                // viewport's padding after those rows settle at their real height.
                for _ in 0..<5 {
                    host.driver?.toEnd?()
                    settle(host)
                    scroll.contentView.scroll(
                        to: NSPoint(x: 0, y: max(0, document.frame.maxY - scroll.contentSize.height)))
                    scroll.reflectScrolledClipView(scroll.contentView)
                    settle(host)
                }
            }
            #expect(scroll.documentVisibleRect.maxY >= document.frame.maxY - 1)
            #expect(scroll.documentVisibleRect.minY < document.frame.maxY)
        }

        private func scrolls(in view: NSView) -> [NSScrollView] {
            ((view as? NSScrollView).map { [$0] } ?? []) + view.subviews.flatMap { scrolls(in: $0) }
        }

        private func textFields(in view: NSView) -> [NSTextField] {
            ((view as? NSTextField).map { [$0] } ?? []) + view.subviews.flatMap { textFields(in: $0) }
        }

        private func settle(_ host: NSView) {
            var ancestor: NSView? = host
            while ancestor != nil && !(ancestor is ReaderHost) { ancestor = ancestor?.superview }
            FreesideFont.$screenshotDynamicTypeSize.withValue((ancestor as? ReaderHost)?.testTypeSize) {
                // Lazy rows materialize after mount, so keep the screenshot
                // font bridge active throughout subsequent layout passes.
                let deadline = Date().addingTimeInterval(0.3)
                repeat {
                    host.layoutSubtreeIfNeeded()
                    RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                } while Date() < deadline
            }
        }
    }
#endif
