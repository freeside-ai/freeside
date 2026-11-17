import Foundation
import FreesideAPI

/// Where a task's time went, across all its runs (#3234): one time axis from
/// the first run's start to the last run's end, each run's part cut as its
/// own run strip cuts it, and the time between runs as a kind that only
/// this strip has.
package enum TaskTimeStrip {
    /// What the strip knows of one run of the task timeline.
    package struct Run: Equatable, Sendable {
        package let runID: String
        /// Whether the run's strip ends at its newest milestone rather than
        /// at now, as `RunTimeStrip` decides when the run serves no clocks.
        package let finished: Bool
        /// As the task timeline serves them: newest first, ties in record
        /// order.
        package let milestones: [Components.Schemas.RunMilestone]
        /// The run's served holds, empty from a daemon older than #3236.
        package let holdHistory: [Components.Schemas.RunHoldSpan]
        /// The run's served clocks, nil from a daemon older than #3236.
        package let clocks: Components.Schemas.RunClocks?

        package init(
            runID: String, finished: Bool, milestones: [Components.Schemas.RunMilestone],
            holdHistory: [Components.Schemas.RunHoldSpan] = [], clocks: Components.Schemas.RunClocks? = nil
        ) {
            self.runID = runID
            self.finished = finished
            self.milestones = milestones
            self.holdHistory = holdHistory
            self.clocks = clocks
        }
    }

    /// One run's stretch of the task strip, cut into the run strip's kinds.
    package struct RunPart: Equatable, Sendable {
        package let runID: String
        package let start: Date
        package let end: Date
        /// Oldest first and touching, the first at `start` and the last at
        /// `end`.
        package let segments: [RunTimeStrip.Segment]

        package var duration: TimeInterval { end.timeIntervalSince(start) }

        /// The part's stretch from `from` to `to`, both inside it, its
        /// segments cut to fit.
        func slice(from: Date, to: Date) -> RunPart {
            let kept = segments.filter { $0.end > from && $0.start < to }.map { segment in
                RunTimeStrip.Segment(
                    kind: segment.kind, start: max(segment.start, from), end: min(segment.end, to),
                    holdReason: segment.holdReason)
            }
            return RunPart(runID: runID, start: from, end: to, segments: kept)
        }
    }

    package enum Part: Equatable, Sendable {
        case run(RunPart)
        /// No run of the task spans this time.
        case betweenRuns(start: Date, end: Date)

        package var start: Date {
            switch self {
            case .run(let part): part.start
            case .betweenRuns(let start, _): start
            }
        }

        package var end: Date {
            switch self {
            case .run(let part): part.end
            case .betweenRuns(_, let end): end
            }
        }

        package var duration: TimeInterval { end.timeIntervalSince(start) }
    }

    package struct Strip: Equatable, Sendable {
        /// Oldest first. Each part starts where the one before it ends, the
        /// first at `start` and the last at `end`, and no two touching parts
        /// are both between runs or both one run's. A run that another run
        /// interrupts has a part before and after it.
        package let parts: [Part]

        package var start: Date { parts[0].start }
        package var end: Date { parts[parts.count - 1].end }
        package var elapsed: TimeInterval { end.timeIntervalSince(start) }

        package var runParts: [RunPart] {
            parts.compactMap { part in
                if case .run(let run) = part { run } else { nil }
            }
        }

        /// How many runs the strip draws, each counted once however many
        /// parts it has.
        package var runCount: Int { Set(runParts.map(\.runID)).count }

        /// One run kind's total across every run's part.
        package func total(_ kind: RunTimeStrip.Kind) -> TimeInterval {
            runParts.flatMap(\.segments).filter { $0.kind == kind }.reduce(0) { $0 + $1.duration }
        }

        package var betweenRuns: TimeInterval {
            parts.reduce(0) { total, part in
                if case .betweenRuns = part { total + part.duration } else { total }
            }
        }
    }

    /// The task's strip, or nil when no run has a strip of its own.
    ///
    /// Each run is cut by `RunTimeStrip.strip(runID:finished:…)`, which
    /// pairs an invocation's start with its ending in record order. The
    /// task timeline serves milestones newest first under a stable sort, so
    /// equal instants keep record order there; rebuilding oldest first
    /// sorts by instant and then by served position, never by reversing,
    /// which would flip those ties.
    ///
    /// The strip runs from the earliest run's start to the latest run's
    /// end. Nothing in the served data rules out two runs overlapping, so
    /// each stretch of time belongs to the latest-started run that spans it
    /// (the longer, of two that start together): an earlier run's part ends
    /// where a later run starts, and resumes if it outlasts that run. A run
    /// that starts with a longer one has no part. Time no run spans is a
    /// between-runs part.
    package static func strip(
        runs: [Run],
        attentionItems: [Components.Schemas.AttentionItemSnapshot],
        now: Date
    ) -> Strip? {
        let ordered = runs.compactMap { run -> RunPart? in
            RunTimeStrip.strip(
                runID: run.runID, finished: run.finished, milestones: oldestFirst(run.milestones),
                attentionItems: attentionItems, holdHistory: run.holdHistory, clocks: run.clocks, now: now
            ).map { RunPart(runID: run.runID, start: $0.start, end: $0.end, segments: $0.segments) }
        }
        .sorted { ($0.start, $0.end, $0.runID) < ($1.start, $1.end, $1.runID) }

        // Every run edge cuts the axis; each piece between two cuts lies
        // wholly inside or wholly outside any one run.
        let cuts = Set(ordered.flatMap { [$0.start, $0.end] }).sorted()
        var parts: [Part] = []
        for (from, to) in zip(cuts, cuts.dropFirst()) {
            let owner = ordered.last { $0.start <= from && $0.end >= to }
            switch (owner, parts.last) {
            case (let owner?, .run(let previous)?) where previous.runID == owner.runID:
                parts[parts.count - 1] = .run(owner.slice(from: previous.start, to: to))
            case (let owner?, _):
                parts.append(.run(owner.slice(from: from, to: to)))
            case (nil, .betweenRuns(let start, _)?):
                parts[parts.count - 1] = .betweenRuns(start: start, end: to)
            case (nil, _):
                parts.append(.betweenRuns(start: from, end: to))
            }
        }
        return parts.isEmpty ? nil : Strip(parts: parts)
    }

    /// Served milestones (newest first, ties in record order) back in
    /// record order.
    static func oldestFirst(_ milestones: [Components.Schemas.RunMilestone]) -> [Components.Schemas.RunMilestone] {
        milestones.enumerated()
            .sorted { ($0.element.recorded_at, $0.offset) < ($1.element.recorded_at, $1.offset) }
            .map(\.element)
    }
}
