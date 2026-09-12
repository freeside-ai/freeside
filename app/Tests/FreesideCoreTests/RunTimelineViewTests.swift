import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct RunTimelineViewTests {
    @Test func invalidReviewTextIsLabeledWithoutChangingRetainedBytes() {
        let bytes: [UInt8] = [0x61, 0xff, 0x62]
        let output = ReviewOutputText(bytes: bytes)
        #expect(output.hasReplacementCharacters)
        #expect(output.bytes == bytes)
        #expect(!ReviewOutputText(bytes: Array("valid text".utf8)).hasReplacementCharacters)
    }

    @Test func reviewFactsKeepUnknownDistinctFromClean() {
        for state in [Components.Schemas.ReviewProgressState.pending, .running, .failed] {
            let round = RunFixtures.reviewRound(state)
            #expect(round.outcome == nil)
            #expect(round.findings_count == nil)
            #expect(RunDisplay.reviewIdentity(round) == "Reviewer unknown · Model unknown")
            #expect(!RunDisplay.label(state).isEmpty)
        }
        let clean = RunFixtures.reviewRound(.completed)
        #expect(clean.outcome?.value1 == .clean)
        #expect(clean.findings_count == 0)
        #expect(RunDisplay.reviewIdentity(clean) == "openai · codex/high")
        let findings = RunFixtures.reviewRound(.completed, findings: true)
        #expect(findings.dispositions?.value1.open == 1)
    }

    @Test func roundIsOmittedUntilAStageHasAnAttempt() {
        var stage = RunFixtures.defaultRuns()[0].run.stages[0]
        stage.attempts = []

        #expect(RunDisplay.round(stage) == nil)
    }

    @Test func timelineHeaderPhaseAndRoundMatchTheRowTitle() throws {
        for run in RunFixtures.defaultRuns().map(\.run) + [RunFixtures.refreshedHistoryRun().run] {
            let heading = try #require(RunDisplay.stageHeading(run))
            #expect([heading.label, heading.round].compactMap { $0 }.joined(separator: " · ") == RunDisplay.title(run))
            if run.id == RunFixtures.activeRunID {
                #expect(heading.label == "Verification")
                #expect(heading.round == "Round 1")
            }
        }
        var run = RunFixtures.defaultRuns()[0].run
        run.stages = []
        #expect(RunDisplay.stageHeading(run) == nil)
    }

    @Test func timelineTitleIsTheAttemptNumberOrTheRunID() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        #expect(try #require(run.campaign_id).count == 73)
        #expect(RunDisplay.timelineTitle(run) == "Attempt 2")

        run.campaign_id = "campaign-short"
        #expect(RunDisplay.timelineTitle(run) == "Attempt 2")

        run.attempt_number = nil
        #expect(RunDisplay.timelineTitle(run) == RunFixtures.activeRunID)

        run.attempt_number = 2
        run.campaign_id = nil
        #expect(RunDisplay.timelineTitle(run) == RunFixtures.activeRunID)
    }

    @Test func observationsGroupByOwningStageNewestFirst() throws {
        let run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        let base = Date(timeIntervalSinceReferenceDate: 1_000)
        func observation(_ id: String, at offset: TimeInterval) -> Components.Schemas.InvocationObservation {
            .init(invocation_id: id, run_id: run.id, status: .completed, live: false, observed_at: base + offset)
        }
        // Attempt 1 re-observed after attempt 2, and the stray observed newest
        // of all: attempt order, not observation time, decides the rows, and
        // Unattributed sorts last however fresh its row.
        let first = observation("inv-\(run.id)-1", at: 120)
        let second = observation("inv-\(run.id)-2", at: 60)
        let stray = observation("inv-unknown", at: 240)

        let groups = RunTimelineGrouping.groups(invocations: [first, stray, second], stages: run.stages)

        #expect(groups.map(\.label) == ["Implementation", RunTimelineGrouping.unattributedLabel])
        #expect(groups[0].invocations.map(\.invocation_id) == [second.invocation_id, first.invocation_id])
        #expect(groups[1].invocations.map(\.invocation_id) == [stray.invocation_id])
    }

    @Test func daemonShapedImplementStageGroupsAsImplementation() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        run.stages[0].name = "implement"
        let owned = Components.Schemas.InvocationObservation(
            invocation_id: "inv-\(run.id)-2", run_id: run.id, status: .running, live: true,
            observed_at: Date(timeIntervalSinceReferenceDate: 1_000))

        let groups = RunTimelineGrouping.groups(invocations: [owned], stages: run.stages)

        #expect(groups.map(\.label) == ["Implementation"])
    }

    @Test func repeatedImplementStagesShareOneImplementationGroup() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        run.stages[0].name = "implement"
        run.stages.append(
            .init(
                id: "remediation-1-\(run.id)", run_id: run.id, name: "implement",
                attempts: [
                    .init(
                        id: "attempt-remediation-1", stage_id: "remediation-1-\(run.id)",
                        number: 1, invocation_id: "inv-remediation-1")
                ]))
        let base = Date(timeIntervalSinceReferenceDate: 1_000)
        // The earlier production attempt is re-observed after the remediation
        // attempt; the later stage still leads by attempt position.
        let production = Components.Schemas.InvocationObservation(
            invocation_id: "inv-\(run.id)-2", run_id: run.id, status: .completed, live: false, observed_at: base + 60)
        let remediation = Components.Schemas.InvocationObservation(
            invocation_id: "inv-remediation-1", run_id: run.id, status: .running, live: true, observed_at: base)

        let groups = RunTimelineGrouping.groups(invocations: [production, remediation], stages: run.stages)

        #expect(groups.map(\.label) == ["Implementation"])
        #expect(groups[0].invocations.map(\.invocation_id) == [remediation.invocation_id, production.invocation_id])
    }

    @Test func unattributedObservationsAlwaysSortLast() throws {
        let run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        let base = Date(timeIntervalSinceReferenceDate: 1_000)
        let owned = Components.Schemas.InvocationObservation(
            invocation_id: "inv-\(run.id)-2", run_id: run.id, status: .running, live: true, observed_at: base)
        // Observed after the owned attempt, yet Unattributed is always last.
        let strayLater = Components.Schemas.InvocationObservation(
            invocation_id: "inv-zzz", run_id: run.id, status: .gone, live: false, observed_at: base + 100)
        let strayEarlier = Components.Schemas.InvocationObservation(
            invocation_id: "inv-aaa", run_id: run.id, status: .gone, live: false, observed_at: base + 50)

        let groups = RunTimelineGrouping.groups(
            invocations: [owned, strayLater, strayEarlier], stages: run.stages)

        #expect(groups.map(\.label) == ["Implementation", RunTimelineGrouping.unattributedLabel])
        // Within Unattributed, rows order by invocation_id, not observation.
        #expect(groups[1].invocations.map(\.invocation_id) == ["inv-aaa", "inv-zzz"])
    }

    @Test func refreshedOldAttemptsKeepAttemptOrder() throws {
        let run = RunFixtures.refreshedHistoryRun().run
        let timeline = RunFixtures.refreshedHistoryTimeline()

        let groups = RunTimelineGrouping.groups(
            invocations: timeline.invocations, stages: run.stages,
            reviewRounds: timeline.review?.value1.rounds ?? [], milestones: timeline.milestones)
        let implementation = try #require(groups.first { $0.label == "Implementation" })

        // The completed remediation attempt leads, then the gone attempt 2,
        // then the failed attempt 1, even though the two early attempts carry
        // the newest observed_at.
        #expect(
            implementation.invocations.map(\.invocation_id) == [
                "inv-\(RunFixtures.refreshedRunID)-remediation-1",
                "inv-\(RunFixtures.refreshedRunID)-2",
                "inv-\(RunFixtures.refreshedRunID)-1",
            ])
    }

    @Test func reviewGroupPlacementFollowsStartNotObservation() throws {
        let run = RunFixtures.refreshedHistoryRun().run
        let timeline = RunFixtures.refreshedHistoryTimeline()
        let rounds = try #require(timeline.review?.value1.rounds)
        let newestImplementationStart = try #require(
            timeline.milestones.first {
                $0.kind == .invocation_started
                    && $0.invocation_id == "inv-\(RunFixtures.refreshedRunID)-remediation-1"
            }
        ).recorded_at

        func leadingLabel(reviewRequestedAt: Date, observedShift: TimeInterval) -> String {
            var shiftedRounds = rounds
            shiftedRounds[0].requested_at = reviewRequestedAt
            let shiftedInvocations = timeline.invocations.map {
                invocation -> Components.Schemas.InvocationObservation in
                var moved = invocation
                moved.observed_at = moved.observed_at.addingTimeInterval(observedShift)
                return moved
            }
            return RunTimelineGrouping.groups(
                invocations: shiftedInvocations, stages: run.stages,
                reviewRounds: shiftedRounds, milestones: timeline.milestones
            ).first?.label ?? ""
        }

        // Review requested after the newest implementation start leads;
        // requested before it, Implementation leads.
        #expect(
            leadingLabel(reviewRequestedAt: newestImplementationStart.addingTimeInterval(60), observedShift: 0)
                == "Review")
        #expect(
            leadingLabel(reviewRequestedAt: newestImplementationStart.addingTimeInterval(-60), observedShift: 0)
                == "Implementation")
        // Moving every observed_at far forward changes neither leader.
        #expect(
            leadingLabel(reviewRequestedAt: newestImplementationStart.addingTimeInterval(60), observedShift: 100_000)
                == "Review")
        #expect(
            leadingLabel(reviewRequestedAt: newestImplementationStart.addingTimeInterval(-60), observedShift: 100_000)
                == "Implementation")
    }

    @Test func groupsWithoutAStartFactSortAfterThoseWithOne() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        run.stages[0].name = "implement"
        let base = Date(timeIntervalSinceReferenceDate: 1_000)
        let owned = Components.Schemas.InvocationObservation(
            invocation_id: "inv-\(run.id)-2", run_id: run.id, status: .completed, live: false, observed_at: base)
        let reviewObservation = Components.Schemas.InvocationObservation(
            invocation_id: "review-\(run.id)-1", run_id: run.id, status: .completed, live: false,
            observed_at: base + 1_000)
        // A review round with no requested_at has no start fact.
        var round = RunFixtures.reviewRound(.completed)
        round.invocation_id = "review-\(run.id)-1"
        round.requested_at = nil
        // The implementation attempt has an invocation_started start fact.
        let started = Components.Schemas.RunMilestone(
            run_id: run.id, kind: .invocation_started, invocation_id: owned.invocation_id, recorded_at: base)

        let groups = RunTimelineGrouping.groups(
            invocations: [reviewObservation, owned], stages: run.stages,
            reviewRounds: [round], milestones: [started])

        #expect(groups.map(\.label) == ["Implementation", "Review"])
    }

    @Test func movingObservedTimeNeverReordersHistory() {
        let run = RunFixtures.refreshedHistoryRun().run
        let timeline = RunFixtures.refreshedHistoryTimeline()
        func order(shift: TimeInterval) -> [[String]] {
            let shifted = timeline.invocations.map { invocation -> Components.Schemas.InvocationObservation in
                var moved = invocation
                moved.observed_at = moved.observed_at.addingTimeInterval(shift)
                return moved
            }
            return RunTimelineGrouping.groups(
                invocations: shifted, stages: run.stages,
                reviewRounds: timeline.review?.value1.rounds ?? [], milestones: timeline.milestones
            ).map { $0.invocations.map(\.invocation_id) }
        }

        #expect(order(shift: 0) == order(shift: 500_000))
        #expect(order(shift: 0) == order(shift: -500_000))
    }

    @Test func staleGoneObservationShowsAnObservationGap() {
        let now = Date(timeIntervalSinceReferenceDate: 1_000)
        let observation = Components.Schemas.InvocationObservation(
            invocation_id: "invocation-1",
            run_id: "run-1",
            status: .gone,
            live: false,
            observed_at: now.addingTimeInterval(-31)
        )

        let presentation = InvocationPresentation(observation, asOf: now)

        #expect(presentation.label == "Observation gap")
        #expect(presentation.symbol == "exclamationmark.triangle")
    }

    @Test func daemonSnapshotClockKeepsAHealthyObservationLive() {
        let daemonNow = Date(timeIntervalSinceReferenceDate: 1_000)
        let observation = Components.Schemas.InvocationObservation(
            invocation_id: "invocation-1",
            run_id: "run-1",
            status: .running,
            live: true,
            observed_at: daemonNow.addingTimeInterval(-29)
        )

        let presentation = InvocationPresentation(observation, asOf: daemonNow)

        #expect(presentation.label == "Running")
        #expect(presentation.symbol == "wave.3.right.circle.fill")
    }

    @Test func specificationCampaignLabelsItsSourceSpecification() {
        var specification = RunFixtures.defaultRuns()[0].run
        specification.stages[0].name = "specification"

        #expect(RunDisplay.specificationLabel(specification) == "Source specification")
    }

    @Test func productionLaneKeepsApprovedSpecificationAfterLaterStages() {
        var active = RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }!.run
        var ready = RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.readyRunID }!.run
        active.stages.insert(
            .init(id: "stage-active-implement", run_id: active.id, name: "implement", attempts: []),
            at: 0)
        ready.stages.insert(
            .init(id: "stage-ready-implement", run_id: ready.id, name: "implement", attempts: []),
            at: 0)

        #expect(RunDisplay.specificationLabel(active) == "Approved specification")
        #expect(RunDisplay.specificationLabel(ready) == "Approved specification")
    }

    @Test func historyEntriesLeadWithTheNewestMilestoneMarkedCurrent() throws {
        let timeline = try #require(
            RunFixtures.defaultTimelines().first { $0.run_id == RunFixtures.activeRunID })
        let milestones = timeline.milestones
        #expect(milestones.count == 6)

        let entries = RunHistoryPresentation.entries(
            milestones: milestones, detail: { _ in nil }, context: { _ in nil })

        #expect(entries.count == milestones.count)
        #expect(entries.first?.state == .current)
        #expect(entries.dropFirst().allSatisfy { $0.state == .completed })
        #expect(entries.map(\.title) == milestones.reversed().map { RunDisplay.label($0.kind) })
    }

    @Test func equalTimestampMilestonesKeepReverseDaemonOrder() {
        let stamp = Date(timeIntervalSinceReferenceDate: 5_000)
        let earlier = Components.Schemas.RunMilestone(
            run_id: "run-1", kind: .invocation_admitted, invocation_id: "inv-a", recorded_at: stamp)
        let later = Components.Schemas.RunMilestone(
            run_id: "run-1", kind: .invocation_started, invocation_id: "inv-b", recorded_at: stamp)

        // Reversal is index-based, so equal timestamps never depend on a
        // sort's stability: the later-recorded milestone leads every run.
        for _ in 0..<8 {
            let entries = RunHistoryPresentation.entries(
                milestones: [earlier, later], detail: { _ in nil }, context: { $0 })
            #expect(entries.map(\.context) == ["inv-b", "inv-a"])
            #expect(entries.first?.state == .current)
        }
    }

    @Test func reorderingPreservesEachEntrysFields() throws {
        let timeline = try #require(
            RunFixtures.defaultTimelines().first { $0.run_id == RunFixtures.activeRunID })
        let milestones = timeline.milestones
        let detail: (Components.Schemas.RunMilestone) -> String? = { "detail-\($0.kind.rawValue)" }
        let context: (String?) -> String? = { $0.map { "ctx-\($0)" } }

        let entries = RunHistoryPresentation.entries(
            milestones: milestones, detail: detail, context: context)

        let oldestFirst = milestones.enumerated().map { index, milestone in
            DecisionStageRailPresentation.Entry(
                id: "\(index)-\(milestone.kind.rawValue)-\(milestone.recorded_at.timeIntervalSince1970)",
                title: RunDisplay.label(milestone.kind),
                detail: detail(milestone),
                context: context(milestone.invocation_id),
                timestamp: milestone.recorded_at.formatted(date: .abbreviated, time: .shortened),
                state: index == milestones.count - 1 ? .current : .completed)
        }
        #expect(entries == Array(oldestFirst.reversed()))
    }

    @Test func reviewRoundsLeadWithTheHighestRound() {
        let facts = Components.Schemas.RunReviewFacts(rounds: [
            RunFixtures.reviewRound(.completed, round: 1, findings: true),
            RunFixtures.reviewRound(.running, round: 2),
        ])
        #expect(RunHistoryPresentation.rounds(facts).map(\.round) == [2, 1])
        #expect(RunHistoryPresentation.rounds(Components.Schemas.RunReviewFacts(rounds: [])).isEmpty)
        #expect(RunHistoryPresentation.rounds(nil).isEmpty)
    }

    @Test func timelineRequestKeyChangesOnBootstrapAndEpochRotation() throws {
        let snapshot = try #require(
            RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID })
        let cursors = SyncCursors(
            syncEpoch: "epoch-1", lastFullSnapshotRevision: 10,
            highestObservedServerRevision: 10)
        let key = RunTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: cursors)

        // Same cursors and snapshot: an adopted-nothing heartbeat leaves the
        // key unchanged, so the view does not refetch on every beat.
        #expect(key == RunTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: cursors))

        // A same-epoch bootstrap advances the full-snapshot revision.
        let afterBootstrap = SyncCursors(
            syncEpoch: "epoch-1", lastFullSnapshotRevision: 11,
            highestObservedServerRevision: 11)
        #expect(
            key != RunTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: afterBootstrap))

        // An epoch rotation changes the key even at an equal revision.
        let afterEpoch = SyncCursors(
            syncEpoch: "epoch-2", lastFullSnapshotRevision: 10,
            highestObservedServerRevision: 10)
        #expect(key != RunTimelineView.TimelineRequestKey(snapshot: snapshot, cursors: afterEpoch))
    }

    private func stage(
        _ name: String, id: String, attempts: [(number: Int, invocation: String)]
    ) -> Components.Schemas.Stage {
        .init(
            id: id, run_id: "run", name: name,
            attempts: attempts.map {
                .init(id: "\(id)-\($0.number)", stage_id: id, number: $0.number, invocation_id: $0.invocation)
            })
    }

    @Test func singleStageKeepsThePlainRoundLabel() {
        let stages = [stage("implement", id: "s1", attempts: [(1, "inv-1"), (2, "inv-2")])]
        #expect(
            RunHistoryPresentation.attemptContext(invocationID: "inv-1", stages: stages, reviewRounds: [])
                == "Implementation · Round 1")
        #expect(
            RunHistoryPresentation.attemptContext(invocationID: "inv-2", stages: stages, reviewRounds: [])
                == "Implementation · Round 2")
    }

    @Test func repeatedStagesQualifyEachLabelWithItsPass() {
        let run = RunFixtures.refreshedHistoryRun().run
        let id = RunFixtures.refreshedRunID
        func label(_ invocation: String) -> String? {
            RunHistoryPresentation.attemptContext(invocationID: invocation, stages: run.stages, reviewRounds: [])
        }
        #expect(label("inv-\(id)-1") == "Implementation · Pass 1 · Round 1")
        #expect(label("inv-\(id)-2") == "Implementation · Pass 1 · Round 2")
        #expect(label("inv-\(id)-remediation-1") == "Implementation · Pass 2 · Round 1")
    }

    @Test func passCountingFollowsTheCanonicalStageName() {
        // `implement` folds into `implementation`, so these are passes 1 and 2
        // of one series; the differently named stage between them isn't counted.
        let stages = [
            stage("implement", id: "s1", attempts: [(1, "inv-a")]),
            stage("audit", id: "s2", attempts: [(1, "inv-mid")]),
            stage("implementation", id: "s3", attempts: [(1, "inv-b")]),
        ]
        #expect(
            RunHistoryPresentation.attemptContext(invocationID: "inv-a", stages: stages, reviewRounds: [])
                == "Implementation · Pass 1 · Round 1")
        #expect(
            RunHistoryPresentation.attemptContext(invocationID: "inv-b", stages: stages, reviewRounds: [])
                == "Implementation · Pass 2 · Round 1")
        #expect(
            RunHistoryPresentation.attemptContext(invocationID: "inv-mid", stages: stages, reviewRounds: [])
                == "Audit · Round 1")
    }

    @Test func reviewRoundLabelWinsOverStages() {
        var round = RunFixtures.reviewRound(.completed, round: 3)
        round.invocation_id = "inv-review"
        // Two implement stages would otherwise pass-qualify the label, and a
        // stage even references the review invocation; the review round wins.
        let stages = [
            stage("implement", id: "s1", attempts: [(7, "inv-review")]),
            stage("implement", id: "s2", attempts: [(1, "inv-x")]),
        ]
        #expect(
            RunHistoryPresentation.attemptContext(
                invocationID: "inv-review", stages: stages, reviewRounds: [round]) == "Review · Round 3")
    }

    @Test func unmatchedInvocationYieldsNil() {
        let stages = [stage("implement", id: "s1", attempts: [(1, "inv-1")])]
        #expect(
            RunHistoryPresentation.attemptContext(invocationID: "inv-unknown", stages: stages, reviewRounds: [])
                == nil)
        #expect(RunHistoryPresentation.attemptContext(invocationID: nil, stages: stages, reviewRounds: []) == nil)
    }

    @Test func historyRailEntriesCarryThePassQualifiedContext() {
        let run = RunFixtures.refreshedHistoryRun().run
        let timeline = RunFixtures.refreshedHistoryTimeline()
        let context: (String?) -> String? = {
            RunHistoryPresentation.attemptContext(
                invocationID: $0, stages: run.stages, reviewRounds: timeline.review?.value1.rounds ?? [])
        }
        let entries = RunHistoryPresentation.entries(
            milestones: timeline.milestones, detail: { _ in nil }, context: context)

        // Each remediation-invocation milestone (admitted, started, terminal)
        // carries the Pass 2 context; attempt 2's two milestones carry Pass 1,
        // Round 2; and attempt 1's four milestones carry Pass 1, Round 1.
        func count(_ label: String) -> Int { entries.filter { $0.context == label }.count }
        #expect(count("Implementation · Pass 2 · Round 1") == 3)
        #expect(count("Implementation · Pass 1 · Round 2") == 2)
        #expect(count("Implementation · Pass 1 · Round 1") == 4)
    }
}
