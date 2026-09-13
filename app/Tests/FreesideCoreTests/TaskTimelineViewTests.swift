import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

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
