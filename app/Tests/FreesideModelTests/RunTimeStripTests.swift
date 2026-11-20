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

    /// A hold from `from`, cleared at `to`, or open with no `to`.
    private static func hold(
        _ reason: Components.Schemas.RunHoldReason, _ from: Int, _ to: Int? = nil
    ) -> Components.Schemas.RunHoldSpan {
        .init(
            run_id: runID, invocation_id: nil, reason: reason, first_observed_at: at(from),
            last_observed_at: at(to ?? from), cleared_at: to.map(at))
    }

    private static func strip(
        _ lifecycle: Components.Schemas.RunLifecycle, _ milestones: [Components.Schemas.RunMilestone],
        cards: [Components.Schemas.AttentionItemSnapshot] = [], holds: [Components.Schemas.RunHoldSpan] = [],
        clocks: Components.Schemas.RunClocks? = nil, now: Int
    ) -> RunTimeStrip.Strip? {
        RunTimeStrip.strip(
            run: run(lifecycle), milestones: milestones, attentionItems: cards, holdHistory: holds, clocks: clocks,
            now: at(now))
    }

    /// Each held segment's reason, oldest first.
    private static func holdReasons(_ strip: RunTimeStrip.Strip?) -> [Components.Schemas.RunHoldReason?] {
        strip?.segments.filter { $0.kind == .held }.map(\.holdReason) ?? []
    }

    /// The segments as (kind, start minute, end minute), with the strip's
    /// invariants checked on the way: contiguous from the bar's start to its
    /// end, a hold reason on held segments only, no touching segments of
    /// one kind and reason, and lengths and totals that sum to the elapsed
    /// time.
    private static func shape(_ strip: RunTimeStrip.Strip?) -> [(Kind, Int, Int)] {
        guard let strip else { return [] }
        var cursor = strip.start
        for (index, segment) in strip.segments.enumerated() {
            #expect(segment.start == cursor && segment.end > segment.start)
            #expect((segment.kind == .held) == (segment.holdReason != nil))
            if index > 0 {
                let previous = strip.segments[index - 1]
                #expect(previous.kind != segment.kind || previous.holdReason != segment.holdReason)
            }
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

    /// A verification works from its start to its own result, as the
    /// daemon's clocks count it (#3446); the writer's outcome on another
    /// invocation doesn't end it.
    @Test func aVerificationWorksFromItsStartToItsResult() {
        let strip = Self.strip(
            .active,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 2),
                Self.milestone(.execution_export_recorded, 18),
                Self.milestone(.verification_started, 19, invocation: 9),
                Self.milestone(.execution_outcome_recorded, 20),
                Self.milestone(.verification_passed, 24, invocation: 9),
            ], now: 30)
        #expect(
            Self.same(
                Self.shape(strip),
                [(.other, 0, 2), (.agentWorking, 2, 18), (.other, 18, 19), (.agentWorking, 19, 24), (.other, 24, 30)]))
    }

    @Test func aVerificationWithNoResultRunsToTheNextStart() {
        let strip = Self.strip(
            .active,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.verification_started, 5, invocation: 9),
                Self.milestone(.invocation_started, 12, invocation: 2),
                Self.milestone(.execution_export_recorded, 20, invocation: 2),
            ], now: 25)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 5), (.agentWorking, 5, 20), (.other, 20, 25)]))
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

    @Test func theFixtureRunSplitsFourWays() {
        let snapshot = RunFixtures.timeStripRun()
        let timeline = RunFixtures.timeStripTimeline()
        let strip = RunTimeStrip.strip(
            run: snapshot.run, milestones: timeline.milestones,
            attentionItems: AttentionFixtures.timeStripItems() + AttentionFixtures.defaultInbox(),
            holdHistory: timeline.hold_history ?? [], clocks: timeline.clocks?.value1, now: RunFixtures.timeStripNow)
        #expect(
            Self.same(
                Self.shape(strip),
                [
                    (.other, 0, 3), (.agentWorking, 3, 41), (.waitingOnYou, 41, 100), (.held, 100, 132),
                    (.other, 132, 135), (.agentWorking, 135, 172), (.other, 172, 175), (.waitingOnYou, 175, 230),
                ]))
        #expect(Self.holdReasons(strip) == [.quota_window_exhausted])
    }

    // MARK: Held time and served clocks (#3237)

    @Test func aHoldBeforeTheFirstInvocationIsHeld() {
        let strip = Self.strip(
            .finished,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_admitted, 10),
                Self.milestone(.invocation_started, 12), Self.milestone(.execution_export_recorded, 20),
            ], holds: [Self.hold(.machine_capacity, 1, 10)], now: 60)
        #expect(
            Self.same(
                Self.shape(strip), [(.other, 0, 1), (.held, 1, 10), (.other, 10, 12), (.agentWorking, 12, 20)]))
        #expect(Self.holdReasons(strip) == [.machine_capacity])
    }

    @Test func aHoldBetweenInvocationsIsHeld() {
        let strip = Self.strip(
            .finished,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 2),
                Self.milestone(.execution_outcome_recorded, 10), Self.milestone(.invocation_started, 30, invocation: 2),
                Self.milestone(.execution_export_recorded, 40, invocation: 2),
            ], holds: [Self.hold(.quota_window_exhausted, 12, 28)], now: 60)
        #expect(
            Self.same(
                Self.shape(strip),
                [
                    (.other, 0, 2), (.agentWorking, 2, 10), (.other, 10, 12), (.held, 12, 28), (.other, 28, 30),
                    (.agentWorking, 30, 40),
                ]))
    }

    @Test func anOpenHoldRunsToTheBarsEnd() {
        let strip = Self.strip(
            .active, [Self.milestone(.run_submitted, 0, invocation: nil)],
            holds: [Self.hold(.usage_pool_exhausted, 5)], now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 5), (.held, 5, 30)]))
    }

    @Test func twoTouchingHoldsKeepTheirOwnReasons() {
        let strip = Self.strip(
            .active, [Self.milestone(.run_submitted, 0, invocation: nil)],
            holds: [Self.hold(.machine_capacity, 2, 10), Self.hold(.quota_window_exhausted, 10, 20)], now: 25)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 2), (.held, 2, 10), (.held, 10, 20), (.other, 20, 25)]))
        #expect(Self.holdReasons(strip) == [.machine_capacity, .quota_window_exhausted])
    }

    @Test func whereTwoHoldsOverlapTheOneObservedLaterWins() {
        // Served out of order, so the order the hold was observed decides,
        // not its place in the list.
        let strip = Self.strip(
            .active, [Self.milestone(.run_submitted, 0, invocation: nil)],
            holds: [Self.hold(.base_advanced, 10, 15), Self.hold(.machine_capacity, 2, 20)], now: 25)
        #expect(
            Self.same(
                Self.shape(strip), [(.other, 0, 2), (.held, 2, 10), (.held, 10, 15), (.held, 15, 20), (.other, 20, 25)]
            ))
        #expect(Self.holdReasons(strip) == [.machine_capacity, .base_advanced, .machine_capacity])
    }

    @Test func agentWorkingWinsOverAHold() {
        let strip = Self.strip(
            .active,
            [
                Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 5),
                Self.milestone(.execution_export_recorded, 15),
            ], holds: [Self.hold(.machine_capacity, 2, 20)], now: 30)
        #expect(
            Self.same(
                Self.shape(strip),
                [(.other, 0, 2), (.held, 2, 5), (.agentWorking, 5, 15), (.held, 15, 20), (.other, 20, 30)]))
    }

    @Test func waitingOnYouWinsOverAHold() {
        let strip = Self.strip(
            .active, [Self.milestone(.run_submitted, 0, invocation: nil)], cards: [Self.card(created: 10)],
            holds: [Self.hold(.trust_blocked, 5)], now: 30)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 5), (.held, 5, 10), (.waitingOnYou, 10, 30)]))
    }

    @Test func aHoldPastAFinishedRunsEndIsCutThere() {
        let milestones = [
            Self.milestone(.run_submitted, 0, invocation: nil), Self.milestone(.invocation_started, 2),
            Self.milestone(.execution_outcome_recorded, 6), Self.milestone(.terminal_recorded, 10, invocation: nil),
        ]
        for hold in [Self.hold(.external_conflict, 8, 30), Self.hold(.external_conflict, 8)] {
            let strip = Self.strip(.finished, milestones, holds: [hold], now: 60)
            #expect(
                Self.same(Self.shape(strip), [(.other, 0, 2), (.agentWorking, 2, 6), (.other, 6, 8), (.held, 8, 10)]))
            #expect(strip?.end == Self.at(10))
        }
    }

    @Test func anUnknownReasonDrawsAsHeld() {
        let strip = Self.strip(
            .active, [Self.milestone(.run_submitted, 0, invocation: nil)], holds: [Self.hold(.unknown, 2, 5)], now: 10)
        #expect(Self.same(Self.shape(strip), [(.other, 0, 2), (.held, 2, 5), (.other, 5, 10)]))
        #expect(Self.holdReasons(strip) == [.unknown])
    }

    /// Served clocks set the bar and the agent's spans over what the
    /// milestones would say, and their end, not the run's lifecycle,
    /// decides whether the run finished.
    @Test func servedClocksSetTheBarAndTheAgentWorking() {
        let milestones = [
            Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 2),
            Self.milestone(.execution_export_recorded, 10),
        ]
        let finished = Components.Schemas.RunClocks(
            started_at: Self.at(1), ended_at: Self.at(12), elapsed_seconds: 11 * 60, active_seconds: 6 * 60,
            active_intervals: [.init(invocation_id: "inv-1", started_at: Self.at(3), ended_at: Self.at(9))])
        let ended = Self.strip(.active, milestones, clocks: finished, now: 60)
        #expect(Self.same(Self.shape(ended), [(.other, 1, 3), (.agentWorking, 3, 9), (.other, 9, 12)]))

        // A live run's open interval runs to the bar's end, which is now.
        let live = Components.Schemas.RunClocks(
            started_at: Self.at(0), ended_at: nil, elapsed_seconds: 10 * 60, active_seconds: 8 * 60,
            active_intervals: [.init(invocation_id: "inv-1", started_at: Self.at(2), ended_at: nil)])
        let running = Self.strip(.finished, milestones, clocks: live, now: 30)
        #expect(Self.same(Self.shape(running), [(.other, 0, 2), (.agentWorking, 2, 30)]))
    }

    /// A finished run's totals, rounded down, are the served seconds, with
    /// fractional instants and overlapping intervals counted once.
    @Test func aFinishedRunsTotalsRoundDownToTheServedSeconds() throws {
        let start = Self.base.addingTimeInterval(0.4)
        let clocks = Components.Schemas.RunClocks(
            started_at: start, ended_at: Self.base.addingTimeInterval(3_600.9), elapsed_seconds: 3_600,
            active_seconds: 990,
            active_intervals: [
                .init(
                    invocation_id: "inv-1", started_at: Self.base.addingTimeInterval(10.25),
                    ended_at: Self.base.addingTimeInterval(700.8)),
                .init(
                    invocation_id: "inv-2", started_at: Self.base.addingTimeInterval(600.5),
                    ended_at: Self.base.addingTimeInterval(1_000.3)),
            ])
        let strip = try #require(
            Self.strip(
                .finished, [Self.milestone(.run_submitted, 0), Self.milestone(.terminal_recorded, 60)],
                holds: [Self.hold(.machine_capacity, 20, 30)], clocks: clocks, now: 90))
        #expect(Int64(strip.elapsed.rounded(.down)) == clocks.elapsed_seconds)
        #expect(Int64(strip.total(.agentWorking).rounded(.down)) == clocks.active_seconds)
    }

    /// One invocation's working time is the span the strip draws for it:
    /// its served interval, or its own start-to-ending milestones, and no
    /// time at all where the strip would only guess an end.
    @Test func anInvocationsDurationIsItsOwnSpan() {
        let milestones = [
            Self.milestone(.run_submitted, 0), Self.milestone(.invocation_started, 2),
            Self.milestone(.invocation_started, 5, invocation: 2),
            Self.milestone(.execution_outcome_recorded, 11), Self.milestone(.invocation_started, 12, invocation: 3),
        ]
        #expect(RunTimeStrip.invocationDuration("inv-1", milestones: milestones) == TimeInterval(9 * 60))
        // No ending of its own: the strip runs it to the next start, but
        // the duration says nothing.
        #expect(RunTimeStrip.invocationDuration("inv-2", milestones: milestones) == nil)
        #expect(RunTimeStrip.invocationDuration("inv-9", milestones: milestones) == nil)

        let clocks = Components.Schemas.RunClocks(
            started_at: Self.at(0), ended_at: nil, elapsed_seconds: 20 * 60, active_seconds: 10 * 60,
            active_intervals: [
                .init(invocation_id: "inv-1", started_at: Self.at(3), ended_at: Self.at(7)),
                .init(invocation_id: "inv-3", started_at: Self.at(12), ended_at: nil),
            ])
        #expect(
            RunTimeStrip.invocationDuration("inv-1", milestones: milestones, clocks: clocks) == TimeInterval(4 * 60))
        // Still working, or not served: no time.
        #expect(RunTimeStrip.invocationDuration("inv-3", milestones: milestones, clocks: clocks) == nil)
        #expect(RunTimeStrip.invocationDuration("inv-2", milestones: milestones, clocks: clocks) == nil)
    }
}
