import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// The stage the task detail page names in its header: on the status chip
/// while the task works, and on the hold callout's first line.
@Suite struct TaskDetailStageTests {
    private typealias Stage = Components.Schemas.StageName

    private func stage(
        _ page: TaskProgressFixtures.Case, reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:]
    ) -> Stage? {
        TaskDisplay.detailStage(page.task, runs: page.runs, history: page.history, reviewRounds: reviewRounds)
    }

    private func chip(
        _ page: TaskProgressFixtures.Case, reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:]
    ) -> String {
        TaskDisplay.detailStatus(
            TaskDisplay.rowLines(page.task, position: page.position).status,
            stage: stage(page, reviewRounds: reviewRounds))
    }

    private func holdCallout(_ page: TaskProgressFixtures.Case) -> TaskDisplay.HoldCallout? {
        TaskDisplay.holdCallout(page.task, position: page.position, stage: stage(page))
    }

    private func held(
        _ page: TaskProgressFixtures.Case, on reason: Components.Schemas.RunHoldReason = .input_unavailable
    ) -> TaskProgressFixtures.Case {
        var page = page
        page.runs[0].run.hold_reason = .init(value1: reason)
        page.task.current_position?.value1.hold_reason = .init(value1: reason)
        return page
    }

    /// A working task past its review: no hold, and the run's publication
    /// position says Verification.
    private var verifying: TaskProgressFixtures.Case {
        var page = TaskStageFixtures.page(.verification)
        page.runs[0].run.hold_reason = nil
        page.runs[0].run.latest_milestone = .init(value1: .publication_ready)
        return page
    }

    /// A task whose stage nothing says: no timeline, and no stage in its
    /// position.
    private var unknown: TaskProgressFixtures.Case {
        var page = TaskStageFixtures.page(.implementation)
        page.history = nil
        page.task.current_position?.value1.stage = nil
        return page
    }

    @Test func aWorkingTasksChipNamesItsStage() {
        #expect(chip(TaskStageFixtures.page(.specification)) == "In Specification")
        #expect(chip(TaskStageFixtures.page(.implementation)) == "In Implementation")
        #expect(chip(TaskStageFixtures.page(.review)) == "In Review")
        #expect(chip(verifying) == "In Verification")
        // A new attempt after review is implementation again.
        #expect(chip(TaskStageFixtures.page(.returned)) == "In Implementation")
        #expect(chip(TaskStageFixtures.page(.secondCampaign)) == "In Specification")
        // A remediation is implementation again too, once the round's
        // review facts say whose invocation it is; until then the round's
        // start is the newest the events show.
        let remediation = TaskStageFixtures.page(.remediation)
        let rounds = TaskStageFixtures.script(.remediation).reviewRounds
        #expect(chip(remediation, reviewRounds: rounds) == "In Implementation")
        #expect(chip(remediation) == "In Review")
        // The row keeps its own words: the task list is unchanged.
        let review = TaskStageFixtures.page(.review)
        #expect(TaskDisplay.rowLines(review.task, position: review.position).status == "In Progress")
    }

    @Test func anUnknownStageKeepsInProgress() {
        #expect(stage(unknown) == nil)
        #expect(chip(unknown) == "In Progress")
        // A stage name the app does not know is not guessed at.
        var unnamed = unknown
        unnamed.task.current_position?.value1.stage = "triage"
        #expect(chip(unnamed) == "In Progress")
        // A run with no role draws no start point, so only its position
        // can say, and a position with no stage leaves it unknown.
        var noRole = TaskStageFixtures.page(.noRole)
        noRole.history?.events.removeAll { $0.kind == .review_requested }
        #expect(stage(noRole) == .implementation)
        noRole.task.current_position?.value1.stage = nil
        #expect(chip(noRole) == "In Progress")
    }

    @Test func theHeaderReadsTheNewestStartPointElseThePosition() {
        // The timeline's newest start point wins over the position, which
        // reads Implementation while review rounds run.
        let review = TaskStageFixtures.page(.review)
        #expect(review.task.current_position?.value1.stage.map(RunDisplay.canonicalStageName) == "implementation")
        #expect(stage(review) == .review)
        // Until the timeline loads, the position is all there is.
        var unloaded = review
        unloaded.history = nil
        #expect(stage(unloaded) == .implementation)
        #expect(chip(unloaded) == "In Implementation")
        // Verification has no start point: the run's publication position
        // says it, over a timeline whose newest start is Review.
        let verification = TaskStageFixtures.page(.verification)
        let rows = TaskTimelinePresentation.rows(TaskStageFixtures.history(.verification))
        #expect(TaskTimelinePresentation.newestStage(rows) == .review)
        #expect(stage(verification) == .verification)
        // With the run not synced, the publication position is unknown and
        // the start point stands.
        var runless = verification
        runless.runs = []
        #expect(stage(runless) == .review)
        #expect(stage(TaskProgressFixtures.make("No position")) == nil)
    }

    @Test func everyOtherStatusKeepsItsWords() {
        for stage in Stage.allCases {
            for status in [
                "On Hold", "Queued", "Specification Approval Required", "Stopping", "Execution Failed",
                "Run Details Unavailable", "Finished · See Recorded Outcome", "Published · See Review Status",
            ] {
                #expect(TaskDisplay.detailStatus(status, stage: stage) == status)
            }
            #expect(
                TaskDisplay.detailStatus(TaskDisplay.inProgress, stage: stage)
                    == "In \(AttentionDisplay.label(stage))")
        }
        #expect(TaskDisplay.detailStatus(TaskDisplay.inProgress, stage: nil) == "In Progress")
        #expect(chip(TaskStageFixtures.page(.verification)) == "On Hold")
        #expect(chip(held(TaskStageFixtures.page(.specification), on: .identity_parallelism)) == "Queued")
        #expect(chip(TaskProgressFixtures.make("Approval required")) == "Specification Approval Required")
    }

    @Test func theCalloutLeadsWithTheStageAndCarriesNoRound() throws {
        let expected: [(TaskProgressFixtures.Case, String?)] = [
            (held(TaskStageFixtures.page(.specification)), "Specification"),
            (held(TaskStageFixtures.page(.implementation)), "Implementation"),
            (held(TaskStageFixtures.page(.review)), "Review"),
            (held(verifying), "Verification"),
            (held(unknown), nil),
        ]
        for (page, stage) in expected {
            let callout = try #require(holdCallout(page))
            #expect(callout.stage == stage)
            #expect(callout.hold == "Hold: Required input unavailable")
            #expect(!callout.hold.contains("Round"))
            #expect(callout.stage?.contains("Round") != true)
        }
        let verification = try #require(holdCallout(TaskStageFixtures.page(.verification)))
        #expect(verification == .init(stage: "Verification", hold: "Hold: Verification findings block publication"))
        // The row's own facts keep the round; the callout does not repeat it.
        let page = TaskStageFixtures.page(.verification)
        #expect(page.position?.heading?.round != nil)
        // A working task has no callout: its stage is on the chip alone.
        #expect(holdCallout(TaskStageFixtures.page(.review)) == nil)
    }
}
