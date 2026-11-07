import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

/// The task's event rail as rows: which daemon events it keeps, how each
/// reads, and where a stage start is drawn.
@Suite struct TaskPageRowsTests {
    private typealias Row = TimelineRail.Row

    /// A row as one line: `>` for a stage start, `!` for a failure, and a
    /// trailing `*` on the rows that read as the head of the rail.
    private func lines(
        _ timeline: Components.Schemas.TaskTimeline,
        reviewRounds: [String: [Components.Schemas.RunReviewRound]] = [:]
    ) -> [String] {
        TaskTimelinePresentation.rows(timeline, reviewRounds: reviewRounds).map { row in
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
        "> Specification started · Attempt 1",
        "Task started",
        "Task created",
    ]

    @Test func aSingleCampaignReadsThroughToVerification() {
        let timeline = TaskStageFixtures.history(.verification)
        // The attempt's result is recorded after the review of its work
        // and does not read as a return to implementation. Verification
        // starts under its first recorded result.
        #expect(
            lines(timeline)
                == [
                    "Attempt 1 completed *",
                    "Publication Blocked · Recorded hold: Verification findings block publication"
                        + " (verification_findings)",
                    "> Verification started · Verification recorded · Degraded",
                    "Round 1 completed · Clean",
                    "> Review started · Round 1 requested",
                    "> Implementation started · Attempt 1",
                ] + specificationPhase)
        #expect(TaskTimelinePresentation.newestStage(TaskTimelinePresentation.rows(timeline)) == .verification)
    }

