import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

@Suite struct TaskTimeStripTests {
    private static let base = Date(timeIntervalSince1970: 1_786_502_400)

    private static func at(_ minute: Int) -> Date {
        base.addingTimeInterval(TimeInterval(minute * 60))
    }

    private static func milestone(
        _ runID: String, _ kind: Components.Schemas.RunMilestoneKind, _ minute: Int, invocation: Int? = 1
    ) -> Components.Schemas.RunMilestone {
        .init(
            run_id: runID, kind: kind, invocation_id: invocation.map { "inv-\(runID)-\($0)" },
            recorded_at: at(minute))
    }

    /// A run submitted at `submitted` whose one invocation works from
    /// `working` to `ended`, served newest first as the task timeline
    /// serves it. With no `ended` the invocation is still working.
    private static func run(
        _ id: String, submitted: Int, working: Int, ended: Int? = nil, finished: Bool = true
    ) -> TaskTimeStrip.Run {
        var milestones = [
            milestone(id, .run_submitted, submitted, invocation: nil), milestone(id, .invocation_started, working),
        ]
        if let ended { milestones.append(milestone(id, .terminal_recorded, ended)) }
        return .init(runID: id, finished: finished, milestones: milestones.reversed())
    }

    private static func strip(
        _ runs: [TaskTimeStrip.Run], cards: [Components.Schemas.AttentionItemSnapshot] = [], now: Int
    ) -> TaskTimeStrip.Strip? {
        TaskTimeStrip.strip(runs: runs, attentionItems: cards, now: at(now))
    }

    private static func minute(_ date: Date) -> Int {
        Int(date.timeIntervalSince(base) / 60)
    }

    private static func name(_ kind: RunTimeStrip.Kind) -> String {
        switch kind {
        case .agentWorking: "agent"
        case .waitingOnYou: "waiting"
        case .other: "other"
        }
    }

