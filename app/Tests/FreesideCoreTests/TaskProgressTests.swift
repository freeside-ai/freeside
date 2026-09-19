import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct TaskProgressTests {
    @Test func stateMatrixUsesExplicitOutcomesAndGuidance() {
        let expected = [
            "Queued": "Queued", "No position": "No Execution Position Recorded",
            "Approval required": "Specification Approval Required", "Implementation": "In Progress",
            "Review": "In Progress", "Verification": "On Hold",
            "Stop pending": "Stop Requested · Awaiting Confirmation",
            "Stop failed": "Failed to Stop · Execution May Continue", "Stopped": "Stopped",
            "Failed": "Execution Failed", "Ready": "Ready for Final Review",
            "Ready degraded": "Ready for Final Review (Degraded)",
            "Finished": "Finished · See Recorded Outcome", "Superseded record": "Superseded Run · Historical",
            "History unavailable": "In Progress", "Run unavailable": "Run Details Unavailable",
            "Abandoned": "Abandoned",
        ]
        for (name, status) in expected {
            let fixture = TaskProgressFixtures.make(name)
            let lines = TaskDisplay.progressLines(fixture.task, position: fixture.position)
            #expect(lines.first == status, "\(name)")
            #expect(Set(lines).count == lines.count)
            let guidance =
                name.hasPrefix("Ready")
                ? "Review the pull request from Inbox."
                : name == "Approval required"
                    ? "Review the specification in Inbox."
                    : name == "Queued"
                        ? "The configured agent account is busy. This work is queued; no action is needed for this wait."
                        : "Open task details."
            #expect(lines.last == guidance)
        }
    }

    /// The chip's cut comes from what the operator can do, not the status
    /// word: accent only where the guidance names a bound Inbox item, faint
    /// only for a superseded run or an abandoned task, ink for everything
    /// the daemon is doing or has said, a stopped or finished task included.
    @Test func chipCutFollowsInboxGuidanceAndHistory() {
        let attention: Set = ["Approval required", "Ready", "Ready degraded"]
        let faint: Set = ["Superseded record", "Abandoned"]
        let ink = [
            "Queued", "No position", "Implementation", "Review", "Verification", "Stop pending", "Stop failed",
            "Stopped", "Failed", "Finished", "History unavailable", "Run unavailable",
        ]
        for name in attention.union(faint).union(ink) {
            let fixture = TaskProgressFixtures.make(name)
            let expected: StateChip.Cut = attention.contains(name) ? .attention : faint.contains(name) ? .faint : .ink
            #expect(TaskDisplay.statusCut(fixture.task, position: fixture.position) == expected, "\(name)")
            if let position = fixture.position {
                #expect(position.attention == attention.contains(name), "\(name)")
                #expect(position.attention == position.guidance.contains("Inbox"), "\(name)")
            }
        }
    }

    /// The row draws the same strings VoiceOver reads, by slot: round and
    /// hold share a line, an Inbox sentence becomes a link title without
    /// its full stop, a capacity wait stays a sentence, and the default
    /// guidance draws nothing while staying in the spoken order.
    @Test func rowLinesKeepTheSpokenOrderAndChooseVisibleGuidance() throws {
        for name in ["Queued", "No position", "Approval required", "Verification", "Stopped", "Ready"] {
            let fixture = TaskProgressFixtures.make(name)
            let lines = TaskDisplay.rowLines(fixture.task, position: fixture.position)
            #expect(lines.all == TaskDisplay.progressLines(fixture.task, position: fixture.position), "\(name)")
            #expect(lines.all.first == lines.status && lines.all.last == lines.guidance, "\(name)")
        }
        let approval = TaskProgressFixtures.make("Approval required")
        #expect(
            TaskDisplay.rowLines(approval.task, position: approval.position).visibleGuidance(attention: true)
                == .link("Review the specification in Inbox"))
        let queued = TaskProgressFixtures.make("Queued")
        let queuedLines = TaskDisplay.rowLines(queued.task, position: queued.position)
        #expect(queuedLines.visibleGuidance(attention: false) == .sentence(queuedLines.guidance))
        let held = TaskProgressFixtures.make("Verification")
        let heldLines = TaskDisplay.rowLines(held.task, position: held.position)
        #expect(heldLines.visibleGuidance(attention: false) == nil)
        #expect(heldLines.guidance == "Open task details.")
        #expect(heldLines.facts.contains { $0.hasPrefix("Hold: ") })
    }

    @Test func progressRetainsApprovalAndEveryRecordedPhase() throws {
        for name in ["Implementation", "Review", "Verification", "Failed", "Run unavailable"] {
            let fixture = TaskProgressFixtures.make(name)
            let position = try #require(fixture.position)
            let text = TaskDisplay.progressLines(fixture.task, position: position).joined(separator: ". ")
            #expect(text.contains("Specification Completed"))
            for entry in position.rail.entries { #expect(text.contains(entry.title)) }
        }
        let unavailable = TaskProgressFixtures.make("History unavailable")
        let text = TaskDisplay.progressLines(unavailable.task, position: unavailable.position).joined()
        #expect(text.contains("Specification Approval History Unavailable"))
        #expect(!text.contains("Specification Pending"))
        let revised = TaskProgressFixtures.make("Approval required")
        #expect(revised.position?.rail.entries.first?.state != .completed)
    }

    @Test func supersededPredecessorCannotReplaceCurrentSuccessor() {
        let fixture = TaskProgressFixtures.make("Implementation")
        #expect(fixture.runs[1].run.superseded_by == fixture.runs[0].run.id)
        let reversed = TaskDisplay.position(fixture.task, runs: fixture.runs.reversed(), history: fixture.history)
        #expect(reversed?.status == "In Progress")
        #expect(reversed?.rail == fixture.position?.rail)
    }

    @Test func approvalGuidanceRequiresTheCurrentOpenBoundItem() {
        let fixture = TaskProgressFixtures.make("Approval required")
        let changes: [(inout Components.Schemas.AttentionItem) -> Void] = [
            { $0.status = .resolved }, { $0.project_id = "foreign" }, { $0._type = .agent_question },
            {
                $0.subject = .run(.init(subject_type: .run, subject_id: "old", run_id: "old", task_id: fixture.task.id))
            },
            {
                $0.subject = .run(
                    .init(
                        subject_type: .run, subject_id: fixture.runs[0].run.id,
                        run_id: fixture.runs[0].run.id, task_id: "foreign"))
            },
        ]
        for change in changes {
            var item = fixture.items[0]
            change(&item.item)
            let position = TaskDisplay.position(
                fixture.task, runs: fixture.runs, attentionItems: [item], history: fixture.history)
            #expect(position?.guidance == "Open task details.")
        }
    }

    @Test func cancellationSuppressesStaleActionsWithoutClaimingCompletion() {
        for name in ["Stop pending", "Stop failed", "Stopped"] {
            var fixture = TaskProgressFixtures.make(name)
            fixture.items = [AttentionFixtures.publishedTaskReady()]
            fixture.runs[0].run.outcome = .published
            #expect(fixture.position?.guidance == "Open task details.")
            #expect(fixture.position?.status?.contains("Ready") == false)
            let recordedRound = RunDisplay.stageHeading(fixture.runs[0].run)?.round
            #expect(recordedRound != nil)
            #expect(fixture.position?.heading?.round == recordedRound)
            #expect(TaskDisplay.progressLines(fixture.task, position: fixture.position).contains(recordedRound ?? ""))
            fixture.task.current_position?.value1.round = 3
            fixture.runs = []
            #expect(fixture.position?.heading?.round == "Round 3")
            #expect(TaskDisplay.progressLines(fixture.task, position: fixture.position).contains("Round 3"))
            if name != "Stopped" { #expect(fixture.position?.status != "Stopped") }
        }
        var completed = TaskProgressFixtures.make("Stopped")
        completed.task.lifecycle = .finished
        completed.runs[0].run.outcome = .completed
        #expect(completed.position?.status == "Finished · See Recorded Outcome")
        #expect(
            TaskDisplay.progressLines(completed.task, position: completed.position).contains(
                "Stop Confirmation Recorded"))
        var noRun = completed.task
        noRun.current_position = nil
        noRun.lifecycle = .stopped
        #expect(TaskDisplay.progressLines(noRun, position: nil).first == "Stopped")
    }

    @Test func historicalConfirmationDoesNotSuppressCurrentGuidance() throws {
        let stopped = TaskProgressFixtures.make("Stopped").task
        for name in ["Approval required", "Ready", "Ready degraded"] {
            for hasRun in [true, false] {
                var fixture = TaskProgressFixtures.make(name)
                if !hasRun { fixture.runs = [] }
                let current = fixture.position
                fixture.task.cancellation = stopped.cancellation
                fixture.task.lifecycle_facts = stopped.lifecycle_facts
                let runID = try #require(fixture.task.current_position?.value1.run_id)
                fixture.task.lifecycle_facts.append(
                    .init(
                        kind: .started, run_id: runID,
                        recorded_at: fixture.task.last_activity_at))
                #expect(fixture.task.lifecycle == .active)
                #expect(fixture.position == current)
                let lines = TaskDisplay.progressLines(fixture.task, position: fixture.position)
                #expect(!lines.contains("Stopping Confirmed"))
                #expect(!lines.contains("Stop Confirmation Recorded"))
                fixture.task.lifecycle = .finished
                let finished = TaskDisplay.progressLines(fixture.task, position: fixture.position)
                #expect(finished.contains("Stop Confirmation Recorded"))
                #expect(!finished.contains("Stopping Confirmed"))
                #expect(finished.last == "Open task details.")
            }
        }
    }

    @Test func stoppedSpecificationWithUnavailableHistoryIsHistorical() {
        var fixture = TaskProgressFixtures.make("Approval required")
        fixture.task.lifecycle = .stopped
        fixture.history = nil
        let text = TaskDisplay.progressLines(fixture.task, position: fixture.position).joined()
        #expect(text.contains("Specification Last Recorded Phase, Approval History Unavailable"))
        #expect(!text.contains("Specification Current"))
        #expect(!text.contains("Specification Pending"))
    }
}

