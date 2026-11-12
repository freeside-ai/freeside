import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore
@testable import FreesideModel

@Suite struct HoldPresentationTests {
    @Test func everyHoldHasOneReadableLabelAndRetainsItsDiagnosticCode() throws {
        let labels: [Components.Schemas.RunHoldReason: String] = [
            .scope_conflict: "Required work outside approved scope",
            .operation_stopped: "Unattended operation stopped",
            .blocking_system_health: "System issue preventing work",
            .input_unavailable: "Required input unavailable",
            .backend_not_conformant: "Runner checks not current",
            .admission_policy_refused: "Current policy prevents execution",
            .backup_protection_unready: "Backup protection not ready",
            .repository_untrusted: "Repository trust not configured",
            .provider_authority_unavailable: "GitHub App access unavailable",
            .attended_mode_active: "Unattended work paused in attended mode",
            .publication_environment: "Publication environment unavailable",
            .external_conflict: "Conflicting branch or pull request",
            .recipe_revoked: "Verification recipe no longer approved",
            .verification_findings: "Verification findings block publication",
            .trust_blocked: "Trust checks block publication",
            .base_advanced: "Base branch changed",
            .identity_parallelism: "Waiting for agent capacity",
            .usage_pool_exhausted: "Waiting for agent capacity",
            .image_upkeep_pending: "Waiting for project image upkeep",
            .verification_memory_limit: "Verification ran out of memory",
            .quota_window_exhausted: "Waiting for quota window to reset",
            .machine_capacity: "Waiting for machine capacity",
            .update_drain: "Waiting for an update",
        ]
        #expect(Set(labels.keys) == Set(Components.Schemas.RunHoldReason.allCases))
        for reason in Components.Schemas.RunHoldReason.allCases {
            #expect(RunDisplay.label(reason) == labels[reason])
            #expect(AttentionDisplay.label(reason) == labels[reason])
            let milestone = Components.Schemas.RunMilestone(
                run_id: "run", kind: .publication_blocked, reason: .init(value1: reason),
                recorded_at: RunFixtures.screenshotInstant)
            let detail = try #require(RunHistoryPresentation.detail(milestone))
            #expect(detail.hasPrefix("Recorded hold:"))
            #expect(detail.contains(reason.rawValue))
            #expect(!detail.contains("no action"))

            var item = AttentionFixtures.fixture(type: .publish_blocked).item
            item.publish_block?.value1.trust_rule = nil
            item.publish_block?.value1.hold_reason = .init(value1: reason)
            let facts = AttentionDisplay.cardFacts(item, now: RunFixtures.screenshotInstant)
            #expect(facts.count == 1)
            #expect(
                AttentionDisplay.detailIdentifierRows(item).contains {
                    $0.label == "Hold Code" && $0.value == reason.rawValue
                })
        }
    }

    @Test func machineCapacityGuidanceNamesTheMemoryWait() throws {
        let position = try #require(HoldPresentationFixtures.make("Machine capacity").position)
        #expect(position.status == "Queued")
        #expect(position.hold == "Waiting for machine capacity")
        #expect(
            position.guidance
                == "This machine's memory budget can't take this launch yet. This work is queued; no action is needed for this wait."
        )
        #expect(position.holdFacts?.machine_capacity?.value1.ahead == 1)
    }

    @Test func holdFactWordsOnlyTheFactThatBelongsToTheHold() throws {
        func fact(_ name: String) throws -> Components.Schemas.RunHold {
            try #require(HoldPresentationFixtures.hold(name, runID: "run"))
        }
        let pool = try fact("Usage pool")
        #expect(RunDisplay.holdFact(pool, runID: "run") == "Pool claude-max at its limit of parallel runs")
        var memory = try fact("Machine capacity")
        #expect(
            RunDisplay.holdFact(memory, runID: "run")
                == "7.0 GiB of 8.0 GiB reserved · 2.5 GiB for this launch · 1 launch ahead")
        memory.machine_capacity?.value1.ahead = 3
        #expect(
            RunDisplay.holdFact(memory, runID: "run")
                == "7.0 GiB of 8.0 GiB reserved · 2.5 GiB for this launch · 3 launches ahead")
        memory.machine_capacity?.value1.ahead = 0
        #expect(RunDisplay.holdFact(memory, runID: "run") == "7.0 GiB of 8.0 GiB reserved · 2.5 GiB for this launch")

        // A stored identity_parallelism hold names no pool, and a hold with
        // its fact missing has nothing to say.
        #expect(RunDisplay.holdFact(try fact("Capacity"), runID: "run") == nil)
        var bare = pool
        bare.usage_pool = nil
        #expect(RunDisplay.holdFact(bare, runID: "run") == nil)
        memory.machine_capacity = nil
        #expect(RunDisplay.holdFact(memory, runID: "run") == nil)

        // Another run's hold, or a fact beside a reason it doesn't belong
        // to, is ignored.
        #expect(RunDisplay.holdFact(pool, runID: "other") == nil)
        var misplaced = pool
        misplaced.reason = .identity_parallelism
        #expect(RunDisplay.holdFact(misplaced, runID: "run") == nil)
        misplaced.reason = .machine_capacity
        #expect(RunDisplay.holdFact(misplaced, runID: "run") == nil)
    }

    /// An admission hold names the selection part it refused, and the
    /// enrollment the daemon holds for a model or effort (#2992). A hold
    /// with no part reads as it always did.
    @Test func admissionHoldNamesTheUnservedPart() throws {
        var hold = try #require(HoldPresentationFixtures.hold("Model unserved", runID: "run"))
        #expect(RunDisplay.holdFact(hold, runID: "run") == "The model isn't one claude-ig serves")
        hold.detail?.value1.unserved = .init(value1: .init(part: .effort, enrollment_id: "claude-ig"))
        #expect(RunDisplay.holdFact(hold, runID: "run") == "The effort isn't one claude-ig serves for this model")
        hold.detail?.value1.unserved = .init(value1: .init(part: .enrollment))
        #expect(RunDisplay.holdFact(hold, runID: "run") == "The enrollment isn't one the daemon holds")
        hold.detail?.value1.unserved = nil
        #expect(RunDisplay.holdFact(hold, runID: "run") == nil)
        hold.detail = nil
        #expect(RunDisplay.holdFact(hold, runID: "run") == nil)
    }

    @Test func admissionGuidanceNamesTheUnservedPart() throws {
        let unserved = try #require(HoldPresentationFixtures.make("Model unserved").position)
        #expect(unserved.hold == "Current policy prevents execution")
        #expect(unserved.guidance == "The model isn't one claude-ig serves. Change the task's agent.")
        // An admission hold that names no part keeps today's guidance.
        let unnamed = HoldPresentationFixtures.make("Configuration unavailable").position
        #expect(unnamed?.guidance.contains("Change the task's agent") == false)
    }

    /// A quota-window park offers its hold's agents to move to, and only
    /// while the position is current and its recorded hold is that park.
    @Test func quotaWindowParkOffersItsAgentsWhileCurrent() throws {
        let parked = HoldPresentationFixtures.make("Quota window")
        #expect(TaskDisplay.moveOffers(parked.position).map(\.enrollment) == [UsageFixtures.readingCollectorID])

        var unloaded = parked
        unloaded.history = nil
        #expect(TaskDisplay.moveOffers(unloaded.position).isEmpty)
        #expect(TaskDisplay.moveOffers(HoldPresentationFixtures.make("Usage pool").position).isEmpty)

        let runID = parked.runs[0].run.id
        let section = try #require(parked.history?.sections.firstIndex { $0.runs.contains { $0.run_id == runID } })
        let run = try #require(parked.history?.sections[section].runs.firstIndex { $0.run_id == runID })
        var none = parked
        none.history?.sections[section].runs[run].hold?.value1.quota_window?.value1.alternate_agents = nil
        #expect(TaskDisplay.moveOffers(none.position).isEmpty)
        var finished = parked
        finished.runs[0].run.lifecycle = .finished
        #expect(TaskDisplay.moveOffers(finished.position).isEmpty)
    }

    @Test func poolGuidanceNamesThePoolOnlyFromTheCurrentHold() throws {
        let busy = "The configured agent account is busy. This work is queued; no action is needed for this wait."
        let named =
            "Pool claude-max is at its limit of parallel runs. This work is queued; no action is needed for this wait."
        let loaded = HoldPresentationFixtures.make("Usage pool")
        #expect(loaded.position?.hold == "Waiting for agent capacity")
        #expect(loaded.position?.guidance == named)
        #expect(HoldPresentationFixtures.make("Usage pool without facts").position?.guidance == busy)
        // The stored identity_parallelism hold carries its record but no pool.
        let stored = HoldPresentationFixtures.make("Capacity")
        #expect(stored.position?.holdFacts?.reason == .identity_parallelism)
        #expect(stored.position?.guidance == busy)

        var unloaded = loaded
        unloaded.history = nil
        #expect(unloaded.position?.holdFacts == nil)
        #expect(unloaded.position?.guidance == busy)

        // A recorded hold that disagrees with the position (another run,
        // another reason, another task's timeline) is ignored.
        let runID = loaded.runs[0].run.id
        let index = try #require(
            loaded.history?.sections.firstIndex { $0.runs.contains { $0.run_id == runID } })
        let runIndex = try #require(loaded.history?.sections[index].runs.firstIndex { $0.run_id == runID })
        var otherRun = loaded
        otherRun.history?.sections[index].runs[runIndex].hold?.value1.run_id = "other"
        var otherReason = loaded
        otherReason.history?.sections[index].runs[runIndex].hold?.value1.reason = .machine_capacity
        var otherTask = loaded
        otherTask.history?.task_id = "other"
        for fixture in [otherRun, otherReason, otherTask] {
            #expect(fixture.position?.holdFacts == nil)
            #expect(fixture.position?.guidance == busy)
        }
    }

    @Test func capacityGuidanceAgreesWithTheCurrentPositionFallback() throws {
        let loaded = HoldPresentationFixtures.make("Capacity")
        let fallback = HoldPresentationFixtures.make("Capacity without run")
        for fixture in [loaded, fallback] {
            let position = try #require(fixture.position)
            #expect(position.status == "Queued")
            #expect(position.hold == "Waiting for agent capacity")
            #expect(
                position.guidance
                    == "The configured agent account is busy. This work is queued; no action is needed for this wait.")
            let lines = TaskDisplay.progressLines(fixture.task, position: position)
            #expect(lines.filter { $0.contains("configured agent account") }.count == 1)
            #expect(!lines.joined().contains("identity_parallelism"))
        }
        var cleared = loaded
        cleared.runs[0].run.hold_reason = nil
        #expect(cleared.position?.status == "In Progress")
        #expect(cleared.position?.hold == nil)
        #expect(cleared.position?.guidance == "Open task details.")
    }

    @Test func approvalStopFailureAndHistoryOutrankCapacityGuidance() throws {
        let statuses = [
            "Approval beside capacity": "Specification Approval Required",
            "Stop requested beside capacity": "Stopping",
            "Stop failed beside capacity": "Stop Unconfirmed · No New Work",
            "Stopped beside capacity": "Stopped",
            "Historical capacity": "Finished · See Recorded Outcome",
            "Failed beside capacity": "Execution Failed",
        ]
        for (name, status) in statuses {
            for loaded in [true, false] {
                var fixture = HoldPresentationFixtures.make(name)
                if !loaded { fixture.runs = [] }
                let position = try #require(fixture.position)
                // A missing run cannot report an execution failure it hasn't loaded.
                let expected = !loaded && name == "Failed beside capacity" ? "Finished · See Recorded Outcome" : status
                #expect(position.status == expected)
                #expect(
                    position.guidance
                        == (name == "Approval beside capacity"
                            ? "Review the specification." : "Open task details."))
            }
        }
        var stoppedApproval = HoldPresentationFixtures.make("Approval beside capacity")
        stoppedApproval.task.cancellation = TaskProgressFixtures.make("Stop pending").task.cancellation
        #expect(stoppedApproval.position?.guidance == "Open task details.")
        #expect(stoppedApproval.position?.status == "Stopping")
        #expect(
            TaskDisplay.progressLines(stoppedApproval.task, position: stoppedApproval.position).contains(
                "Last recorded hold: Waiting for agent capacity"))
    }

    @Test func unavailableAndMissingFactsNeverInventARepairOrApproval() throws {
        for name in ["Input unavailable", "Configuration unavailable", "Unattended operation stopped", "Missing cause"]
        {
            let fixture = HoldPresentationFixtures.make(name)
            #expect(fixture.position?.guidance == "Open task details.")
            #expect(fixture.position?.status != "Stopped")
        }
        var missing = AttentionFixtures.fixture(type: .publish_blocked).item
        missing.publish_block = nil
        missing.reason = "identity_parallelism"
        #expect(AttentionDisplay.cardFacts(missing, now: RunFixtures.screenshotInstant).isEmpty)
        #expect(!AttentionDisplay.detailIdentifierRows(missing).contains { $0.label == "Hold Code" })
    }

    @Test func finishedAndSupersededRunsCannotReadAsActiveQueues() throws {
        var fixture = HoldPresentationFixtures.make("Capacity")
        fixture.runs[0].run.lifecycle = .finished
        #expect(fixture.position?.status == "Run Finished · See Recorded Outcome")
        #expect(fixture.position?.guidance == "Open task details.")
        #expect(RunDisplay.secondaryLine(fixture.runs[0].run) == .hold("Recorded hold: Waiting for agent capacity"))
        fixture.runs[0].run.superseded_by = "successor"
        #expect(fixture.position?.status == "Superseded Run · Historical")
        #expect(fixture.position?.guidance == "Open task details.")
        fixture.runs[0].run.lifecycle = .active
        fixture.runs[0].run.superseded_by = nil
        fixture.runs[0].run.outcome = .lost
        #expect(fixture.position?.status == "Execution Lost")
        #expect(fixture.position?.guidance == "Open task details.")
    }

    @Test func calloutStatesTheRowsHoldAndNoRound() {
        for name in HoldPresentationFixtures.groups.joined() {
            for loaded in [true, false] {
                var fixture = HoldPresentationFixtures.make(name)
                if !loaded { fixture.runs = [] }
                let lines = TaskDisplay.rowLines(fixture.task, position: fixture.position)
                let callout = TaskDisplay.holdCallout(fixture.task, position: fixture.position)
                let rowHolds = lines.facts.filter {
                    $0.hasPrefix("Hold: ") || $0.hasPrefix("Last recorded hold: ")
                }
                #expect(rowHolds == [callout?.hold].compactMap { $0 }, "\(name), loaded: \(loaded)")
                if let callout {
                    // The callout repeats the row's hold and leaves the
                    // round to the row; its stage line is the detail page's
                    // to supply (`TaskDetailStageTests`).
                    let round = fixture.position?.heading?.round
                    #expect(lines.facts == [round, callout.hold].compactMap { $0 }, "\(name), loaded: \(loaded)")
                    #expect(callout.stage == nil, "\(name), loaded: \(loaded)")
                }
            }
        }
        #expect(TaskDisplay.holdCallout(TaskProgressFixtures.make("No position").task, position: nil) == nil)
    }

    @Test func calloutWordingKeepsEachHoldKindDistinct() throws {
        let holds: [String: String?] = [
            "Capacity": "Hold: Waiting for agent capacity",
            "Capacity without run": "Hold: Waiting for agent capacity",
            "Approval beside capacity": "Hold: Waiting for agent capacity",
            "Input unavailable": "Hold: Required input unavailable",
            "Configuration unavailable": "Hold: Current policy prevents execution",
            "Unattended operation stopped": "Hold: Unattended operation stopped",
            "Stop requested beside capacity": "Last recorded hold: Waiting for agent capacity",
            "Stop failed beside capacity": "Last recorded hold: Waiting for agent capacity",
            // A stopped or finished task's hold is history, not where it stands.
            "Stopped beside capacity": nil,
            "Historical capacity": nil,
            "Missing cause": nil,
            "Failed beside capacity": nil,
            "Usage pool": "Hold: Waiting for agent capacity",
            "Usage pool without facts": "Hold: Waiting for agent capacity",
            "Machine capacity": "Hold: Waiting for machine capacity",
        ]
        #expect(Set(holds.keys) == Set(HoldPresentationFixtures.groups.joined()))
        for (name, hold) in holds {
            let fixture = HoldPresentationFixtures.make(name)
            let callout = TaskDisplay.holdCallout(fixture.task, position: fixture.position)
            #expect(callout?.hold == hold, "\(name)")
        }
        // The callout is the same for a capacity wait and a hold the operator
        // must clear; the guidance beside it is what says no action is needed.
        let capacity = try #require(HoldPresentationFixtures.make("Capacity").position)
        #expect(capacity.guidance.contains("no action is needed for this wait"))
        #expect(HoldPresentationFixtures.make("Input unavailable").position?.guidance == "Open task details.")
        // A hold the loaded run reports reaches the callout even when the
        // task position carries none.
        let verification = TaskProgressFixtures.make("Verification")
        #expect(
            TaskDisplay.holdCallout(verification.task, position: verification.position)?.hold
                == "Hold: Verification findings block publication")
    }
}

