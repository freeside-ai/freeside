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

    @Test func specificationCampaignLabelsItsSourceSpecification() {
        var specification = RunFixtures.defaultRuns()[0].run
        specification.stages[0].name = "specification"

        #expect(RunDisplay.specificationLabel(specification) == "Source specification")
    }

    @Test func productionLaneKeepsApprovedSpecificationAfterLaterStages() {
        var active = RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }!.run
        var ready = RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.readyRunID }!.run
        active.stages.insert(
            .init(id: "stage-active-implement", run_id: active.id, name: "implement", attempts: []),
            at: 0)
        ready.stages.insert(
            .init(id: "stage-ready-implement", run_id: ready.id, name: "implement", attempts: []),
            at: 0)

        #expect(RunDisplay.specificationLabel(active) == "Approved specification")
        #expect(RunDisplay.specificationLabel(ready) == "Approved specification")
    }
}
