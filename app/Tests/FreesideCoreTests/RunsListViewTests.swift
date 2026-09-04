import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct RunsListViewTests {
    @Test func campaignAttemptLabelCarriesExactIdentity() {
        let run = RunFixtures.defaultRuns().first { $0.run.id == RunFixtures.activeRunID }!.run

        #expect(
            RunDisplay.campaign(run)
                == "Campaign campaign-freeside-acceptance · Attempt 2")
        #expect(run.attempt_reason == "Retry after repairing the acceptance rig")
        #expect(run.parent_run_id == "run-freeside-656")
    }

    /// A completed run (#1134) carries the widened contract without any new
    /// rendering: the outcome labels quietly, the lifecycle is finished, and
    /// the summary carries the completion facts and the spend figure.
    @Test func completedRunCarriesCompletionLifecycleAndSpend() {
        let run = RunFixtures.completedRun().run

        #expect(run.outcome == .completed)
        #expect(run.lifecycle == .finished)
        #expect(run.superseded_by == nil)
        #expect(run.completion?.value1.pr_number == 105)
        #expect(run.completion?.value1.bound_issue == 80)
        #expect(run.billable_cost_so_far?.value1.amount == "23.75")
        #expect(RunDisplay.label(Components.Schemas.RunOutcome.completed) == "Merged")
        #expect(RunDisplay.secondaryLine(run) == .milestone("Work Unit Completed"))
        for snapshot in RunFixtures.defaultRuns() {
            let finished = snapshot.run.lifecycle == .finished
            let terminal = [.failed, .lost, .unobserved].contains(snapshot.run.outcome)
            #expect(finished == terminal, "\(snapshot.run.id) lifecycle mirrors its outcome")
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

        #expect(RunDisplay.primaryLine(active) == "freeside · Implementation · Round 2")
        #expect(RunDisplay.secondaryLine(active) == .hold("Verification Findings"))
        #expect(RunDisplay.secondaryLine(ready) == .milestone("Publication Ready"))
        #expect(RunDisplay.secondaryLine(legacy) == .milestone("No milestone recorded"))
    }

    @Test func ordersByActivityFallbackAndID() {
        let first = Date(timeIntervalSinceReferenceDate: 100)
        let second = Date(timeIntervalSinceReferenceDate: 200)
        var runs = RunFixtures.defaultRuns()
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
}
