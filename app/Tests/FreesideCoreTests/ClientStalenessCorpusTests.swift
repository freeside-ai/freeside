import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

// The client-staleness class, enumerated in one place so the axis list is the
// spec (the client mirror of the daemon's returned-object forge corpus). Each
// axis is a way a client could serve stale run/timeline state as current; the
// covering test named beside it proves it cannot. A new axis added here without
// a covering test is a gap by construction.
//
//  1. Cache-format-version invalidation. A pre-runs format-2 cache forces one
//     bootstrap on upgrade while preserving the pending-command ledger.
//     Covered: CacheStoreTests.aPreRunsFormatTwoCachePreservesOnlyTheCommandLedger,
//     .roundTripsThePendingCommandLedger; SyncCoordinatorTests
//     .aHeartbeatEpochDiscardPreservesTheLedger, .aBootstrapEpochDiscardPreservesTheLedger.
//  2. Bootstrap older than a completed partial read is rejected and refetched.
//     Covered: SyncCoordinatorTests.aBootstrapOlderThanAPartialReadRefetchesBeforeAdopting.
//  3. Partial-read cursors rechecked before the heartbeat path marks fresh.
//     Covered: SyncCoordinatorTests.aHeartbeatOlderThanAPartialReadBootstrapsBeforeFreshness,
//     .runAndTimelinePartialReadsDoNotMarkTheCacheCurrent.
//  4. An empty run list still converges freshness against the server revision.
//     Covered: SyncCoordinatorTests.emptyRunListBootstrapsAfterTheServerRevisionAdvances.
//  5. Daemon-clock as_of, not the device clock, drives observation freshness.
//     Covered: RunTimelineViewTests.daemonSnapshotClockKeepsAHealthyObservationLive,
//     .staleGoneObservationShowsAnObservationGap.
//  6. Liveness is re-evaluated from each timeline response's as_of, so a newer
//     response decays a previously-live observation.
//     Covered here: aNewerTimelineResponseDecaysAPreviouslyLiveObservation.
@Suite struct ClientStalenessCorpusTests {
    // Axis 6. The presentation reads the arriving response's daemon-clock
    // as_of (RunTimelineView renders InvocationPresentation from timeline.as_of
    // on every pass), so a later timeline response whose as_of has advanced
    // past the freshness window re-evaluates a running invocation whose own
    // observed_at did not advance: the daemon kept projecting but stopped
    // observing this attempt. It must decay from live "Running" to an
    // observation gap rather than keep claiming liveness against a frozen
    // instant. The two responses share one observation to isolate the
    // response's as_of as the only thing that changed.
    @Test func aNewerTimelineResponseDecaysAPreviouslyLiveObservation() {
        let observedAt = Date(timeIntervalSinceReferenceDate: 1_000)
        let observation = Components.Schemas.InvocationObservation(
            invocation_id: "invocation-1",
            run_id: "run-1",
            status: .running,
            live: true,
            observed_at: observedAt
        )

        // First response: the daemon observed the attempt running moments ago.
        let firstResponse = InvocationPresentation(observation, asOf: observedAt.addingTimeInterval(5))
        #expect(firstResponse.label == "Running")
        #expect(firstResponse.symbol == "wave.3.right.circle.fill")

        // A later response's as_of has advanced past the 30s window while the
        // observation is unchanged; liveness re-evaluates to a gap.
        let laterResponse = InvocationPresentation(observation, asOf: observedAt.addingTimeInterval(45))
        #expect(laterResponse.label == "Observation gap")
        #expect(laterResponse.symbol == "exclamationmark.triangle")
    }
}
