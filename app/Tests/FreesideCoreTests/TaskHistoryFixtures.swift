import Foundation
import FreesideAPI

@testable import FreesideCore

/// Complete, synthetic relationships for history tests. Unlike the demo
/// campaign samples, every approval here has a specification predecessor.
enum TaskHistoryFixtures {
    enum Scenario: CaseIterable {
        case creation, approved, published, retry, revised, legacy
    }

    static let instant = RunFixtures.screenshotInstant

    static func history(_ scenario: Scenario) -> Components.Schemas.TaskTimeline {
        func at(_ minutes: Int) -> Date { instant.addingTimeInterval(TimeInterval(minutes * 60)) }
        func run(
            _ id: String, role: Components.Schemas.TaskRunRole, attempt: Int = 1
        )
            -> Components.Schemas.TaskTimelineRun
        {
            .init(
                run_id: id, role: .init(value1: role), attempt_number: attempt,
                milestones: [
                    .init(run_id: id, kind: .invocation_started, recorded_at: at(role == .specification ? -55 : -40)),
                    .init(run_id: id, kind: .run_submitted, recorded_at: at(role == .specification ? -60 : -50)),
                ], events: [])
        }
        let specification = run("history-spec-1", role: .specification)
        var implementation = run("history-implementation-1", role: .implementation)
        var sections: [Components.Schemas.TaskTimelineSection] = []
        if scenario != .creation && scenario != .legacy {
            if scenario == .published {
                implementation.milestones.insert(
                    .init(run_id: implementation.run_id, kind: .publication_ready, recorded_at: at(-10)), at: 0)
                implementation.events = [
                    .init(kind: .pr_opened, recorded_at: at(-10), run_id: implementation.run_id, pr_number: 110)
                ]
            }
            var members = [implementation, specification]
            if scenario == .retry {
                var retry = run("history-implementation-2", role: .implementation, attempt: 2)
                retry.milestones[0].recorded_at = at(-14)
                retry.milestones[1].recorded_at = at(-15)
                retry.parent_run_id = implementation.run_id
                retry.attempt_reason = "Retry after verification findings"
                members[0].superseded_by = retry.run_id
                members.insert(retry, at: 0)
            }
            sections = [
                .init(
                    campaign_id: "history-campaign-1",
                    events: [
                        .init(
                            kind: .specification_approved, recorded_at: at(-50),
                            campaign_id: "history-campaign-1", run_id: implementation.run_id,
                            approved_spec_digest: .init(value1: "sha256:" + String(repeating: "a", count: 64)),
                            specification_run_id: specification.run_id),
                        .init(
                            kind: .campaign_allocated, recorded_at: at(-60),
                            campaign_id: "history-campaign-1", specification_run_id: specification.run_id),
                    ], runs: members)
            ]
            if scenario == .revised {
                var revised = run("history-spec-revised", role: .specification)
                revised.milestones[0].recorded_at = at(-4)
                revised.milestones[1].recorded_at = at(-5)
                sections.insert(
                    .init(
                        campaign_id: "history-campaign-revised",
                        events: [
                            .init(
                                kind: .campaign_allocated, recorded_at: at(-5),
                                campaign_id: "history-campaign-revised", specification_run_id: revised.run_id)
                        ], runs: [revised]), at: 0)
            }
        } else if scenario == .legacy {
            var legacy = implementation
            legacy.role = nil
            legacy.attempt_number = nil
            sections = [.init(campaign_id: nil, events: [], runs: [legacy])]
        }
        var events: [Components.Schemas.TaskEvent] = [.init(kind: .task_created, recorded_at: at(-65))]
        if let firstRun = sections.flatMap(\.runs).last {
            events.append(.init(kind: .task_started, recorded_at: at(-60), run_id: firstRun.run_id))
        }
        for section in sections {
            events += section.events
            for run in section.runs {
                events += run.events
                events += run.milestones.map {
                    .init(
                        milestone: .init(value1: $0), kind: .run_milestone, recorded_at: $0.recorded_at,
                        run_id: run.run_id)
                }
                if run.role?.value1 != .specification {
                    let head = String(repeating: run.attempt_number == 2 ? "b" : "a", count: 40)
                    let base = String(repeating: "a", count: 40)
                    events.append(
                        .init(
                            review: .init(
                                value1: .init(
                                    invocation_id: "review-\(run.run_id)", round: 1, head_sha: head, base_sha: base)),
                            kind: .review_requested, recorded_at: at(run.attempt_number == 2 ? -10 : -30),
                            run_id: run.run_id))
                    events.append(
                        .init(
                            review: .init(
                                value1: .init(
                                    invocation_id: "review-\(run.run_id)", round: 1, head_sha: head, base_sha: base,
                                    outcome: .init(value1: .clean))),
                            kind: .review_completed, recorded_at: at(run.attempt_number == 2 ? -5 : -20),
                            run_id: run.run_id))
                    if scenario == .published {
                        events.append(
                            .init(
                                verification: .init(
                                    value1: .init(
                                        item_id: "history-ready", _class: .ready_clean, head_sha: head, base_sha: base)),
                                kind: .verification_recorded, recorded_at: at(-15), run_id: run.run_id))
                    }
                }
            }
        }
        events.sort { $0.recorded_at > $1.recorded_at }
        return .init(
            as_of_revision: 12, as_of: instant, task_id: "history-task", project_id: "freeside",
            name: .init(text: "Explain release changes", source: ._operator),
            events: events, sections: sections)
    }

