import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite @MainActor struct TaskReviewLoadingTests {
    private func server() -> MockServer {
        let timelines = RunFixtures.defaultTimelines().map { timeline in
            var timeline = timeline
            var round = RunFixtures.reviewRound(.completed, availability: .available)
            round.invocation_id = "review-\(timeline.run_id)"
            if timeline.run_id == "run-freeside-656" {
                round = RunFixtures.reviewRound(.completed, round: 2, findings: true, availability: .available)
                round.invocation_id = "review-superseded"
            }
            timeline.review = .init(value1: .init(rounds: [round]))
            return timeline
        }
        return MockServer(timelines: timelines)
    }

    private func loadTask(_ coordinator: SyncCoordinator, id: String = TaskFixtures.retryTaskID) async {
        await coordinator.bootstrap()
        await coordinator.refreshTaskTimeline(for: id)
    }

    @Test func publishedTaskLoadsReviewAndAuthenticatedOutputWithoutOpeningRun() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        let run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.completedRunID })
        await loadTask(coordinator, id: run.run.task_id)
        let fullRevision = coordinator.cursors?.lastFullSnapshotRevision
        await coordinator.refreshTaskReviews(for: run.run.task_id, revision: 12)
        let round = try #require(coordinator.timelinesByRunID[run.run.id]?.review?.value1.rounds.first)
        #expect(round.outcome?.value1 == .clean)
        let evidence = try await coordinator.reviewEvidence(for: run.run.id, round: round)
        #expect(evidence.run_id == run.run.id)
        #expect(evidence.invocation_id == round.invocation_id)
        #expect(evidence.source_head_sha == round.head_sha)
        #expect(!evidence.publish_eligible)
        #expect(coordinator.cursors?.lastFullSnapshotRevision == fullRevision)
        var wrong = round
        wrong.head_sha = String(repeating: "d", count: 40)
        var wrongInvocation = round
        wrongInvocation.invocation_id = "another-review"
        var wrongSource = round
        wrongSource.source = .init(kind: "github")
        for selected in [wrong, wrongInvocation, wrongSource] {
            await #expect(throws: SyncCoordinator.ReviewEvidenceReadError.self) {
                try await coordinator.reviewEvidence(for: run.run.id, round: selected)
            }
        }
    }

    @Test func twoRunsKeepDistinctReviewsAndUnchangedReadsAreDeduplicated() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        let calls = Counter()
        await loadTask(coordinator)
        await server.setBeforeRespond { operation in
            if operation == "getRunTimeline" { await calls.increment() }
        }
        let key = try #require(coordinator.taskReviewRequestKey(for: TaskFixtures.retryTaskID, revision: 12))
        #expect(Set(key.runIDs) == [RunFixtures.activeRunID, "run-freeside-656"])
        await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        #expect(await calls.count == 2)
        #expect(coordinator.timelinesByRunID.count == 2)
        let newest = try #require(coordinator.timelinesByRunID[RunFixtures.activeRunID]?.review?.value1.rounds.first)
        let older = try #require(coordinator.timelinesByRunID["run-freeside-656"]?.review?.value1.rounds.first)
        #expect(newest.head_sha != older.head_sha)
        #expect(newest.outcome?.value1 == .clean)
        #expect(older.outcome?.value1 == .findings)
        #expect(
            try await coordinator.reviewEvidence(for: "run-freeside-656", round: older).invocation_id
                == older.invocation_id)
        await #expect(throws: SyncCoordinator.ReviewEvidenceReadError.self) {
            try await coordinator.reviewEvidence(for: RunFixtures.activeRunID, round: older)
        }
        await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 13)
        #expect(await calls.count == 4)
        await coordinator.bootstrap()
        await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 13)
        #expect(await calls.count == 6)
    }

    @Test func legacyTaskLoadsRecordedReviewAndAuthenticatedOutputWithoutInventingRole() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await loadTask(coordinator, id: TaskFixtures.legacyTaskID)
        let history = try #require(coordinator.taskTimelinesByTaskID[TaskFixtures.legacyTaskID])
        let run = try #require(history.sections.flatMap(\.runs).first)
        #expect(run.role == nil)
        await coordinator.refreshTaskReviews(for: history.task_id, revision: 12)
        let round = try #require(coordinator.timelinesByRunID[run.run_id]?.review?.value1.rounds.first)
        #expect(round.outcome?.value1 == .clean)
        let evidence = try await coordinator.reviewEvidence(for: run.run_id, round: round)
        #expect(evidence.run_id == run.run_id)
        #expect(evidence.invocation_id == round.invocation_id)
        #expect(evidence.source_head_sha == round.head_sha)
        #expect(!evidence.publish_eligible)
        #expect(coordinator.taskTimelinesByTaskID[history.task_id]?.sections.flatMap(\.runs).first?.role == nil)
    }

    @Test func specificationTaskDoesNotReadReviewTimelines() async throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == "run-freeside-specification" })
        run.run.campaign_id = "campaign-specification"
        run.run.attempt_number = 1
        var task = try #require(TaskFixtures.defaultTasks().first { $0.task.id == run.run.task_id })
        task.task.campaign_ids = ["campaign-specification"]
        let server = MockServer(runs: [run], tasks: [task])
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await loadTask(coordinator, id: run.run.task_id)
        let history = try #require(coordinator.taskTimelinesByTaskID[run.run.task_id])
        let member = try #require(history.sections.flatMap(\.runs).first { $0.run_id == run.run.id })
        #expect(member.role?.value1 == .specification)
        let calls = Counter()
        await server.setBeforeRespond { operation in
            if operation == "getRunTimeline" { await calls.increment() }
        }
        await coordinator.refreshTaskReviews(for: run.run.task_id, revision: 12)
        #expect(await calls.count == 0)
        #expect(coordinator.timelinesByRunID.isEmpty)
    }

    @Test func repeatedMembershipReadsEachRunOnceAndMissingHistoryReadsNothing() async throws {
        let server = server()
        let cache = InMemoryCacheStore()
        let client = APIClientFactory.mock(server: server)
        let coordinator = SyncCoordinator(client: client, cache: cache)
        await loadTask(coordinator)
        var saved = try #require(cache.load())
        saved.taskTimelines[0].sections.append(saved.taskTimelines[0].sections[0])
        try cache.save(saved)
        let relaunched = SyncCoordinator(client: client, cache: cache)
        let calls = Counter()
        await server.setBeforeRespond { operation in
            if operation == "getRunTimeline" { await calls.increment() }
        }
        await relaunched.refreshTaskReviews(for: "not-fetched", revision: 12)
        #expect(await calls.count == 0)
        await relaunched.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        #expect(await calls.count == 2)
    }

    @Test(arguments: [false, true])
    func failedReadsPreserveCachedFactsAndRemainRetryable(preload: Bool) async throws {
        let server = server()
        let cache = InMemoryCacheStore()
        let client = APIClientFactory.mock(server: server)
        let coordinator = SyncCoordinator(client: client, cache: cache)
        await loadTask(coordinator)
        if preload { await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12) }
        let relaunched = SyncCoordinator(client: client, cache: cache)
        #expect(relaunched.timelinesByRunID.count == (preload ? 2 : 0))
        await server.setBeforeRespond { _ in throw InjectedFailure() }
        await relaunched.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        #expect(relaunched.timelinesByRunID.count == (preload ? 2 : 0))
        #expect(relaunched.timelineLoadStates[RunFixtures.activeRunID] == .unavailable)
        #expect(
            RunReviewSection.availabilityMessage(
                hasTimeline: preload, state: .unavailable, freshness: relaunched.store.freshness) != nil)
        await server.setBeforeRespond(nil)
        await relaunched.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        #expect(relaunched.timelineLoadStates[RunFixtures.activeRunID] == .loaded)
        #expect(relaunched.timelinesByRunID.count == 2)
        await server.rotateEpoch()
        await relaunched.heartbeat()
        #expect(relaunched.timelinesByRunID.isEmpty)
        #expect(relaunched.taskTimelinesByTaskID.isEmpty)
    }

    @Test func overlappingRequestsCoalesceAndCancelledNavigationDoesNotAdopt() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await loadTask(coordinator)
        let calls = Counter()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operation in
            if operation == "getRunTimeline" {
                await calls.increment()
                await reached.open()
                await release.wait()
            }
        }
        let first = Task { await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12) }
        await reached.wait()
        // Hold the shared read while both views leave, including a transport
        // that finishes its response despite cancellation.
        let second = Task { await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12) }
        first.cancel()
        second.cancel()
        await release.open()
        await first.value
        await second.value
        #expect(await calls.count <= 2)
        #expect(coordinator.timelinesByRunID.isEmpty)
        await server.setBeforeRespond(nil)
        await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        #expect(coordinator.timelinesByRunID.count == 2)
    }

    @Test(arguments: [false, true])
    func bootstrapOrEpochChangeDoesNotLetAnOldRequestSuppressFreshReads(rotate: Bool) async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await loadTask(coordinator)
        let calls = Counter()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operation in
            if operation == "getRunTimeline", await calls.incrementAndGet() == 1 {
                await reached.open()
                await release.wait()
            }
        }
        let old = Task { await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12) }
        await reached.wait()
        if rotate { await server.rotateEpoch() }
        await coordinator.bootstrap()
        await coordinator.refreshTaskTimeline(for: TaskFixtures.retryTaskID)
        await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        let current = coordinator.timelinesByRunID
        #expect(current.count == 2)
        await release.open()
        await old.value
        #expect(coordinator.timelinesByRunID == current)
        #expect(await calls.count == 3)
    }

    @Test func reopeningWhileACancelledReadFinishesRetriesWithoutAnotherUserAction() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await loadTask(coordinator)
        let calls = Counter()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operation in
            if operation == "getRunTimeline", await calls.incrementAndGet() == 1 {
                await reached.open()
                await release.wait()
            }
        }
        let old = Task { await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12) }
        await reached.wait()
        old.cancel()
        let entered = AsyncGate()
        let replacement = Task {
            await entered.open()
            await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        }
        await entered.wait()
        await Task.yield()
        await release.open()
        await old.value
        await replacement.value
        #expect(coordinator.timelinesByRunID.count == 2)
        #expect(await calls.count == 3)
    }

    @Test func simultaneousUncancelledCallersShareTheWholeReadSequence() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await loadTask(coordinator)
        let calls = Counter()
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operation in
            if operation == "getRunTimeline" {
                await calls.increment()
                await reached.open()
                await release.wait()
            }
        }
        let first = Task { await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12) }
        await reached.wait()
        let second = Task { await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12) }
        let third = Task { await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12) }
        await Task.yield()
        await release.open()
        await first.value
        await second.value
        await third.value
        #expect(await calls.count == 2)
        #expect(coordinator.timelinesByRunID.count == 2)
    }

    @Test func missingRunDetailsCannotBecomeAnEmptyReview() async {
        let server = server()
        let coordinator = SyncCoordinator(
            client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await loadTask(coordinator)
        await server.setBeforeRespond { operation in
            if operation == "getRunTimeline" { throw MockServer.ForcedStatus(404) }
        }
        await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        #expect(coordinator.timelinesByRunID.isEmpty)
        #expect(coordinator.timelineLoadStates[RunFixtures.activeRunID] == .unavailable)
    }

    @Test func evidenceFromAnEarlierEpochFailsClosed() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        await loadTask(coordinator)
        await coordinator.refreshTaskReviews(for: TaskFixtures.retryTaskID, revision: 12)
        let round = try #require(coordinator.timelinesByRunID[RunFixtures.activeRunID]?.review?.value1.rounds.first)
        let reached = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operation in
            if operation == "getReviewEvidence" {
                await reached.open()
                await release.wait()
            }
        }
        let read = Task { try await coordinator.reviewEvidence(for: RunFixtures.activeRunID, round: round) }
        await reached.wait()
        await server.rotateEpoch()
        await coordinator.bootstrap()
        await release.open()
        await #expect(throws: CancellationError.self) { try await read.value }
    }
}

