import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct RunTimelineViewTests {
    @Test func roundIsOmittedUntilAStageHasAnAttempt() {
        var stage = RunFixtures.defaultRuns()[0].run.stages[0]
        stage.attempts = []

        #expect(RunDisplay.round(stage) == nil)
    }

    @Test func staleGoneObservationShowsAnObservationGap() {
        let now = Date(timeIntervalSinceReferenceDate: 1_000)
        let observation = Components.Schemas.InvocationObservation(
            invocation_id: "invocation-1",
            run_id: "run-1",
            status: .gone,
            live: false,
            observed_at: now.addingTimeInterval(-31)
        )

        let presentation = InvocationPresentation(observation, asOf: now)

        #expect(presentation.label == "Observation gap")
        #expect(presentation.symbol == "exclamationmark.triangle")
    }

    @Test func daemonSnapshotClockKeepsAHealthyObservationLive() {
        let daemonNow = Date(timeIntervalSinceReferenceDate: 1_000)
        let observation = Components.Schemas.InvocationObservation(
            invocation_id: "invocation-1",
            run_id: "run-1",
            status: .running,
            live: true,
            observed_at: daemonNow.addingTimeInterval(-29)
        )

        let presentation = InvocationPresentation(observation, asOf: daemonNow)

        #expect(presentation.label == "Running")
        #expect(presentation.symbol == "wave.3.right.circle.fill")
    }
}
