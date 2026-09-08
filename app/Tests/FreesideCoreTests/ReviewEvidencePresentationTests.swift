import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct ReviewEvidencePresentationTests {
    private func read(_ events: String = "", result: String? = #"{"findings":[]}"#) -> ReviewEvidencePresentation {
        .init(events: Array(events.utf8), result: result.map { Array($0.utf8) }, exitStatus: 0)
    }

    @Test func cleanResultIsOnlyAReviewerClaim() {
        let value = read()
        #expect(value.conclusion == .noFindings)
        #expect(value.explanation == nil)
        #expect(value.terminalState == nil)
        #expect(value.entries.isEmpty)
        #expect(value.exitStatus == 0)
    }

    @Test func structuredFinalResultDoesNotInventAnExplanation() {
        let value = read(
            #"{"type":"item.completed","item":{"id":"1","type":"agent_message","text":"{\"findings\":[]}"}}"#)
        #expect(value.conclusion == .noFindings)
        #expect(value.explanation == nil)
        #expect(value.messageCount == 1)
        #expect(value.entries[0].text == "The reviewer reported no findings in this message.")
    }

    @Test func absentAndUnreadableResultsAreNeverClean() {
        #expect(read(result: nil).conclusion == .absent)
        #expect(read(result: "").conclusion == .absent)
        for result in [" ", "not JSON", "{}", "null", #"{"findings":null}"#, #"{"findings":[],"extra":true}"#] {
            #expect(read(result: result).conclusion == .unreadable)
        }
    }

    @Test func duplicateKeysCannotBecomeReadableClaims() {
        for result in [
            #"{"findings":[],"findings":null}"#,
            #"{"findings":[],"find\u0069ngs":[{"severity":"P1","location":{"path":"a","whole_file":true},"explanation":"gap"}]}"#,
            #"{"findings":[{"severity":"P1","severity":"P2","location":{"path":"a","whole_file":true},"explanation":"gap"}]}"#,
            #"{"findings":[{"severity":"P1","location":{"path":"a","whole_file":true,"whole_file":false},"explanation":"gap"}]}"#,
        ] {
            #expect(read(result: result).conclusion == .unreadable)
        }
        let event = #"{"type":"turn.completed","type":"turn.failed"}"#
        let value = read(event)
        #expect(value.terminalState == nil)
        #expect(value.entries.first?.text == event)
        #expect(value.entries.first?.kind == .diagnostic)
        #expect(
            read(
                #"{"type":"item.completed","item":{"type":"agent_message","text":"A quoted \"key\": is prose, {not an object}."}}"#
            ).messageCount == 1)
    }

    @Test func findingsPreserveSeverityLocationAndExplanation() throws {
        let round = RunFixtures.reviewRound(.completed, findings: true, availability: .available)
        let evidence = RunFixtures.reviewEvidence(runID: RunFixtures.activeRunID, round: round)
        let value = ReviewEvidencePresentation(
            events: Array(evidence.events!.data), result: Array(evidence.result!.data), exitStatus: evidence.exit_status
        )
        guard case .findings(let findings) = value.conclusion else {
            Issue.record("Expected the fixture’s three findings")
            return
        }
        #expect(findings.count == 3)
        #expect(findings[0].severity == "P1")
        #expect(findings[0].location.path == "Sources/Cache.swift")
        #expect(findings[0].location.startLine == 42)
        #expect(findings[0].location.endLine == 45)
        #expect(findings[0].explanation.contains("failed write"))
        #expect(findings[2].location.label == "README.md · Whole file")
        #expect(value.commandCount == 1)
        #expect(value.messageCount == 1)
        #expect(value.explanation?.contains("did not run the tests") == true)
    }

    @Test func unsupportedFindingShapesAreUnreadable() {
        for location in [
            #"{"path":"a","whole_file":false}"#,
            #"{"path":"a","whole_file":1}"#,
            #"{"path":"a","start_line":0,"end_line":1}"#,
            #"{"path":"a","start_line":2,"end_line":1}"#,
            #"{"path":"a","start_line":true,"end_line":1}"#,
            #"{"path":"a","start_line":1}"#,
            #"{"path":"a","whole_file":true,"start_line":1,"end_line":1}"#,
        ] {
            #expect(
                read(result: "{\"findings\":[{\"severity\":\"P1\",\"location\":\(location),\"explanation\":\"gap\"}]}")
                    .conclusion == .unreadable)
        }
        #expect(
            read(
                result:
                    #"{"findings":[{"severity":"high","location":{"path":"a","whole_file":true},"explanation":"gap"}]}"#
            ).conclusion == .unreadable)
    }

    @Test func itemUpdatesCollapseAndKeepCommandDetails() throws {
        let value = read(
            """
            {"type":"item.started","item":{"id":"1","type":"command_execution","command":"git diff","status":"in_progress"}}
            {"type":"item.updated","item":{"id":"1","type":"command_execution","command":"git diff","aggregated_output":"partial"}}
            {"type":"item.completed","item":{"id":"1","type":"command_execution","command":"git diff","aggregated_output":"first\\nsecond","exit_code":1,"status":"completed"}}
            {"type":"item.completed","item":{"id":"2","type":"agent_message","text":"I inspected the diff.\\nI could not run tests."}}
            """)
        #expect(value.entries.count == 2)
        #expect(value.commandCount == 1)
        #expect(value.messageCount == 1)
        let command = try #require(value.entries.first)
        #expect(command.command == "git diff")
        #expect(command.text == "first\nsecond")
        #expect(command.exitCode == 1)
        #expect(command.status == "completed")
        #expect(value.explanation == "I inspected the diff.\nI could not run tests.")
        #expect(value.terminalState == nil)
    }

    @Test func lifecycleAndFailureMessagesStayDistinctFromCommandExit() {
        let value = read(
            """
            {"type":"thread.started","thread_id":"1"}
            {"type":"turn.started"}
            {"type":"turn.completed"}
            {"type":"turn.failed","error":{"message":"Access denied"}}
            """)
        #expect(
            value.entries.map(\.text) == [
                "Review session started", "Turn started", "Turn completed", "Turn failed: Access denied",
            ])
        #expect(value.terminalState == "Turn failed: Access denied")
        #expect(read(#"{"type":"turn.failed"}"#).terminalState == "Turn failed: No failure message retained")
        #expect(read("{\"type\":\"turn.completed\"}\n{\"type\":\"turn.started\"}").terminalState == nil)
    }

    @Test func diagnosticsAndAccessChecksAreNotReviewerActivity() {
        let marker =
            "freeside-review-access-v1 base=\(String(repeating: "a", count: 40)) head=\(String(repeating: "b", count: 40)) cwd=/workspace"
        let lines = [
            "startup diagnostic", #"{"type":"future.event","payload":1}"#,
            #"{"type":"item.completed","item":{"type":"future_item","id":"1"}}"#,
            #"{"type":"item.completed","item":{"type":"agent_message"}}"#, marker,
        ]
        let value = read(lines.joined(separator: "\n"))
        #expect(value.entries.map(\.text) == lines)
        #expect(value.entries.last?.kind == .accessCheck)
        #expect(value.diagnosticCount == 5)
        #expect(value.commandCount == 0)
        #expect(value.messageCount == 0)
    }

    @Test func reviewerNotesAndErrorsKeepTheirOwnLabels() {
        let value = read(
            """
            {"type":"item.completed","item":{"id":"1","type":"reasoning","text":"Checking callers"}}
            {"type":"item.completed","item":{"id":"2","type":"error","message":"Tool unavailable"}}
            """)
        #expect(value.entries.map(\.kind) == [.reasoning, .error])
        #expect(value.explanation == nil)
    }

    @Test func invalidUTF8IsFlaggedPerLineAndInputsAreUnchanged() {
        let events = Array("stderr: ".utf8) + [0xff, 10] + Array(#"{"type":"turn.completed"}"#.utf8)
        let result = Array(#"{"findings":[]}"#.utf8)
        let originalEvents = events
        let originalResult = result
        let value = ReviewEvidencePresentation(events: events, result: result, exitStatus: 0)
        #expect(events == originalEvents)
        #expect(result == originalResult)
        #expect(value.entries[0].invalidUTF8)
        #expect(value.entries[0].text == "stderr: �")
        #expect(!value.entries[1].invalidUTF8)
        #expect(value.terminalState == "Turn completed")
        #expect(ReviewEvidencePresentation(events: [], result: [0xff], exitStatus: nil).conclusion == .unreadable)
    }

    @Test func failedMockHasDiagnosticsAndNoReadableConclusion() {
        let round = RunFixtures.reviewRound(.failed, availability: .available)
        let evidence = RunFixtures.reviewEvidence(runID: RunFixtures.activeRunID, round: round)
        let value = ReviewEvidencePresentation(
            events: Array(evidence.events!.data), result: Array(evidence.result!.data), exitStatus: evidence.exit_status
        )
        #expect(value.conclusion == .absent)
        #expect(value.diagnosticCount == 2)
        #expect(value.messageCount == 0)
        #expect(value.explanation == nil)
        #expect(value.terminalState == "Turn failed: The reviewer could not inspect the bound diff.")
    }
}
