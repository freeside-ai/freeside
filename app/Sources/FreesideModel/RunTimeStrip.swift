import Foundation
import FreesideAPI

/// Where a run's time went, to scale: the run's elapsed time cut into
/// ordered, non-overlapping segments of four kinds. Built from the run's
/// milestones, its served hold history and clocks (#3236), and the synced
/// attention items. A timeline that serves no clocks and no hold history
/// (a daemon older than #3236) draws from the milestones alone, with no
/// held time.
///
/// Where kinds overlap, the first in `Kind` order wins: agent working,
/// waiting on you, held, other. A hold doesn't stop an invocation that
/// already started (`devlog/2026-11-16-1845-run-clock-definition.md`), and
/// a hold that waits on a decision usually has a card, which is what the
/// reader can act on. Where two holds overlap, the one observed later wins.
package enum RunTimeStrip {
    /// In precedence order, which is also the legend's order.
    package enum Kind: CaseIterable, Equatable, Sendable {
        /// An agent invocation, from its start to its own export, outcome,
        /// or terminal milestone, or a verification, from its start to its
        /// result (`RunWorkSpan`).
        case agentWorking
        /// An attention item about the run, open or later closed, while no
        /// agent works.
        case waitingOnYou
        /// A served hold, while no agent works and no card waits.
        case held
        /// Everything else: queueing before admission, publication, and
        /// verification on a run that recorded no `verification_started`.
        case other
    }

    package struct Segment: Equatable, Sendable {
        package let kind: Kind
        package let start: Date
        package let end: Date
        /// The hold's reason on a held segment; nil on every other kind.
        package let holdReason: Components.Schemas.RunHoldReason?

        package init(
            kind: Kind, start: Date, end: Date, holdReason: Components.Schemas.RunHoldReason? = nil
        ) {
            self.kind = kind
            self.start = start
            self.end = end
            self.holdReason = holdReason
        }

        package var duration: TimeInterval { end.timeIntervalSince(start) }
    }

    package struct Strip: Equatable, Sendable {
        package let start: Date
        package let end: Date
        /// Oldest first. Each segment starts where the one before it ends,
        /// the first at `start` and the last at `end`, and no two touching
        /// segments share both a kind and a hold reason: two touching holds
        /// with different reasons stay two segments.
        package let segments: [Segment]

        package var elapsed: TimeInterval { end.timeIntervalSince(start) }

        package func total(_ kind: Kind) -> TimeInterval {
            segments.filter { $0.kind == kind }.reduce(0) { $0 + $1.duration }
        }
    }

    /// The run's strip, or nil when the run has no milestones or no elapsed
    /// time.
    ///
    /// With served `clocks`, the bar starts at `clocks.started_at`, a run
    /// with `clocks.ended_at` ends there, and agent working is
    /// `clocks.active_intervals`, an open interval running to the bar's
    /// end. So a finished run's elapsed and agent working totals, rounded
    /// down, are the served `elapsed_seconds` and `active_seconds`, to the
    /// millisecond the client decodes instants at. A live
    /// run still ends at `now`, never before its newest milestone, so a
    /// skewed device clock can't make a negative length.
    ///
    /// Without clocks, the strip derives the same from the milestones, as
    /// before #3236: the bar starts at `run_submitted`, else the earliest
    /// milestone, and a finished run ends at its newest milestone.
    /// Milestones come in daemon record order (`ORDER BY id`), which pairs
    /// each start with its own later ending.
    ///
    /// Each served hold is held from `first_observed_at` to `cleared_at`,
    /// or to the bar's end while open, cut to the bar. Items whose subject
    /// names no run (task-scoped ones) and items with no placeable instant
    /// add nothing.
    package static func strip(
        run: Components.Schemas.Run,
        milestones: [Components.Schemas.RunMilestone],
        attentionItems: [Components.Schemas.AttentionItemSnapshot],
        holdHistory: [Components.Schemas.RunHoldSpan] = [],
        clocks: Components.Schemas.RunClocks? = nil,
        now: Date
    ) -> Strip? {
        strip(
            runID: run.id, finished: run.lifecycle == .finished, milestones: milestones,
            attentionItems: attentionItems, holdHistory: holdHistory, clocks: clocks, now: now)
    }

    /// The same strip for a run known only by its id and whether it
    /// finished, as the task strip knows a task timeline's runs (#3234).
    /// Served `clocks` decide whether the run finished; `finished` counts
    /// only without them.
    package static func strip(
        runID: String,
        finished: Bool,
        milestones: [Components.Schemas.RunMilestone],
        attentionItems: [Components.Schemas.AttentionItemSnapshot],
        holdHistory: [Components.Schemas.RunHoldSpan] = [],
        clocks: Components.Schemas.RunClocks? = nil,
        now: Date
    ) -> Strip? {
        guard let newest = milestones.map(\.recorded_at).max(),
            let earliest = milestones.map(\.recorded_at).min()
        else { return nil }
        let start: Date
        let end: Date
        if let clocks {
            start = clocks.started_at
            end = clocks.ended_at ?? max(now, newest)
        } else {
            start = milestones.first { $0.kind == .run_submitted }?.recorded_at ?? earliest
            end = finished ? newest : max(now, newest)
        }
        guard end > start else { return nil }

        let agent =
            clocks.map { clocks in
                clocks.active_intervals.map { Span(start: $0.started_at, end: $0.ended_at ?? end) }
            } ?? agentSpans(milestones, barEnd: end)
        // An open card waits to the bar's end, so a device clock behind
        // the daemon's can't drop a card created moments ago.
        let waiting = waitingSpans(runID: runID, attentionItems, now: end)
        // Oldest observed first, so the last span covering a piece is the
        // one observed later.
        let holds = holdHistory.map { hold in
            (span: Span(start: hold.first_observed_at, end: hold.cleared_at ?? end), reason: hold.reason)
        }
        .sorted { $0.span.start < $1.span.start }

        // Every span edge inside the bar cuts it; each piece between two
        // cuts lies wholly inside or wholly outside any one span.
        let edges = (agent + waiting + holds.map(\.span)).flatMap { [$0.start, $0.end] }
            .filter { $0 > start && $0 < end }
        let cuts = Array(Set(edges + [start, end])).sorted()
        var segments: [Segment] = []
        for (pieceStart, pieceEnd) in zip(cuts, cuts.dropFirst()) {
            let covers = { (span: Span) in span.start <= pieceStart && span.end >= pieceEnd }
            let kind: Kind
            var reason: Components.Schemas.RunHoldReason?
            if agent.contains(where: covers) {
                kind = .agentWorking
            } else if waiting.contains(where: covers) {
                kind = .waitingOnYou
            } else if let hold = holds.last(where: { covers($0.span) }) {
                kind = .held
                reason = hold.reason
            } else {
                kind = .other
            }
            if let last = segments.last, last.kind == kind, last.holdReason == reason {
                segments[segments.count - 1] = Segment(kind: kind, start: last.start, end: pieceEnd, holdReason: reason)
            } else {
                segments.append(Segment(kind: kind, start: pieceStart, end: pieceEnd, holdReason: reason))
            }
        }
        return Strip(start: start, end: end, segments: segments)
    }

    private struct Span {
        let start: Date
        let end: Date
    }

    /// Each span `RunWorkSpan` opens, to its ending, else to the bar's end.
    private static func agentSpans(_ milestones: [Components.Schemas.RunMilestone], barEnd: Date) -> [Span] {
        milestones.indices.compactMap { index in
            guard RunWorkSpan.startsWork(milestones[index].kind) else { return nil }
            let ending = RunWorkSpan.ending(of: index, in: milestones)
            return Span(start: milestones[index].recorded_at, end: ending?.recorded_at ?? barEnd)
        }
    }

    /// How long one invocation worked, by the span the strip draws for it:
    /// its served active interval, or without `clocks` its
    /// `invocation_started` to the first later ending milestone on the same
    /// invocation. Nil while it is still working or when neither is there,
    /// so a reader shows no time rather than a guess. The transcript
    /// sheet's `Worked for` and `Failed after` read it, so they agree with
    /// the strip (#3231).
    package static func invocationDuration(
        _ invocationID: String, milestones: [Components.Schemas.RunMilestone],
        clocks: Components.Schemas.RunClocks? = nil
    ) -> TimeInterval? {
        if let clocks {
            guard let interval = clocks.active_intervals.first(where: { $0.invocation_id == invocationID }),
                let ended = interval.ended_at
            else { return nil }
            return ended.timeIntervalSince(interval.started_at)
        }
        guard
            let index = milestones.firstIndex(where: {
                $0.kind == .invocation_started && $0.invocation_id == invocationID
            }),
            let ending = RunWorkSpan.ownEnding(of: index, in: milestones)
        else { return nil }
        return ending.recorded_at.timeIntervalSince(milestones[index].recorded_at)
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