    /// The parts as text, `run-a: other 0-2, agent 2-10` or
    /// `between 10-30`, with the strip's invariants checked on the way:
    /// contiguous from start to end, every part longer than nothing, no two
    /// between-runs parts or parts of one run touching, each run's segments
    /// filling its part,
    /// and totals that sum to the elapsed time.
    private static func shape(_ strip: TaskTimeStrip.Strip?) -> [String] {
        guard let strip else { return [] }
        var cursor = strip.start
        for (index, part) in strip.parts.enumerated() {
            #expect(part.start == cursor && part.end > part.start)
            if index > 0 {
                switch (strip.parts[index - 1], part) {
                case (.betweenRuns, .betweenRuns): Issue.record("two between-runs parts touch")
                case (.run(let previous), .run(let run)) where previous.runID == run.runID:
                    Issue.record("two parts of \(run.runID) touch")
                default: break
                }
            }
            if case .run(let run) = part {
                #expect(run.segments.first?.start == run.start && run.segments.last?.end == run.end)
            }
            cursor = part.end
        }
        #expect(cursor == strip.end)
        #expect(
            RunTimeStrip.Kind.allCases.reduce(strip.betweenRuns) { $0 + strip.total($1) } == strip.elapsed)
        return strip.parts.map { part in
            switch part {
            case .run(let run):
                "\(run.runID): "
                    + run.segments.map { "\(name($0.kind)) \(minute($0.start))-\(minute($0.end))" }
                    .joined(separator: ", ")
            case .betweenRuns(let start, let end):
                "between \(minute(start))-\(minute(end))"
            }
        }
    }

    @Test func oneRunIsItsOwnRunStrip() {
        let strip = Self.strip([Self.run("a", submitted: 0, working: 2, ended: 10)], now: 60)
        #expect(Self.shape(strip) == ["a: other 0-2, agent 2-10"])
        #expect(strip?.runCount == 1)
        #expect(strip?.betweenRuns == 0)
    }

    @Test func aGapBetweenTwoRunsIsBetweenRuns() {
        // Served newest first, as the task timeline lists runs; the strip
        // places them by start.
        let strip = Self.strip(
            [
                Self.run("b", submitted: 30, working: 31, ended: 40),
                Self.run("a", submitted: 0, working: 2, ended: 10),
            ], now: 60)
        #expect(Self.shape(strip) == ["a: other 0-2, agent 2-10", "between 10-30", "b: other 30-31, agent 31-40"])
        #expect(strip?.betweenRuns == TimeInterval(20 * 60))
        #expect(strip?.total(.agentWorking) == TimeInterval(17 * 60))
        #expect(strip?.elapsed == TimeInterval(40 * 60))
    }

    @Test func anOverlappingRunEndsWhereTheLaterOneStarts() {
        let strip = Self.strip(
            [
                Self.run("a", submitted: 0, working: 2, ended: 20),
                Self.run("b", submitted: 15, working: 16, ended: 30),
            ], now: 60)
        #expect(Self.shape(strip) == ["a: other 0-2, agent 2-15", "b: other 15-16, agent 16-30"])
        #expect(strip?.betweenRuns == 0)
    }

    /// A cut that lands on a segment's edge keeps the segment before it
    /// whole and nothing after it.
    @Test func aRunStartingOnASegmentEdgeCutsThere() {
        let strip = Self.strip(
            [
                Self.run("a", submitted: 0, working: 2, ended: 20),
                Self.run("b", submitted: 2, working: 3, ended: 30),
            ], now: 60)
        #expect(Self.shape(strip) == ["a: other 0-2", "b: other 2-3, agent 3-30"])
    }

    /// A run inside a longer one interrupts it; the longer run resumes when
    /// it ends, and the strip still runs to the longer run's end.
    @Test func anEarlierRunOutlastingALaterOneResumesAfterIt() {
        let strip = Self.strip(
            [
                Self.run("a", submitted: 0, working: 2, ended: 30),
                Self.run("b", submitted: 5, working: 6, ended: 10),
            ], now: 60)
        #expect(
            Self.shape(strip) == ["a: other 0-2, agent 2-5", "b: other 5-6, agent 6-10", "a: agent 10-30"])
        #expect(strip?.runCount == 2)
        #expect(strip?.end == Self.at(30))
    }

    /// With a run inside a longer one and a third after both, time is
    /// between runs only from the longer run's end.
    @Test func aContainedRunFollowedByAnotherLeavesNoFalseGap() {
        let strip = Self.strip(
            [
                Self.run("a", submitted: 0, working: 2, ended: 30),
                Self.run("b", submitted: 5, working: 6, ended: 10),
                Self.run("c", submitted: 40, working: 41, ended: 50),
            ], now: 60)
        #expect(
            Self.shape(strip) == [
                "a: other 0-2, agent 2-5", "b: other 5-6, agent 6-10", "a: agent 10-30", "between 30-40",
                "c: other 40-41, agent 41-50",
            ])
        #expect(strip?.betweenRuns == TimeInterval(10 * 60))
    }

    /// A run that starts with a longer one has no time of its own: the
    /// longer run owns the shared time.
    @Test func aRunStartingWithALongerOneIsDropped() {
        let strip = Self.strip(
            [
                Self.run("a", submitted: 0, working: 2, ended: 5),
                Self.run("b", submitted: 0, working: 1, ended: 30),
            ], now: 60)
        #expect(Self.shape(strip) == ["b: other 0-1, agent 1-30"])
    }

    @Test func noRunsOrNoMilestonesDrawNoStrip() {
        #expect(Self.strip([], now: 10) == nil)
        let empty: [TaskTimeStrip.Run] = [
            .init(runID: "a", finished: true, milestones: []),
            .init(runID: "b", finished: false, milestones: []),
        ]
        #expect(Self.strip(empty, now: 10) == nil)
    }

    @Test func aRunWithNoMilestonesIsSkipped() {
        let strip = Self.strip(
            [
                Self.run("a", submitted: 0, working: 2, ended: 10),
                .init(runID: "empty", finished: false, milestones: []),
                Self.run("b", submitted: 30, working: 31, ended: 40),
            ], now: 60)
        #expect(Self.shape(strip) == ["a: other 0-2, agent 2-10", "between 10-30", "b: other 30-31, agent 31-40"])
    }

    @Test func anUnfinishedLastRunEndsAtNow() {
        let strip = Self.strip(
            [
                Self.run("a", submitted: 0, working: 2, ended: 10),
                Self.run("b", submitted: 30, working: 31, finished: false),
            ], now: 60)
        #expect(Self.shape(strip) == ["a: other 0-2, agent 2-10", "between 10-30", "b: other 30-31, agent 31-60"])
        #expect(strip?.end == Self.at(60))
    }

    /// The daemon stable-sorts a run's milestones newest first, so an
    /// invocation that started and ended in the same instant is served
    /// start then ending. Reversing the list would put the ending first and
    /// leave the start to run on to the next invocation.
    @Test func equalInstantsKeepRecordOrder() {
        let id = "a"
        let served = [
            Self.milestone(id, .terminal_recorded, 15, invocation: nil),
            Self.milestone(id, .execution_export_recorded, 12, invocation: 2),
            Self.milestone(id, .invocation_started, 8, invocation: 2),
            Self.milestone(id, .invocation_started, 2, invocation: 1),
            Self.milestone(id, .execution_export_recorded, 2, invocation: 1),
            Self.milestone(id, .run_submitted, 0, invocation: nil),
        ]
        let strip = Self.strip([.init(runID: id, finished: true, milestones: served)], now: 60)
        #expect(Self.shape(strip) == ["a: other 0-8, agent 8-12, other 12-15"])
    }

    @Test func waitsOnARunCountInItsPart() {
        var card = AttentionFixtures.fixture(type: .agent_question)
        card.item.subject = .run(.init(subject_type: .run, subject_id: "b", run_id: "b", task_id: "task"))
        card.item.created_at = Self.at(35)
        let strip = Self.strip(
            [
                Self.run("a", submitted: 0, working: 2, ended: 10),
                .init(
                    runID: "b", finished: false,
                    milestones: [
                        Self.milestone("b", .execution_outcome_recorded, 35),
                        Self.milestone("b", .invocation_started, 31),
                        Self.milestone("b", .run_submitted, 30, invocation: nil),
                    ]),
            ], cards: [card], now: 50)
        #expect(
            Self.shape(strip) == [
                "a: other 0-2, agent 2-10", "between 10-30", "b: other 30-31, agent 31-35, waiting 35-50",
            ])
        #expect(strip?.total(.waitingOnYou) == TimeInterval(15 * 60))
    }
}
