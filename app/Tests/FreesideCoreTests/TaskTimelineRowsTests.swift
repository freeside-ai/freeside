import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// The task's event rail as rows: which daemon events it keeps, how each
/// reads, and where a stage start is drawn.
@Suite struct TaskTimelineRowsTests {
    private typealias Row = TaskTimelinePresentation.Row

    /// A row as one line: `>` for a stage start, `!` for a failure, and a
    /// trailing `*` on the rows that read as the head of the rail.
    private func lines(_ timeline: Components.Schemas.TaskTimeline) -> [String] {
        TaskTimelinePresentation.rows(timeline).map { row in
            let mark: String
            switch row.kind {
            case .event: mark = ""
            case .failure: mark = "! "
            case .stageStart: mark = "> "
            }
            return mark + row.label + (row.isNewest ? " *" : "")
        }
    }

    private let specificationPhase = [
        "Specification approved",
        "Attempt 1 completed",
        "Attempt 1 started",
        "> Specification started",
        "Task started",
        "Task created",
    ]

    @Test func aSingleCampaignReadsThroughToVerification() {
        let timeline = TaskStageFixtures.history(.verification)
        // The attempt's result is recorded after the review of its work
        // and does not read as a return to implementation. Verification
        // shows through its result entries and has no start point.
        #expect(
            lines(timeline)
                == [
                    "Attempt 1 completed *",
                    "Publication Blocked · Recorded hold: Verification findings block publication"
                        + " (verification_findings)",
                    "Verification recorded · Degraded",
                    "Round 1 completed · Clean",
                    "Round 1 requested",
                    "> Review started",
                    "Attempt 1 started",
                    "> Implementation started",
                ] + specificationPhase)
        #expect(TaskTimelinePresentation.newestStage(TaskTimelinePresentation.rows(timeline)) == .review)
    }

    @Test func aRetryInsideImplementationDrawsNoSecondStart() {
        #expect(
            lines(TaskStageFixtures.history(.retry))
                == [
                    "Attempt 2 started *",
                    "! Attempt 1 failed",
                    "Attempt 1 started",
                    "> Implementation started",
                ] + specificationPhase)
    }

    @Test func aReturnToImplementationAfterReviewDrawsANewStart() {
        // The review round's own invocation result is left out, and the
        // attempt's second work result names its pass.
        #expect(
            lines(TaskStageFixtures.history(.returned))
                == [
                    "Attempt 2 started *",
                    "> Implementation started *",
                    "Attempt 1, pass 2 completed",
                    "Attempt 1 completed",
                    "Round 2 completed · Findings",
                    "Round 2 requested",
                    "! Round 1 failed",
                    "Round 1 requested",
                    "> Review started",
                    "Attempt 1 started",
                    "> Implementation started",
                ] + specificationPhase)
    }

    @Test func aSecondCampaignStartsItsSpecificationAgain() {
        #expect(
            lines(TaskStageFixtures.history(.secondCampaign))
                == [
                    "Attempt 1 started *",
                    "> Specification started *",
                    "Attempt 1 started",
                    "> Implementation started",
                ] + specificationPhase)
        // A campaign that never left Specification is still followed by a
        // new start point: the campaign changed, though the stage did not.
        var script = TaskStageFixtures.Script()
        script.campaign("first", specification: "spec-1")
        script.submit("spec-1", role: .specification, attempt: 1, campaign: "first")
        script.wait()
        script.campaign("second", specification: "spec-2")
        script.submit("spec-2", role: .specification, attempt: 1, campaign: "second")
        #expect(
            lines(script.timeline) == [
                "Attempt 1 started *", "> Specification started *", "Attempt 1 started", "> Specification started",
            ])
    }

    @Test func aRunWithNoRoleKeepsItsLabelsAndDrawsNoStart() {
        // Its review is still a stage the events name.
        #expect(
            lines(TaskStageFixtures.history(.noRole)) == [
                "Round 1 requested *",
                "> Review started *",
                "Invocation Ended · Completed",
                "Run Submitted",
                "Task started",
                "Task created",
            ])
    }

    @Test func aStartPointDrawnFromTheNewestEventReadsAsNewestToo() throws {
        for (scenario, stage) in [
            (TaskStageFixtures.Scenario.specification, Components.Schemas.StageName.specification),
            (.implementation, .implementation), (.review, .review),
        ] {
            let rows = TaskTimelinePresentation.rows(TaskStageFixtures.history(scenario))
            #expect(rows[0].kind == .event)
            #expect(rows[1].kind == .stageStart(stage))
            #expect(rows.map(\.isNewest) == [true, true] + Array(repeating: false, count: rows.count - 2))
            // The start point takes the time of the event it is drawn from.
            #expect(rows[1].recordedAt == rows[0].recordedAt)
            #expect(TaskTimelinePresentation.newestStage(rows) == stage)
        }
        let retry = TaskTimelinePresentation.rows(TaskStageFixtures.history(.retry))
        #expect(retry.map(\.isNewest).filter { $0 }.count == 1)
        #expect(TaskTimelinePresentation.newestStage(retry) == .implementation)
        #expect(TaskTimelinePresentation.newestStage(TaskTimelinePresentation.rows(history([]))) == nil)
    }

    @Test func rowsKeepTheDaemonsOrderAtOneInstant() {
        // Every event shares one instant, so only the order received can
        // place them, start points included.
        var script = TaskStageFixtures.Script()
        script.event(.task_created)
        script.approve("campaign", specification: "spec", implementation: "implementation")
        script.submit("implementation", role: .implementation, attempt: 1, campaign: "campaign")
        var timeline = script.timeline
        for index in timeline.events.indices { timeline.events[index].recorded_at = RunFixtures.screenshotInstant }
        #expect(
            lines(timeline) == [
                "Attempt 1 started *", "> Implementation started *", "Specification approved", "Task created",
            ])
    }

    @Test func failuresAndPullRequestsReadInPlainWords() {
        var script = TaskStageFixtures.Script()
        script.submit("run", role: .implementation, attempt: 3, campaign: "campaign")
        script.wait()
        script.event(.pr_opened, run: "run", pr: 110)
        script.wait()
        script.event(.stop_requested)
        script.wait()
        script.event(.stop_failed)
        script.wait()
        script.event(.pr_merged, run: "run", pr: 110)
        let rows = TaskTimelinePresentation.rows(script.timeline)
        #expect(
            lines(script.timeline) == [
                "PR merged · #110 *", "! Stop failed", "Stop requested", "PR opened · #110", "Attempt 3 started",
                "> Implementation started",
            ])
        // No row carries a run, campaign, or invocation id.
        for row in rows {
            #expect(!row.label.contains("campaign"))
            #expect(!row.label.contains("run"))
            #expect(!row.label.contains("invocation"))
        }
    }

    @Test func aSavedTimelineWithOnlyCreationRecoversItsNestedEvents() {
        // The recovery path for an older saved projection still feeds the
        // rail: the section's and run's events, newest first.
        let legacy = TaskHistoryFixtures.history(.legacy)
        var saved = legacy
        saved.sections[0].runs[0].events = legacy.events.filter { $0.kind != .task_created }
        saved.events = legacy.events.filter { $0.kind == .task_created }
        #expect(lines(saved) == lines(legacy))
        #expect(lines(saved).first == "Round 1 completed · Clean *")
    }

    private func history(_ events: [Components.Schemas.TaskEvent]) -> Components.Schemas.TaskTimeline {
        .init(
            as_of_revision: 1, as_of: RunFixtures.screenshotInstant, task_id: "task", project_id: "freeside",
            name: .init(text: "Task", source: ._operator), events: events, sections: [])
    }
}
