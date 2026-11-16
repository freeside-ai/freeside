import Foundation
import FreesideAPI
import Testing

@testable import FreesideModel

@Suite struct RunTimeStripTests {
    private typealias Kind = RunTimeStrip.Kind
    private static let runID = "run-strip"
    private static let base = Date(timeIntervalSince1970: 1_786_502_400)

    private static func at(_ minute: Int) -> Date {
        base.addingTimeInterval(TimeInterval(minute * 60))
    }

    private static func milestone(
        _ kind: Components.Schemas.RunMilestoneKind, _ minute: Int, invocation: Int? = 1
    ) -> Components.Schemas.RunMilestone {
        .init(
            run_id: runID, kind: kind, invocation_id: invocation.map { "inv-\($0)" }, recorded_at: at(minute))
    }

    private static func run(_ lifecycle: Components.Schemas.RunLifecycle) -> Components.Schemas.Run {
        var run = RunFixtures.defaultRuns()[0].run
        run.id = runID
        run.lifecycle = lifecycle
        return run
    }

    /// A card about the run, open unless `closed` gives its conclusion
    /// (and, when `decided`, the decision instant the daemon stamps with it).
    private static func card(
        created: Int?, closed: Int? = nil, decided: Bool = true,
        subject: Components.Schemas.Subject = .run(
            .init(subject_type: .run, subject_id: runID, run_id: runID, task_id: "task-strip"))
    ) -> Components.Schemas.AttentionItemSnapshot {
        var snapshot = AttentionFixtures.fixture(type: .agent_question)
        snapshot.item.subject = subject
        snapshot.item.created_at = created.map(at)
        if let closed {
            snapshot.item.status = decided ? .resolved : .expired
            snapshot.item.decided_at = decided ? at(closed) : nil
            snapshot.item.conclusion = .init(
                value1: .init(
                    cause: decided ? .decision : .expired, concluded_at: at(closed), reason: "",
                    successor_item_id: nil, decision: nil))
        }
        return snapshot
    }

    private static func strip(
        _ lifecycle: Components.Schemas.RunLifecycle, _ milestones: [Components.Schemas.RunMilestone],
        cards: [Components.Schemas.AttentionItemSnapshot] = [], now: Int
    ) -> RunTimeStrip.Strip? {
        RunTimeStrip.strip(run: run(lifecycle), milestones: milestones, attentionItems: cards, now: at(now))
    }

