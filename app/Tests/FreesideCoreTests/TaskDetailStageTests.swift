import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

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
        // The run's publication position and the rail's newest start both
        // say Verification.
        let verification = TaskStageFixtures.page(.verification)
        let rows = TaskTimelinePresentation.rows(TaskStageFixtures.history(.verification))
        #expect(TaskTimelinePresentation.newestStage(rows) == .verification)
        #expect(stage(verification) == .verification)
        // With the run not synced, the publication position is unknown and
        // the start point stands.
        var runless = verification
        runless.runs = []
        #expect(stage(runless) == .verification)
        #expect(stage(TaskProgressFixtures.make("No position")) == nil)
    }

    /// An admitted external review sends a published task back to review
    /// (#2350). Through each window the task row's stage and round and the
    /// header's chip name the rail's newest start: published, after the
    /// ready card was withdrawn but before round 2 was requested (with the
    /// run's newest milestone still its publication, as the iPhone held it,
    /// or a later one, as the Mac did), and after the request.
    @Test func theRowAndHeaderAgreeThroughAnExternalReviewReEntry() throws {
        func page(
            _ history: Components.Schemas.TaskTimeline, outcome: Components.Schemas.RunOutcome,
            milestone: Components.Schemas.RunMilestoneKind
        ) -> TaskProgressFixtures.Case {
            var page = TaskStageFixtures.page(history)
            page.runs[0].run.outcome = outcome
            page.runs[0].run.latest_milestone = .init(value1: milestone)
            return page
        }
        let windows: [(String, TaskProgressFixtures.Case, Stage, String?)] = [
            (
                "published",
                page(TaskHistoryFixtures.history(.published), outcome: .published, milestone: .publication_ready),
                .verification, nil
            ),
            (
                "withdrawn, publication newest",
                page(
                    TaskHistoryFixtures.externallyReviewed(requested: false), outcome: .pending,
                    milestone: .publication_ready),
                .review, "Round 2"
            ),
            (
                "withdrawn, a later milestone",
                page(
                    TaskHistoryFixtures.externallyReviewed(requested: false), outcome: .pending,
                    milestone: .invocation_started),
                .review, "Round 2"
            ),
            (
                "round 2 requested",
                page(
                    TaskHistoryFixtures.externallyReviewed(requested: true), outcome: .pending,
                    milestone: .invocation_started),
                .review, "Round 2"
            ),
        ]
        for (name, page, expected, round) in windows {
            let history = try #require(page.history)
            let rail = TaskTimelinePresentation.newestStage(TaskTimelinePresentation.rows(history))
            let current = TaskDisplay.currentStage(page.task, runs: page.runs, history: history)
            let position = TaskDisplay.position(
                page.task, runs: page.runs, attentionItems: page.items, history: history, stage: current)
            let heading = try #require(position?.heading)
            #expect(rail == expected, "\(name)")
            #expect(stage(page) == expected, "\(name)")
            #expect(heading.label == AttentionDisplay.label(expected), "\(name)")
            // A published run keeps the round its run names; a re-entry's
            // is the round it opened.
            if let round { #expect(heading.round == round, "\(name)") }
            // The spoken rail marks the stage the heading names; a published
            // run's Verification reads completed.
            if expected == .review {
                #expect(position?.rail.entries.first { $0.state == .current }?.id == expected.rawValue, "\(name)")
            }
        }
        // Before the timeline loads, the row keeps its run's own heading.
        var unloaded = windows[2].1
        unloaded.history = nil
        #expect(TaskDisplay.currentStage(unloaded.task, runs: unloaded.runs, history: nil) == nil)
    }

    @Test func aPublicationHoldNewerThanTheRailsStartSaysVerification() {
        // The rail draws no event for a publication hold, so a hold observed
        // after round 1's request still reads as Verification.
        var page = TaskStageFixtures.page(.review)
        page.runs[0].run.hold_reason = .init(value1: .verification_findings)
        let requested = page.history?.events.first?.recorded_at ?? .distantPast
        // The page's current run is its history's newest.
        page.history?.sections[0].runs[0].hold = .init(
            value1: .init(
                run_id: page.runs[0].run.id, reason: .verification_findings,
                first_observed_at: requested.addingTimeInterval(60), last_observed_at: requested.addingTimeInterval(60))
        )
        #expect(stage(page) == .verification)
        #expect(TaskDisplay.currentStage(page.task, runs: page.runs, history: page.history)?.stage == .verification)
    }

    @Test func aHoldOutsidePublicationLeavesAReEntrysReview() throws {
        // In attended mode the re-entered cycle's publication waits on a
        // hold observed after the re-entry; it is not a publication hold.
        var page = TaskStageFixtures.page(TaskHistoryFixtures.externallyReviewed(requested: false))
        page.runs[0].run.outcome = .pending
        page.runs[0].run.latest_milestone = .init(value1: .publication_ready)
        page.runs[0].run.hold_reason = .init(value1: .attended_mode_active)
        let reentered = try #require(page.history?.events.first?.recorded_at)
        page.history?.sections[0].runs[0].hold = .init(
            value1: .init(
                run_id: page.runs[0].run.id, reason: .attended_mode_active,
                first_observed_at: reentered.addingTimeInterval(60), last_observed_at: reentered.addingTimeInterval(60))
        )
        #expect(stage(page) == .review)
        #expect(
            TaskDisplay.currentStage(page.task, runs: page.runs, history: page.history)
                == .init(stage: .review, round: "Round 2"))
    }

    @Test func aRoundRequestedAfterPublicationSaysReviewWithoutExternalReview() throws {
        // A readiness re-entry opens round 2 while the run's newest
        // milestone is still its publication.
        var history = TaskHistoryFixtures.history(.published)
        let published = try #require(history.events.first?.recorded_at)
        let head = String(repeating: "a", count: 40)
        history.events.insert(
            .init(
                review: .init(
                    value1: .init(
                        invocation_id: "review-history-implementation-1-2", round: 2, head_sha: head, base_sha: head)),
                kind: .review_requested, recorded_at: published.addingTimeInterval(60),
                run_id: "history-implementation-1"),
            at: 0)
        var page = TaskStageFixtures.page(history)
        page.runs[0].run.outcome = .pending
        page.runs[0].run.latest_milestone = .init(value1: .publication_ready)
        let current = TaskDisplay.currentStage(page.task, runs: page.runs, history: page.history)
        #expect(current == .init(stage: .review, round: "Round 2"))
        #expect(stage(page) == .review)
        let position = TaskDisplay.position(
            page.task, runs: page.runs, attentionItems: page.items, history: page.history, stage: current)
        #expect(position?.heading?.text == "Review · Round 2")
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
        #expect(chip(held(TaskStageFixtures.page(.specification), on: .usage_pool_exhausted)) == "Queued")
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