@Suite struct TaskTimelineViewTests {
    /// Instants deliberately oldest first: the daemon orders its lists, and
    /// the client must render them as received, never by timestamp.
    private static let instants = (0..<4).map {
        Date(timeIntervalSince1970: 1_700_000_000 + TimeInterval($0 * 60))
    }

    private static func milestone(
        _ kind: Components.Schemas.RunMilestoneKind, at instant: Date
    ) -> Components.Schemas.RunMilestone {
        .init(run_id: "run-1", kind: kind, recorded_at: instant)
    }

    private static func timeline() -> Components.Schemas.TaskTimeline {
        let t = instants
        return .init(
            as_of_revision: 1, as_of: t[3], task_id: "task-1", project_id: "freeside",
            name: .init(text: "Named", source: ._operator),
            events: [
                .init(kind: .task_created, recorded_at: t[0]),
                .init(kind: .specification_approved, recorded_at: t[1]),
            ],
            sections: [
                .init(
                    campaign_id: "campaign-a",
                    events: [.init(kind: .campaign_allocated, recorded_at: t[0])],
                    runs: [
                        .init(
                            run_id: "run-1", attempt_number: 1,
                            milestones: [
                                milestone(.run_submitted, at: t[0]),
                                milestone(.invocation_started, at: t[1]),
                            ],
                            events: [.init(kind: .pr_opened, recorded_at: t[2], pr_number: 5)]),
                        .init(run_id: "run-2", attempt_number: 2, milestones: [], events: []),
                    ]),
                .init(campaign_id: nil, events: [], runs: [.init(run_id: "run-0", milestones: [], events: [])]),
            ])
    }

