#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The card's place in the detail pane (R18, revised 3 Nov 2026): one
    /// column, centered in the pane, as wide as the pane's margins allow up
    /// to its cap.
    @Suite(.serialized) @MainActor struct DecisionPaneWidthTests {
        /// Every card in the detail column is centered in the pane, starts
        /// at the pane's top margin, and takes the pane's width less its
        /// margins up to the column's cap, so selecting something changes
        /// the content and not its shape.
        @Test(arguments: [CGFloat(1_000), 1_620])
        func aCardCentersInThePaneAndStopsAtTheColumnsCap(paneWidth: CGFloat) throws {
            typealias Scale = DecisionCardComposition.Scale
            let margin = Scale.paneMargin(compact: false)

            let wide = try #require(cardFrame(paneWidth: paneWidth))
            #expect(wide.midX == paneWidth / 2)
            #expect(wide.minY == 20)
            #expect(wide.minY == margin.top)
            #expect(wide.width == min(paneWidth - 48, 960))
            #expect(wide.width == min(paneWidth - margin.leading - margin.trailing, Scale.columnCap))

            // A pane narrower than the cap and its margins shrinks the card
            // instead of cutting it.
            let narrow = try #require(cardFrame(paneWidth: 600))
            #expect(narrow.origin == CGPoint(x: margin.leading, y: margin.top))
            #expect(narrow.width == 552)

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
            let narrow = try #require(actionRegionFrame(paneWidth: 688))
            let wide = try #require(actionRegionFrame(paneWidth: 1_620))
            #expect(narrow.minX == 0)
            #expect(narrow.width == 640 - padding.leading - padding.trailing)
            #expect(wide.minX == 0)
            #expect(
                wide.width
                    == DecisionCardComposition.Scale.columnCap - padding.leading - padding.trailing)
        }

        /// Running text keeps the prose measure in a wide column, inside
        /// the card's content, while the controls take the card's full
        /// content width. The agent's summary stands for the card's prose:
        /// it is the line that mounts as its own view, so its frame is the
        /// one a test can read from the laid-out card.
        @Test func aWideCardHoldsItsProseAtTheMeasure() throws {
            typealias Scale = DecisionCardComposition.Scale
            let padding = Scale.padding(compact: false)
            let paneWidth: CGFloat = 1_620
            let card = try #require(cardFrame(paneWidth: paneWidth))
            let contentX = card.minX + padding.leading
            let contentWidth = card.width - padding.leading - padding.trailing
            #expect(contentWidth > Scale.proseMeasure)

            // A summary long enough to wrap at any width this pane offers.
            let sentence = "The run passed every check and the reviewer found nothing left to change. "
            var snapshot = AttentionFixtures.fixture(type: .ready_for_final_review)
            let index = try #require(snapshot.item.agent_claims.firstIndex { $0.label == "freeside.summary" })
            snapshot.item.agent_claims[index].text = .init(
                media_type: .text_sol_markdown, content: String(repeating: sentence, count: 4))
            let summary = try #require(
                mountedTextFrame(of: snapshot, paneWidth: paneWidth) { $0.hasPrefix(sentence.prefix(40)) })
            #expect(summary.minX >= contentX)
            #expect(summary.width <= Scale.proseMeasure)
            // Wrapped against the measure, not short of it.
            #expect(summary.width > Scale.proseMeasure - 40)

            let controls = try #require(actionRegionFrame(paneWidth: paneWidth))
            #expect(controls.width == contentWidth)
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

        /// The frame, in the pane's coordinates, of the first text mounted
        /// as its own view whose string `matches`, in the card the decision
        /// detail draws for `snapshot` in a pane of the given width.
        private func mountedTextFrame(
            of snapshot: Components.Schemas.AttentionItemSnapshot, paneWidth: CGFloat,
            matching matches: (String) -> Bool
        ) -> CGRect? {
            _ = FreesideFont.registration
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            store.replaceAll(with: [snapshot])
            let detail = DecisionDetailView(
                store: store,
                itemID: snapshot.item.id,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: AttentionFixtures.createdInstant)
            let card =
                detail
                .screenshotCard(snapshot.item, at: .large)
                .environment(\.dynamicTypeSize, .large)
                .frame(width: paneWidth, alignment: .topLeading)
                .fixedSize(horizontal: false, vertical: true)
            return mountedTextFrame(in: AnyView(card), paneWidth: paneWidth, matching: matches)
        }

        /// The frame, in the pane's coordinates, of the first text mounted
        /// as its own view under `root` whose string `matches`.
        private func mountedTextFrame(
            in root: AnyView, paneWidth: CGFloat, matching matches: (String) -> Bool
        ) -> CGRect? {
            let host = NSHostingView(rootView: root)
            // Selectable text mounts its own view only inside a window.
            let window = NSWindow(
                contentRect: CGRect(x: 0, y: 0, width: paneWidth, height: 3_000), styleMask: [.borderless],
                backing: .buffered, defer: false)
            window.contentView = host
            func find(in view: NSView) -> CGRect? {
                let string: String?
                if let field = view as? NSTextField {
                    string = field.stringValue
                } else if view.accessibilityRole() == .staticText {
                    string = view.accessibilityValue() as? String
                } else {
                    string = nil
                }
                if let string, matches(string) { return view.convert(view.bounds, to: host) }
                for subview in view.subviews { if let frame = find(in: subview) { return frame } }
                return nil
            }
            let deadline = Date().addingTimeInterval(5)
            var frame: CGRect?
            repeat {
                RunLoop.main.run(until: Date().addingTimeInterval(0.02))
                host.layoutSubtreeIfNeeded()
                frame = find(in: host)
            } while frame == nil && Date() < deadline
            return frame
        }

        /// A finding's card in the System Health pane on the Mac (#2920)
        /// takes the pane's width inside the detail card's margins, under its
        /// back control, and its prose stays inside the card's content at no
        /// more than the measure. The seat places any card the same way; a
        /// summary long enough to wrap stands for the prose, since it mounts
        /// as its own view and a finding's lines don't.
        @Test func aCardInTheSystemHealthSeatFillsThePaneInsideTheCardMargins() throws {
            typealias Scale = DecisionCardComposition.Scale
            _ = FreesideFont.registration
            let margin = Scale.paneMargin(compact: false)
            let padding = Scale.padding(compact: false)
            let paneWidth = SettingsWindowContent.paneWidth
            let sentence = "The run passed every check and the reviewer found nothing left to change. "
            var snapshot = AttentionFixtures.fixture(type: .ready_for_final_review)
            let index = try #require(snapshot.item.agent_claims.firstIndex { $0.label == "freeside.summary" })
            snapshot.item.agent_claims[index].text = .init(
                media_type: .text_sol_markdown, content: String(repeating: sentence, count: 4))
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            store.replaceAll(with: [snapshot])
            let seat = AnyView(
                SystemHealthCardSeat(
                    itemID: snapshot.item.id, store: store, onBack: {}, rendersInteractiveControls: false
                )
                .environment(\.pinnedNow, AttentionFixtures.createdInstant)
                .environment(\.dynamicTypeSize, .large)
                .frame(width: paneWidth, alignment: .topLeading)
                .fixedSize(horizontal: false, vertical: true))

            let summary = try #require(
                mountedTextFrame(in: seat, paneWidth: paneWidth) { $0.hasPrefix(sentence.prefix(40)) })
            // The same card in a detail pane of the same width: the seat
            // keeps its x and width and moves it down under the back row.
            let inDetailPane = try #require(
                mountedTextFrame(of: snapshot, paneWidth: paneWidth) { $0.hasPrefix(sentence.prefix(40)) })
            #expect(summary.minX == inDetailPane.minX)
            #expect(summary.width == inDetailPane.width)
            #expect(summary.minY > inDetailPane.minY)
            #expect(summary.minX >= margin.leading + padding.leading)
            #expect(summary.maxX <= paneWidth - margin.trailing - padding.trailing)
            #expect(summary.width <= Scale.proseMeasure)
        }

        /// An empty state in the detail pane centers in the column a card
        /// takes, which on the Mac is centered in the pane, so a wide pane
        /// holds it over the place its cards draw. A pane no wider than
        /// that column centers it across itself, and so does every other
        /// pane at any width.
        @Test func anEmptyStateInTheDetailPaneCentersInTheCardsColumn() throws {
            func block(_ seat: UnavailableStateView.Seat, paneWidth: CGFloat) throws -> CGRect {
                try #require(
                    probeFrame(paneWidth: paneWidth, probeWidth: 200) {
                        AnyView($0.emptyStateSeat(seat))
                    })
            }
            for paneWidth in [CGFloat(1_000), 1_620] {
                let seated = try block(.detailColumn, paneWidth: paneWidth)
                let card = try #require(cardFrame(paneWidth: paneWidth))
                #expect(seated.midX == card.midX)
                #expect(seated.midX == paneWidth / 2)
                #expect(seated.midY == 300)

                let acrossPane = try block(.pane, paneWidth: paneWidth)
                #expect(acrossPane.midX == paneWidth / 2)
                #expect(acrossPane.midY == 300)
            }

            for seat in [UnavailableStateView.Seat.pane, .detailColumn] {
                let phone = try block(seat, paneWidth: 390)
                #expect(phone.midX == 195)
                #expect(phone.midY == 300)
            }
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
        /// The probe takes the width it is offered unless given its own.
        private func probeFrame(
            paneWidth: CGFloat, probeWidth: CGFloat? = nil, seat: (AnyView) -> AnyView
        ) -> CGRect? {
            measuredFrame(
                of: AnyView(Color.clear.frame(width: probeWidth, height: 100)), paneWidth: paneWidth,
                seat: seat)
        }

        /// The frame of `view` once `seat` has placed it in a 600pt-tall
        /// pane of the given width, in the pane's coordinates.
        private func measuredFrame(
            of view: AnyView, paneWidth: CGFloat, seat: (AnyView) -> AnyView
        ) -> CGRect? {
            final class Measurement: @unchecked Sendable {
                var content: CGRect?
            }
            let measurement = Measurement()
            let probe =
                view
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
