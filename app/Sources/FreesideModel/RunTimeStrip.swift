import Foundation
import FreesideAPI

/// Where a run's time went, to scale: the run's elapsed time cut into
/// ordered, non-overlapping segments of three kinds. Built only from what
/// the client already holds, the run's milestones and the synced attention
/// items, so it needs no daemon record of its own.
package enum RunTimeStrip {
    package enum Kind: CaseIterable, Equatable, Sendable {
        /// An agent invocation, from its start to its own export, outcome,
        /// or terminal milestone.
        case agentWorking
        /// An attention item about the run, open or later closed, while no
        /// agent works.
        case waitingOnYou
        /// Everything else: queueing, holds, verification, publication.
        case other
    }

    package struct Segment: Equatable, Sendable {
        package let kind: Kind
        package let start: Date
        package let end: Date

        package var duration: TimeInterval { end.timeIntervalSince(start) }
    }

    package struct Strip: Equatable, Sendable {
        package let start: Date
        package let end: Date
        /// Oldest first. Each segment starts where the one before it ends,
        /// the first at `start` and the last at `end`, and no two touching
        /// segments share a kind.
        package let segments: [Segment]

        package var elapsed: TimeInterval { end.timeIntervalSince(start) }

        package func total(_ kind: Kind) -> TimeInterval {
            segments.filter { $0.kind == kind }.reduce(0) { $0 + $1.duration }
        }
    }

    /// The milestone kinds that end an invocation's working span.
    private static let invocationEndings: Set<Components.Schemas.RunMilestoneKind> = [
        .execution_export_recorded, .execution_outcome_recorded, .terminal_recorded,
    ]

    /// The run's strip, or nil when the run has no milestones or no elapsed
    /// time.
    ///
    /// The bar starts at `run_submitted`, else the earliest milestone. A
    /// finished run ends at its newest milestone; any other ends at `now`,
    /// never before its newest milestone, so a skewed device clock can't
    /// make a negative length. Milestones come in daemon record order
    /// (`ORDER BY id`), which pairs each start with its own later ending.
    ///
    /// Agent working wins where it overlaps waiting on you: a card left
    /// open while the agent works doesn't stop the run. Items whose subject
    /// names no run (task-scoped ones) and items with no placeable instant
    /// add nothing.
    package static func strip(
        run: Components.Schemas.Run,
        milestones: [Components.Schemas.RunMilestone],
        attentionItems: [Components.Schemas.AttentionItemSnapshot],
        now: Date
    ) -> Strip? {
        strip(
            runID: run.id, finished: run.lifecycle == .finished, milestones: milestones,
            attentionItems: attentionItems, now: now)
    }

    /// The same strip for a run known only by its id and whether it
    /// finished, as the task strip knows a task timeline's runs (#3234).
    package static func strip(
        runID: String,
        finished: Bool,
        milestones: [Components.Schemas.RunMilestone],
        attentionItems: [Components.Schemas.AttentionItemSnapshot],
        now: Date
    ) -> Strip? {
        guard let newest = milestones.map(\.recorded_at).max(),
            let earliest = milestones.map(\.recorded_at).min()
        else { return nil }
        let start = milestones.first { $0.kind == .run_submitted }?.recorded_at ?? earliest
        let end = finished ? newest : max(now, newest)
        guard end > start else { return nil }

        let agent = agentSpans(milestones, barEnd: end)
        // An open card waits to the bar's end, so a device clock behind
        // the daemon's can't drop a card created moments ago.
        let waiting = waitingSpans(runID: runID, attentionItems, now: end)

        // Every span edge inside the bar cuts it; each piece between two
        // cuts lies wholly inside or wholly outside any one span.
        let edges = (agent + waiting).flatMap { [$0.start, $0.end] }.filter { $0 > start && $0 < end }
        let cuts = Array(Set(edges + [start, end])).sorted()
        var segments: [Segment] = []
        for (pieceStart, pieceEnd) in zip(cuts, cuts.dropFirst()) {
            let covers = { (span: Span) in span.start <= pieceStart && span.end >= pieceEnd }
            let kind: Kind =
                agent.contains(where: covers) ? .agentWorking : waiting.contains(where: covers) ? .waitingOnYou : .other
            if let last = segments.last, last.kind == kind {
                segments[segments.count - 1] = Segment(kind: kind, start: last.start, end: pieceEnd)
            } else {
                segments.append(Segment(kind: kind, start: pieceStart, end: pieceEnd))
            }
        }
        return Strip(start: start, end: end, segments: segments)
    }

    private struct Span {
        let start: Date
        let end: Date
    }

    /// Each `invocation_started` to the first later ending milestone on the
    /// same invocation; with none, to the next invocation's start, else to
    /// the bar's end.
    private static func agentSpans(_ milestones: [Components.Schemas.RunMilestone], barEnd: Date) -> [Span] {
        milestones.indices.compactMap { index in
            let started = milestones[index]
            guard started.kind == .invocation_started else { return nil }
            let later = milestones[(index + 1)...]
            let ending =
                started.invocation_id.flatMap { invocation in
                    later.first { $0.invocation_id == invocation && invocationEndings.contains($0.kind) }
                }
                ?? later.first { $0.kind == .invocation_started }
            return Span(start: started.recorded_at, end: ending?.recorded_at ?? barEnd)
        }
    }

    /// Each item about the run from its creation to its conclusion (or its
    /// decision, on a row with no conclusion), or to `now` (the bar's end)
    /// while open.
    private static func waitingSpans(
        runID: String, _ items: [Components.Schemas.AttentionItemSnapshot], now: Date
    ) -> [Span] {
        items.compactMap { snapshot in
            let item = snapshot.item
            guard DecisionModel.runID(of: item.subject) == runID, let created = item.created_at else { return nil }
            let closed = item.status == .open ? now : item.conclusion?.value1.concluded_at ?? item.decided_at
            return closed.map { Span(start: created, end: $0) }
        }
    }
}
