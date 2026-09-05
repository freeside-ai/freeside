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

    @Test func timelineTitleIsTheCampaignIdentityOrTheRunID() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        #expect(RunDisplay.timelineTitle(run) == "Campaign campaign-freeside-acceptance · Attempt 2")

        run.attempt_number = nil
        #expect(RunDisplay.timelineTitle(run) == RunFixtures.activeRunID)

        run.attempt_number = 2
        run.campaign_id = nil
        #expect(RunDisplay.timelineTitle(run) == RunFixtures.activeRunID)
    }

    @Test func observationsGroupByOwningStageNewestFirst() throws {
        let run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        let base = Date(timeIntervalSinceReferenceDate: 1_000)
        func observation(_ id: String, at offset: TimeInterval) -> Components.Schemas.InvocationObservation {
            .init(invocation_id: id, run_id: run.id, status: .completed, live: false, observed_at: base + offset)
        }
        let first = observation("inv-\(run.id)-1", at: 0)
        let second = observation("inv-\(run.id)-2", at: 60)
        let stray = observation("inv-unknown", at: 30)

        let groups = RunTimelineGrouping.groups(invocations: [first, stray, second], stages: run.stages)

        #expect(groups.map(\.label) == ["Implementation", RunTimelineGrouping.unattributedLabel])
        #expect(groups[0].invocations.map(\.invocation_id) == [second.invocation_id, first.invocation_id])
        #expect(groups[1].invocations.map(\.invocation_id) == [stray.invocation_id])
    }

    @Test func daemonShapedImplementStageGroupsAsImplementation() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        run.stages[0].name = "implement"
        let owned = Components.Schemas.InvocationObservation(
            invocation_id: "inv-\(run.id)-2", run_id: run.id, status: .running, live: true,
            observed_at: Date(timeIntervalSinceReferenceDate: 1_000))

        let groups = RunTimelineGrouping.groups(invocations: [owned], stages: run.stages)

        #expect(groups.map(\.label) == ["Implementation"])
    }

    @Test func repeatedImplementStagesShareOneImplementationGroup() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        run.stages[0].name = "implement"
        run.stages.append(
            .init(
                id: "remediation-1-\(run.id)", run_id: run.id, name: "implement",
                attempts: [
                    .init(
                        id: "attempt-remediation-1", stage_id: "remediation-1-\(run.id)",
                        number: 1, invocation_id: "inv-remediation-1")
                ]))
        let base = Date(timeIntervalSinceReferenceDate: 1_000)
        let production = Components.Schemas.InvocationObservation(
            invocation_id: "inv-\(run.id)-2", run_id: run.id, status: .completed, live: false, observed_at: base)
        let remediation = Components.Schemas.InvocationObservation(
            invocation_id: "inv-remediation-1", run_id: run.id, status: .running, live: true, observed_at: base + 60)

        let groups = RunTimelineGrouping.groups(invocations: [production, remediation], stages: run.stages)

        #expect(groups.map(\.label) == ["Implementation"])
        #expect(groups[0].invocations.map(\.invocation_id) == [remediation.invocation_id, production.invocation_id])
    }

    @Test func newestUnattributedObservationLeadsTheGroups() throws {
        let run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        let base = Date(timeIntervalSinceReferenceDate: 1_000)
        let owned = Components.Schemas.InvocationObservation(
            invocation_id: "inv-\(run.id)-2", run_id: run.id, status: .running, live: true, observed_at: base)
        let stray = Components.Schemas.InvocationObservation(
            invocation_id: "inv-unknown", run_id: run.id, status: .gone, live: false, observed_at: base + 1)

        let groups = RunTimelineGrouping.groups(invocations: [owned, stray], stages: run.stages)

        #expect(groups.map(\.label) == [RunTimelineGrouping.unattributedLabel, "Implementation"])
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
