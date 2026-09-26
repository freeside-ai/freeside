import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct TaskApprovalTests {
    @Test func recordedCampaignApprovalCompletesSpecification() throws {
        let fixture = TaskFixtures.approvedCampaign()
        for outcome in Components.Schemas.RunOutcome.allCases {
            var run = fixture.runs[0]
            run.run.outcome = outcome
            let position = try #require(
                TaskDisplay.position(
                    fixture.task.task, runs: [run], history: fixture.history))
            #expect(position.rail.entries.first { $0.id == "specification" }?.state == .completed)
            #expect(
                position.rail.entries.filter { $0.id != "specification" }
                    == RunDisplay.stageRail(run.run).entries.filter { $0.id != "specification" })
            #expect(position.hold == run.run.hold_reason.map { RunDisplay.label($0.value1) })
            #expect(position.rail.summary.contains("Specification completed"))
            #expect(position.qualification == nil)
        }
        for degraded in [false, true] {
            let item = AttentionFixtures.publishedTaskReady(degraded: degraded)
            let position = TaskDisplay.position(
                fixture.task.task, runs: fixture.runs, attentionItems: [item], history: fixture.history)
            #expect(
                position?.heading?.label == (degraded ? "Ready for Final Review (Degraded)" : "Ready for Final Review"))
        }
    }

    @Test func retryAndHistoricalSelectionUseTheirOwnCampaign() throws {
        var fixture = TaskFixtures.approvedCampaign()
        var retry = fixture.runs[0]
        retry.run.id = "retry"
        retry.run.attempt_number = 2
        retry.run.parent_run_id = fixture.runs[0].run.id
        fixture.runs[0].run.superseded_by = retry.run.id
        fixture.task.task.run_ids.append(retry.run.id)
        fixture.task.task.current_position?.value1.run_id = retry.run.id
        var member = try #require(fixture.history.sections[0].runs.first { $0.run_id == retry.run.parent_run_id })
        member.run_id = retry.run.id
        member.attempt_number = 2
        member.parent_run_id = retry.run.parent_run_id
        fixture.history.sections[0].runs.insert(member, at: 0)
        #expect(
            TaskDisplay.specificationApproval(
                fixture.task.task, runID: retry.run.id, run: retry.run, history: fixture.history) == .approved)
        let retryPosition = TaskDisplay.position(fixture.task.task, runs: [retry], history: fixture.history)
        #expect(
            TaskDisplay.progressLines(fixture.task.task, position: retryPosition).joined()
                .contains("Specification Completed"))
        // A revised campaign starts with its own unapproved specification.
        var revised = fixture.runs[1]
        revised.run.id = "revised-specification"
        revised.run.campaign_id = "revised-campaign"
        revised.run.lifecycle = .active
        revised.run.superseded_by = nil
        fixture.task.task.campaign_ids.append("revised-campaign")
        fixture.task.task.run_ids.append(revised.run.id)
        fixture.task.task.current_position?.value1.run_id = revised.run.id
        fixture.history.sections.insert(
            .init(
                campaign_id: "revised-campaign", events: [],
                runs: [
                    .init(run_id: revised.run.id, role: .init(value1: .specification), milestones: [], events: [])
                ]), at: 0)
        #expect(
            TaskDisplay.specificationApproval(
                fixture.task.task, runID: revised.run.id, run: revised.run, history: fixture.history) == .unapproved)
        #expect(
            TaskDisplay.position(fixture.task.task, runs: [revised], history: fixture.history)?
                .rail.entries.first { $0.id == "specification" }?.state != .completed)
        #expect(
            TaskDisplay.specificationApproval(
                fixture.task.task, runID: fixture.runs[0].run.id, run: fixture.runs[0].run,
                history: fixture.history) == .approved)
    }

    @Test func contradictoryOrMissingHistoryCannotSupplyApproval() {
        let fixture = TaskFixtures.approvedCampaign()
        let changes: [(inout Components.Schemas.TaskTimeline) -> Void] = [
            { $0.task_id = "foreign" }, { $0.project_id = "foreign" },
            { $0.sections[0].campaign_id = "foreign" },
            { $0.sections[0].events[0].campaign_id = "foreign" },
            { $0.sections[0].events[0].run_id = "missing" },
            { $0.sections[0].events[0].specification_run_id = RunFixtures.readyRunID },
            { $0.sections[0].events[0].approved_spec_digest = nil },
            { $0.sections[0].events[0].approved_spec_digest = .init(value1: "wrong") },
            { $0.sections[0].runs.removeAll { $0.role?.value1 == .specification } },
            { $0.sections[0].runs[0].role = nil },
            { $0.sections[0].runs[0].role = .init(value1: .specification) },
            { $0.sections[0].runs[0].attempt_number = 2 },
            { $0.sections[0].events = [] }, { $0.sections = [] },
        ]
        for change in changes {
            var history = fixture.history
            change(&history)
            #expect(
                TaskDisplay.specificationApproval(
                    fixture.task.task, runID: fixture.runs[0].run.id, run: fixture.runs[0].run,
                    history: history) == .unavailable)
        }
        for change: (inout Components.Schemas.Run) -> Void in [
            { $0.task_id = "foreign" }, { $0.project_id = "foreign" }, { $0.campaign_id = "foreign" },
            { $0.spec_digest = "wrong" },
        ] {
            var run = fixture.runs[0].run
            change(&run)
            #expect(
                TaskDisplay.specificationApproval(
                    fixture.task.task, runID: run.id, run: run, history: fixture.history) == .unavailable)
        }
    }

    @Test func unavailableApprovalHistoryPreservesKnownSpecificationAccessibility() throws {
        var fixture = TaskFixtures.approvedCampaign()
        var run = fixture.runs[1]
        run.run.lifecycle = .active
        run.run.superseded_by = nil
        fixture.task.task.current_position?.value1.run_id = run.run.id
        fixture.task.task.current_position?.value1.stage = "specification"
        var contradictoryHistory = fixture.history
        contradictoryHistory.project_id = "foreign"
        for outcome in [Components.Schemas.RunOutcome.pending, .failed] {
            run.run.outcome = outcome
            let state = outcome == .failed ? "failed" : "current"
            for history in [nil, contradictoryHistory] {
                let position = try #require(
                    TaskDisplay.position(fixture.task.task, runs: [run], history: history))
                #expect(position.rail.summary.contains("Specification \(state)"))
                #expect(position.rail.summary.contains("approval history unavailable"))
            }
        }
        let fallback = try #require(TaskDisplay.position(fixture.task.task, runs: []))
        #expect(fallback.rail.summary.contains("Specification current"))
        #expect(fallback.rail.summary.contains("approval history unavailable"))
    }

    @Test func missingSnapshotUsesBoundHistoryAndQualifiesUnavailableFacts() throws {
        let fixture = TaskFixtures.approvedCampaign()
        let approved = try #require(TaskDisplay.position(fixture.task.task, runs: [], history: fixture.history))
        let unavailable = try #require(TaskDisplay.position(fixture.task.task, runs: []))
        #expect(approved.rail.entries.first { $0.id == "specification" }?.state == .completed)
        #expect(approved.rail.entries.dropFirst() == unavailable.rail.entries.dropFirst())
        #expect(approved.heading == unavailable.heading)
        #expect(unavailable.qualification == "Specification approval history unavailable")
        #expect(unavailable.rail.summary.contains("Specification approval history unavailable"))
        #expect(!unavailable.rail.summary.contains("Specification pending"))
    }
}