enum TaskProgressFixtures {
    struct Case {
        let name: String
        var task: Components.Schemas.Task
        var runs: [Components.Schemas.RunSnapshot]
        var history: Components.Schemas.TaskTimeline?
        var items: [Components.Schemas.AttentionItemSnapshot] = []

        var position: TaskDisplay.Position? {
            TaskDisplay.position(task, runs: runs, attentionItems: items, history: history)
        }
    }

    static func make(_ name: String) -> Case {
        let fixture = TaskFixtures.approvedCampaign()
        var result = Case(name: name, task: fixture.task.task, runs: fixture.runs, history: fixture.history)
        result.task.display_names.task.text = "Update session handling across desktop and mobile clients"
        result.runs[0].run.outcome = .pending
        result.runs[0].run.latest_milestone = .init(value1: .invocation_started)
        switch name {
        case "Queued":
            result.runs[0].run.hold_reason = .init(value1: .identity_parallelism)
            result.task.current_position?.value1.hold_reason = .init(value1: .identity_parallelism)
        case "No position":
            result.task.current_position = nil
            result.task.lifecycle = nil
            result.task.run_ids = []
            result.runs = []
            result.history = nil
        case "Approval required":
            result.runs = [fixture.runs[1]]
            result.runs[0].run.lifecycle = .active
            result.runs[0].run.superseded_by = nil
            result.task.current_position?.value1.run_id = result.runs[0].run.id
            result.task.current_position?.value1.stage = "specification"
            result.history?.sections[0].events = []
            guard var item = AttentionFixtures.defaultInbox().first(where: { $0.item._type == .spec_approval }) else {
                preconditionFailure("Task progress fixtures require a specification approval item")
            }
            item.item.project_id = result.task.project_id
            item.item.subject = .run(
                .init(
                    subject_type: .run, subject_id: result.runs[0].run.id,
                    run_id: result.runs[0].run.id, task_id: result.task.id))
            result.items = [item]
        case "Review":
            result.runs[0].run.stages.append(
                .init(
                    id: "review", run_id: result.runs[0].run.id, name: "review", attempts: []))
        case "Verification":
            result.runs[0].run.hold_reason = .init(value1: .verification_findings)
        case "Stop pending", "Stop failed", "Stopped":
            result.task = TaskFixtures.confirmedStopped(fixture.task).task
            if name != "Stopped" {
                result.task.lifecycle = .active
                result.task.wip = fixture.task.task.wip
                result.task.lifecycle_facts = fixture.task.task.lifecycle_facts
                result.task.cancellation?.value1.state = name == "Stop pending" ? .requested : .failed_to_stop
                if name == "Stop pending" {
                    result.task.cancellation?.value1.acknowledgement = nil
                } else {
                    result.task.cancellation?.value1.acknowledgement?.value1.state = .failed_to_stop
                }
            }
        case "Failed":
            result.runs[0].run.outcome = .failed
            result.runs[0].run.lifecycle = .finished
            result.task.lifecycle = .finished
        case "Ready", "Ready degraded":
            result.runs = fixture.runs
            result.items = [AttentionFixtures.publishedTaskReady(degraded: name == "Ready degraded")]
        case "Finished":
            result.runs[0].run.lifecycle = .finished
            result.runs[0].run.outcome = .completed
            result.task.lifecycle = .finished
        case "Superseded record":
            result.task.current_position?.value1.run_id = result.runs[1].run.id
        case "History unavailable":
            result.history = nil
        case "Run unavailable":
            result.runs = []
        case "Abandoned":
            result.task = TaskFixtures.explicitlyAbandoned(fixture.task).task
        default: break
        }
        return result
    }

    static let groups = [
        ["Queued", "No position", "Approval required", "Implementation"],
        ["Review", "Verification", "Stop pending", "Stop failed"],
        ["Stopped", "Failed", "Ready", "Ready degraded"],
        ["Finished", "Superseded record", "History unavailable", "Run unavailable"],
    ]
}
