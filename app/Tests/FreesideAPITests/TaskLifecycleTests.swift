import Foundation
import Testing

@testable import FreesideAPI

@Suite struct TaskLifecycleTests {
    @Test func confirmedAndAbandonedFixturesRoundTripThroughMockBootstrap() async throws {
        let active = try #require(TaskFixtures.defaultTasks().first { $0.task.lifecycle == .active })
        for terminal in [TaskFixtures.confirmedStopped(active), TaskFixtures.explicitlyAbandoned(active)] {
            let server = MockServer(tasks: [terminal])
            let bootstrap = try await APIClientFactory.mock(server: server).getSyncBootstrap().ok.body.json
            let actual = try #require(bootstrap.tasks.first)
            #expect(actual.task == terminal.task)
            #expect(!actual.task.wip)
            #expect(actual.task.run_ids == active.task.run_ids)
            #expect(actual.task.current_position == active.task.current_position)
        }
    }

    @Test func confirmedQueuedTaskNeedsNoRunPositionOrSyntheticFact() async throws {
        var queued = try #require(TaskFixtures.defaultTasks().first)
        queued.task.run_ids = []
        queued.task.campaign_ids = []
        queued.task.current_position = nil
        queued.task.lifecycle = nil
        queued.task.lifecycle_facts = []
        queued.task.wip = false
        let confirmed = TaskFixtures.confirmedStopped(queued)
        let server = MockServer(items: [], runs: [], tasks: [confirmed])
        let bootstrap = try await APIClientFactory.mock(server: server).getSyncBootstrap().ok.body.json
        let actual = try #require(bootstrap.tasks.first)
        #expect(actual.task.lifecycle == .stopped)
        #expect(actual.task.current_position == nil)
        #expect(actual.task.lifecycle_facts.isEmpty)
        #expect(actual.task.run_ids.isEmpty)
        for state in [Components.Schemas.TaskCancellationState.requested, .failed_to_stop] {
            var forged = confirmed
            forged.task.cancellation?.value1.state = state
            #expect(MockContractValidation.taskSnapshotBreach(forged, serverRevision: forged.as_of_revision) != nil)
        }
        var fabricated = confirmed
        fabricated.task.cancellation = nil
        #expect(MockContractValidation.taskSnapshotBreach(fabricated, serverRevision: fabricated.as_of_revision) != nil)
        fabricated = queued
        fabricated.task.lifecycle = .abandoned
        #expect(MockContractValidation.taskSnapshotBreach(fabricated, serverRevision: fabricated.as_of_revision) != nil)
        fabricated = confirmed
        fabricated.task.wip = true
        #expect(MockContractValidation.taskSnapshotBreach(fabricated, serverRevision: fabricated.as_of_revision) != nil)
    }

    @Test func pendingAndFailedCancellationDoNotFabricateStoppedProjection() async throws {
        let active = try #require(TaskFixtures.defaultTasks().first { $0.task.lifecycle == .active })
        for state in [Components.Schemas.TaskCancellationState.requested, .failed_to_stop] {
            var snapshot = active
            snapshot.task.cancellation = TaskFixtures.confirmedStopped(active).task.cancellation
            snapshot.task.cancellation?.value1.state = state
            if state == .requested {
                snapshot.task.cancellation?.value1.acknowledgement = nil
            } else {
                snapshot.task.cancellation?.value1.acknowledgement?.value1.state = .failed_to_stop
            }
            let bootstrap = try await APIClientFactory.mock(server: MockServer(tasks: [snapshot]))
                .getSyncBootstrap().ok.body.json
            let actual = try #require(bootstrap.tasks.first)
            #expect(actual.task.lifecycle == active.task.lifecycle)
            #expect(actual.task.wip == active.task.wip)
            #expect(actual.task.lifecycle_facts == active.task.lifecycle_facts)
        }
    }
}