    @Test func entriesKeepTheDaemonsOrderWithoutSorting() {
        let timeline = Self.timeline()

        #expect(
            TaskTimelinePresentation.entries(timeline) == [
                .section(campaignID: "campaign-a"),
                .event(.campaign_allocated),
                .run("run-1"),
                .milestone(runID: "run-1", kind: .run_submitted),
                .milestone(runID: "run-1", kind: .invocation_started),
                .event(.pr_opened),
                .run("run-2"),
                .section(campaignID: nil),
                .run("run-0"),
                .event(.task_created),
                .event(.specification_approved),
            ])

        // Reversing every list the daemon sent reverses the render order of
        // that list, so no timestamp sort is hiding behind the fixture order.
        var reversed = timeline
        reversed.events.reverse()
        reversed.sections.reverse()
        reversed.sections[1].runs.reverse()
        reversed.sections[1].runs[1].milestones.reverse()
        #expect(
            TaskTimelinePresentation.entries(reversed) == [
                .section(campaignID: nil),
                .run("run-0"),
                .section(campaignID: "campaign-a"),
                .event(.campaign_allocated),
                .run("run-2"),
                .run("run-1"),
                .milestone(runID: "run-1", kind: .invocation_started),
                .milestone(runID: "run-1", kind: .run_submitted),
                .event(.pr_opened),
                .event(.specification_approved),
                .event(.task_created),
            ])
    }

    @Test func milestoneEntriesLeadWithTheFirstReceivedAsCurrent() {
        let run = Self.timeline().sections[0].runs[0]

        let entries = TaskTimelinePresentation.milestoneEntries(run)

        #expect(entries.map(\.title) == ["Run Submitted", "Invocation Started"])
        #expect(entries.map(\.state) == [.current, .completed])
        #expect(TaskTimelinePresentation.milestoneEntries(Self.timeline().sections[0].runs[1]).isEmpty)
    }

    @Test func retryTaskTimelineListsBothAttemptsNewestFirst() throws {
        let timeline = try #require(
            TaskFixtures.defaultTimelines().first { $0.task_id == TaskFixtures.retryTaskID })

        let runs = timeline.sections.flatMap(\.runs)

        #expect(timeline.name == RunFixtures.retryTaskName)
        #expect(timeline.sections.map(\.campaign_id) == [RunFixtures.retryCampaignID])
        #expect(runs.map(\.run_id) == [RunFixtures.activeRunID, "run-freeside-656"])
        #expect(runs.map(TaskTimelinePresentation.runTitle) == ["Attempt 2", "Attempt 1"])
        #expect(runs.map { $0.role?.value1 } == [.implementation, .implementation])
        #expect(runs[0].hold?.value1.reason == .verification_findings)
        #expect(runs[1].superseded_by == RunFixtures.activeRunID)
    }

    @Test func labelsReadAsProse() {
        #expect(TaskTimelinePresentation.label(Components.Schemas.TaskEventKind.pr_merged) == "PR merged")
        #expect(TaskTimelinePresentation.label(Components.Schemas.TaskEventKind.task_created) == "Task created")
        #expect(TaskTimelinePresentation.label(Components.Schemas.TaskRunRole.specification) == "Specification")
        #expect(
            TaskTimelinePresentation.detail(
                .init(kind: .pr_merged, recorded_at: Self.instants[0], pr_number: 105, merge_commit_sha: "abc"))
                == "PR #105")
        #expect(
            TaskTimelinePresentation.detail(
                .init(
                    kind: .specification_approved, recorded_at: Self.instants[0],
                    approved_spec_digest: .init(value1: "sha256:abc"), specification_run_id: "run-spec"))
                == "sha256:abc")
        #expect(TaskTimelinePresentation.detail(.init(kind: .task_created, recorded_at: Self.instants[0])) == nil)
        #expect(
            TaskTimelinePresentation.runTitle(.init(run_id: "run-legacy", milestones: [], events: []))
                == "run-legacy")
        #expect(
            TaskTimelinePresentation.sectionTitle(.init(campaign_id: nil, events: [], runs: []))
                == "Outside a campaign")
        #expect(TaskTimelinePresentation.sectionTitle(.init(campaign_id: "c", events: [], runs: [])) == "Campaign")
    }

    @Test func headerNamePrefersTheFetchedTimelineOverTheSnapshot() throws {
        let snapshot = try #require(
            TaskFixtures.defaultTasks().first { $0.task.id == TaskFixtures.retryTaskID })
        var timeline = try #require(
            TaskFixtures.defaultTimelines().first { $0.task_id == TaskFixtures.retryTaskID })
        timeline.name = .init(text: "Renamed after the bootstrap", source: ._operator)

        #expect(TaskTimelinePresentation.headerName(timeline: timeline, snapshot: snapshot) == timeline.name)
        #expect(
            TaskTimelinePresentation.headerName(timeline: nil, snapshot: snapshot)
                == snapshot.task.display_names.task)
    }

    @Test func requestKeyChangesOnBootstrapAndEpochRotation() throws {
        let snapshot = try #require(
            TaskFixtures.defaultTasks().first { $0.task.id == TaskFixtures.retryTaskID })
        let cursors = SyncCursors(
            syncEpoch: "epoch-1", lastFullSnapshotRevision: 10, highestObservedServerRevision: 10)
        let key = TaskTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: cursors)

        #expect(key == TaskTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: cursors))
        let afterBootstrap = SyncCursors(
            syncEpoch: "epoch-1", lastFullSnapshotRevision: 11, highestObservedServerRevision: 11)
        #expect(key != TaskTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: afterBootstrap))
        let afterEpoch = SyncCursors(
            syncEpoch: "epoch-2", lastFullSnapshotRevision: 10, highestObservedServerRevision: 10)
        #expect(key != TaskTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: afterEpoch))
        var revised = snapshot
        revised.as_of_revision += 1
        #expect(key != TaskTimelineView.TimelineRequestKey(snapshot: revised, cursors: cursors))
    }
}
