#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The decision detail picks one column or two from the width of its
    /// pane. The card caps its own width until that choice is made, so the
    /// measurement must not read the card back. The card's own place in the
    /// pane (R18) is measured here too.
    @Suite(.serialized) @MainActor struct DecisionPaneWidthTests {
        @Test func aScrollViewNarrowerThanItsPaneReportsThePane() {
            final class Measurement: @unchecked Sendable {
                var width: CGFloat?
            }
            let measurement = Measurement()
            let paneWidth: CGFloat = 1_200
            let pane = ScrollView {
                Color.clear.frame(width: 560, height: 100)
            }
            .onPaneWidthChange { measurement.width = $0 }
            .frame(width: paneWidth, height: 600)

            let host = NSHostingView(rootView: pane)
            host.frame = CGRect(x: 0, y: 0, width: paneWidth, height: 600)
            host.layoutSubtreeIfNeeded()
            // The geometry callback lands on a later main-queue turn.
            let deadline = Date().addingTimeInterval(5)
            while measurement.width == nil, Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                host.layoutSubtreeIfNeeded()
            }

            #expect(measurement.width == paneWidth)
        }

        /// R18: every card in the detail column starts at one x and one y,
        /// and a one-column card stops at its cap, so selecting something
        /// changes the content and not its shape.
        @Test func aOneColumnCardSitsTopLeadingAndStopsAtItsCap() throws {
            typealias Scale = DecisionCardComposition.Scale
            let margin = Scale.paneMargin(compact: false)

            let wide = try #require(cardFrame(paneWidth: 1_000, twoColumns: false))
            #expect(wide.origin == CGPoint(x: 24, y: 20))
            #expect(wide.origin == CGPoint(x: margin.leading, y: margin.top))
            #expect(wide.width == 640)
            #expect(wide.width == Scale.cardWidth)

            // A pane narrower than the cap and its margins shrinks the card
            // instead of cutting it.
            let narrow = try #require(cardFrame(paneWidth: 600, twoColumns: false))
            #expect(narrow.origin == wide.origin)
            #expect(narrow.width == 600 - margin.leading - margin.trailing)

            // A card under a row of the column's own keeps the x and takes
            // the row's gap for its top margin.
            let underRow = try #require(cardFrame(paneWidth: 1_000, twoColumns: false, topMargin: 10))
            #expect(underRow.origin == CGPoint(x: wide.minX, y: 10))
            #expect(underRow.width == wide.width)
        }

        /// The two-column card fills the pane where two columns begin, at
        /// the one-column card's x and y, and its right column is 360 with
        /// the rest left to the modules.
        @Test func theTwoColumnCardFillsThePaneBesideA360Column() throws {
            typealias Scale = DecisionCardComposition.Scale
            let margin = Scale.paneMargin(compact: false)
            let padding = Scale.padding(compact: false)

            let card = try #require(cardFrame(paneWidth: 1_000, twoColumns: true))
            #expect(card.origin == CGPoint(x: margin.leading, y: margin.top))
            #expect(card.width == 1_000 - margin.leading - margin.trailing)
            #expect(card.width == 952)

            // The laid-out card: its action region spans the right column,
            // so the region's frame in the card's own space is the column's.
            let column = try #require(actionRegionFrame(paneWidth: 1_000))
            #expect(column.width == 360)
            #expect(column.width == Scale.controlColumnWidth)
            #expect(column.minX == 520 + Scale.columnGap)
            #expect(column.maxX == card.width - padding.leading - padding.trailing)

            // Past its own cap the card stops growing; what a wider pane
            // does with the rest is still the owner's call.
            let widest = try #require(cardFrame(paneWidth: 1_620, twoColumns: true))
            #expect(widest.origin == card.origin)
            #expect(widest.width == Scale.wideCardWidth)
        }

        /// A timeline in a detail column takes the card's seat, and a size
        /// class that changes under an open timeline (a resized iPad window,
        /// a rotated phone) re-seats it without rebuilding it, so its folds
        /// and loaded evidence stay.
        @Test func aTimelinePageKeepsItsContentAcrossASizeClassChange() throws {
            let pane = TimelinePageProbe.Pane()
            let host = NSHostingView(rootView: TimelinePageProbe(pane: pane))
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

            settle { pane.appearances > 0 && pane.content != nil }
            let padding = DecisionCardComposition.Scale.padding(compact: false)
            let card = try #require(cardFrame(paneWidth: 1_000, twoColumns: false))
            let seated = try #require(pane.content)
            #expect(seated.minX == card.minX + padding.leading)
            #expect(seated.minY == card.minY + padding.top)
            #expect(seated.width == card.width - padding.leading - padding.trailing)

            pane.compact = true
            settle { pane.content?.minX == 24 }
            #expect(pane.content?.origin == CGPoint(x: 24, y: 24))
            #expect(pane.content?.size == CGSize(width: 820 - 48, height: 100))
            #expect(pane.appearances == 1)
        }

        /// The action region's frame in the space of the card the decision
        /// detail lays out in a pane of the given width.
        private func actionRegionFrame(paneWidth: CGFloat) -> CGRect? {
            _ = FreesideFont.registration
            let snapshot = AttentionFixtures.fixture(type: .ready_for_final_review)
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            store.replaceAll(with: [snapshot])
            let detail = DecisionDetailView(
                store: store,
                itemID: snapshot.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: AttentionFixtures.createdInstant)

            final class Measurement: @unchecked Sendable {
                var frame: CGRect?
            }
            let measurement = Measurement()
            let card =
                detail
                .screenshotCard(
                    snapshot.item, at: .large, detailWidth: paneWidth,
                    actionRegionFrameChanged: { measurement.frame = $0 }
                )
                .environment(\.dynamicTypeSize, .large)
                .frame(width: paneWidth, alignment: .topLeading)
                .fixedSize(horizontal: false, vertical: true)

            let host = NSHostingView(rootView: AnyView(card))
            host.frame = CGRect(x: 0, y: 0, width: paneWidth, height: 4_000)
            host.layoutSubtreeIfNeeded()
            // The callback can report more than once while the layout
            // settles, so read the frame once it has held still.
            let deadline = Date().addingTimeInterval(5)
            var last: CGRect?
            var unchangedSince: Date?
            while Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                host.layoutSubtreeIfNeeded()
                if measurement.frame != last {
                    last = measurement.frame
                    unchangedSince = last == nil ? nil : Date()
                } else if let unchangedSince, Date().timeIntervalSince(unchangedSince) >= 0.25 {
                    break
                }
            }
            return measurement.frame
        }

        /// The frame of the card `detailCard` draws around a probe in a
        /// pane of the given width, in the pane's coordinates. The probe is
        /// the card's content, so the card is the probe's frame grown by the
        /// card's own padding.
        private func cardFrame(
            paneWidth: CGFloat, twoColumns: Bool, topMargin: CGFloat? = nil
        ) -> CGRect? {
            final class Measurement: @unchecked Sendable {
                var content: CGRect?
            }
            let measurement = Measurement()
            let pane = Color.clear
                .frame(height: 100)
                .onGeometryChange(for: CGRect.self) { geometry in
                    geometry.frame(in: .named("pane"))
                } action: { frame in
                    measurement.content = frame
                }
                .detailCard(compact: false, twoColumns: twoColumns, topMargin: topMargin)
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

            guard let content = measurement.content else { return nil }
            let padding = DecisionCardComposition.Scale.padding(compact: false)
            return CGRect(
                x: content.minX - padding.leading,
                y: content.minY - padding.top,
                width: content.width + padding.leading + padding.trailing,
                height: content.height + padding.top + padding.bottom)
        }

        /// The right column holds the control group and the folds. A
        /// read-only card has neither, so two columns would leave it an
        /// empty pane beside its modules.
        @Test func aCardWithNothingForItsRightColumnStaysOneColumn() {
            let blocked = AttentionFixtures.fixture(type: .blocked)
            let review = AttentionFixtures.fixture(type: .ready_for_final_review)
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            store.replaceAll(with: [blocked, review])
            let detail = DecisionDetailView(
                store: store,
                itemID: blocked.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: AttentionFixtures.createdInstant)

            #expect(blocked.item.requested_decision.isEmpty)
            #expect(!detail.drawsTwoColumns(blocked.item, paneIsWide: true))
            #expect(detail.drawsTwoColumns(review.item, paneIsWide: true))
            #expect(!detail.drawsTwoColumns(review.item, paneIsWide: false))
        }
    }

    /// A probe under `timelinePage` whose size class the test flips: it
    /// counts its appearances and reports its frame in the pane.
    private struct TimelinePageProbe: View {
        @Observable final class Pane: @unchecked Sendable {
            var compact = false
            var appearances = 0
            var content: CGRect?
        }

        let pane: Pane

        var body: some View {
            Color.clear
                .frame(height: 100)
                .onAppear { pane.appearances += 1 }
                .onGeometryChange(for: CGRect.self) { geometry in
                    geometry.frame(in: .named("pane"))
                } action: { frame in
                    pane.content = frame
                }
                .timelinePage(compact: pane.compact)
                .frame(width: 1_000, height: 600, alignment: .topLeading)
                .coordinateSpace(name: "pane")
        }
    }
#endif