    static func snapshot(_ history: Components.Schemas.TaskTimeline) -> Components.Schemas.TaskSnapshot {
        guard let creation = history.events.first(where: { $0.kind == .task_created }) else {
            preconditionFailure("History fixtures must include task creation")
        }
        return .init(
            as_of_revision: history.as_of_revision, entity_version: 1,
            task: .init(
                id: history.task_id, project_id: history.project_id,
                display_names: .init(project: .init(text: "Sample project", source: ._operator), task: history.name),
                created_at: creation.recorded_at, last_activity_at: history.as_of,
                lifecycle: history.sections.isEmpty ? nil : .active,
                campaign_ids: history.sections.compactMap(\.campaign_id).reversed(),
                run_ids: history.sections.flatMap(\.runs).map(\.run_id).reversed(),
                wip: !history.sections.isEmpty,
                lifecycle_facts: history.sections.flatMap(\.runs).last.map {
                    [
                        .init(
                            kind: .started, run_id: $0.run_id,
                            recorded_at: creation.recorded_at.addingTimeInterval(5 * 60))
                    ]
                } ?? []))
    }

    @MainActor
    static func coordinator(
        _ history: Components.Schemas.TaskTimeline, server: MockServer = MockServer()
    ) throws -> SyncCoordinator {
        let cache = InMemoryCacheStore()
        let timelines = history.sections.flatMap(\.runs).filter { $0.role?.value1 != .specification }.map { run in
            var round = RunFixtures.reviewRound(.completed, availability: .available)
            round.invocation_id = "review-\(run.run_id)"
            round.head_sha = String(repeating: run.attempt_number == 2 ? "b" : "a", count: 40)
            round.requested_at = instant.addingTimeInterval(run.attempt_number == 2 ? -10 * 60 : -30 * 60)
            round.completed_at = instant.addingTimeInterval(run.attempt_number == 2 ? -5 * 60 : -20 * 60)
            return Components.Schemas.RunTimeline(
                as_of_revision: history.as_of_revision, as_of: history.as_of,
                run_id: run.run_id, milestones: run.milestones, invocations: [],
                review: .init(value1: .init(rounds: [round])))
        }
        try cache.save(
            .init(
                cursors: .init(
                    syncEpoch: "history-epoch", lastFullSnapshotRevision: 12, highestObservedServerRevision: 12),
                attentionItems: [], runTimelines: timelines, tasks: [snapshot(history)], taskTimelines: [history]))
        return SyncCoordinator(client: APIClientFactory.mock(server: server), cache: cache)
    }
}
