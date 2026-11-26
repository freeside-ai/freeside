import FreesideAPI

/// Where work sits right now (plan §8, the work status view): how many
/// tasks hold each position under the project filter. It is read from the
/// unified list the same inputs build, so a count and the list its
/// position filter opens hold the same tasks: a Needs You task row waits
/// on you, a Working row sits at its own position, and Finished rows and
/// scoped items aren't counted.
package struct WorkStatus {
    /// The one position a counted task holds. A task with an open item
    /// waits on you whatever its lifecycle; an active task otherwise sits
    /// under its hold, else its stage, else hasn't started.
    package enum Position: Hashable, Sendable {
        case waitingOnYou
        /// A stored `identity_parallelism` hold reads as
        /// `usage_pool_exhausted`: the app names both "Waiting for agent
        /// capacity", so they are one group.
        case held(Components.Schemas.RunHoldReason)
        /// A canonical stage name (`RunDisplay.canonicalStageName`).
        case stage(String)
        case notStarted

        package var label: String {
            switch self {
            case .waitingOnYou: "Waiting on you"
            case .held(let reason): RunDisplay.label(reason)
            case .stage(let name): RunDisplay.stageLabel(name)
            case .notStarted: "Not started"
            }
        }
    }

    package struct Group: Identifiable, Equatable {
        package let position: Position
        /// In list order.
        package let taskIDs: [String]

        package var id: Position { position }
        package var label: String { position.label }
        package var count: Int { taskIDs.count }
    }

    /// The non-empty groups: waiting on you, the pipeline stages in
    /// pipeline order, any other stage by name, not started, then the
    /// holds in the vocabulary's order.
    package let groups: [Group]

    /// `open` is every open item, as `UnifiedList` takes it.
    package init(
        open: [Components.Schemas.AttentionItemSnapshot],
        tasks: [Components.Schemas.TaskSnapshot],
        projectFilter: String?
    ) {
        // Closed items only add Finished rows and scoped items, neither
        // of which is counted.
        let list = UnifiedList(open: open, closed: [], tasks: tasks, projectFilter: projectFilter)
        var taskIDsByPosition: [Position: [String]] = [:]
        for row in list.needsYou {
            if let snapshot = row.taskSnapshot {
                taskIDsByPosition[.waitingOnYou, default: []].append(snapshot.task.id)
            }
        }
        for row in list.working {
            if let snapshot = row.taskSnapshot, let position = Self.position(ofQuiet: snapshot.task) {
                taskIDsByPosition[position, default: []].append(snapshot.task.id)
            }
        }
        groups = Self.order(Array(taskIDsByPosition.keys)).compactMap { position in
            taskIDsByPosition[position].map { Group(position: position, taskIDs: $0) }
        }
    }

    /// The scale every row's bar shares: the largest count across all
    /// groups, held ones included, or 0 when nothing is counted.
    package var largestCount: Int {
        groups.map(\.count).max() ?? 0
    }

    /// How much of the track a count fills against the tab's largest
    /// count: 0 when the largest is 0, so an all-zero tab draws empty
    /// tracks rather than dividing by zero.
    package static func barFraction(_ count: Int, largest: Int) -> Double {
        largest > 0 ? Double(count) / Double(largest) : 0
    }

    /// The position of a task with no open item: nil for a finished,
    /// stopped, or abandoned task, which isn't counted. It reads
    /// `TaskDisplay.isActive`, as the list's Working section does, not
    /// `wip`.
    package static func position(ofQuiet task: Components.Schemas.Task) -> Position? {
        guard TaskDisplay.isActive(task) else { return nil }
        guard let current = task.current_position?.value1 else { return .notStarted }
        if let reason = current.hold_reason?.value1 {
            return .held(reason == .identity_parallelism ? .usage_pool_exhausted : reason)
        }
        return current.stage.map { .stage(RunDisplay.canonicalStageName($0)) } ?? .notStarted
    }

    private static func order(_ positions: [Position]) -> [Position] {
        let pipeline = RunDisplay.pipelineStages.map(\.rawValue)
        let holds = Components.Schemas.RunHoldReason.allCases
        func rank(_ position: Position) -> (Int, Int, String) {
            switch position {
            case .waitingOnYou: (0, 0, "")
            case .stage(let name): pipeline.firstIndex(of: name).map { (1, $0, "") } ?? (2, 0, name)
            case .notStarted: (3, 0, "")
            case .held(let reason): (4, holds.firstIndex(of: reason) ?? holds.count, "")
            }
        }
        return positions.sorted { rank($0) < rank($1) }
    }
}
