#if os(macOS)
    import AppKit
    import FreesideAPI
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The decision detail picks one column or two from the width of its
    /// pane. The card caps its own width until that choice is made, so the
    /// measurement must not read the card back.
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
#endif
