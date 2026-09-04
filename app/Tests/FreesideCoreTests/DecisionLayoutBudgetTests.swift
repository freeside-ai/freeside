#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The first-viewport budget for the decision shell: on a 560pt Mac card
    /// the operator must reach the action region without scrolling, so the
    /// finding_adjudication lead collapses and its rows expand in place above
    /// the actions rather than pushing them further down (#1107).
    @Suite(.serialized) @MainActor struct DecisionLayoutBudgetTests {
        /// The Mac detail column at its narrow width, where the budget binds.
        private let cardWidth: CGFloat = 560
        /// Measured from the card's own top edge, so the padding the screenshot
        /// wrapper adds around it does not spend the budget.
        private let actionRowBudget: CGFloat = 520

        @Test func twoCollapsedFindingsKeepTheActionRegionInTheFirstViewport()
            throws
        {
            let offset = try actionRegionOffset(expandedFindings: [])

            #expect(offset <= actionRowBudget)
        }

        /// Expanding a finding opens its proposal and daemon facts above the
        /// actions, which is what "in place" means here: the action region
        /// moves down by the opened content instead of the content appearing
        /// below the actions.
        @Test func expandingAFindingOpensItAboveTheActionRegion() throws {
            let collapsed = try actionRegionOffset(expandedFindings: [])
            let expanded = try actionRegionOffset(expandedFindings: ["review-finding-17"])

            #expect(expanded > collapsed)
        }

        private func actionRegionOffset(expandedFindings: Set<String>) throws -> CGFloat {
            _ = FreesideFont.registration
            let snapshot = AttentionFixtures.fixture(type: .finding_adjudication)
            let store = InboxStore(client: APIClientFactory.mock(server: MockServer()))
            store.replaceAll(with: [snapshot])
            let detail = DecisionDetailView(
                store: store,
                itemID: snapshot.item.id,
                expandedFindings: expandedFindings,
                loadsAttachments: false,
                showsValidationProgress: false,
                now: AttentionFixtures.createdInstant)

            final class Measurement: @unchecked Sendable {
                var minY: CGFloat?
            }
            let measurement = Measurement()
            let card =
                detail
                .screenshotCard(
                    snapshot.item,
                    at: .large,
                    detailWidth: cardWidth,
                    actionRegionFrameChanged: { measurement.minY = $0.minY }
                )
                .environment(\.dynamicTypeSize, .large)
                .frame(width: cardWidth, alignment: .topLeading)
                .fixedSize(horizontal: false, vertical: true)

            let host = NSHostingView(rootView: AnyView(card))
            host.frame = CGRect(x: 0, y: 0, width: cardWidth, height: 4_000)
            host.layoutSubtreeIfNeeded()
            // The geometry callback lands on the next main-queue turn, so the
            // run loop is pumped until it arrives rather than read
            // immediately. onGeometryChange can report more than once while
            // the layout settles, and returning the first frame would let an
            // intermediate value satisfy the budget while the settled layout
            // misses it, so the pump runs on until the reported frame holds
            // still for a settle window.
            let deadline = Date().addingTimeInterval(5)
            let settleWindow: TimeInterval = 0.25
            var lastObserved: CGFloat?
            var unchangedSince: Date?
            while Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
                host.layoutSubtreeIfNeeded()
                let observed = measurement.minY
                if observed != lastObserved {
                    lastObserved = observed
                    unchangedSince = observed == nil ? nil : Date()
                    continue
                }
                if let unchangedSince,
                    Date().timeIntervalSince(unchangedSince) >= settleWindow
                {
                    break
                }
            }
            return try #require(
                lastObserved,
                "the action region never reported a frame that settled")
        }
    }
#endif
