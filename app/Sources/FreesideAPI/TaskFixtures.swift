import Foundation

/// Stable task identities group a campaign's attempts in the sample snapshots.
public enum TaskFixtures {
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
            let names = Components.Schemas.DisplayNames(
                project: newest.display_names?.value1.project ?? .init(text: newest.project_id, source: .identifier),
                task: .init(text: id, source: .identifier))
            return .init(
                as_of_revision: snapshots.map(\.as_of_revision).max() ?? 1,
                entity_version: snapshots.map(\.entity_version).max() ?? 1,
                task: .init(
                    id: id, project_id: newest.project_id, display_names: names,
                    created_at: created, last_activity_at: lastActivity,
                    lifecycle: .init(rawValue: newest.lifecycle.rawValue),
                    current_position: .init(
                        value1: .init(
                            run_id: newest.id, stage: newest.stages.last?.name,
                            hold_reason: newest.hold_reason.map { .init(value1: $0.value1) })),
                    campaign_ids: campaigns, run_ids: runs.map(\.id)))
        }
    }
}
