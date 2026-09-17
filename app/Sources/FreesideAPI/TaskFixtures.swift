import Foundation

/// Stable task identities group a campaign's attempts in the sample snapshots.
public enum TaskFixtures {
    /// Complete daemon-shaped history, including the specification predecessor
    /// that the default samples omit. The source and approved digests differ.
    public static func approvedCampaign() -> (
        task: Components.Schemas.TaskSnapshot, runs: [Components.Schemas.RunSnapshot],
        history: Components.Schemas.TaskTimeline
    ) {
        guard let implementation = RunFixtures.defaultRuns().first(where: { $0.run.id == RunFixtures.readyRunID }),
            var task = defaultTasks().first(where: { $0.task.id == implementation.run.task_id })
        else { preconditionFailure("The approved campaign requires its implementation run and task fixtures") }
        var specification = RunFixtures.handedOffSpecificationRun()
        specification.run.spec_digest = "sha256:\(String(repeating: "3", count: 64))"
        task.task.run_ids.insert(specification.run.id, at: 0)
        let runs = [implementation, specification]
        var history = timeline(
            for: task.task, runs: runs.map(\.run), timelines: [:],
            revision: task.as_of_revision, asOf: RunFixtures.screenshotInstant)
        history.sections[0].events = [
            .init(
                kind: .specification_approved, recorded_at: RunFixtures.screenshotInstant,
                campaign_id: implementation.run.campaign_id, run_id: implementation.run.id,
                approved_spec_digest: .init(value1: implementation.run.spec_digest),
                specification_run_id: specification.run.id)
        ]
        return (task, runs, history)
    }

    /// Synthetic daemon acknowledgement fixture. Stop acceptance in MockServer
    /// remains pending; tests must explicitly supply this confirmed history.
    public static func confirmedStopped(_ snapshot: Components.Schemas.TaskSnapshot) -> Components.Schemas.TaskSnapshot
    {
        var result = snapshot
        let instant = result.task.last_activity_at
        let episode = result.task.lifecycle_facts.lastIndex(where: { $0.kind == .started }).map { $0 + 1 } ?? 0
        let runs = RunFixtures.defaultRuns().map(\.run)
        let target = Components.Schemas.TaskCancellationTarget(
            task_id: result.task.id, project_id: result.task.project_id, episode_ordinal: episode,
            runs: result.task.run_ids.map { id in
                .init(run_id: id, campaign_id: runs.first(where: { $0.id == id })?.campaign_id)
            })
        let epoch = "fixture-epoch"
        let digest = MockContractValidation.cancellationTargetDigest(target, epoch: epoch)
        let requestID = "cancel-\(result.task.id)"
        result.task.cancellation = .init(
            value1: .init(
                request_id: requestID, target: target, target_digest: digest, sync_epoch: epoch,
                fence_revision: result.as_of_revision, requested_at: instant, state: .confirmed,
                acknowledgement: .init(
                    value1: .init(
                        id: "ack-\(result.task.id)", request_id: requestID, target_digest: digest,
                        state: .confirmed, evidence_digest: digest, recorded_at: instant))))
        if result.task.wip, episode > 0 {
            result.task.lifecycle_facts.append(
                .init(
                    kind: .abandoned, run_id: result.task.lifecycle_facts[episode - 1].run_id, recorded_at: instant))
        }
        result.task.wip = false
        result.task.lifecycle = .stopped
        return result
    }

    public static func explicitlyAbandoned(
        _ snapshot: Components.Schemas.TaskSnapshot
    ) -> Components.Schemas.TaskSnapshot {
        var result = snapshot
        if let start = result.task.lifecycle_facts.last(where: { $0.kind == .started }) {
            result.task.lifecycle_facts.append(
                .init(
                    kind: .abandoned, run_id: start.run_id, recorded_at: result.task.last_activity_at))
            result.task.lifecycle = .abandoned
            result.task.wip = false
        }
        return result
    }

    /// Group the existing sample run observations by task. The sample run
    /// collection omits some specification predecessors, so it supplies no
    /// allocation or approval event for those incomplete campaign histories.
    public static func defaultTimelines() -> [Components.Schemas.TaskTimeline] {
        let runs = RunFixtures.defaultRuns().map(\.run)
        let timelines = Dictionary(uniqueKeysWithValues: RunFixtures.defaultTimelines().map { ($0.run_id, $0) })
        return defaultTasks().map { snapshot in
            timeline(
                for: snapshot.task, runs: runs, timelines: timelines,
                revision: snapshot.as_of_revision, asOf: RunFixtures.screenshotInstant)
        }
    }

