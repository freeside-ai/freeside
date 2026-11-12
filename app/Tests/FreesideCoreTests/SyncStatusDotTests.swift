#if os(macOS)
    import Foundation
    import SwiftUI
    import Testing

    @testable import FreesideCore

    /// The sidebar's sync dot: colour only for attention, failure over
    /// staleness, and hover text that names a failure before the age.
    @MainActor
    struct SyncStatusDotTests {
        private let now = Date(timeIntervalSinceReferenceDate: 10_000)
        private var recent: Date { now.addingTimeInterval(-(SyncCoordinator.stalenessThreshold - 1)) }
        private var stale: Date { now.addingTimeInterval(-SyncCoordinator.stalenessThreshold) }

        @Test func currentUntilTheStalenessThreshold() {
            #expect(SyncStatusDot.status(freshness: .fresh, lastUpdatedAt: recent, at: now) == .current)
            #expect(SyncStatusDot.status(freshness: .unvalidated, lastUpdatedAt: recent, at: now) == .current)
            #expect(SyncStatusDot.status(freshness: .fresh, lastUpdatedAt: stale, at: now) == .stale)
        }

        @Test func neverUpdatedIsStale() {
            #expect(SyncStatusDot.status(freshness: .unvalidated, lastUpdatedAt: nil, at: now) == .stale)
        }

        @Test func everyFailingStateIsFailingEvenWhenRecent() {
            let failing: [InboxStore.Freshness] = [
                .unreachable, .syncFailing, .incompatible(.appBelowFloor(floor: 1, daemonVersion: "other")),
                .unauthenticated,
            ]
            for freshness in failing {
                #expect(SyncStatusDot.status(freshness: freshness, lastUpdatedAt: recent, at: now) == .failing)
            }
        }

        @Test func colourMarksOnlyAttention() {
            #expect(SyncStatusDot.tint(.current) == Color.inkFaint)
            #expect(SyncStatusDot.tint(.stale) == Color.accentText)
            #expect(SyncStatusDot.tint(.failing) == Color.waxText)
            #expect(SyncStatusDot.shapeSymbol(.current) == nil)
            #expect(SyncStatusDot.shapeSymbol(.stale) != SyncStatusDot.shapeSymbol(.failing))
        }

        @Test func summaryNamesTheFailureBeforeTheAge() {
            #expect(
                SyncStatusDot.summary(freshness: .fresh, lastUpdatedAt: recent, at: now)
                    == "Updated recently")
            #expect(
                SyncStatusDot.summary(freshness: .unreachable, lastUpdatedAt: recent, at: now)
                    == "Daemon unreachable · Updated recently")
            #expect(
                SyncStatusDot.summary(freshness: .unauthenticated, lastUpdatedAt: nil, at: now)
                    == "Device access revoked · Not updated yet")
        }
    }
#endif
