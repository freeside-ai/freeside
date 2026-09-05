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
    @Test func campaignAttemptLabelCarriesExactIdentity() {
        let run = RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }!.run

        #expect(
            RunDisplay.campaign(run)
                == "Campaign campaign-freeside-acceptance · Attempt 2")
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

        #expect(RunDisplay.title(active) == "Implementation · Round 2")
        #expect(RunDisplay.metaLine(active).hasPrefix("freeside · #724 · started "))
        #expect(RunDisplay.metaLine(legacy) == "freeside")
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
        var run = RunFixtures.defaultRuns()[0].run
        run.created_at = nil
        run.display_names = nil
        #expect(RunDisplay.title(run) == "Implementation · Round 2")
        #expect(RunDisplay.metaLine(run) == "freeside")
        run.display_names = .init(
            value1: .init(
                project: .init(text: "Project name", source: .name), work_unit: .init(text: "#12", source: .name)))
        #expect(RunDisplay.metaLine(run) == "Project name · #12")
        run.display_names?.value1.project.text = ""
        run.display_names?.value1.work_unit.text = ""
        #expect(RunDisplay.metaLine(run) == "freeside")
    }

    @Test func metaLineCarriesTheStartClockTimeAndTitleFallsBackToTheProject() {
        var run = RunFixtures.defaultRuns()[0].run
        let started = Date(timeIntervalSince1970: 1_767_323_045)
        run.created_at = started
        let clock = started.formatted(date: .omitted, time: .shortened)
        #expect(RunDisplay.metaLine(run) == "freeside · #724 · started \(clock)")

        run.stages = []
        #expect(RunDisplay.title(run) == "freeside")
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
