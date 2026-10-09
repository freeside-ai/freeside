#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The card's place in the detail pane (R18): one x, one y, and one
    /// width, whatever the pane's.
    @Suite(.serialized) @MainActor struct DecisionPaneWidthTests {
        /// Every card in the detail column starts at one x and one y and
        /// stops at its cap at any pane width, so selecting something
        /// changes the content and not its shape, and a wide pane never
        /// widens a card.
        @Test(arguments: [CGFloat(1_000), 1_620])
        func aCardSitsTopLeadingAndStopsAtItsCap(paneWidth: CGFloat) throws {
            typealias Scale = DecisionCardComposition.Scale
            let margin = Scale.paneMargin(compact: false)

            let wide = try #require(cardFrame(paneWidth: paneWidth))
            #expect(wide.origin == CGPoint(x: 24, y: 20))
            #expect(wide.origin == CGPoint(x: margin.leading, y: margin.top))
            #expect(wide.width == 640)
            #expect(wide.width == Scale.cardWidth)

            // A pane narrower than the cap and its margins shrinks the card
            // instead of cutting it.
            let narrow = try #require(cardFrame(paneWidth: 600))
            #expect(narrow.origin == wide.origin)
            #expect(narrow.width == 600 - margin.leading - margin.trailing)

            // A card under a row of the column's own keeps the x and takes
            // the row's gap for its top margin.
            let underRow = try #require(cardFrame(paneWidth: paneWidth, topMargin: 10))
            #expect(underRow.origin == CGPoint(x: wide.minX, y: 10))
            #expect(underRow.width == wide.width)
        }

        /// The laid-out decision card keeps its one column in a wide pane:
        /// the action region spans the card under its modules and never
        /// moves beside them.
        @Test func aWidePaneKeepsTheActionsUnderTheModules() throws {
            let padding = DecisionCardComposition.Scale.padding(compact: false)
            let atCap = try #require(actionRegionFrame(paneWidth: 688))
            let wide = try #require(actionRegionFrame(paneWidth: 1_620))
            #expect(wide == atCap)
            #expect(wide.minX == 0)
            #expect(
                wide.width
                    == DecisionCardComposition.Scale.cardWidth - padding.leading - padding.trailing)
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
            let card = try #require(cardFrame(paneWidth: 1_000))
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
                    snapshot.item, at: .large,
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
        private func cardFrame(paneWidth: CGFloat, topMargin: CGFloat? = nil) -> CGRect? {
            guard
                let content = probeFrame(
                    paneWidth: paneWidth,
                    seat: { AnyView($0.detailCard(compact: false, topMargin: topMargin)) })
            else { return nil }
            let padding = DecisionCardComposition.Scale.padding(compact: false)
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
