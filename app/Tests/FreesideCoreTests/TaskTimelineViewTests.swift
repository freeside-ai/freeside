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

    /// While a daemon keeps writing, the detail's own reads run ahead of the
    /// last full snapshot. That is not stale data, so a loaded section says
    /// nothing. A daemon that stops answering is, and every section says so
    /// beside the freshness banner.
    @Test func loadedSectionsStayQuietUntilSyncFails() async throws {
        let server = server()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: InMemoryCacheStore())
        let taskID = TaskFixtures.retryTaskID
        let runID = RunFixtures.activeRunID
        await coordinator.refresh()
        await coordinator.refreshTaskTimeline(for: taskID)
        await coordinator.refreshTaskReviews(for: taskID, revision: 12)
        func history() -> String? {
            TaskTimelinePresentation.availabilityMessage(
                state: coordinator.taskTimelineLoadStates[taskID], freshness: coordinator.store.freshness)
        }
        func review() -> String? {
            RunReviewSection.availabilityMessage(
                hasTimeline: coordinator.timelinesByRunID[runID] != nil,
                state: coordinator.timelineLoadStates[runID], freshness: coordinator.store.freshness)
        }
        func banner() -> Bool {
            FreshnessBanner.isShowing(
                freshness: coordinator.store.freshness, lastUpdatedAt: coordinator.lastUpdatedAt, at: .now)
        }

        // The daemon writes, and the review read sees the newer revision.
        await server.advanceRun(id: runID)
        await coordinator.refreshTimeline(for: runID)
        #expect(coordinator.store.freshness == .unvalidated)
        #expect(history() == nil)
        #expect(review() == nil)
        #expect(!RunReviewSection.showsRetry(state: coordinator.timelineLoadStates[runID]))
        #expect(coordinator.taskStop.unavailableReason == nil)
        #expect(coordinator.taskStop.prepare(taskID: taskID) != nil)
        #expect(!banner())

        await server.setBeforeRespond { _ in throw InjectedFailure() }
        await coordinator.refresh()
        #expect(coordinator.store.freshness == .unreachable)
        #expect(history() == "Saved task history. Freshness unconfirmed.")
        #expect(review() == "Saved review details. Freshness unconfirmed.")
        #expect(coordinator.taskStop.unavailableReason == "Offline. Connect to the daemon before sending Stop.")
        #expect(banner())
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
    @Test(arguments: TaskHistoryFixtures.Scenario.allCases)
    func completeHistoryRetainsEveryPartition(scenario: TaskHistoryFixtures.Scenario) throws {
        let history = TaskHistoryFixtures.history(scenario)
        let entries = TaskTimelinePresentation.entries(history)
        #expect(history.events.last?.kind == .task_created)
        // The event rail leads the page and ends at the task's creation;
        // every campaign follows it.
        let rows = TaskTimelinePresentation.rows(history)
        #expect(Array(entries.prefix(rows.count)) == rows.map { .row($0.label) })
        #expect(rows.last?.label == "Task created")
        #expect(entries.dropFirst(rows.count).allSatisfy { if case .row = $0 { false } else { true } })
        let runs = history.sections.flatMap(\.runs)
        #expect(entries.compactMap { if case .run(let id) = $0 { id } else { nil } } == runs.map(\.run_id))
        #expect(TaskHistoryFixtures.snapshot(history).task.run_ids.count == runs.count)
        for section in history.sections {
            for event in section.events where event.kind == .specification_approved {
                let specification = try #require(section.runs.first { $0.run_id == event.specification_run_id })
                let implementation = try #require(section.runs.first { $0.run_id == event.run_id })
                #expect(specification.role?.value1 == .specification)
                #expect(implementation.role?.value1 == .implementation)
                #expect(event.campaign_id == section.campaign_id)
                #expect(event.approved_spec_digest != nil)
                #expect(specification.run_id != implementation.run_id)
            }
        }
        switch scenario {
        case .creation:
            #expect(history.sections.isEmpty)
            #expect(entries == [.row("Task created")])
        case .approved:
            #expect(runs.count == 2)
            #expect(!entries.contains(.row("PR opened · #110")))
        case .published:
            #expect(entries.contains(.row("PR opened · #110")))
            #expect(!entries.contains(.row("PR merged · #110")))
        case .retry:
            #expect(runs[1].superseded_by == runs[0].run_id)
            #expect(runs[0].parent_run_id == runs[1].run_id)
            #expect(history.sections[0].events.filter { $0.kind == .specification_approved }.count == 1)
        case .revised:
            #expect(history.sections[0].campaign_id != history.sections[1].campaign_id)
            #expect(history.sections[0].events.allSatisfy { $0.kind != .specification_approved })
            #expect(history.sections[1].events.contains { $0.kind == .specification_approved })
        case .legacy:
            #expect(history.sections[0].campaign_id == nil)
            #expect(runs.allSatisfy { $0.role == nil })
        }
    }

    @Test func savedHistoryNeverReadsAsAnEmptyOrCurrentResult() {
        #expect(TaskTimelinePresentation.availabilityMessage(state: .loaded, freshness: .fresh) == nil)
        #expect(
            TaskTimelinePresentation.availabilityMessage(state: .loading, freshness: .fresh)?.contains("refreshing")
                == true)
        #expect(
            TaskTimelinePresentation.availabilityMessage(state: .unavailable, freshness: .fresh)?.contains("failed")
                == true)
        for state: SyncCoordinator.TimelineLoadState? in [nil, .idle, .loaded] {
            #expect(
                TaskTimelinePresentation.availabilityMessage(state: state, freshness: .unreachable)?.contains("Saved")
                    == true)
        }
        #expect(TaskTimelinePresentation.availabilityMessage(state: nil, freshness: .fresh) != nil)
        // A read that ran ahead of the snapshot leaves a loaded history
        // current, and vouches for nothing that has not loaded.
        #expect(TaskTimelinePresentation.availabilityMessage(state: .loaded, freshness: .unvalidated) == nil)
        #expect(TaskTimelinePresentation.availabilityMessage(state: nil, freshness: .unvalidated) != nil)
        let failing: [InboxStore.Freshness] = [
            .unreachable, .syncFailing, .contractMismatch(daemonContract: "other"), .unauthenticated,
        ]
        for freshness in failing {
            #expect(TaskTimelinePresentation.availabilityMessage(state: .loaded, freshness: freshness) != nil)
        }
    }

    @Test func milestoneDatesUseTheSameTimeZoneAsReviewAndEvents() throws {
        let run = Self.timeline().sections[0].runs[0]
        let locale = Locale(identifier: "en_US")
        let utc = try #require(TimeZone(secondsFromGMT: 0))
        let west = try #require(TimeZone(secondsFromGMT: -5 * 3_600))
        let utcEntries = TaskTimelinePresentation.milestoneEntries(run, locale: locale, timeZone: utc)
        let westEntries = TaskTimelinePresentation.milestoneEntries(run, locale: locale, timeZone: west)
        #expect(utcEntries.map(\.timestamp) != westEntries.map(\.timestamp))
        // The short-time grammar: the date and the clock joined by a comma,
        // with the year because the milestone isn't from this one.
        #expect(utcEntries[0].timestamp?.replacing("\u{202F}", with: " ") == "Nov 14, 2023, 10:13 PM")
    }

    @Test(arguments: [false, true]) @MainActor
    func savedCompleteHistorySurvivesFailureButNotAnEpochReset(legacy: Bool) async throws {
        var history = TaskHistoryFixtures.history(.published)
        if legacy { history.events = history.events.filter { $0.kind == .task_created } }
        let expectedEvents = TaskTimelinePresentation.events(history)
        #expect(expectedEvents.contains { $0.kind == .pr_opened })
        let cache = InMemoryCacheStore()
        try cache.save(
            .init(
                cursors: .init(
                    syncEpoch: "history-epoch", lastFullSnapshotRevision: 12, highestObservedServerRevision: 12),
                attentionItems: [], tasks: [TaskHistoryFixtures.snapshot(history)], taskTimelines: [history]))
        let server = MockServer()
        let coordinator = SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache)
        #expect(coordinator.taskTimelinesByTaskID[history.task_id] == history)
        let entered = AsyncGate()
        let release = AsyncGate()
        await server.setBeforeRespond { operation in
            if operation == "getTaskTimeline" {
                await entered.open()
                await release.wait()
                throw MockServer.ForcedStatus(500)
            }
        }
        let read = Task { await coordinator.refreshTaskTimeline(for: history.task_id) }
        await entered.wait()
        #expect(coordinator.taskTimelineLoadStates[history.task_id] == .loading)
        #expect(coordinator.taskTimelinesByTaskID[history.task_id] == history)
        await release.open()
        await read.value
        #expect(coordinator.taskTimelineLoadStates[history.task_id] == .unavailable)
        #expect(coordinator.taskTimelinesByTaskID[history.task_id] == history)
        #expect(coordinator.cursors?.lastFullSnapshotRevision == 12)
        #expect(cache.load()?.taskTimelines == [history])
        let saved = try #require(coordinator.taskTimelinesByTaskID[history.task_id])
        #expect(TaskTimelinePresentation.events(saved) == expectedEvents)
        await server.setBeforeRespond(nil)
        await server.rotateEpoch()
        await coordinator.bootstrap()
        #expect(coordinator.taskTimelinesByTaskID[history.task_id] == nil)
        #expect(coordinator.taskTimelineLoadStates[history.task_id] == nil)
    }

    @Test func legacyEventsKeepRecordedFactsWithoutDuplicates() {
        var history = TaskHistoryFixtures.history(.published)
        let currentEvents = history.events
        #expect(TaskTimelinePresentation.events(history) == currentEvents)
        history.events = currentEvents.filter { $0.kind == .task_created }
        history.sections += history.sections
        let events = TaskTimelinePresentation.events(history)
        #expect(events.contains { $0.kind == .campaign_allocated })
        #expect(events.contains { $0.kind == .pr_opened })
        #expect(events.filter { $0.kind == .pr_opened }.count == 1)
        #expect(!events.contains { $0.kind == .verification_recorded })
        #expect(events.map(\.recorded_at) == events.map(\.recorded_at).sorted(by: >))
        #expect(TaskTimelinePresentation.events(history) == events)
    }

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

    private static func emptyTimeline() -> Components.Schemas.TaskTimeline {
        .init(
            as_of_revision: 1, as_of: instants[0], task_id: "task-1", project_id: "freeside",
            name: .init(text: "Named", source: ._operator), events: [], sections: [])
    }

    @Test func entriesLeadWithEventsAndKeepTheDaemonsOrder() {
        let timeline = Self.timeline()

        #expect(
            TaskTimelinePresentation.entries(timeline) == [
                .row("Task created"),
                .row("Specification approved"),
                .section(campaignID: "campaign-a"),
                .run("run-1"),
                .milestone(runID: "run-1", kind: .run_submitted),
                .milestone(runID: "run-1", kind: .invocation_started),
                .run("run-2"),
                .section(campaignID: nil),
                .run("run-0"),
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
                .row("Specification approved"),
                .row("Task created"),
                .section(campaignID: nil),
                .run("run-0"),
                .section(campaignID: "campaign-a"),
                .run("run-2"),
                .run("run-1"),
                .milestone(runID: "run-1", kind: .invocation_started),
                .milestone(runID: "run-1", kind: .run_submitted),
            ])
    }

    @Test func milestoneEntriesLeadWithTheFirstReceivedAsCurrent() {
        let run = Self.timeline().sections[0].runs[0]

        let entries = TaskTimelinePresentation.milestoneEntries(run)

        #expect(entries.map(\.title) == ["Run Submitted", "Invocation Started"])
        #expect(entries.map(\.state) == [.current, .completed])
        #expect(TaskTimelinePresentation.milestoneEntries(Self.timeline().sections[0].runs[1]).isEmpty)
    }

    @Test(arguments: [Components.Schemas.ReviewProgressState.running, .completed])
    func milestoneEntriesNameEachInvocationsRole(state: Components.Schemas.ReviewProgressState) {
        var run = Self.timeline().sections[0].runs[0]
        run.milestones = RemediationFixtures.milestones(remediatorStarted: true).reversed()
        let rounds = [RemediationFixtures.findingsRound(), RemediationFixtures.reReview(state)]

        let entries = TaskTimelinePresentation.milestoneEntries(
            run, reviewRounds: rounds, stages: RemediationFixtures.stages)

        // The implementer reads as the attempt the review round's title names.
        let implementer = "Implementation · Pass 1 · Round 2"
        #expect(
            entries.map(\.context) == [
                "Remediation for Review 1", "Remediation for Review 1", nil, implementer, implementer, implementer,
            ])
        // Before the run's snapshot syncs, the implementer is named by kind.
        #expect(
            TaskTimelinePresentation.milestoneEntries(run, reviewRounds: rounds).map(\.context).last
                == "Implementation")
        #expect(entries[2].title == "Findings Adjudicated")
        #expect(entries[2].detail == "Remediate 1 finding from Review 1")
        #expect(entries.map(\.state) == [.current] + Array(repeating: .completed, count: 5))
        // Without the run timeline the rail reads as before.
        #expect(TaskTimelinePresentation.milestoneEntries(run).map(\.context).allSatisfy { $0 == nil })
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
            TaskTimelinePresentation.runTitle(.init(run_id: "run-legacy", milestones: [], events: []))
                == "Run run-legacy")
        #expect(
            TaskTimelinePresentation.sectionTitle(.init(campaign_id: nil, events: [], runs: []))
                == "Outside a Campaign")
        #expect(TaskTimelinePresentation.sectionTitle(.init(campaign_id: "c", events: [], runs: [])) == "Campaign")
    }

    @Test func historicalResultsReadInWordsAndKeepTheirCommitsInTechnicalDetails() {
        let failed = Components.Schemas.TaskEvent(
            review: .init(
                value1: .init(
                    invocation_id: "review-old", round: 2, head_sha: "old-head", base_sha: "old-base",
                    failure: "provider_error")),
            kind: .review_failed, recorded_at: Self.instants[1], run_id: "run-old")
        let verification = Components.Schemas.TaskEvent(
            verification: .init(
                value1: .init(item_id: "ready-old", _class: .ready_degraded, head_sha: "old-head", base_sha: "old-base")
            ),
            kind: .verification_recorded, recorded_at: Self.instants[0], run_id: "run-old")
        var timeline = Self.emptyTimeline()
        timeline.events = [failed, verification]
        timeline.sections = [
            .init(campaign_id: "campaign-old", events: [], runs: [.init(run_id: "run-old", milestones: [], events: [])])
        ]
        // The rail names a result in words: no run or campaign id, no commit.
        let rows = TaskTimelinePresentation.rows(timeline)
        #expect(rows.map(\.label) == ["Round 2 failed", "Verification recorded · Degraded"])
        #expect(rows.map(\.kind) == [.failure, .event])
        for value in ["run-old", "campaign-old", "old-head", "old-base", "ready-old", "review-old"] {
            #expect(!rows.contains { $0.label.contains(value) })
        }
        // What left the line is in the run's technical details; the review
        // round's head and base are in the run's Review section.
        let technical = TaskTimelinePresentation.technicalRows(run: timeline.sections[0].runs[0], in: timeline)
        #expect(
            technical.map(\.label) == ["Run ID", "Verification Inbox Item ID", "Verified Head", "Verified Base"])
        #expect(technical.map(\.value) == ["run-old", "ready-old", "old-head", "old-base"])
        #expect(TaskTimelinePresentation.label(Components.Schemas.TaskEventKind.stop_failed) == "Stop failed")
        #expect(TaskTimelinePresentation.label(Components.Schemas.TaskEventKind.task_stopped) == "Task stopped")
    }

    @Test func runReferenceNamesAttemptWithinTheTimelineElseShortId() {
        let timeline = Self.timeline()
        // Self.timeline()'s runs carry no role, so the reference is the bare
        // attempt or a short id.
        #expect(TaskTimelinePresentation.runReference("run-2", in: timeline) == "attempt 2")
        #expect(TaskTimelinePresentation.runReference("run-0", in: timeline) == "run-0")
        let long = "run-\(String(repeating: "a", count: 64))"
        #expect(TaskTimelinePresentation.runReference(long, in: timeline) == "run-aaaaaaaa…")
        // A fact links only a run this history holds, named in sentence
        // case; a run it doesn't hold has no title to link.
        #expect(TaskTimelinePresentation.runLinkTitle("run-2", in: timeline) == "Attempt 2")
        #expect(TaskTimelinePresentation.runLinkTitle("run-0", in: timeline) == "run-0")
        #expect(TaskTimelinePresentation.runLinkTitle("run-absent", in: timeline) == nil)
    }

    @Test func runReferenceRoleQualifiesAttemptOneWithinACampaign() {
        // A campaign's specification run and its first implementation run both
        // carry attempt 1; the reference names the role so they stay distinct.
        let timeline = Components.Schemas.TaskTimeline(
            as_of_revision: 1, as_of: Self.instants[0], task_id: "t", project_id: "p",
            name: .init(text: "n", source: ._operator), events: [],
            sections: [
                .init(
                    campaign_id: "campaign-a", events: [],
                    runs: [
                        .init(
                            run_id: "run-spec", role: .init(value1: .specification), attempt_number: 1,
                            superseded_by: "run-impl", milestones: [], events: []),
                        .init(
                            run_id: "run-impl", role: .init(value1: .implementation), attempt_number: 1,
                            milestones: [], events: []),
                    ])
            ])
        #expect(TaskTimelinePresentation.runReference("run-impl", in: timeline) == "implementation attempt 1")
        #expect(TaskTimelinePresentation.runReference("run-spec", in: timeline) == "specification run")
    }

    @Test func runCardAccessibilityLabelNamesRoleWithoutTheId() {
        let impl = Components.Schemas.TaskTimelineRun(
            run_id: "run-\(String(repeating: "a", count: 64))", role: .init(value1: .implementation),
            attempt_number: 2, milestones: [], events: [])
        let legacy = Components.Schemas.TaskTimelineRun(run_id: "run-legacy", milestones: [], events: [])
        let single = Components.Schemas.TaskTimeline(
            as_of_revision: 1, as_of: Self.instants[0], task_id: "t", project_id: "p",
            name: .init(text: "n", source: ._operator), events: [],
            sections: [.init(campaign_id: "campaign-a", events: [], runs: [impl, legacy])])
        #expect(
            TaskTimelinePresentation.runCardAccessibilityLabel(impl, in: single)
                == "Open detailed run history for Attempt 2, Implementation run")
        #expect(
            TaskTimelinePresentation.runCardAccessibilityLabel(legacy, in: single)
                == "Open detailed run history for Run run-legacy")
    }

    @Test func runCardAccessibilityLabelNamesTheCampaignWhenAttemptsRestart() {
        // Two campaigns, each with an Attempt 1 specification run: the title
        // and role are identical, so the control names its campaign.
        func specRun() -> Components.Schemas.TaskTimelineRun {
            .init(
                run_id: "run-spec", role: .init(value1: .specification), attempt_number: 1, milestones: [], events: [])
        }
        let a = specRun()
        var b = specRun()
        b.run_id = "run-spec-2"
        let timeline = Components.Schemas.TaskTimeline(
            as_of_revision: 1, as_of: Self.instants[0], task_id: "t", project_id: "p",
            name: .init(text: "n", source: ._operator), events: [],
            sections: [
                .init(campaign_id: "campaign-a", events: [], runs: [a]),
                .init(campaign_id: "campaign-b", events: [], runs: [b]),
            ])
        #expect(
            TaskTimelinePresentation.runCardAccessibilityLabel(a, in: timeline)
                == "Open detailed run history for Attempt 1, Specification run, campaign campaign-a")
        #expect(
            TaskTimelinePresentation.runCardAccessibilityLabel(b, in: timeline)
                == "Open detailed run history for Attempt 1, Specification run, campaign campaign-b")
    }

    @Test func runTitleShortensAnOpaqueIdInTheFallback() {
        let long = "run-\(String(repeating: "b", count: 64))"
        #expect(
            TaskTimelinePresentation.runTitle(.init(run_id: long, milestones: [], events: []))
                == "Run run-bbbbbbbb…")
        #expect(
            TaskTimelinePresentation.runTitle(.init(run_id: long, attempt_number: 3, milestones: [], events: []))
                == "Attempt 3")
    }

    @Test func technicalRowsCarryTheExactSourceValues() {
        let t = Self.instants
        let campaignID = "campaign-\(String(repeating: "c", count: 64))"
        let digest = "sha256:\(String(repeating: "d", count: 64))"
        let runID = "run-\(String(repeating: "e", count: 64))"
        let parentID = "run-\(String(repeating: "f", count: 64))"
        let successorID = "run-\(String(repeating: "0", count: 64))"

        let section = Components.Schemas.TaskTimelineSection(
            campaign_id: campaignID,
            events: [
                .init(
                    kind: .specification_approved, recorded_at: t[0],
                    approved_spec_digest: .init(value1: digest))
            ],
            runs: [])
        let sectionRows = TaskTimelinePresentation.technicalRows(section: section)
        #expect(sectionRows.map(\.label) == ["Campaign ID", "Approved Specification Digest"])
        #expect(sectionRows.map(\.value) == [campaignID, digest])

        let timeline = Components.Schemas.TaskTimeline(
            as_of_revision: 1, as_of: t[0], task_id: "task-x", project_id: "p",
            name: .init(text: "n", source: ._operator),
            events: [
                .init(
                    verification: .init(
                        value1: .init(item_id: "item-ready", _class: .ready_clean, head_sha: "h", base_sha: "b")),
                    kind: .verification_recorded, recorded_at: t[0], run_id: runID)
            ],
            sections: [
                .init(
                    campaign_id: campaignID, events: [],
                    runs: [
                        .init(
                            run_id: runID, parent_run_id: parentID, superseded_by: successorID,
                            milestones: [], events: [])
                    ])
            ])
        let run = timeline.sections[0].runs[0]
        let runRows = TaskTimelinePresentation.technicalRows(run: run, in: timeline)
        #expect(
            runRows.map(\.label) == [
                "Run ID", "Parent Run ID", "Superseded by Run ID", "Verification Inbox Item ID", "Verified Head",
                "Verified Base",
            ])
        #expect(runRows.map(\.value) == [runID, parentID, successorID, "item-ready", "h", "b"])
        #expect(TaskTimelinePresentation.technicalRows(taskID: "task-x").map(\.value) == ["task-x"])
    }

    /// The footer holds the rows each card held, grouped by owner in page
    /// order: the task, then each campaign followed by its attempts. An
    /// owner with no identifiers draws no group.
    @Test func identifierGroupsFollowPageOrder() {
        let t = Self.instants
        let timeline = Components.Schemas.TaskTimeline(
            as_of_revision: 1, as_of: t[0], task_id: "task-x", project_id: "p",
            name: .init(text: "n", source: ._operator),
            events: [],
            sections: [
                .init(
                    campaign_id: "campaign-new", events: [],
                    runs: [
                        .init(run_id: "run-2", attempt_number: 2, parent_run_id: "run-1", milestones: [], events: []),
                        .init(run_id: "run-1", attempt_number: 1, milestones: [], events: []),
                    ]),
                .init(campaign_id: "campaign-old", events: [], runs: []),
                .init(events: [], runs: [.init(run_id: "run-legacy", milestones: [], events: [])]),
            ])
        let groups = TaskTimelinePresentation.identifierGroups(timeline, taskID: "task-x")

        #expect(
            groups.map(\.owner) == [
                "Task", "Campaign campaign-new", "Attempt 2", "Attempt 1", "Campaign campaign-old", "Run run-legacy",
            ])
        #expect(groups[0].rows == TaskTimelinePresentation.technicalRows(taskID: "task-x"))
        #expect(groups[1].rows == TaskTimelinePresentation.technicalRows(section: timeline.sections[0]))
        #expect(
            groups[2].rows == TaskTimelinePresentation.technicalRows(run: timeline.sections[0].runs[0], in: timeline))
        // The section outside a campaign records no id of its own, so only
        // its run appears.
        #expect(!groups.map(\.owner).contains("Outside a Campaign"))
        #expect(TaskTimelinePresentation.identifiersSummary(timeline) == "task · 2 campaigns · 3 runs")

        let single = Components.Schemas.TaskTimeline(
            as_of_revision: 1, as_of: t[0], task_id: "task-x", project_id: "p",
            name: .init(text: "n", source: ._operator), events: [],
            sections: [
                .init(campaign_id: "campaign-a", events: [], runs: [.init(run_id: "run-a", milestones: [], events: [])])
            ])
        #expect(
            TaskTimelinePresentation.identifierGroups(single, taskID: "task-x").map(\.owner)
                == ["Task", "Campaign", "Run run-a"])
        #expect(TaskTimelinePresentation.identifiersSummary(single) == "task · campaign · 1 run")
    }

    @Test func identifierOwnersTellSharedAttemptNumbersApart() {
        let t = Self.instants
        func run(
            _ id: String, _ role: Components.Schemas.TaskRunRole, attempt: Int
        ) -> Components.Schemas.TaskTimelineRun {
            .init(run_id: id, role: .init(value1: role), attempt_number: attempt, milestones: [], events: [])
        }
        // The daemon numbers a campaign's specification run and its first
        // implementation run both attempt 1, and restarts in a new campaign.
        let timeline = Components.Schemas.TaskTimeline(
            as_of_revision: 1, as_of: t[0], task_id: "task-x", project_id: "p",
            name: .init(text: "n", source: ._operator), events: [],
            sections: [
                .init(
                    campaign_id: "campaign-new", events: [],
                    runs: [run("run-b1", .implementation, attempt: 1), run("run-bs", .specification, attempt: 1)]),
                .init(
                    campaign_id: "campaign-old", events: [],
                    runs: [run("run-a2", .implementation, attempt: 2), run("run-a1", .implementation, attempt: 1)]),
            ])
        let owners = TaskTimelinePresentation.identifierGroups(timeline, taskID: "task-x").map(\.owner)

        #expect(
            owners == [
                "Task", "Campaign campaign-new", "Implementation attempt 1, campaign campaign-new", "Specification run",
                "Campaign campaign-old", "Implementation attempt 2", "Implementation attempt 1, campaign campaign-old",
            ])
        #expect(Set(owners).count == owners.count)
    }

    @Test func runTechnicalRowsKeepEveryVerificationItemID() {
        let t = Self.instants
        let runID = "run-x"
        func verification(_ item: String, at instant: Date) -> Components.Schemas.TaskEvent {
            .init(
                verification: .init(value1: .init(item_id: item, _class: .ready_clean, head_sha: "h", base_sha: "b")),
                kind: .verification_recorded, recorded_at: instant, run_id: runID)
        }
        // A remediated run records one verification event per ready item; every
        // item id is kept, not just the first, with the commits it was run on.
        let timeline = Components.Schemas.TaskTimeline(
            as_of_revision: 1, as_of: t[0], task_id: "task-x", project_id: "p",
            name: .init(text: "n", source: ._operator),
            events: [verification("item-1", at: t[0]), verification("item-2", at: t[1])],
            sections: [
                .init(campaign_id: "campaign-a", events: [], runs: [.init(run_id: runID, milestones: [], events: [])])
            ])
        let rows = TaskTimelinePresentation.technicalRows(run: timeline.sections[0].runs[0], in: timeline)
        // Numbered when there is more than one, so each copy control's
        // accessibility label ("Copy Verification Inbox item ID 1") is distinct.
        #expect(
            rows.map(\.label) == [
                "Run ID", "Verification Inbox Item ID 1", "Verified Head 1", "Verified Base 1",
                "Verification Inbox Item ID 2", "Verified Head 2", "Verified Base 2",
            ])
        #expect(rows.map(\.value) == [runID, "item-1", "h", "b", "item-2", "h", "b"])
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