    @Test func aPublishedTaskStartsVerificationUnderItsVerificationRecord() {
        let timeline = TaskHistoryFixtures.history(.published)
        // The PR and publication readiness sit above the start, in
        // Verification, not as rows under Review's.
        #expect(
            lines(timeline) == [
                "PR opened · #110 *",
                "Publication Ready",
                "> Verification started · Verification recorded · Clean",
                "Round 1 completed · Clean",
                "> Review started · Round 1 requested",
                "> Implementation started · Attempt 1",
                "Specification approved",
                "> Specification started · Attempt 1",
                "Task started",
                "Task created",
            ])
        #expect(TaskTimelinePresentation.newestStage(TaskTimelinePresentation.rows(timeline)) == .verification)
    }

    @Test func aPublicationMilestoneWithNoVerificationRecordStartsVerification() {
        // A legacy record carries no verification detail, so the
        // publication milestone is the first event at Verification.
        var script = TaskStageFixtures.script(.review)
        script.wait(9)
        script.review(TaskStageFixtures.implementationRun, round: 1, .review_completed, outcome: .clean)
        script.wait(6)
        script.milestone(TaskStageFixtures.implementationRun, .publication_ready)
        #expect(
            Array(lines(script.timeline).prefix(3)) == [
                "> Verification started · Publication Ready *", "Round 1 completed · Clean",
                "> Review started · Round 1 requested",
            ])
    }

    @Test func aSecondVerificationResultDrawsNoSecondStart() {
        // A reevaluation records another result while the task already
        // stands at Verification.
        var script = TaskStageFixtures.script(.review)
        script.wait(9)
        script.review(TaskStageFixtures.implementationRun, round: 1, .review_completed, outcome: .clean)
        script.wait(6)
        script.verify(TaskStageFixtures.implementationRun, clean: true)
        script.wait()
        script.verify(TaskStageFixtures.implementationRun, clean: false)
        #expect(
            lines(script.timeline)
                == [
                    "Verification recorded · Degraded *",
                    "> Verification started · Verification recorded · Clean",
                    "Round 1 completed · Clean",
                    "> Review started · Round 1 requested",
                    "> Implementation started · Attempt 1",
                ] + specificationPhase)
    }

    @Test func aRetryInsideImplementationDrawsNoSecondStart() {
        #expect(
            lines(TaskStageFixtures.history(.retry))
                == [
                    "Attempt 2 started *",
                    "! Attempt 1 failed",
                    "> Implementation started · Attempt 1",
                ] + specificationPhase)
    }

    @Test func aReturnToImplementationAfterReviewDrawsANewStart() {
        // The review round's own invocation result is left out, and the
        // attempt's second work result names its pass.
        #expect(
            lines(TaskStageFixtures.history(.returned))
                == [
                    "> Implementation started · Attempt 2 *",
                    "Attempt 1, pass 2 completed",
                    "Attempt 1 completed",
                    "Round 2 completed · Findings",
                    "Round 2 requested",
                    "! Round 1 failed",
                    "> Review started · Round 1 requested",
                    "> Implementation started · Attempt 1",
                ] + specificationPhase)
    }

    @Test func aSecondCampaignStartsItsSpecificationAgain() {
        #expect(
            lines(TaskStageFixtures.history(.secondCampaign))
                == [
                    "> Specification started · Attempt 1 *",
                    "> Implementation started · Attempt 1",
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
                "> Specification started · Attempt 1 *", "> Specification started · Attempt 1",
            ])
    }

    private let firstReview = [
        "Round 1 completed · Findings",
        "> Review started · Round 1 requested",
        "> Implementation started · Attempt 1",
    ]

    private func lines(_ script: TaskStageFixtures.Script) -> [String] {
        lines(script.timeline, reviewRounds: script.reviewRounds)
    }

    @Test func aRemediationReadsAsAReturnToImplementation() {
        var script = TaskStageFixtures.script(.remediation)
        // The decision sits under the remediator's start, at its own time.
        #expect(
            lines(script)
                == [
                    "> Implementation started · Remediation · Round 1 *",
                    "Round 1 findings decided · 2 to remediation · 1 declined",
                ] + firstReview + specificationPhase)
        let rows = TaskTimelinePresentation.rows(script.timeline, reviewRounds: script.reviewRounds)
        #expect(TaskTimelinePresentation.newestStage(rows) == .implementation)
        #expect(
            rows[1].recordedAt
                == script.reviewRounds[TaskStageFixtures.implementationRun]?.first?.remediation?.value1.decided_at)
        // The remediator's result is the remediation's, not a second pass
        // of the attempt, and the re-review returns to Review.
        script.wait(20)
        script.milestone(
            TaskStageFixtures.implementationRun, .terminal_recorded, invocation: TaskStageFixtures.remediator,
            terminal: .completed)
        script.wait(2)
        script.review(TaskStageFixtures.implementationRun, round: 2, .review_requested)
        #expect(
            lines(script)
                == [
                    "> Review started · Round 2 requested *",
                    "Remediation completed",
                    "> Implementation started · Remediation · Round 1",
                    "Round 1 findings decided · 2 to remediation · 1 declined",
                ] + firstReview + specificationPhase)
        // With no review facts loaded the rail reads as it did before them:
        // the remediator's start is machinery and its result a work result.
        #expect(
            lines(script.timeline)
                == ["Round 2 requested *", "Attempt 1 completed"] + firstReview + specificationPhase)
    }

    @Test func aFailedRemediationReadsInWax() {
        var script = TaskStageFixtures.script(.remediation)
        script.wait(20)
        script.milestone(
            TaskStageFixtures.implementationRun, .terminal_recorded, invocation: TaskStageFixtures.remediator,
            terminal: .failed)
        #expect(
            lines(script).prefix(2) == [
                "! Remediation failed *", "> Implementation started · Remediation · Round 1",
            ])
    }

    @Test func aDecisionLeadsTheRailUntilTheRemediatorStarts() {
        var script = TaskStageFixtures.script(.review)
        script.wait(9)
        script.review(TaskStageFixtures.implementationRun, round: 1, .review_completed, outcome: .findings)
        script.wait(4)
        script.decide(
            TaskStageFixtures.implementationRun, round: 1, remediate: 3, declined: 0,
            remediator: TaskStageFixtures.remediator)
        // No declined count when nothing was declined, and no stage start:
        // the task is still in Review until the remediator starts.
        #expect(lines(script) == ["Round 1 findings decided · 3 to remediation *"] + firstReview + specificationPhase)
        let rows = TaskTimelinePresentation.rows(script.timeline, reviewRounds: script.reviewRounds)
        #expect(TaskTimelinePresentation.newestStage(rows) == .review)
    }

    @Test func aDecisionWhoseRemediatorNeverStartsKeepsItsPlace() {
        var script = TaskStageFixtures.script(.review)
        script.wait(9)
        script.review(TaskStageFixtures.implementationRun, round: 1, .review_completed, outcome: .findings)
        script.wait(4)
        script.decide(
            TaskStageFixtures.implementationRun, round: 1, remediate: 3, declined: 0,
            remediator: TaskStageFixtures.remediator)
        script.wait()
        script.event(.stop_requested)
        // The stop is newer than the decision, so it leads the rail.
        #expect(
            lines(script)
                == ["Stop requested *", "Round 1 findings decided · 3 to remediation"] + firstReview
                + specificationPhase)
    }

    @Test func aRemediatorThatStartsTwiceDrawsOneStart() {
        var script = TaskStageFixtures.script(.remediation)
        let once = lines(script)
        script.wait()
        script.milestone(
            TaskStageFixtures.implementationRun, .invocation_started, invocation: TaskStageFixtures.remediator)
        #expect(lines(script) == once)
    }

    @Test func aSecondRemediationInOneRunReadsAsItsOwnReturn() {
        let run = TaskStageFixtures.implementationRun
        let second = "\(run)-remediation-2"
        var script = TaskStageFixtures.script(.remediation)
        script.wait(20)
        script.milestone(run, .terminal_recorded, invocation: TaskStageFixtures.remediator, terminal: .completed)
        script.wait(2)
        script.review(run, round: 2, .review_requested)
        script.wait(9)
        script.review(run, round: 2, .review_completed, outcome: .findings)
        script.wait(3)
        script.decide(run, round: 2, remediate: 1, declined: 0, remediator: second)
        script.wait(2)
        script.milestone(run, .invocation_started, invocation: second)
        #expect(
            lines(script)
                == [
                    "> Implementation started · Remediation · Round 2 *",
                    "Round 2 findings decided · 1 to remediation",
                    "Round 2 completed · Findings",
                    "> Review started · Round 2 requested",
                    "Remediation completed",
                    "> Implementation started · Remediation · Round 1",
                    "Round 1 findings decided · 2 to remediation · 1 declined",
                ] + firstReview + specificationPhase)
    }

    @Test func anAllDeclinedRoundAddsNoRows() {
        var script = TaskStageFixtures.script(.review)
        script.wait(9)
        script.review(TaskStageFixtures.implementationRun, round: 1, .review_completed, outcome: .findings)
        script.wait(4)
        script.decide(TaskStageFixtures.implementationRun, round: 1, remediate: 0, declined: 3)
        #expect(lines(script) == lines(script.timeline))
        #expect(lines(script) == ["Round 1 completed · Findings *"] + firstReview.dropFirst() + specificationPhase)
    }

    @Test func aSecondCampaignsRemediationIsDrawnInItsOwnCampaign() {
        var script = TaskStageFixtures.Script()
        for (campaign, round) in [("first", 1), ("second", 1)] {
            script.campaign(campaign, specification: "\(campaign)-spec")
            script.submit("\(campaign)-spec", role: .specification, attempt: 1, campaign: campaign)
            script.wait()
            script.approve(campaign, specification: "\(campaign)-spec", implementation: "\(campaign)-impl")
            script.submit("\(campaign)-impl", role: .implementation, attempt: 1, campaign: campaign)
            script.wait()
            script.review("\(campaign)-impl", round: round, .review_requested)
            script.wait()
            script.review("\(campaign)-impl", round: round, .review_completed, outcome: .findings)
            script.wait()
        }
        // The first campaign's round was declined outright; the second's
        // went to a remediator, whose start is a return to the second
        // campaign's Implementation.
        script.decide("first-impl", round: 1, remediate: 0, declined: 2)
        script.decide("second-impl", round: 1, remediate: 1, declined: 1, remediator: "second-remediation")
        script.wait()
        script.milestone("second-impl", .invocation_started, invocation: "second-remediation")
        let campaign = [
            "Round 1 completed · Findings",
            "> Review started · Round 1 requested",
            "> Implementation started · Attempt 1",
            "Specification approved",
            "> Specification started · Attempt 1",
        ]
        #expect(
            lines(script)
                == [
                    "> Implementation started · Remediation · Round 1 *",
                    "Round 1 findings decided · 1 to remediation · 1 declined",
                ] + campaign + campaign)
    }

    @Test func anExternalReviewReEntryStartsReviewUnderItsCause() {
        // The withdrawn ready card's cause names the reviewer and the pull
        // request, and starts Review before the round's request does.
        let published = lines(TaskHistoryFixtures.history(.published)).map {
            $0.replacingOccurrences(of: " *", with: "")
        }
        #expect(
            lines(TaskHistoryFixtures.externallyReviewed(requested: false))
                == ["> Review started · External review by @maintainer on #110 · Round 2 *"] + published)
        #expect(
            lines(TaskHistoryFixtures.externallyReviewed(requested: true))
                == [
                    "Round 2 requested *", "> Review started · External review by @maintainer on #110 · Round 2",
                ] + published)
        #expect(
            TaskTimelinePresentation.newestReviewRound(TaskHistoryFixtures.externallyReviewed(requested: false)) == 2)
    }

    @Test func aTaskLineNamesTheActorOnlyWhenItIsTheRouter() {
        // Only a replaced agent takes the arrow: the router's fallback never
        // ran the agent it passed over. No line event starts a stage.
        let published = lines(TaskHistoryFixtures.history(.published)).map {
            $0.replacingOccurrences(of: " *", with: "")
        }
        #expect(
            lines(TaskHistoryFixtures.linesSet())
                == [
                    "Specifying agent changed by router · claude-b → claude-a *",
                    "Reviewing agent changed · claude-a → codex",
                    "Implementing agent set by router · claude-b instead of claude-a",
                    "Specifying agent set by router · claude-b",
                    "Remediating agent set · claude-a",
                ] + published)
    }

    @Test func aTriageLineReadsAsSetByTriage() {
        let digest = "sha256:" + String(repeating: "a", count: 64)
        let escalated = Components.Schemas.TaskEventTaskLine(
            line_id: digest, role: .specifier, agent: "claude-code-specifier-escalated", version: 1,
            source: .triage)
        #expect(
            TaskTimelinePresentation.label(escalated)
                == "Specifying agent set by triage · claude-code-specifier-escalated")
    }

    @Test func aTriageEventNamesTheTierOrTheFailureAndItsReason() throws {
        // The task proceeds whatever the outcome, so the row is an event, and
        // the triager's advisory reason is its sub line.
        let rows = TaskTimelinePresentation.rows(TaskStageFixtures.history(.specification))
        let row = try #require(rows.first { $0.label.hasPrefix("Specifying tier") })
        #expect(row.label == "Specifying tier · Escalated")
        #expect(row.kind == .event)
        #expect(row.context == "Changes the API schema and its generated clients.")
        #expect(rows.filter { $0.context != nil }.count == 1)
        func label(
            _ outcome: Components.Schemas.TriageOutcome, _ tier: Components.Schemas.TriageTier? = nil
        ) -> String {
            TaskTimelinePresentation.label(
                Components.Schemas.TaskEventTriage(
                    outcome: outcome, tier: tier.map { .init(value1: $0) }, escalated: tier != .standard))
        }
        #expect(label(.answered, .standard) == "Specifying tier · Standard")
        #expect(label(.answered, .escalated) == "Specifying tier · Escalated")
        #expect(label(.call_failed) == "Specifying tier escalated · call failed")
        #expect(label(.invalid_answer) == "Specifying tier escalated · invalid answer")
        #expect(label(.budget_spent) == "Specifying tier escalated · budget spent")
        #expect(label(.no_triager_line) == "Specifying tier escalated · no triager line")
        #expect(TaskTimelinePresentation.label(.specifier_triaged) == "Specifying tier chosen")
    }

    @Test func aClearedLineReadsAsBackToTheLineup() {
        let digest = "sha256:" + String(repeating: "a", count: 64)
        let cleared = Components.Schemas.TaskEventTaskLine(
            line_id: digest, role: .reviewer, version: 2, source: .cli_line_change, superseded_agent: "codex")
        #expect(TaskTimelinePresentation.label(cleared) == "Reviewing agent back to lineup · codex")
        // The version after a clear replaces no agent, so it has no arrow.
        let again = Components.Schemas.TaskEventTaskLine(
            line_id: digest, role: .reviewer, agent: "claude-a", version: 3, source: .cli_line_change)
        #expect(TaskTimelinePresentation.label(again) == "Reviewing agent set · claude-a")
    }

    /// A version reads in the encoding it recorded: a legacy line by agent
    /// name, a selection as `enrollment · model · effort`, so a move from a
    /// name to a selection names both sides.
    @Test func aSelectionLineReadsAsItsEnrollmentModelAndEffort() {
        let digest = "sha256:" + String(repeating: "a", count: 64)
        let selection = Components.Schemas.TaskLineSelection(
            enrollment_id: "claude-a/claude_code", model_id: "opus", effort: "high")
        let changed = Components.Schemas.TaskEventTaskLine(
            line_id: digest, role: .implementer, selection: .init(value1: selection), version: 2,
            source: .client_line_change, superseded_agent: "claude-old")
        #expect(
            TaskTimelinePresentation.label(changed)
                == "Implementing agent changed · claude-old → claude-a/claude_code · opus · high")
        let cleared = Components.Schemas.TaskEventTaskLine(
            line_id: digest, role: .implementer, version: 3, source: .client_line_change,
            superseded_selection: .init(value1: selection))
        #expect(
            TaskTimelinePresentation.label(cleared)
                == "Implementing agent back to lineup · claude-a/claude_code · opus · high")
    }

    @Test func aRunWithNoRoleKeepsItsLabelsAndDrawsNoStart() {
        // Its review is still a stage the events name.
        #expect(
            lines(TaskStageFixtures.history(.noRole)) == [
                "> Review started · Round 1 requested *",
                "Invocation Ended · Completed",
                "Run Submitted",
                "Task started",
                "Task created",
            ])
    }

    @Test func aStartPointDrawnFromTheNewestEventIsThatEventsRow() throws {
        for (scenario, stage) in [
            (TaskStageFixtures.Scenario.specification, Components.Schemas.StageName.specification),
            (.implementation, .implementation), (.review, .review),
        ] {
            let timeline = TaskStageFixtures.history(scenario)
            let rows = TaskTimelinePresentation.rows(timeline)
            #expect(rows[0].kind == .stageStart(stage))
            #expect(rows.map(\.isNewest) == [true] + Array(repeating: false, count: rows.count - 1))
            // The start point is the event's own row, at its time. The
            // invocation's start after a submission is machinery.
            let source = timeline.events.first { $0.milestone?.value1.kind != .invocation_started }
            #expect(rows[0].recordedAt == source?.recorded_at)
            #expect(TaskTimelinePresentation.newestStage(rows) == stage)
        }
        let retry = TaskTimelinePresentation.rows(TaskStageFixtures.history(.retry))
        #expect(retry.map(\.isNewest).filter { $0 }.count == 1)
        #expect(TaskTimelinePresentation.newestStage(retry) == .implementation)
        #expect(TaskTimelinePresentation.newestStage(TaskTimelinePresentation.rows(history([]))) == nil)
    }

    /// Each stage start is the row of the event it is read from: the stage
    /// first, the event's words after, its time and item link, and no
    /// separate start row. Covers a run's submission, a review request, a
    /// verification record, and a remediator's start.
    @Test func eachStageStartMergesIntoTheEventItIsReadFrom() throws {
        let run = TaskStageFixtures.implementationRun
        var script = TaskStageFixtures.script(.remediation)
        script.wait(20)
        script.milestone(run, .terminal_recorded, invocation: TaskStageFixtures.remediator, terminal: .completed)
        script.wait(2)
        script.review(run, round: 2, .review_requested)
        script.wait(9)
        script.review(run, round: 2, .review_completed, outcome: .clean)
        script.wait(6)
        script.verify(run, clean: true)
        let held = TaskStageFixtures.item(.ready_for_final_review, id: "\(run)-ready", hoursOld: 0).item
        let rows = TaskTimelinePresentation.rows(script.timeline, reviewRounds: script.reviewRounds, items: [held])
        let starts = rows.filter(\.isStageStart)
        #expect(
            starts.map(\.label) == [
                "Verification started · Verification recorded · Clean",
                "Review started · Round 2 requested",
                "Implementation started · Remediation · Round 1",
                "Review started · Round 1 requested",
                "Implementation started · Attempt 1",
                "Specification started · Attempt 1",
            ])
        #expect(
            starts.map(\.kind) == [
                .stageStart(.verification), .stageStart(.review), .stageStart(.implementation),
                .stageStart(.review), .stageStart(.implementation), .stageStart(.specification),
            ])
        // No row states a start on its own, and no event row repeats one.
        let bare = ["Specification", "Implementation", "Review", "Verification"].map { "\($0) started" }
        #expect(!rows.contains { bare.contains($0.label) })
        #expect(!rows.contains { ["Round 1 requested", "Round 2 requested", "Attempt 1 started"].contains($0.label) })
        // Each start carries its event's time and its event's link.
        let events = script.timeline.events
        let verification = try #require(events.first { $0.kind == .verification_recorded })
        let request = try #require(events.first { $0.kind == .review_requested && $0.review?.value1.round == 2 })
        let remediator = try #require(
            events.first {
                $0.milestone?.value1.kind == .invocation_started
                    && $0.milestone?.value1.invocation_id == TaskStageFixtures.remediator
            })
        let submission = try #require(
            events.first { $0.milestone?.value1.kind == .run_submitted && $0.milestone?.value1.run_id == run })
        #expect(starts[0].recordedAt == verification.recorded_at)
        #expect(starts[0].itemID == "\(run)-ready")
        #expect(starts[1].recordedAt == request.recorded_at)
        #expect(starts[2].recordedAt == remediator.recorded_at)
        #expect(starts[4].recordedAt == submission.recorded_at)
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
                "> Implementation started · Attempt 1 *", "Specification approved", "Task created",
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
                "PR merged · #110 *", "! Stop failed", "Stop requested", "PR opened · #110",
                "> Implementation started · Attempt 3",
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
