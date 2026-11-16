import Foundation

/// Tasks and items at every position the work status view counts, and the
/// ones it leaves out, for its tests and screenshots: several stages
/// (`implement` among them), an unknown stage, two hold reasons with the
/// capacity fold, a held task and a finished task with open cards, tasks
/// with no position or stage, finished and stopped tasks, scoped items, and
/// a task in another project.
public enum WorkStatusFixtures {
    public static let project = "freeside"
    public static let otherProject = "atlas"

    public static func tasks() -> [Components.Schemas.TaskSnapshot] {
        [
            task("task-spec", "Draft the import contract", stage: "specification", minutesAgo: 5),
            task("task-implement", "Add the work status view", stage: "implement", minutesAgo: 8),
            task("task-implementation", "Retry admission on a moved base", stage: "implementation", minutesAgo: 12),
            task("task-review", "Tighten the export scan", stage: "review", minutesAgo: 20),
            task("task-triage", "Sort the backlog labels", stage: "triage", minutesAgo: 25),
            task("task-unpositioned", "Rename the pool settings", position: false, minutesAgo: 30),
            task("task-unstaged", "Pin the linter version", lifecycle: nil, stage: nil, minutesAgo: 35),
            task(
                "task-capacity", "Profile the sync loop", stage: "implementation", hold: .usage_pool_exhausted,
                minutesAgo: 40),
            task(
                "task-parallel", "Split the card renderer", stage: "implement", hold: .identity_parallelism,
                minutesAgo: 45),
            task("task-base", "Refresh the API client", stage: "review", hold: .base_advanced, minutesAgo: 50),
            task("task-card", "Decide the retention window", stage: "review", hold: .scope_conflict, minutesAgo: 55),
            task(
                "task-finished-card", "Publish the release notes", lifecycle: .finished, stage: "verification",
                minutesAgo: 60),
            task(
                "task-finished", "Document the pairing flow", lifecycle: .finished, stage: "verification",
                minutesAgo: 90),
            task(
                "task-stopped", "Prototype the timeline", lifecycle: .stopped, stage: "implementation", minutesAgo: 120),
            task("task-other-project", "Index the atlas tiles", project: otherProject, stage: "review", minutesAgo: 15),
        ]
    }

    /// Open cards on two tasks, plus a project-scoped and a system-scoped
    /// item, which the view never counts.
    public static func openItems() -> [Components.Schemas.AttentionItemSnapshot] {
        [
            taskItem("item-card", task: "task-card", minutesAgo: 2),
            taskItem("item-finished-card", task: "task-finished-card", minutesAgo: 3),
            scopedItem("item-project", subject: .project(.init(subject_type: .project, subject_id: project))),
            scopedItem("item-system", subject: .system(.init(subject_type: .system, subject_id: "system"))),
        ]
    }

    /// Two days of the project's work hours before the capture day, for
    /// `MockServer.setWorkHours`: a day with every class, two hold reasons,
    /// and two tasks, then a day of agent work alone. The bounds are UTC; the
    /// mock replaces them with the request's.
    public static func workHoursDays() -> [Components.Schemas.WorkHoursDay] {
        [
            workHoursDay(
                "2026-08-10", working: 7_200, waiting: 1_800,
                held: [
                    .init(reason: .usage_pool_exhausted, seconds: 900), .init(reason: .base_advanced, seconds: 300),
                ],
                other: 600,
                tasks: [
                    .init(task_id: "task-implement", elapsed_seconds: 7_800),
                    .init(task_id: "task-review", elapsed_seconds: 3_000),
                ]),
            workHoursDay(
                "2026-08-11", working: 3_600, waiting: 0, held: [], other: 0,
                tasks: [.init(task_id: "task-implement", elapsed_seconds: 3_600)]),
        ]
    }

    private static func workHoursDay(
        _ date: String, working: Int64, waiting: Int64, held: [Components.Schemas.WorkHoursHeld], other: Int64,
        tasks: [Components.Schemas.WorkHoursTask]
    ) -> Components.Schemas.WorkHoursDay {
        let heldSeconds = held.map(\.seconds).reduce(0, +)
        let start = ISO8601DateFormatter().date(from: date + "T00:00:00Z") ?? RunFixtures.screenshotInstant
        return .init(
            date: date, starts_at: start, ends_at: start.addingTimeInterval(86_400),
            elapsed_seconds: working + waiting + heldSeconds + other, agent_working_seconds: working,
            waiting_on_you_seconds: waiting, held_seconds: heldSeconds, held_by_reason: held, other_seconds: other,
            tasks: tasks)
    }

    private static func task(
        _ id: String, _ name: String, project: String = project,
        lifecycle: Components.Schemas.Task.lifecyclePayload? = .active, position: Bool = true,
        stage: String? = nil, hold: Components.Schemas.RunHoldReason? = nil, minutesAgo: Double
    ) -> Components.Schemas.TaskSnapshot {
        let activity = RunFixtures.screenshotInstant.addingTimeInterval(-minutesAgo * 60)
        return .init(
            as_of_revision: 1, entity_version: 1,
            task: .init(
                id: id, project_id: project,
                display_names: .init(
                    project: .init(text: project, source: .identifier), task: .init(text: name, source: .name)),
                created_at: activity.addingTimeInterval(-3_600), last_activity_at: activity,
                lifecycle: lifecycle,
                current_position: position
                    ? .init(
                        value1: .init(
                            run_id: "run-\(id)", stage: stage, round: nil, hold_reason: hold.map { .init(value1: $0) }))
                    : nil,
                campaign_ids: [], run_ids: position ? ["run-\(id)"] : [], wip: lifecycle == .active,
                lifecycle_facts: [], task_lines: [], attachments: []))
    }

    private static func taskItem(
        _ id: String, task taskID: String, minutesAgo: Double
    )
        -> Components.Schemas
        .AttentionItemSnapshot
    {
        scopedItem(
            id, subject: .task(.init(subject_type: .task, subject_id: taskID, task_id: taskID)), minutesAgo: minutesAgo)
    }

    private static func scopedItem(
        _ id: String, subject: Components.Schemas.Subject, minutesAgo: Double = 4
    ) -> Components.Schemas.AttentionItemSnapshot {
        var snapshot = AttentionFixtures.fixture(type: .agent_question)
        snapshot.item.id = id
        snapshot.item.status = .open
        snapshot.item.project_id = project
        snapshot.item.subject = subject
        snapshot.item.created_at = RunFixtures.screenshotInstant.addingTimeInterval(-minutesAgo * 60)
        snapshot.item.decided_at = nil
        return snapshot
    }
}
