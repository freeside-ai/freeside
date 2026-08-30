import Foundation
import Testing

@testable import FreesideCore

@MainActor
struct FreshnessBannerTests {
    @Test func missingUpdateHasNoStaleInstant() {
        #expect(FreshnessBanner.staleAt(lastUpdatedAt: nil) == nil)
    }

    @Test func staleInstantFollowsTheFreshnessThreshold() {
        let updated = Date(timeIntervalSinceReferenceDate: 10_000)

        #expect(
            FreshnessBanner.staleAt(lastUpdatedAt: updated)
                == updated.addingTimeInterval(SyncCoordinator.stalenessThreshold))
    }
}
