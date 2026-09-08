import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

#if os(macOS)
    import AppKit
    import Observation
    import SwiftUI
#endif

@Suite struct RunsListViewTests {
    @Test func campaignAttemptKeepsExactIdentity() {
        let run = RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }!.run

        #expect(
            run.campaign_id
                == "campaign-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
        #expect(run.attempt_number == 2)
        let predecessor = RunFixtures.defaultRuns().first { $0.run.id == "run-freeside-656" }!.run
        #expect(predecessor.campaign_id == run.campaign_id)
        #expect(run.attempt_reason == "Retry after repairing the acceptance rig")
        #expect(run.parent_run_id == "run-freeside-656")
    }

    @Test func completedRunCarriesCompletionLifecycleAndSpend() {
        let run = RunFixtures.completedRun().run

        #expect(run.outcome == .completed)
        #expect(run.lifecycle == .finished)
        #expect(run.superseded_by == nil)
        #expect(run.completion?.value1.pr_number == 105)
        #expect(run.completion?.value1.bound_issue == 80)
        #expect(run.billable_cost_so_far?.value1.amount == "23.75")
        #expect(RunDisplay.label(Components.Schemas.RunOutcome.completed) == "Merged")
        #expect(RunDisplay.secondaryLine(run) == .completion("Merged PR #105"))
        for snapshot in RunFixtures.defaultRuns() {
            let finished = snapshot.run.lifecycle == .finished
            let terminal =
                [.completed, .failed, .lost, .unobserved].contains(snapshot.run.outcome)
                || snapshot.run.superseded_by != nil
            #expect(finished == terminal, "\(snapshot.run.id) lifecycle mirrors the daemon partition")
        }
    }

    @Test func rowLinesSeparateIdentityFromTheCurrentHold() {
        let active = RunFixtures.defaultRuns().first {
            $0.run.id == RunFixtures.activeRunID
        }!.run
        let ready = RunFixtures.defaultRuns().first {
            $0.run.id == RunFixtures.readyRunID
        }!.run
        let legacy = RunFixtures.defaultRuns().first {
            $0.run.id == RunFixtures.legacyRunID
        }!.run

        let now = RunFixtures.screenshotInstant
        #expect(RunDisplay.title(active) == "Implementation · Round 2")
        #expect(RunDisplay.metaLine(active, now: now).hasPrefix("freeside · #724 · last active "))
        #expect(RunDisplay.metaLine(legacy, now: now) == "freeside")
        #expect(RunDisplay.secondaryLine(active) == .hold("Verification Findings"))
        #expect(RunDisplay.secondaryLine(ready) == .milestone("Publication Ready"))
        #expect(RunDisplay.secondaryLine(legacy) == .milestone("No milestone recorded"))
    }

    @Test func ordersByActivityFallbackAndID() {
        let first = Date(timeIntervalSinceReferenceDate: 100)
        let second = Date(timeIntervalSinceReferenceDate: 200)
        var runs = RunFixtures.defaultRuns().filter {
            [RunFixtures.activeRunID, RunFixtures.readyRunID, "run-oriole-121", RunFixtures.legacyRunID]
                .contains($0.run.id)
        }
        for index in runs.indices {
            switch runs[index].run.id {
            case RunFixtures.activeRunID:
                runs[index].run.created_at = first
                runs[index].run.last_activity_at = second
            case RunFixtures.readyRunID:
                runs[index].run.created_at = first
                runs[index].run.last_activity_at = nil
            case "run-oriole-121":
                runs[index].run.created_at = first
                runs[index].run.last_activity_at = second
            default:
                runs[index].run.created_at = nil
                runs[index].run.last_activity_at = nil
            }
        }

        let ordered = RunDisplay.sortedRuns(runs.reversed()).map(\.run.id)

        #expect(
            ordered == [
                RunFixtures.activeRunID,
                "run-oriole-121",
                RunFixtures.readyRunID,
                RunFixtures.legacyRunID,
            ])
    }

    @Test func scopesUseLifecycleAndPreserveActivityOrder() {
        var runs = RunFixtures.defaultRuns()
        var superseded = runs[0]
        superseded.run.id = "superseded-pending"
        superseded.run.lifecycle = .finished
        superseded.run.superseded_by = RunFixtures.activeRunID
        var blocked = runs[0]
        blocked.run.id = "blocked"
        blocked.run.outcome = .blocked
        var lost = runs[0]
        lost.run.id = "lost"
        lost.run.outcome = .lost
        lost.run.lifecycle = .finished
        runs += [superseded, blocked, lost]

        let active = RunListFilter().rows(in: runs.reversed())
        let finished = RunListFilter(scope: .finished).rows(in: runs.reversed())

        #expect(
            active.map(\.run.id)
                == RunDisplay.sortedRuns(runs.filter { $0.run.lifecycle == .active }).map(\.run.id))
        #expect(
            finished.map(\.run.id)
                == RunDisplay.sortedRuns(runs.filter { $0.run.lifecycle == .finished }).map(\.run.id))
        #expect(active.contains { $0.run.outcome == .published })
        #expect(active.contains { $0.run.outcome == .blocked })
        #expect(finished.contains { $0.run.id == superseded.run.id })
        #expect(RunListFilter().scope == .active)
        #expect(
            RunListFilter(scope: .all).rows(in: runs.reversed()).map(\.run.id)
                == RunDisplay.sortedRuns(runs).map(\.run.id))
    }

    @Test func scopeCountsMatchRowsWithinTheProjectFilter() {
        let runs = RunFixtures.defaultRuns()
        for project in [nil, "freeside", "oriole", "missing"] as [String?] {
            for scope in RunListFilter.Scope.allCases {
                let filter = RunListFilter(scope: scope, projectID: project)
                let rows = filter.rows(in: runs)
                #expect(rows.allSatisfy { project == nil || $0.run.project_id == project })
                #expect(filter.count(in: runs, scope: scope) == rows.count)
                #expect(
                    filter.count(in: runs, scope: .all)
                        == filter.count(in: runs, scope: .active) + filter.count(in: runs, scope: .finished))
                #expect(filter.rows(in: []).isEmpty)
                #expect(filter.count(in: [], scope: scope) == 0)
            }
        }
        let finishedProject = RunListFilter(scope: .active, projectID: "oriole")
        #expect(finishedProject.rows(in: runs).isEmpty)
        #expect(finishedProject.count(in: runs, scope: .finished) > 0)
    }

    @Test func explicitRunLinksRevealTheirScopeAndProject() {
        let finished = RunFixtures.completedRun().run
        let active = RunFixtures.defaultRuns()[0].run
        var filter = RunListFilter(projectID: "oriole")
        filter.reveal(finished)
        #expect(filter.scope == .finished)
        #expect(filter.projectID == nil)
        #expect(filter.rows(in: [RunFixtures.completedRun()]).count == 1)

        filter.reveal(active)
        #expect(filter.scope == .active)
        filter = RunListFilter(scope: .all, projectID: active.project_id)
        filter.reveal(active)
        #expect(filter.scope == .all)
        #expect(filter.projectID == active.project_id)
    }

    @Test @MainActor func changingScopeRepairsNavigationAgainstVisibleRows() {
        let runs = RunFixtures.defaultRuns()
        let path = [RunFixtures.completedRunID]
        let activeIDs = Set(RunListFilter().rows(in: runs).map(\.run.id))
        let finishedIDs = Set(RunListFilter(scope: .finished).rows(in: runs).map(\.run.id))
        #expect(NavigationModel.repairedPath(path, availableIDs: activeIDs).isEmpty)
        #expect(NavigationModel.repairedPath(path, availableIDs: finishedIDs) == path)
    }

    #if os(macOS)
        @Test @MainActor func finishedSelectionSurvivesColdSnapshotLoad() throws {
            let state = RunListProbeState(runs: [], selection: RunFixtures.completedRunID)
            let host = NSHostingView(rootView: RunListProbe(state: state))
            host.setFrameSize(NSSize(width: 400, height: 600))
            host.layoutSubtreeIfNeeded()
            try #require(pumpUntil { state.appeared })

            state.runs = RunFixtures.defaultRuns()
            try #require(pumpUntil { state.updates > 0 })
            #expect(state.selection == RunFixtures.completedRunID)
            withExtendedLifetime(host) {}
        }

        @Test @MainActor func lifecycleUpdateKeepsTheOpenDetailSelected() throws {
            let state = RunListProbeState(
                runs: RunFixtures.defaultRuns(), selection: RunFixtures.activeRunID)
            let host = NSHostingView(rootView: RunListProbe(state: state))
            host.setFrameSize(NSSize(width: 400, height: 600))
            host.layoutSubtreeIfNeeded()
            try #require(pumpUntil { state.appeared })

            let index = try #require(state.runs.firstIndex { $0.run.id == RunFixtures.activeRunID })
            state.runs[index].run.lifecycle = .finished
            try #require(pumpUntil { state.updates > 0 })
            #expect(state.selection == RunFixtures.activeRunID)
            withExtendedLifetime(host) {}
        }

        @MainActor private func pumpUntil(_ condition: () -> Bool) -> Bool {
            let deadline = Date().addingTimeInterval(2)
            while !condition(), Date() < deadline {
                RunLoop.main.run(until: Date().addingTimeInterval(0.01))
            }
            return condition()
        }
    #endif

    @Test func labelsFallBackWithoutDisplayNames() {
        let now = RunFixtures.screenshotInstant
        var run = RunFixtures.defaultRuns()[0].run
        run.created_at = nil
        run.last_activity_at = nil
        run.display_names = nil
        #expect(RunDisplay.title(run) == "Implementation · Round 2")
        #expect(RunDisplay.metaLine(run, now: now) == "freeside")
        run.display_names = .init(
            value1: .init(
                project: .init(text: "Project name", source: .name), work_unit: .init(text: "#12", source: .name)))
        #expect(RunDisplay.metaLine(run, now: now) == "Project name · #12")
        run.display_names?.value1.project.text = ""
        run.display_names?.value1.work_unit.text = ""
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

        #expect(try shown(30 * 60) == "freeside · #724 · last active 30m ago")
        #expect(try shown(86_400 - 60) == "freeside · #724 · last active 23h ago")
        // The dated form's text is locale-dependent, so assert its shape.
        let dated = try shown(86_400)
        #expect(dated.hasPrefix("freeside · #724 · last active "))
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

        #expect(RunDisplay.metaLine(run, now: now) == "freeside · #724")
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
        #expect(active.map(\.1) == [.pending, .current, .pending, .pending])

        let ready = try rail(RunFixtures.readyRunID)
        #expect(ready.map(\.0) == known + ["Publication"])
        #expect(ready.map(\.1) == [.pending, .pending, .pending, .pending, .completed])

        let failed = try rail("run-oriole-121")
        #expect(failed.map(\.1) == [.pending, .pending, .pending, .failed])

        let completed = try rail(RunFixtures.completedRunID)
        #expect(completed.map(\.0) == known + ["Publication"])
        #expect(completed.map(\.1) == [.pending, .pending, .pending, .pending, .completed])

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

        #expect(RunDisplay.title(run) == "Implementation · Round 2")
        let rail = RunDisplay.stageRail(run)
        #expect(rail.entries.map(\.title) == ["Specification", "Implementation", "Review", "Verification"])
        #expect(rail.entries.map(\.state) == [.pending, .current, .pending, .pending])
    }

    @Test func stageRailMarksEarlierStagesCompletedAndSummarizesEveryDot() throws {
        var run = try #require(RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }).run
        run.stages.insert(
            .init(id: "stage-spec", run_id: run.id, name: "specification", attempts: []), at: 0)
        let rail = RunDisplay.stageRail(run)

        #expect(rail.entries.map(\.state) == [.completed, .current, .pending, .pending])
        #expect(
            rail.summary
                == "Specification completed, Implementation current, Review pending, Verification pending")
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
                let index = specification ? 0 : 1
                #expect(rail.entries[index].state == expected)
                #expect(!rail.entries.contains { $0.state == .current })
                #expect(!rail.summary.contains("current"))
                #expect(rail.entries.enumerated().allSatisfy { $0.offset == index || $0.element.state == .pending })

                run.lifecycle = .active
                run.superseded_by = nil
                let activeState: DecisionStageRailPresentation.State =
                    outcome == .pending || outcome == .blocked ? .current : finishedState
                #expect(RunDisplay.stageRail(run).entries[index].state == activeState)
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

#if os(macOS)
    @MainActor @Observable private final class RunListProbeState {
        var runs: [Components.Schemas.RunSnapshot]
        var selection: String?
        var appeared = false
        var updates = 0

        init(runs: [Components.Schemas.RunSnapshot], selection: String?) {
            self.runs = runs
            self.selection = selection
        }
    }

    @MainActor private struct RunListProbe: View {
        @Bindable var state: RunListProbeState

        var body: some View {
            RunsListView(runs: state.runs, schedules: [], selection: $state.selection)
                .onAppear { state.appeared = true }
                .onChange(of: state.runs.map(\.run.id)) { state.updates += 1 }
                .onChange(of: state.runs.map(\.run.lifecycle)) { state.updates += 1 }
        }
    }
#endif
