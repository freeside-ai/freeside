import Foundation
import Testing

@testable import FreesideCore

/// The freshness label reports whether the data is recent, not a live
/// second count. These pin the buckets: recent within the staleness
/// window, coarse relative time once stale, never a seconds unit.
@MainActor
struct LastUpdatedLabelTests {
    private let now = Date(timeIntervalSinceReferenceDate: 10_000)

    @Test func neverUpdatedReadsNotUpdatedYet() {
        #expect(LastUpdatedLabel.text(for: nil, at: now) == "Not updated yet")
    }

    @Test func freshlyUpdatedReadsRecently() {
        #expect(LastUpdatedLabel.text(for: now, at: now) == "Updated recently")
    }

    @Test func justInsideStalenessWindowStillReadsRecently() {
        let updated = now.addingTimeInterval(-(SyncCoordinator.stalenessThreshold - 1))
        #expect(LastUpdatedLabel.text(for: updated, at: now) == "Updated recently")
    }

    @Test func atStalenessThresholdSwitchesToRelativeTime() {
        let updated = now.addingTimeInterval(-SyncCoordinator.stalenessThreshold)
        let text = LastUpdatedLabel.text(for: updated, at: now)
        #expect(text.hasPrefix("Updated "))
        #expect(text != "Updated recently")
    }

    @Test func staleShowsCoarseRelativeTimeNotSeconds() {
        let updated = now.addingTimeInterval(-5 * 60)
        let text = LastUpdatedLabel.text(for: updated, at: now)
        #expect(text.hasPrefix("Updated "))
        #expect(text != "Updated recently")
        // Coarse by construction: no per-second resolution in the phrasing.
        #expect(!text.lowercased().contains("sec"))
    }
}
