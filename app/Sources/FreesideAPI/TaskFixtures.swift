import Foundation

/// Stable task identities group a campaign's attempts in the sample snapshots.
public enum TaskFixtures {
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
