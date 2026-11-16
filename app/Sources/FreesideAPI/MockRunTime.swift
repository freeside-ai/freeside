import Foundation

/// The run time fields the daemon serves on both timelines (#3236): the hold
/// history and the clocks. The mock derives them when it serves a timeline,
/// as the daemon does, so every fixture serves them without hand-computed
/// totals.
enum MockRunTime {
    /// The fixture's cleared spans, then the current hold as the open span,
    /// as the daemon's store serves them. A fixture stores only cleared
    /// spans, so a test that changes the current hold keeps a coherent
    /// history.
    static func holdHistory(_ timeline: Components.Schemas.RunTimeline) -> [Components.Schemas.RunHoldSpan] {
        let cleared = (timeline.hold_history ?? []).filter { $0.cleared_at != nil }
        guard let hold = timeline.hold?.value1 else { return cleared }
        return cleared + [
            .init(
                run_id: hold.run_id, invocation_id: hold.invocation_id, reason: hold.reason,
                first_observed_at: hold.first_observed_at, last_observed_at: hold.last_observed_at,
                cleared_at: nil)
        ]
    }

    /// The milestone kinds that end an invocation's working span.
    private static let invocationEndings: Set<Components.Schemas.RunMilestoneKind> = [
        .execution_export_recorded, .execution_outcome_recorded, .terminal_recorded,
    ]

    /// The run's clocks from its milestones in record order, as
    /// `domain.RunClocksOf` in the daemon defines them; nil with no
    /// milestone.
    static func clocks(
        _ milestones: [Components.Schemas.RunMilestone], finished: Bool, asOf: Date
    ) -> Components.Schemas.RunClocks? {
        let instants = milestones.map(\.recorded_at)
        guard let earliest = instants.min(), let newest = instants.max() else { return nil }
        let start = milestones.first { $0.kind == .run_submitted }?.recorded_at ?? earliest
        let end = finished ? newest : max(asOf, newest)
        let intervals: [Components.Schemas.RunActiveInterval] = milestones.indices.compactMap { index in
            let started = milestones[index]
            guard started.kind == .invocation_started, let invocation = started.invocation_id else { return nil }
            let later = milestones[(index + 1)...]
            let ending =
                later.first { $0.invocation_id == invocation && invocationEndings.contains($0.kind) }
                ?? later.first { $0.kind == .invocation_started }
            // An ending recorded at an earlier instant serves a zero-length
            // interval, as the daemon clamps it.
            return .init(
                invocation_id: invocation, started_at: started.recorded_at,
                ended_at: ending.map { max($0.recorded_at, started.recorded_at) } ?? (finished ? end : nil))
        }
        // The union of the intervals inside the run, so overlap counts once.
        let spans = intervals.compactMap { interval -> (from: Date, to: Date)? in
            let from = max(interval.started_at, start)
            let to = min(interval.ended_at ?? end, end)
            return from < to ? (from, to) : nil
        }.sorted { $0.from < $1.from }
        var active: TimeInterval = 0
        var reached = Date.distantPast
        for span in spans where max(span.from, reached) < span.to {
            active += span.to.timeIntervalSince(max(span.from, reached))
            reached = span.to
        }
        return .init(
            started_at: start, ended_at: finished ? end : nil,
            elapsed_seconds: Int64(end.timeIntervalSince(start).rounded(.down)),
            active_seconds: Int64(active.rounded(.down)), active_intervals: intervals)
    }
}