enum HoldPresentationFixtures {
    static let groups = [
        ["Capacity", "Capacity without run", "Approval beside capacity", "Input unavailable"],
        [
            "Configuration unavailable", "Unattended operation stopped", "Stop requested beside capacity",
            "Stop failed beside capacity",
        ],
        ["Stopped beside capacity", "Historical capacity", "Missing cause", "Failed beside capacity"],
        ["Usage pool", "Usage pool without facts", "Machine capacity"],
    ]

    static func make(_ name: String) -> TaskProgressFixtures.Case {
        let base: String
        switch name {
        case "Approval beside capacity": base = "Approval required"
        case "Stop requested beside capacity": base = "Stop pending"
        case "Stop failed beside capacity": base = "Stop failed"
        case "Stopped beside capacity": base = "Stopped"
        case "Historical capacity": base = "Finished"
        case "Failed beside capacity": base = "Failed"
        default: base = "Implementation"
        }
        var fixture = TaskProgressFixtures.make(base)
        let reason: Components.Schemas.RunHoldReason?
        switch name {
        case "Input unavailable": reason = .input_unavailable
        case "Configuration unavailable", "Model unserved": reason = .admission_policy_refused
        case "Unattended operation stopped": reason = .operation_stopped
        case "Missing cause": reason = nil
        case "Machine capacity": reason = .machine_capacity
        case "Usage pool", "Usage pool without facts": reason = .usage_pool_exhausted
        case "Quota window": reason = .quota_window_exhausted
        default: reason = .identity_parallelism
        }
        fixture.runs[0].run.hold_reason = reason.map { .init(value1: $0) }
        fixture.task.current_position?.value1.hold_reason = reason.map { .init(value1: $0) }
        let runID = fixture.runs[0].run.id
        if let hold = hold(name, runID: runID) {
            guard
                let section = fixture.history?.sections.firstIndex(where: {
                    $0.runs.contains { $0.run_id == runID }
                }),
                let index = fixture.history?.sections[section].runs.firstIndex(where: { $0.run_id == runID })
            else { preconditionFailure("Hold fixtures require the current run in the task timeline") }
            fixture.history?.sections[section].runs[index].hold = .init(value1: hold)
        }
        if name == "Capacity without run" { fixture.runs = [] }
        return fixture
    }

