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
        #expect(RunDisplay.title(active) == "Verification · Round 1")
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
