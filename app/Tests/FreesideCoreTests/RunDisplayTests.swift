import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct RunDisplayTests {
    @Test func labelsFallBackWithoutDisplayNames() {
        let now = RunFixtures.screenshotInstant
        var run = RunFixtures.defaultRuns()[0].run
        run.created_at = nil
        run.last_activity_at = nil
        run.display_names = nil
        #expect(RunDisplay.title(run) == "Verification · Round 1")
        #expect(RunDisplay.metaLine(run, now: now) == "freeside")
        run.display_names = .init(
            value1: .init(
                project: .init(text: "Project name", source: .name), task: .init(text: "#12", source: .name)))
        #expect(RunDisplay.metaLine(run, now: now) == "Project name · #12")
        run.display_names?.value1.project.text = ""
        run.display_names?.value1.task.text = ""
        #expect(RunDisplay.metaLine(run, now: now) == "freeside")
    }

    @Test func titleFallsBackToTheProjectWithoutStages() {
        var run = RunFixtures.defaultRuns()[0].run
        run.stages = []
        #expect(RunDisplay.title(run) == "freeside")
    }

    /// The observed #1184 case: a run submitted on day 1 whose last
    /// observation lands on day 2 sorts above a run submitted later on day 2
    /// with an earlier last observation. That is the comparator working as
    /// designed, so the order is pinned.
    @Test func lateObservationLiftsAnEarlierStartedRunAndTheCardsShowIt() {
        let now = MetaLineClock.now
        var runs = MetaLineClock.threeRuns()
        runs[0].run.created_at = MetaLineClock.instant(day: 4, hour: 14, minute: 25)
        runs[0].run.last_activity_at = MetaLineClock.instant(day: 6, hour: 3, minute: 1)
        runs[1].run.created_at = MetaLineClock.instant(day: 6, hour: 2, minute: 26)
        runs[1].run.last_activity_at = MetaLineClock.instant(day: 6, hour: 2, minute: 49)
        runs[2].run.created_at = MetaLineClock.instant(day: 4, hour: 14, minute: 35)
        runs[2].run.last_activity_at = MetaLineClock.instant(day: 4, hour: 15, minute: 14)

        let ordered = RunDisplay.sortedRuns(runs.reversed())

        #expect(ordered.map(\.run.id) == runs.map(\.run.id))
        let activity = ordered.compactMap(\.run.last_activity_at)
        #expect(activity.count == runs.count)
        #expect(activity == activity.sorted(by: >))
        // Each card names the instant that decided its place, so the order
        // reads off the list rather than only out of the comparator.
        let shown = ordered.map { RunDisplay.metaLine($0.run, now: now) }
        #expect(Set(shown).count == shown.count)
    }

    @Test func lastActiveIsRelativeUnderADayAndDatedFromADayOn() throws {
        let now = MetaLineClock.now
        func shown(_ secondsAgo: TimeInterval) throws -> String {
            var run = try MetaLineClock.run()
            run.last_activity_at = now.addingTimeInterval(-secondsAgo)
            return RunDisplay.metaLine(run, now: now)
        }

        #expect(try shown(30 * 60) == "freeside · \(RunFixtures.retryTaskName.text) · last active 30m ago")
        #expect(try shown(86_400 - 60) == "freeside · \(RunFixtures.retryTaskName.text) · last active 23h ago")
        // The dated form's text is locale-dependent, so assert its shape.
        let dated = try shown(86_400)
        #expect(dated.hasPrefix("freeside · \(RunFixtures.retryTaskName.text) · last active "))
        #expect(!dated.hasSuffix(" ago"))
    }

    @Test func datedCardsSeparateTheSameClockTimeOnDifferentDays() throws {
        let now = MetaLineClock.now
        var earlier = try MetaLineClock.run()
        earlier.last_activity_at = MetaLineClock.instant(day: 4, hour: 15, minute: 14)
        var later = try MetaLineClock.run()
        later.last_activity_at = MetaLineClock.instant(day: 5, hour: 15, minute: 14)

        #expect(RunDisplay.metaLine(earlier, now: now) != RunDisplay.metaLine(later, now: now))
    }

    @Test func anUnobservedRunShowsNoTimeSegment() throws {
        let now = MetaLineClock.now
        var run = try MetaLineClock.run()
        run.last_activity_at = nil

        #expect(RunDisplay.metaLine(run, now: now) == "freeside · \(RunFixtures.retryTaskName.text)")
        #expect(RunDisplay.exactActivityTimestamp(run) == nil)
    }

    @Test func hoverCarriesTheExactActivityInstant() throws {
        var run = try MetaLineClock.run()
        let activity = MetaLineClock.instant(day: 6, hour: 3, minute: 1)
        run.last_activity_at = activity

        #expect(RunDisplay.exactActivityTimestamp(run) == activity.formatted(.iso8601))
    }

    /// One fixed clock and UTC instants for the meta-line and ordering
    /// tests, so neither the wall clock nor the host time zone can move
    /// a case across the 24-hour boundary.
    private enum MetaLineClock {
        /// Two days past the newest instant these tests use, so every
        /// fixed-date case lands on the dated side of the 24-hour boundary
        /// and the relative cases set their own offsets from here.
        static let now = instant(day: 8, hour: 14, minute: 0)

        /// 2026-09-01T00:00:00Z, offset arithmetically so the instants stay
        /// UTC whatever the host time zone is.
        private static let september = Date(timeIntervalSince1970: 1_788_220_800)

        static func instant(day: Int, hour: Int, minute: Int) -> Date {
            september.addingTimeInterval(
                TimeInterval((day - 1) * 86_400 + hour * 3_600 + minute * 60))
        }

        static func run() throws -> Components.Schemas.Run {
            try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID })
                .run
        }

        /// Three distinctly identified snapshots to order.
        static func threeRuns() -> [Components.Schemas.RunSnapshot] {
            (0..<3).map { index in
                var snapshot = RunFixtures.defaultRuns()[0]
                snapshot.run.id = "run-\(index)"
                return snapshot
            }
        }
    }

    @Test func stageRailFollowsExistingStagesAndOutcome() throws {
        let runs = RunFixtures.defaultRuns()
        func rail(_ id: String) throws -> [(String, DecisionStageRailPresentation.State)] {
            let run = try #require(runs.first { $0.run.id == id }).run
            return RunDisplay.stageRail(run).entries.map { ($0.title, $0.state) }
        }
        let known = ["Specification", "Implementation", "Review", "Verification"]

        let active = try rail(RunFixtures.activeRunID)
        #expect(active.map(\.0) == known)
        #expect(active.map(\.1) == [.pending, .completed, .completed, .current])

        let ready = try rail(RunFixtures.readyRunID)
        #expect(ready.map(\.0) == known)
        #expect(ready.map(\.1) == [.pending, .completed, .completed, .completed])
        #expect(
            RunDisplay.title(try #require(runs.first { $0.run.id == RunFixtures.readyRunID }).run)
                == "Verification · Round 1")

        let failed = try rail("run-oriole-121")
        #expect(failed.map(\.1) == [.pending, .pending, .pending, .failed])

        let completed = try rail(RunFixtures.completedRunID)
        #expect(completed.map(\.0) == known)
        #expect(completed.map(\.1) == [.pending, .completed, .completed, .completed])
        #expect(RunDisplay.title(RunFixtures.completedRun().run) == "Verification · Round 1")

        let legacy = try rail(RunFixtures.legacyRunID)
        #expect(legacy.map(\.1) == [.pending, .pending, .pending, .pending])
    }

    @Test func daemonShapedImplementStageIsTheImplementationStage() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        run.stages = run.stages.map { stage in
            var stage = stage
            stage.name = "implement"
            return stage
        }

        #expect(RunDisplay.title(run) == "Verification · Round 1")
        let rail = RunDisplay.stageRail(run)
        #expect(rail.entries.map(\.title) == ["Specification", "Implementation", "Review", "Verification"])
        #expect(rail.entries.map(\.state) == [.pending, .completed, .completed, .current])
    }

    @Test func stageRailMarksEarlierStagesCompletedAndSummarizesEveryDot() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        run.stages.insert(
            .init(id: "stage-spec", run_id: run.id, name: "specification", attempts: []), at: 0)
        let rail = RunDisplay.stageRail(run)

        #expect(rail.entries.map(\.state) == [.completed, .completed, .completed, .current])
        #expect(
            rail.summary
                == "Specification completed, Implementation completed, Review completed, Verification current")
    }

    @Test(arguments: [
        Components.Schemas.RunHoldReason.verification_findings, .trust_blocked, .base_advanced,
        .recipe_revoked, .scope_conflict, .publication_environment, .external_conflict,
    ])
    func publicationCycleHoldsAdvanceToVerification(hold: Components.Schemas.RunHoldReason) {
        var run = RunFixtures.defaultRuns()[0].run
        run.hold_reason = .init(value1: hold)

        #expect(RunDisplay.title(run) == "Verification · Round 1")
        #expect(RunDisplay.stageRail(run).entries.map(\.state) == [.pending, .completed, .completed, .current])
    }

    @Test(arguments: [
        Components.Schemas.RunHoldReason.operation_stopped, .admission_policy_refused,
        .input_unavailable, .identity_parallelism,
    ])
    func preImplementationHoldsKeepImplementationCurrent(hold: Components.Schemas.RunHoldReason) {
        var run = RunFixtures.defaultRuns()[0].run
        run.hold_reason = .init(value1: hold)

        #expect(RunDisplay.title(run) == "Implementation · Round 2")
        #expect(RunDisplay.stageRail(run).entries.map(\.state) == [.pending, .current, .pending, .pending])
    }

    @Test func publicationSignalsTakePrecedenceOverEarlierHolds() {
        let cases:
            [(
                Components.Schemas.RunOutcome, Components.Schemas.RunMilestoneKind,
                DecisionStageRailPresentation.State
            )] = [
                (.completed, .invocation_started, .completed),
                (.published, .publication_blocked, .completed),
                (.pending, .work_unit_completed, .completed),
                (.pending, .publication_ready, .completed),
                (.failed, .publication_ready, .completed),
                (.pending, .publication_blocked, .current),
                (.blocked, .invocation_started, .current),
            ]
        for (outcome, milestone, state) in cases {
            var run = RunFixtures.defaultRuns()[0].run
            run.hold_reason = .init(value1: .admission_policy_refused)
            run.outcome = outcome
            run.latest_milestone = .init(value1: milestone)

            #expect(RunDisplay.title(run) == "Verification · Round 1")
            #expect(RunDisplay.stageRail(run).entries.map(\.state) == [.pending, .completed, .completed, state])
        }
    }

    @Test func verificationRoundsCountImplementationPassesInsteadOfAttempts() {
        var run = RunFixtures.refreshedHistoryRun().run
        run.stages[0].name = "implementation"
        run.stages.insert(.init(id: "spec", run_id: run.id, name: "specification", attempts: []), at: 0)

        #expect(RunDisplay.title(run) == "Verification · Round 2")
        run.latest_milestone = .init(value1: .invocation_started)
        #expect(RunDisplay.title(run) == "Implementation · Round 1")
        run.stages[run.stages.count - 1].attempts = []
        #expect(RunDisplay.title(run) == "Implementation")
    }

    @Test func nonProductionStagesKeepTheirRecordedPosition() {
        var run = RunFixtures.defaultRuns()[0].run
        run.stages[0].name = "publication"

        #expect(RunDisplay.workflowPhase(run) == nil)
        #expect(RunDisplay.title(run) == "Publication · Round 2")
        #expect(RunDisplay.stageRail(run).entries.map(\.state) == [.pending, .pending, .pending, .pending, .current])
        let specification = RunFixtures.handedOffSpecificationRun().run
        #expect(RunDisplay.workflowPhase(specification) == nil)
        #expect(RunDisplay.title(specification) == "Specification · Round 1")
    }

    @Test func attemptIdentityResolvesSuccessorAndSuppressesStaleHold() throws {
        let runs = RunFixtures.defaultRuns()
        let active = try #require(runs.first { $0.run.id == RunFixtures.activeRunID }).run
        var parent = try #require(runs.first { $0.run.id == "run-freeside-656" }).run
        #expect(RunDisplay.identityLine(active, runs: runs) == "Attempt 2")
        #expect(RunDisplay.identityLine(parent, runs: runs) == "Attempt 1 · superseded by attempt 2")
        #expect(RunDisplay.identityLine(parent, runs: []) == "Attempt 1 · superseded by run-freeside-657")
        parent.hold_reason = .init(value1: .verification_findings)
        #expect(RunDisplay.secondaryLine(parent, runs: runs) == .supersession("Superseded by attempt 2"))
        #expect(RunDisplay.label(parent.outcome) == "Failed")
        var noCampaign = active
        noCampaign.campaign_id = nil
        noCampaign.attempt_number = nil
        #expect(RunDisplay.identityLine(noCampaign, runs: runs) == nil)
    }

    @Test func specificationHandoffUsesSourceFactsWithOrWithoutSuccessor() {
        let run = RunFixtures.handedOffSpecificationRun().run
        let runs = RunFixtures.defaultRuns()
        #expect(
            RunDisplay.identityLine(run, runs: runs)
                == "Attempt 1 · handed off to implementation (attempt 1)")
        #expect(
            RunDisplay.secondaryLine(run, runs: runs)
                == .supersession("Handed off to implementation (attempt 1)"))
        #expect(
            RunDisplay.identityLine(run, runs: [])
                == "Attempt 1 · handed off to implementation (run-freeside-654)")
        #expect(
            RunDisplay.secondaryLine(run)
                == .supersession("Handed off to implementation (run-freeside-654)"))
        let rail = RunDisplay.stageRail(run)
        #expect(rail.entries.map(\.state) == [.completed, .pending, .pending, .pending])
        #expect(rail.summary == "Specification completed, Implementation pending, Review pending, Verification pending")
    }

    @Test func handoffRequiresBoundCampaignSpecificationShape() {
        let bound = RunFixtures.handedOffSpecificationRun().run
        var unbound = bound
        unbound.superseded_by = nil
        unbound.lifecycle = .active
        #expect(RunDisplay.identityLine(unbound, runs: []) == "Attempt 1")
        #expect(RunDisplay.secondaryLine(unbound) == .milestone("Run Submitted"))
        #expect(RunDisplay.stageRail(unbound).entries.first?.state == .current)

        var noCampaign = bound
        noCampaign.campaign_id = nil
        var noAttempt = bound
        noAttempt.attempt_number = nil
        var mixed = bound
        mixed.stages.append(.init(id: "implement", run_id: bound.id, name: "implement", attempts: []))
        var noStages = bound
        noStages.stages = []
        var active = bound
        active.lifecycle = .active
        var implementation = bound
        implementation.stages[0].name = "implement"
        for run in [noCampaign, noAttempt, mixed, noStages, active, implementation] {
            #expect(
                RunDisplay.secondaryLine(run, runs: RunFixtures.defaultRuns())
                    == .supersession("Superseded by attempt 1"))
            #expect(!(RunDisplay.identityLine(run, runs: []) ?? "").contains("handed off"))
        }
        // An equal-number successor on an implementation is still not a handoff.
        #expect(
            RunDisplay.identityLine(implementation, runs: RunFixtures.defaultRuns())
                == "Attempt 1 · superseded by attempt 1")
    }

    @Test func stageRailRespectsLifecycleWithoutInventingSuccess() {
        let outcomes: [(Components.Schemas.RunOutcome, DecisionStageRailPresentation.State)] = [
            (.pending, .pending), (.blocked, .pending), (.unobserved, .pending),
            (.failed, .failed), (.lost, .failed), (.completed, .completed), (.published, .completed),
        ]
        for (outcome, finishedState) in outcomes {
            for specification in [false, true] {
                var run =
                    specification
                    ? RunFixtures.handedOffSpecificationRun().run : RunFixtures.defaultRuns()[0].run
                run.lifecycle = .finished
                run.superseded_by = "successor"
                run.outcome = outcome
                let expected: DecisionStageRailPresentation.State =
                    specification && outcome == .pending ? .completed : finishedState
                let rail = RunDisplay.stageRail(run)
                let expectedStates: [DecisionStageRailPresentation.State] =
                    specification
                    ? [expected, .pending, .pending, .pending] : [.pending, .completed, .completed, expected]
                #expect(rail.entries.map(\.state) == expectedStates)
                #expect(!rail.entries.contains { $0.state == .current })
                #expect(!rail.summary.contains("current"))

                run.lifecycle = .active
                run.superseded_by = nil
                let activeState: DecisionStageRailPresentation.State =
                    outcome == .pending || outcome == .blocked ? .current : finishedState
                let activeStates: [DecisionStageRailPresentation.State] =
                    specification
                    ? [activeState, .pending, .pending, .pending] : [.pending, .completed, .completed, activeState]
                #expect(RunDisplay.stageRail(run).entries.map(\.state) == activeStates)
            }
        }
    }

    @Test func spendUsesAttentionCardWording() throws {
        let active = RunFixtures.defaultRuns()[0].run
        let cost = try #require(active.billable_cost_so_far?.value1)
        #expect(RunDisplay.spendLine(active) == "USD 8.5 across 1 invocation, still accruing")
        #expect(RunDisplay.spendLine(active) == AttentionDisplay.costSoFar(cost))
        #expect(RunDisplay.spendLine(RunFixtures.completedRun().run) == "USD 23.75 across 2 invocations")
        var absent = active
        absent.billable_cost_so_far = nil
        #expect(RunDisplay.spendLine(absent) == nil)
    }

    @Test func outcomeVocabularyStaysUnchanged() {
        let cases: [(Components.Schemas.RunOutcome, String)] = [
            (.pending, "In progress"), (.published, "Ready"), (.completed, "Merged"),
            (.failed, "Failed"), (.lost, "Lost"), (.blocked, "Blocked"), (.unobserved, "Not observed"),
        ]
        for (outcome, label) in cases { #expect(RunDisplay.label(outcome) == label) }
    }
}