    /// The hold the daemon recorded beside each capacity case, with the fact
    /// that belongs to its reason: a pool, the memory reservations, and a
    /// stored `identity_parallelism` hold, which names no pool. The run
    /// timeline surfaces draw the same holds.
    static func hold(_ name: String, runID: String) -> Components.Schemas.RunHold? {
        let observed = RunFixtures.screenshotInstant
        switch name {
        case "Capacity":
            return .init(
                run_id: runID, reason: .identity_parallelism, first_observed_at: observed, last_observed_at: observed)
        case "Usage pool":
            return .init(
                run_id: runID, reason: .usage_pool_exhausted, usage_pool: "claude-max", first_observed_at: observed,
                last_observed_at: observed)
        case "Quota window":
            // A provider refused the implementer on its pool's weekly window;
            // the routing policy's second agent on another pool is offered.
            return .init(
                run_id: runID, invocation_id: "inv-\(runID)-quota", reason: .quota_window_exhausted,
                quota_window: .init(
                    value1: .init(
                        pool: "claude-max", window: "seven_day", source: .provider_refusal,
                        invocation_id: "inv-\(runID)-quota", resets_at: observed.addingTimeInterval(3 * 3600),
                        set_at: observed,
                        alternate_agents: [
                            .init(
                                enrollment: UsageFixtures.readingCollectorID, pool: "claude-team-pool",
                                harness: .claude_code,
                                model: "claude-opus-5-5", qualification: .different_pool)
                        ])),
                first_observed_at: observed, last_observed_at: observed)
        case "Model unserved":
            // A task line names a model its enrollment doesn't serve (#2992):
            // the hold names the part and the enrollment, never the model.
            return .init(
                run_id: runID, invocation_id: "inv-\(runID)-unserved", reason: .admission_policy_refused,
                detail: .init(
                    value1: .init(
                        check: .agent_resolution,
                        unserved: .init(value1: .init(part: .model, enrollment_id: "claude-ig")))),
                first_observed_at: observed, last_observed_at: observed)
        case "Machine capacity":
            return .init(
                run_id: runID, reason: .machine_capacity,
                machine_capacity: .init(
                    value1: .init(reservation_mib: 2560, reserved_mib: 7168, budget_mib: 8192, ahead: 1)),
                first_observed_at: observed, last_observed_at: observed)
        default:
            return nil
        }
    }
}