    static func timeline(
        for task: Components.Schemas.Task, runs: [Components.Schemas.Run],
        timelines: [String: Components.Schemas.RunTimeline], revision: Int64, asOf: Date
    ) -> Components.Schemas.TaskTimeline {
        let campaignRuns = Dictionary(grouping: runs.filter { $0.task_id == task.id }, by: { $0.campaign_id ?? "" })
        func sectionInstant(_ campaign: String) -> Date {
            let instants = campaignRuns[campaign]?.compactMap(\.created_at) ?? []
            return (campaign.isEmpty ? instants.max() : instants.min()) ?? .distantPast
        }
        let campaignIDs = campaignRuns.keys.sorted { lhs, rhs in
            (sectionInstant(lhs), lhs) > (sectionInstant(rhs), rhs)
        }
        let sections: [Components.Schemas.TaskTimelineSection] = campaignIDs.map { campaign in
            let runs = (campaignRuns[campaign] ?? []).sorted {
                ($0.created_at ?? .distantPast, $0.id) > ($1.created_at ?? .distantPast, $1.id)
            }
            return .init(
                campaign_id: campaign.isEmpty ? nil : campaign, events: [],
                runs: runs.map { run in
                    let timeline = timelines[run.id]
                    var events: [Components.Schemas.TaskEvent] = []
                    if let completion = timeline?.completion?.value1 {
                        events.append(
                            .init(
                                kind: .pr_merged, recorded_at: completion.recorded_at, run_id: run.id,
                                pr_number: completion.pr_number, merge_commit_sha: completion.merge_commit_sha))
                    }
                    let role: Components.Schemas.TaskRunRole? =
                        run.campaign_id == nil
                        ? nil
                        : (run.stages.first?.name == "specification" ? .specification : .implementation)
                    return .init(
                        run_id: run.id, role: role.map { .init(value1: $0) },
                        attempt_number: run.attempt_number, attempt_reason: run.attempt_reason,
                        parent_run_id: run.parent_run_id, superseded_by: run.superseded_by,
                        milestones: (timeline?.milestones ?? []).sorted { $0.recorded_at > $1.recorded_at },
                        hold: timeline?.hold.map { .init(value1: $0.value1) }, events: events)
                })
        }
        return .init(
            as_of_revision: revision, as_of: asOf,
            task_id: task.id, project_id: task.project_id, name: task.display_names.task,
            events: [.init(kind: .task_created, recorded_at: task.created_at)], sections: sections)
    }

    /// The task holding the retry campaign (`run-freeside-656` and
    /// `run-freeside-657`), the one row the Tasks list shows for both attempts.
    public static let retryTaskID = "task-\(RunFixtures.retryCampaignID)"
    public static let legacyTaskID = "task-\(RunFixtures.legacyRunID)"

    /// The intake issue behind the named demo tasks, keyed by task id. The
    /// legacy and oriole tasks have none: `Task.source` is null for demo work
    /// without a recoverable intake reference.
    private static let issueNumbers: [String: Int] = [
        retryTaskID: 724,
        "task-campaign-freeside-ready": 654,
        "task-run-freeside-specification": 731,
        "task-campaign-freeside-completed": 80,
    ]

    public static func defaultTaskIDs() -> [String] {
        defaultTasks().map(\.task.id)
    }

    public static func defaultTasks() -> [Components.Schemas.TaskSnapshot] {
        let groups = Dictionary(grouping: RunFixtures.defaultRuns(), by: { $0.run.task_id })
        return groups.keys.sorted().compactMap { id in
            guard let snapshots = groups[id] else { return nil }
            let runs = snapshots.map(\.run).sorted {
                if $0.attempt_number != $1.attempt_number {
                    return ($0.attempt_number ?? 0) < ($1.attempt_number ?? 0)
                }
                return ($0.created_at ?? .distantPast, $0.id) < ($1.created_at ?? .distantPast, $1.id)
            }
            guard let newest = runs.last else { return nil }
            let created = runs.compactMap(\.created_at).min() ?? RunFixtures.screenshotInstant
            let lastActivity = max(created, runs.compactMap(\.last_activity_at).max() ?? created)
            var campaigns: [String] = []
            for run in runs {
                if let campaign = run.campaign_id, !campaigns.contains(campaign) {
                    campaigns.append(campaign)
                }
            }
            // The runs carry the task's current name, as the daemon copies it;
            // a run without display names belongs to an identifier-only task.
            let names = Components.Schemas.DisplayNames(
                project: newest.display_names?.value1.project ?? .init(text: newest.project_id, source: .identifier),
                task: newest.display_names?.value1.task ?? .init(text: id, source: .identifier))
            let source: Components.Schemas.Task.sourcePayload? = issueNumbers[id].map { number in
                .init(
                    value1: .issue_subject(
                        .init(
                            kind: .issue_subject,
                            issue_subject: .init(
                                repo: "freeside-ai/freeside", repository_id: 1, issue_number: number))))
            }
            // A started fact opens the task's log; a finished task adds a
            // completion bound to the newest run, so wip reads false. Active
            // tasks hold their slot (#1318).
            var lifecycleFacts: [Components.Schemas.TaskLifecycleFact] = [
                .init(kind: .started, run_id: runs.first?.id ?? newest.id, recorded_at: created)
            ]
            let wip: Bool
            switch newest.lifecycle {
            case .finished:
                lifecycleFacts.append(
                    .init(
                        kind: .completed, run_id: newest.id,
                        binding_unit_id: "workunit-\(newest.id)", recorded_at: lastActivity))
                wip = false
            case .active:
                wip = true
            }
            return .init(
                as_of_revision: snapshots.map(\.as_of_revision).max() ?? 1,
                entity_version: snapshots.map(\.entity_version).max() ?? 1,
                task: .init(
                    id: id, project_id: newest.project_id, display_names: names, source: source,
                    created_at: created, last_activity_at: lastActivity,
                    lifecycle: .init(rawValue: newest.lifecycle.rawValue),
                    current_position: .init(
                        value1: .init(
                            run_id: newest.id, stage: newest.stages.last?.name,
                            hold_reason: newest.hold_reason.map { .init(value1: $0.value1) })),
                    campaign_ids: campaigns, run_ids: runs.map(\.id),
                    wip: wip, lifecycle_facts: lifecycleFacts))
        }
    }
}
