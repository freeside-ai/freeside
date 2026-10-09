#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// Where the standing notices sit, and where they leave what follows
    /// them: at the card's x and width at the top of the Mac's detail
    /// column, and in the window's own slot everywhere else.
    @Suite(.serialized) @MainActor struct StandingNoticesLayoutTests {
        private typealias Scale = DecisionCardComposition.Scale

        @Test(arguments: [CGFloat(1_000), 1_620])
        func aNoticeInTheDetailColumnTakesTheCardsXAndWidth(paneWidth: CGFloat) throws {
            let margin = Scale.paneMargin(compact: false)
            let notice = try #require(
                probeFrame(paneWidth: paneWidth) { AnyView($0.standingNoticeInset(.detailColumn)) })
            #expect(notice.minX == 24)
            #expect(notice.minX == margin.leading)
            #expect(notice.width == 640)
            #expect(notice.width == Scale.cardWidth)
            #expect(notice.minY == Scale.moduleGap)

            // A pane narrower than the cap and its margins shrinks the
            // notice with the card.
            let narrow = try #require(
                probeFrame(paneWidth: 600) { AnyView($0.standingNoticeInset(.detailColumn)) })
            #expect(narrow.minX == notice.minX)
            #expect(narrow.width == 600 - margin.leading - margin.trailing)
        }

        /// The slot above the list, on an iPhone and over a collapsed Mac
        /// sidebar, keeps the inset it had.
        @Test(arguments: [CGFloat(390), 1_620])
        func aNoticeInTheWindowSlotSpansTheWindow(paneWidth: CGFloat) throws {
            let notice = try #require(
                probeFrame(paneWidth: paneWidth) { AnyView($0.standingNoticeInset()) })
            #expect(notice.origin == CGPoint(x: 16, y: 4))
            #expect(notice.width == paneWidth - 32)
        }

        /// A notice starts where a card would, and the card follows it a
        /// module gap below at the same x and width. With nothing to say,
        /// the column is the card alone at the pane's own margin.
        @Test(arguments: [CGFloat(1_000), 1_620])
        func aCardFollowsTheNoticesAModuleGapBelow(paneWidth: CGFloat) throws {
            let margin = Scale.paneMargin(compact: false)
            let alone = try #require(cardFrame(paneWidth: paneWidth, notices: nil))
            #expect(alone.origin == CGPoint(x: margin.leading, y: margin.top))

            let quiet = try #require(cardFrame(paneWidth: paneWidth, notices: notices(.fresh)))
            #expect(quiet == alone)

            let one = try #require(cardFrame(paneWidth: paneWidth, notices: notices(.unreachable)))
            let banner = try #require(noticesHeight(notices(.unreachable), paneWidth: paneWidth))
            // The stack's height includes the module gap above its notice,
            // so the notice's own top is the pane's margin.
            #expect(one.minY == margin.top + banner)
            #expect(one.minX == alone.minX)
            #expect(one.width == alone.width)
            #expect(one.width == 640)

            // Two notices: the freshness banner and the stopped indicator.
            let stopped = notices(.unreachable, stopped: true)
            let two = try #require(cardFrame(paneWidth: paneWidth, notices: stopped))
            let both = try #require(noticesHeight(stopped, paneWidth: paneWidth))
            #expect(both > banner)
            #expect(two.minY == margin.top + both)
        }

        /// The column seats its content by asking whether a notice draws,
        /// and each notice decides that in its own body. The two must
        /// agree, or a card sits a module gap under nothing.
        @Test func theColumnExpectsANoticeExactlyWhenOneDraws() throws {
            let now = Date()
            let mismatch = InboxStore.Freshness.contractMismatch(
                daemonContract: "sha256:" + String(repeating: "a", count: 64))
            let states: [InboxStore.Freshness] = [
                .fresh, .unvalidated, .unreachable, .syncFailing, mismatch, .unauthenticated,
            ]
            let updates: [Date?] = [
                nil, now.addingTimeInterval(3_600),
                now.addingTimeInterval(-(SyncCoordinator.stalenessThreshold + 60)),
            ]
            for freshness in states {
                for lastUpdatedAt in updates {
                    for stopped in [false, true] {
                        let stack = notices(
                            freshness, lastUpdatedAt: lastUpdatedAt, stopped: stopped)
                        let height = try #require(noticesHeight(stack, paneWidth: 1_000))
                        #expect(
                            (height > 0) == stack.isShowing(at: now),
                            "\(freshness), updated \(String(describing: lastUpdatedAt)), stopped \(stopped)")
                    }
                }
            }
            #expect(notices(.fresh, saveWarning: "Not saved.").isShowing(at: now))
        }

        /// Collapsing the sidebar moves the notices from the column to the
        /// window's slot, and reopening it moves them back. The content is
        /// the same view through both, so an open card keeps its state and
        /// opens once.
        @Test func theContentSurvivesTheNoticesLeavingTheColumn() throws {
            let column = ColumnProbe.Column(notices: notices(.unreachable))
            let host = NSHostingView(rootView: ColumnProbe(column: column))
            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: 1_000, height: 600), styleMask: [.borderless],
                backing: .buffered, defer: false)
            window.contentView = host
            func settle(until done: () -> Bool) {
                let deadline = Date().addingTimeInterval(5)
                while !done(), Date() < deadline {
                    RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                    host.layoutSubtreeIfNeeded()
                }
            }

            let margin = Scale.paneMargin(compact: false)
            settle { column.appearances > 0 && column.contentTop != nil }
            let underNotice = try #require(column.contentTop)
            #expect(underNotice > margin.top)

            column.notices = nil
            settle { column.contentTop == margin.top }
            #expect(column.contentTop == margin.top)
            #expect(column.appearances == 1)

            column.notices = notices(.unreachable)
            settle { column.contentTop == underNotice }
            #expect(column.contentTop == underNotice)
            #expect(column.appearances == 1)
        }

        private func notices(
            _ freshness: InboxStore.Freshness, lastUpdatedAt: Date? = nil,
            stopped: Bool = false, saveWarning: String? = nil
        ) -> StandingNotices {
            StandingNotices(
                saveWarning: saveWarning, onDismissSaveWarning: {},
                freshness: freshness, lastUpdatedAt: lastUpdatedAt, onRePair: nil,
                operation: stopped
                    ? .init(
                        admission: .stopped,
                        stops: [
                            .init(
                                kind: .operator_stop, item_id: "system-health-stop",
                                command_id: "cmd-stop", since: AttentionFixtures.createdInstant)
                        ]) : nil,
                reason: { _ in nil }, onOpenItem: nil, placement: .detailColumn)
        }

        /// The height the notices take in a pane, read from where a probe
        /// lands under them.
        private func noticesHeight(_ notices: StandingNotices, paneWidth: CGFloat) -> CGFloat? {
            probeFrame(paneWidth: paneWidth) { probe in
                AnyView(
                    VStack(spacing: 0) {
                        notices
                        probe
                    })
            }?.minY
        }

        /// The frame of the card the detail column seats under `notices`.
        private func cardFrame(paneWidth: CGFloat, notices: StandingNotices?) -> CGRect? {
            let content = probeFrame(paneWidth: paneWidth) { probe in
                AnyView(
                    StandingDetailColumn(notices: notices) { topMargin in
                        probe.detailCard(compact: false, topMargin: topMargin)
                    })
            }
            guard let content else { return nil }
            let padding = Scale.padding(compact: false)
            return CGRect(
                x: content.minX - padding.leading,
                y: content.minY - padding.top,
                width: content.width + padding.leading + padding.trailing,
                height: content.height + padding.top + padding.bottom)
        }

        /// The frame of a 100pt-tall probe once `seat` has placed it in a
        /// 600pt-tall pane of the given width, in the pane's coordinates.
        private func probeFrame(paneWidth: CGFloat, seat: (AnyView) -> AnyView) -> CGRect? {
            final class Measurement: @unchecked Sendable {
                var content: CGRect?
            }
            let measurement = Measurement()
            let probe = Color.clear
                .frame(height: 100)
                .onGeometryChange(for: CGRect.self) { geometry in
                    geometry.frame(in: .named("pane"))
                } action: { frame in
                    measurement.content = frame
                }
            let pane = seat(AnyView(probe))
                .frame(width: paneWidth, height: 600, alignment: .topLeading)
                .coordinateSpace(name: "pane")

            let host = NSHostingView(rootView: pane)
            host.frame = CGRect(x: 0, y: 0, width: paneWidth, height: 600)
            host.layoutSubtreeIfNeeded()
            // The geometry callback lands on a later main-queue turn.
            let deadline = Date().addingTimeInterval(5)
            while measurement.content == nil, Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                host.layoutSubtreeIfNeeded()
            }
            return measurement.content
        }
    }

    /// A probe under `StandingDetailColumn` whose notices the test takes
    /// away and gives back: it counts its appearances and reports where its
    /// top lands in the pane.
    private struct ColumnProbe: View {
        @Observable final class Column: @unchecked Sendable {
            var notices: StandingNotices?
            var appearances = 0
            var contentTop: CGFloat?

            init(notices: StandingNotices?) {
                self.notices = notices
            }
        }

        let column: Column

        var body: some View {
            StandingDetailColumn(notices: column.notices) { topMargin in
                Color.clear
                    .frame(height: 100)
                    .onAppear { column.appearances += 1 }
                    .onGeometryChange(for: CGFloat.self) { geometry in
                        geometry.frame(in: .named("pane")).minY
                    } action: { top in
                        column.contentTop = top
                    }
                    .padding(.top, topMargin ?? DecisionCardComposition.Scale.paneMargin(compact: false).top)
            }
            .frame(width: 1_000, height: 600, alignment: .topLeading)
            .coordinateSpace(name: "pane")
        }
    }
#endif