@Suite @MainActor struct TaskApprovalLoadingTests {
    private let fixture = TaskFixtures.approvedCampaign()

    private func server() -> MockServer {
        MockServer(runs: fixture.runs, tasks: [fixture.task], taskTimelines: [fixture.history])
    }

    @Test func coldListAndDetailShareHistoryAndPreservePartialCursor() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        #expect(coordinator.tasks.count == 1)
        let fullRevision = coordinator.cursors?.lastFullSnapshotRevision
        let entered = AsyncGate()
        let release = AsyncGate()
        let calls = Counter()
        await server.setBeforeRespond { operation in
            if operation == "getTaskTimeline" {
                await calls.increment()
                await entered.open()
                await release.wait()
            }
        }
        let list = Task { await coordinator.refreshTaskTimeline(for: fixture.task.task.id) }
        await entered.wait()
        let detail = Task { await coordinator.refreshTaskTimeline(for: fixture.task.task.id) }
        await release.open()
        await list.value
        await detail.value
        await coordinator.refreshTaskTimeline(for: fixture.task.task.id)
        #expect(await calls.count == 1)
        #expect(coordinator.cursors?.lastFullSnapshotRevision == fullRevision)
        let history = try #require(coordinator.taskTimelinesByTaskID[fixture.task.task.id])
        #expect(
            TaskDisplay.position(fixture.task.task, runs: coordinator.runs, history: history)?
                .rail.entries.first { $0.id == "specification" }?.state == .completed)
        let runView = RunTimelineView(coordinator: coordinator, snapshot: fixture.runs[0])
        #expect(runView.specificationApproval == .approved)
        #expect(runView.specificationLabel == "Approved specification")
        let specificationView = RunTimelineView(coordinator: coordinator, snapshot: fixture.runs[1])
        #expect(specificationView.specificationLabel == "Source specification")
        await coordinator.bootstrap()
        await coordinator.refreshTaskTimeline(for: fixture.task.task.id)
        #expect(await calls.count == 2)
    }

    @Test func offlineRelaunchRetriesThenEpochResetAndRemovalDiscardApproval() async throws {
        let server = server()
        let cache = InMemoryCacheStore()
        let client = APIClientFactory.mock(server: server)
        let first = SyncCoordinator(client: client, cache: cache)
        await first.bootstrap()
        await first.refreshTaskTimeline(for: fixture.task.task.id)
        let relaunched = SyncCoordinator(client: client, cache: cache)
        await server.setBeforeRespond { _ in throw InjectedFailure() }
        await relaunched.refreshTaskTimeline(for: fixture.task.task.id)
        #expect(relaunched.taskTimelinesByTaskID[fixture.task.task.id] != nil)
        #expect(relaunched.store.freshness != .fresh)
        #expect(relaunched.taskTimelineLoadStates[fixture.task.task.id] == .unavailable)
        await server.setBeforeRespond(nil)
        await relaunched.refreshTaskTimeline(for: fixture.task.task.id)
        #expect(relaunched.taskTimelineLoadStates[fixture.task.task.id] == .loaded)
        await server.rotateEpoch()
        await relaunched.heartbeat()
        #expect(relaunched.taskTimelinesByTaskID.isEmpty)
        await relaunched.refreshTaskTimeline(for: fixture.task.task.id)
        #expect(relaunched.taskTimelineLoadStates[fixture.task.task.id] == .loaded)
        await server.setBootstrapTransform { snapshot in
            var snapshot = snapshot
            snapshot.tasks = []
            return snapshot
        }
        await relaunched.bootstrap()
        #expect(relaunched.taskTimelinesByTaskID.isEmpty)
    }

    @Test func unavailableHistoryRetriesWithoutBlessingTheWholeCache() async throws {
        var fixture = fixture
        fixture.history.as_of_revision += 1
        let server = MockServer(runs: fixture.runs, tasks: [fixture.task], taskTimelines: [fixture.history])
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        let revision = coordinator.cursors?.lastFullSnapshotRevision
        await server.setBeforeRespond { operation in
            if operation == "getTaskTimeline" { throw InjectedFailure() }
        }
        await coordinator.refreshTaskTimeline(for: fixture.task.task.id)
        #expect(coordinator.taskTimelinesByTaskID.isEmpty)
        #expect(coordinator.taskTimelineLoadStates[fixture.task.task.id] == .unavailable)
        await server.setBeforeRespond(nil)
        await coordinator.refreshTaskTimeline(for: fixture.task.task.id)
        #expect(coordinator.taskTimelineLoadStates[fixture.task.task.id] == .loaded)
        #expect(coordinator.cursors?.lastFullSnapshotRevision == revision)
        #expect(coordinator.cursors?.highestObservedServerRevision == fixture.history.as_of_revision)
        #expect(coordinator.store.freshness == .unvalidated)
        for _ in 0..<2 { await coordinator.refreshTaskTimeline(for: "missing-task") }
        #expect(coordinator.taskTimelinesByTaskID["missing-task"] == nil)
        #expect(coordinator.taskTimelineLoadStates["missing-task"] == .unavailable)
    }

    @Test func foreignProjectHistoryIsRejectedBeforeCaching() async {
        var fixture = fixture
        fixture.history.project_id = "foreign"
        let server = MockServer(runs: fixture.runs, tasks: [fixture.task], taskTimelines: [fixture.history])
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        await coordinator.refreshTaskTimeline(for: fixture.task.task.id)
        #expect(coordinator.taskTimelinesByTaskID.isEmpty)
        #expect(coordinator.taskTimelineLoadStates[fixture.task.task.id] == .unavailable)
    }

    @Test func cancellationDoesNotSuppressReplacementRead() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await coordinator.bootstrap()
        let entered = AsyncGate()
        let release = AsyncGate()
        let calls = Counter()
        await server.setBeforeRespond { operation in
            if operation == "getTaskTimeline", await calls.incrementAndGet() == 1 {
                await entered.open()
                await release.wait()
            }
        }
        let first = Task { await coordinator.refreshTaskTimeline(for: fixture.task.task.id) }
        await entered.wait()
        first.cancel()
        let replacement = Task { await coordinator.refreshTaskTimeline(for: fixture.task.task.id) }
        await release.open()
        await first.value
        await replacement.value
        #expect(await calls.count == 2)
        #expect(coordinator.taskTimelineLoadStates[fixture.task.task.id] == .loaded)
    }
}
