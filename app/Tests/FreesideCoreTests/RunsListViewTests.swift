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
