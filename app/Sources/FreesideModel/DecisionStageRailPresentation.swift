package struct DecisionStageRailPresentation: Equatable {
    package enum State: Equatable {
        case completed
        case current
        case failed
        case pending

        var accessibilityLabel: String {
            switch self {
            case .completed: "completed"
            case .current: "current"
            case .failed: "failed"
            case .pending: "pending"
            }
        }
    }

    package struct Entry: Equatable, Identifiable {
        package let id: String
        package let title: String
        package let state: State

        package var accessibilityLabel: String {
            "\(title), \(state.accessibilityLabel)"
        }
    }

    package let entries: [Entry]
    package let summary: String

    /// The stages the run reached, newest first, as a card lists history
    /// (frame 5.3). A stage the run never reached is left out: above the
    /// failure it would read as the latest event. The summary still counts
    /// every stage.
    package var reachedNewestFirst: Self {
        .init(entries: entries.filter { $0.state != .pending }.reversed(), summary: summary)
    }

    static func failure(stages: [String], failedStageIndex: Int) -> Self? {
        guard stages.indices.contains(failedStageIndex) else { return nil }
        let entries = stages.enumerated().map { index, stage in
            Entry(
                id: "\(index)-\(stage)",
                title: stage,
                state: index < failedStageIndex ? .completed : index == failedStageIndex ? .failed : .pending)
        }
        return .init(
            entries: entries,
            summary:
                "\(stages[failedStageIndex]) failed, stage \(failedStageIndex + 1) of \(stages.count).")
    }
}
