import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

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
                AttentionDisplay.detailBindingRows(item).contains {
                    $0.label == "Hold Code" && $0.value == reason.rawValue
                })
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
            "Stop failed beside capacity": "Failed to Stop · Execution May Continue",
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
                            ? "Review the specification in Inbox." : "Open task details."))
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
        #expect(!AttentionDisplay.detailBindingRows(missing).contains { $0.label == "Hold Code" })
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

    @Test func calloutStatesTheRowsRoundAndHoldAndNothingElse() {
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
                    // The callout repeats the row's facts and adds none.
                    let calloutLines = [callout.round, callout.hold].compactMap { $0 }
                    #expect(calloutLines == lines.facts, "\(name), loaded: \(loaded)")
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
        ]
        #expect(Set(holds.keys) == Set(HoldPresentationFixtures.groups.joined()))
        for (name, hold) in holds {
            let fixture = HoldPresentationFixtures.make(name)
            let callout = TaskDisplay.holdCallout(fixture.task, position: fixture.position)
            #expect(callout?.hold == hold, "\(name)")
            if let callout {
                #expect(callout.round == fixture.position?.heading?.round, "\(name)")
            }
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
        case "Configuration unavailable": reason = .admission_policy_refused
        case "Unattended operation stopped": reason = .operation_stopped
        case "Missing cause": reason = nil
        default: reason = .identity_parallelism
        }
        fixture.runs[0].run.hold_reason = reason.map { .init(value1: $0) }
        fixture.task.current_position?.value1.hold_reason = reason.map { .init(value1: $0) }
        if name == "Capacity without run" { fixture.runs = [] }
        return fixture
    }
}