    /// The segments as (kind, start minute, end minute), with the strip's
    /// invariants checked on the way: contiguous from the bar's start to its
    /// end, no touching segments of one kind, and lengths and totals that
    /// sum to the elapsed time.
    private static func shape(_ strip: RunTimeStrip.Strip?) -> [(Kind, Int, Int)] {
        guard let strip else { return [] }
        var cursor = strip.start
        for (index, segment) in strip.segments.enumerated() {
            #expect(segment.start == cursor && segment.end > segment.start)
            if index > 0 { #expect(strip.segments[index - 1].kind != segment.kind) }
            cursor = segment.end
        }
        #expect(cursor == strip.end)
        #expect(strip.segments.reduce(0) { $0 + $1.duration } == strip.elapsed)
        #expect(Kind.allCases.reduce(0) { $0 + strip.total($1) } == strip.elapsed)
        return strip.segments.map {
            ($0.kind, Int($0.start.timeIntervalSince(base) / 60), Int($0.end.timeIntervalSince(base) / 60))
        }
    }

    private static func same(_ lhs: [(Kind, Int, Int)], _ rhs: [(Kind, Int, Int)]) -> Bool {
        lhs.count == rhs.count && zip(lhs, rhs).allSatisfy { $0 == $1 }
    }

    @Test func aRunStillQueuedIsOtherUntilNow() {
        let strip = Self.strip(.active, [Self.milestone(.run_submitted, 0, invocation: nil)], now: 12)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 12)]))
        #expect(strip?.elapsed == TimeInterval(12 * 60))
    }

    @Test func aRunInProgressRunsToNowWithItsOpenInvocation() {
        let strip = Self.strip(
            .active,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_admitted, 1),
                Self.milestone(.invocation_started, 2),
            ], now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 2), (.agentWorking, 2, 30)]))
    }

    @Test func aFinishedRunEndsAtItsNewestMilestoneAndIgnoresNow() {
        let strip = Self.strip(
            .finished,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 2),
                Self.milestone(.terminal_recorded, 10), Self.milestone(.publication_ready, 14),
            ], now: 600)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 2), (.agentWorking, 2, 10), (.other, 10, 14)]))
    }

    @Test func twoInvocationsEachEndOnTheirOwnMilestone() {
        let strip = Self.strip(
            .finished,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 1),
                Self.milestone(.invocation_started, 3, invocation: 2),
                // The second invocation's ending must not end the first.
                Self.milestone(.terminal_recorded, 5, invocation: 2),
                Self.milestone(.terminal_recorded, 8),
                Self.milestone(.work_unit_completed, 9),
            ], now: 9)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 1), (.agentWorking, 1, 8), (.other, 8, 9)]))
    }

    @Test func anExportEndsItsInvocationBeforeAGapAndTheNextAttempt() {
        let strip = Self.strip(
            .active,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 2),
                Self.milestone(.execution_export_recorded, 18),
                Self.milestone(.invocation_started, 21, invocation: 2),
                Self.milestone(.execution_outcome_recorded, 25, invocation: 2),
            ], now: 30)
        #expect(
            Self.same(
                Self.shape(strip),
                [(.other, 0, 2), (.agentWorking, 2, 18), (.other, 18, 21), (.agentWorking, 21, 25), (.other, 25, 30)]))
    }

    @Test func anInvocationWithNoEndingRunsToTheNextInvocationsStart() {
        let strip = Self.strip(
            .finished,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 2),
                Self.milestone(.invocation_admitted, 9, invocation: 2),
                Self.milestone(.invocation_started, 10, invocation: 2),
                Self.milestone(.terminal_recorded, 15, invocation: 2),
                Self.milestone(.publication_ready, 16, invocation: nil),
            ], now: 16)
        // The first runs to 10 and the second from 10, so they meet as one.
        #expect(Self.same(Self.shape(strip), [(.other, 0, 2), (.agentWorking, 2, 15), (.other, 15, 16)]))
    }

    @Test func anOpenCardWaitsUntilNow() {
        let strip = Self.strip(
            .active,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 1),
                Self.milestone(.execution_outcome_recorded, 5),
            ], cards: [Self.card(created: 5)], now: 50)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 1), (.agentWorking, 1, 5), (.waitingOnYou, 5, 50)]))
        #expect(strip?.total(.waitingOnYou) == TimeInterval(45 * 60))
    }

    @Test func aDecidedCardWaitsUntilItsConclusion() {
        let strip = Self.strip(
            .active, [Self.milestone(.run_submitted, 0)], cards: [Self.card(created: 4, closed: 20)], now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 4), (.waitingOnYou, 4, 20), (.other, 20, 30)]))
    }

    @Test func aCardClosedWithoutADecisionWaitsUntilItsConclusion() {
        let strip = Self.strip(
            .active, [Self.milestone(.run_submitted, 0)], cards: [Self.card(created: 4, closed: 9, decided: false)],
            now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 4), (.waitingOnYou, 4, 9), (.other, 9, 30)]))
    }

    /// A row from an older daemon carries `decided_at` and no conclusion.
    @Test func aDecidedCardWithNoConclusionFallsBackToItsDecision() {
        var card = Self.card(created: 4, closed: 9)
        card.item.conclusion = nil
        let strip = Self.strip(.active, [Self.milestone(.run_submitted, 0)], cards: [card], now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 4), (.waitingOnYou, 4, 9), (.other, 9, 30)]))
    }

    @Test func cardsWithNoPlaceableInstantAddNothing() {
        var undated = Self.card(created: 4, closed: 9)
        undated.item.conclusion = nil
        undated.item.decided_at = nil
        let strip = Self.strip(
            .active, [Self.milestone(.run_submitted, 0)], cards: [Self.card(created: nil), undated], now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 30)]))
    }

    @Test func aTaskScopedCardAndAnotherRunsCardAreIgnored() {
        let task = Self.card(
            created: 2, subject: .task(.init(subject_type: .task, subject_id: "task-strip", task_id: "task-strip")))
        let elsewhere = Self.card(
            created: 2,
            subject: .run(.init(subject_type: .run, subject_id: "run-other", run_id: "run-other", task_id: "task-o")))
        let strip = Self.strip(.active, [Self.milestone(.run_submitted, 0)], cards: [task, elsewhere], now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 30)]))
    }

    @Test func agentWorkingWinsOverACardOpenWhileTheAgentWorks() {
        let strip = Self.strip(
            .active,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 5),
                Self.milestone(.execution_export_recorded, 20),
            ],
            // Two overlapping cards merge into one wait, cut where the agent works.
            cards: [Self.card(created: 2, closed: 12), Self.card(created: 10, closed: 25)], now: 30)
        #expect(
            Self.same(
                Self.shape(strip),
                [
                    (.other, 0, 2), (.waitingOnYou, 2, 5), (.agentWorking, 5, 20), (.waitingOnYou, 20, 25),
                    (.other, 25, 30),
                ]))
    }

    /// A proposal batch names the run it came from and counts toward it.
    @Test func aProposalBatchCardWaitsOnItsRun() {
        let batch = Self.card(
            created: 4, closed: 9,
            subject: .proposal_batch(.init(subject_type: .proposal_batch, subject_id: "batch-1", run_id: Self.runID)))
        let strip = Self.strip(.active, [Self.milestone(.run_submitted, 0)], cards: [batch], now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 4), (.waitingOnYou, 4, 9), (.other, 9, 30)]))
    }

    @Test func anUnknownMilestoneFallsIntoOther() {
        let strip = Self.strip(
            .finished, [Self.milestone(.run_submitted, 0), Self.milestone(.unknown, 7)], now: 7)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 7)]))
    }

    @Test func theBarStartsAtTheEarliestMilestoneWithoutASubmission() {
        let strip = Self.strip(
            .finished,
            [Self.milestone(.invocation_started, 3), Self.milestone(.terminal_recorded, 9)], now: 9)
        #expect(Self.same(Self.shape(strip), [(.agentWorking, 3, 9)]))
    }

    /// A device clock behind the daemon's never ends the bar before the
    /// newest milestone.
    @Test func aSkewedClockNeverCutsAnActiveRunShort() {
        let strip = Self.strip(
            .active,
            [Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 3)],
            cards: [Self.card(created: 1)], now: -10)
        // The open card, created after the device's now, still waits to the
        // bar's end at the newest milestone.
        #expect(Self.same(Self.shape(strip), [(.other, 0, 1), (.waitingOnYou, 1, 3)]))
    }

    @Test func noMilestonesOrNoElapsedTimeDrawsNoStrip() {
        #expect(Self.strip(.active, [], now: 10) == nil)
        #expect(Self.strip(.finished, [Self.milestone(.run_submitted, 0, invocation: nil)], now: 10) == nil)
    }

    @Test func theFixtureRunSplitsThreeWays() {
        let snapshot = RunFixtures.timeStripRun()
        let strip = RunTimeStrip.strip(
            run: snapshot.run, milestones: RunFixtures.timeStripTimeline().milestones,
            attentionItems: AttentionFixtures.timeStripItems() + AttentionFixtures.defaultInbox(),
            now: RunFixtures.timeStripNow)
        #expect(
            Self.same(
                Self.shape(strip),
                [
                    (.other, 0, 3), (.agentWorking, 3, 41), (.waitingOnYou, 41, 130), (.other, 130, 135),
                    (.agentWorking, 135, 172), (.other, 172, 175), (.waitingOnYou, 175, 230),
                ]))
    }
}
