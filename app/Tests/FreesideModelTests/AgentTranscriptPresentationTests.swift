import Foundation
import Testing

@testable import FreesideModel

/// The parser reads the Claude CLI's stream JSON as the writer records it
/// (`--output-format stream-json --verbose` in
/// `daemon/internal/exec/claude/spec.go`). The records here are synthetic,
/// shaped like the CLI's.
@Suite struct AgentTranscriptPresentationTests {
    private typealias Presentation = AgentTranscriptPresentation

    // MARK: Records

    private static func json(_ object: [String: Any]) throws -> String {
        let data = try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])
        return String(decoding: data, as: UTF8.self)
    }

    private static func assistant(_ blocks: [[String: Any]], parent: String? = nil) throws -> String {
        var object: [String: Any] = [
            "type": "assistant", "session_id": "s",
            "message": ["role": "assistant", "content": blocks],
        ]
        object["parent_tool_use_id"] = parent ?? NSNull()
        return try json(object)
    }

    private static func user(_ blocks: [[String: Any]], parent: String? = nil) throws -> String {
        var object: [String: Any] = ["type": "user", "message": ["role": "user", "content": blocks]]
        object["parent_tool_use_id"] = parent ?? NSNull()
        return try json(object)
    }

    private static func text(_ text: String) -> [String: Any] { ["type": "text", "text": text] }
    private static func thinking(_ text: String) -> [String: Any] {
        ["type": "thinking", "thinking": text, "signature": "sig"]
    }

    private static func toolUse(_ id: String, _ name: String, _ input: [String: Any]) -> [String: Any] {
        ["type": "tool_use", "id": id, "name": name, "input": input]
    }

    private static func toolResult(_ id: String, _ content: Any, isError: Bool = false) -> [String: Any] {
        ["type": "tool_result", "tool_use_id": id, "content": content, "is_error": isError]
    }

    /// A nil `structuredOutput` leaves the field out; `NSNull()` writes
    /// JSON null.
    private static func result(
        _ message: String?, isError: Bool = false, subtype: String = "success", structuredOutput: Any? = nil
    ) throws
        -> String
    {
        var object: [String: Any] = [
            "type": "result", "subtype": subtype, "is_error": isError, "total_cost_usd": 1.5,
            "usage": ["input_tokens": 10],
        ]
        object["result"] = message
        object["structured_output"] = structuredOutput
        return try json(object)
    }

    /// A call and its result, as two records.
    private static func call(
        _ id: String, _ name: String, _ input: [String: Any], output: Any = "ok", isError: Bool = false
    ) throws -> [String] {
        [try assistant([toolUse(id, name, input)]), try user([toolResult(id, output, isError: isError)])]
    }

    private static func read(
        _ lines: [String], trailingNewline: Bool = true, streamEnded: Bool = true
    ) -> Presentation {
        Presentation(
            bytes: Array((lines.joined(separator: "\n") + (trailingNewline ? "\n" : "")).utf8),
            streamEnded: streamEnded)
    }

    private static func calls(_ entry: Presentation.Entry?) -> [Presentation.Call] {
        guard case .work(let work)? = entry else { return [] }
        return work.calls
    }

    // MARK: Tests

    @Test func eachRecordTypeBecomesItsPartOfTheConversation() throws {
        let value = Self.read(
            [
                try Self.json(["type": "system", "subtype": "init", "cwd": "/workspace", "tools": ["Bash"]]),
                try Self.json(["type": "control_response", "response": ["subtype": "success"]]),
                try Self.json(["type": "rate_limit_event", "rate_limit_info": ["status": "allowed"]]),
                try Self.assistant([Self.text("I'll read the runner first.")]),
                try Self.assistant([Self.thinking("The probe races the listener.\nSecond line.")]),
                try Self.assistant([Self.toolUse("t1", "Read", ["file_path": "/workspace/rig/runner.go"])]),
                try Self.user([Self.toolResult("t1", [["type": "text", "text": "package rig"]])]),
                try Self.assistant([Self.text("Fixed: the bind now runs first.")]),
                try Self.result("Fixed: the bind now runs first."),
            ])
        #expect(value.unreadable.isEmpty)
        #expect(value.lines.count == 9)
        #expect(value.endsWithPartialLine == false)
        #expect(value.entries.count == 3)
        guard case .message(0, let first) = value.entries[0] else {
            Issue.record("expected the first message as the first item")
            return
        }
        #expect(first == "I'll read the runner first.")
        guard case .reasoning(let reasoning) = value.entries[1] else {
            Issue.record("expected reasoning")
            return
        }
        #expect(reasoning.preview == "The probe races the listener.")
        let read = try #require(Self.calls(value.entries[2]).first)
        #expect(read.id == 2)
        #expect(read.verb == "Read")
        #expect(read.identifier == "rig/runner.go")
        #expect(read.output?.text == "package rig")
        // The result repeats the last message, so it is drawn once, as
        // the final message; its cost and usage are not read.
        #expect(
            value.result
                == .init(message: "Fixed: the bind now runs first.", answer: nil, isError: false, subtype: "success"))
    }

    @Test func callsBetweenMessagesGroupUnderOneSummary() throws {
        let value = Self.read(
            [try Self.assistant([Self.text("Starting.")])]
                + (try Self.call("a", "Read", ["file_path": "/workspace/a.go"]))
                + (try Self.call("b", "Read", ["file_path": "/workspace/b.go"]))
                + (try Self.call("c", "Read", ["file_path": "/workspace/a.go"]))
                + (try Self.call("d", "Bash", ["command": "go test ./..."]))
                + (try Self.call("e", "Bash", ["command": "gofmt -l ."]))
                + [try Self.assistant([Self.text("Now editing.")])]
                + (try Self.call("f", "Edit", ["file_path": "/workspace/a.go"]))
                + (try Self.call("g", "Grep", ["pattern": "listen"]))
                + (try Self.call("h", "TodoWrite", [:]))
                + (try Self.call("i", "WebFetch", ["url": "https://example.com"]))
                + [try Self.assistant([Self.text("One more check.")])]
                + (try Self.call("j", "Glob", ["pattern": "**/*.go"])))
        let summaries = value.entries.compactMap { entry -> String?? in
            guard case .work(let work) = entry else { return nil }
            return work.summary
        }
        #expect(
            summaries == [
                "Read 2 files, ran 2 commands", "Edited 1 file, searched 1 pattern, 2 other actions", nil,
            ])
        // A group of one call shows that call's own line.
        let single = try #require(Self.calls(value.entries.last).first)
        #expect(single.verb == "Searched")
        #expect(single.identifier == "**/*.go")
        let others = Self.calls(value.entries[3]).filter { $0.kind == .other }
        #expect(others.map(\.verb) == ["TodoWrite", "WebFetch"])
        #expect(others.allSatisfy { $0.identifier == nil })
    }

    @Test func reasoningStaysOutsideAGroupUnlessItFallsBetweenItsCalls() throws {
        let value = Self.read(
            [try Self.assistant([Self.thinking("Before.")])]
                + (try Self.call("a", "Bash", ["command": "ls"]))
                + [try Self.assistant([Self.thinking("Between.")])]
                + (try Self.call("b", "Bash", ["command": "pwd"]))
                + [try Self.assistant([Self.thinking("After.")]), try Self.assistant([Self.text("Done.")])])
        #expect(value.entries.count == 4)
        guard case .reasoning(let before) = value.entries[0], case .work(let work) = value.entries[1],
            case .reasoning(let after) = value.entries[2]
        else {
            Issue.record("expected reasoning, work, reasoning")
            return
        }
        #expect(before.text == "Before.")
        #expect(after.text == "After.")
        #expect(work.steps.count == 3)
        guard case .reasoning(let between) = work.steps[1] else {
            Issue.record("expected the reasoning between the calls inside the group")
            return
        }
        #expect(between.text == "Between.")
    }

    @Test func pathsReadRelativeToTheRepository() throws {
        let value = Self.read(
            (try Self.call("a", "Write", ["file_path": "/workspace/rig/new.go"]))
                + (try Self.call("b", "NotebookEdit", ["notebook_path": "/workspace/n.ipynb"]))
                + (try Self.call("c", "Read", ["file_path": "/workspace"]))
                + (try Self.call("d", "Read", ["file_path": "/workspaces/other.go"]))
                + (try Self.call("e", "MultiEdit", ["file_path": "/tmp/scratch.go"])))
        #expect(
            Self.calls(value.entries.first).map(\.identifier) == [
                "rig/new.go", "n.ipynb", ".", "/workspaces/other.go", "/tmp/scratch.go",
            ])
    }

    @Test func aFailedCommandCarriesItsExitCode() throws {
        let value = Self.read(
            (try Self.call("a", "Bash", ["command": "go test"], output: "Exit code 2\n--- FAIL", isError: true))
                + (try Self.call("b", "Bash", ["command": "false"], output: "permission denied", isError: true))
                + (try Self.call("c", "Read", ["file_path": "/x"], output: "Exit code 1", isError: true))
                + (try Self.call("d", "Bash", ["command": "true"], output: "Exit code 3")))
        let calls = Self.calls(value.entries.first)
        #expect(calls.map(\.failed) == [true, true, true, false])
        #expect(calls.map(\.exitCode) == [2, nil, nil, nil])
    }

    @Test func aLongOutputKeepsItsLastLines() throws {
        let long = (1...10).map { "line \($0)" }.joined(separator: "\n")
        let short = (1...8).map { "line \($0)" }.joined(separator: "\n")
        let value = Self.read(
            (try Self.call("a", "Bash", ["command": "a"], output: long))
                + (try Self.call("b", "Bash", ["command": "b"], output: short))
                + (try Self.call("c", "Read", ["file_path": "/i.png"], output: [["type": "image"]]))
                + (try Self.call("d", "Bash", ["command": "d"], output: "")))
        let outputs = Self.calls(value.entries.first).map(\.output)
        #expect(outputs[0]?.lineCount == 10)
        #expect(outputs[0]?.tail == "line 7\nline 8\nline 9\nline 10")
        #expect(outputs[1]?.lineCount == 8)
        #expect(outputs[1]?.tail == nil)
        #expect(outputs[2]?.text == "[Image]")
        #expect(outputs[3] == nil)
    }

    @Test func unreadableLinesAreCountedWithTheirNumbers() throws {
        let lines = [
            "claude: stream interrupted, reconnecting",
            try Self.json(["type": "stream_event", "event": [:]]),
            try Self.json(["type": "assistant"]),
            try Self.user([Self.toolResult("never-called", "ok")]),
            try Self.assistant([["type": "server_tool_use", "id": "x"]]),
            try Self.json(["type": "result", "result": "no is_error"]),
            "",
            try Self.assistant([Self.text("Still here.")]),
            "[1,2]",
        ]
        let value = Self.read(lines)
        #expect(value.unreadable.map(\.id) == [1, 2, 3, 4, 5, 6, 9])
        #expect(value.unreadable[0].text == "claude: stream interrupted, reconnecting")
        // An empty line is no record, and a record with an unknown block
        // adds none of its blocks.
        #expect(value.entries.count == 1)
        #expect(value.lines.count == 9)
        #expect(value.result == nil)
    }

    @Test func aSubagentsOwnTurnsAreLeftToItsParentCall() throws {
        let value = Self.read(
            [
                try Self.assistant([Self.toolUse("task", "Task", ["description": "Survey"])]),
                try Self.assistant([Self.text("Subagent prose.")], parent: "task"),
                try Self.assistant([Self.toolUse("sub", "Bash", ["command": "ls"])], parent: "task"),
                try Self.user([Self.toolResult("sub", "a\nb")], parent: "task"),
                try Self.user([Self.toolResult("task", "Found two files.")]),
            ])
        #expect(value.unreadable.isEmpty)
        let calls = Self.calls(value.entries.first)
        #expect(calls.map(\.verb) == ["Task"])
        #expect(calls.first?.output?.text == "Found two files.")
    }

    /// #1926 reuses the sheet for a running agent, so a stream cut off
    /// mid-record parses without counting the cut line as unreadable.
    @Test func aGrowingStreamParsesWithoutItsPartialLine() throws {
        let complete = try Self.assistant([Self.text("Working on it.")])
        let cut = Array((complete + "\n" + #"{"type":"assistant","mess"#).utf8)
        let growing = Presentation(bytes: cut, streamEnded: false)
        #expect(growing.endsWithPartialLine)
        #expect(growing.unreadable.isEmpty)
        #expect(growing.entries.count == 1)
        #expect(growing.result == nil)
        #expect(growing.lines.count == 2)
        #expect(growing.failedEnding == "The run ended without a final message.")

        // Once the stream has ended, the cut line is a killed writer's
        // last record, and the conversation counts it as unreadable.
        let killed = Presentation(bytes: cut, streamEnded: true)
        #expect(!killed.endsWithPartialLine)
        #expect(killed.unreadable.map(\.id) == [2])
        #expect(killed.entries.count == 1)

        // A whole last line with no newline yet is an ordinary record.
        let whole = Self.read([complete], trailingNewline: false, streamEnded: false)
        #expect(!whole.endsWithPartialLine)
        #expect(whole.entries.count == 1)

        // The same fragment once its line has ended is unreadable.
        let ended = Presentation(bytes: Array((#"{"type":"assistant","mess"# + "\n").utf8), streamEnded: false)
        #expect(!ended.endsWithPartialLine)
        #expect(ended.unreadable.map(\.id) == [1])
    }

    /// While the stream grows, a last call with no result is the call under
    /// way: it leaves its group and takes a present-tense verb. A call whose
    /// empty result arrived isn't under way, and an ended stream has none.
    @Test func aGrowingStreamsLastCallWithoutAResultIsUnderWay() throws {
        let earlier = try Self.call("t1", "Read", ["file_path": "/workspace/rig/run.go"])
        for (name, input, verb, identifier) in [
            ("Bash", ["command": "go test ./rig/..."], "Running", "go test ./rig/..."),
            ("Read", ["file_path": "/workspace/rig/rig.go"], "Reading", "rig/rig.go"),
            ("Edit", ["file_path": "/workspace/rig/rig.go"], "Editing", "rig/rig.go"),
            ("Grep", ["pattern": "Probe"], "Searching", "Probe"),
            ("Task", [:], "Task", nil),
        ] as [(String, [String: Any], String, String?)] {
            let lines = earlier + [try Self.assistant([Self.toolUse("t2", name, input)])]
            let growing = Self.read(lines, streamEnded: false)
            #expect(growing.underWay?.verb == verb)
            #expect(growing.underWay?.identifier == identifier)
            #expect(Self.calls(growing.entries.last).map(\.verb) == ["Read"])

            let ended = Self.read(lines)
            #expect(ended.underWay == nil)
            #expect(Self.calls(ended.entries.last).count == 2)
        }

        let answered = Self.read(
            earlier + (try Self.call("t2", "Bash", ["command": "true"], output: "")), streamEnded: false)
        #expect(answered.underWay == nil)

        let message = Self.read(earlier + [try Self.assistant([Self.text("Done reading.")])], streamEnded: false)
        #expect(message.underWay == nil)
    }

    /// One record can hold several blocks, as when the agent calls two
    /// tools at once; each keeps its own id, so its row and fold do too.
    @Test func blocksSharingARecordKeepDistinctIDs() throws {
        let value = Self.read([
            try Self.assistant([
                Self.thinking("Both files first."),
                Self.toolUse("t1", "Read", ["file_path": "/workspace/a.go"]),
                Self.toolUse("t2", "Read", ["file_path": "/workspace/b.go"]),
            ]),
            try Self.user([Self.toolResult("t1", "package a"), Self.toolResult("t2", "package b")]),
        ])
        #expect(value.unreadable.isEmpty)
        guard case .reasoning(let reasoning) = value.entries.first else {
            Issue.record("expected the reasoning first")
            return
        }
        #expect(reasoning.id == 0)
        let calls = Self.calls(value.entries.last)
        #expect(calls.map(\.id) == [1, 2])
        #expect(calls.map(\.output?.text) == ["package a", "package b"])
    }

    @Test func aFailedRunSaysHowItsStreamEnded() throws {
        #expect(Self.read([]).failedEnding == "The run ended without a final message.")
        #expect(
            Self.read([try Self.result("Done.")]).failedEnding
                == "The agent sent a final message, but the invocation failed.")
        #expect(
            Self.read([try Self.result(nil, isError: true, subtype: "error_max_turns")]).failedEnding
                == "The agent stopped at its turn limit.")
        #expect(
            Self.read([try Self.result(nil, isError: true, subtype: "error_during_execution")]).failedEnding
                == "The agent's final record reports an error.")
    }

    @Test func aFinalMessageThatDiffersFromTheLastMessageKeepsBoth() throws {
        let value = Self.read([
            try Self.assistant([Self.text("Checking one more thing.")]), try Self.result("All done."),
        ])
        #expect(value.entries.count == 1)
        #expect(value.result?.message == "All done.")
    }

    // MARK: Structured Output

    private static var answer: [String: Any] {
        ["specification": ["summary": "S", "body": "a/b\nc", "addressals": [Any]()], "reply": NSNull()]
    }

    private static func callNames(_ value: Presentation) -> [String] {
        value.entries.flatMap { Self.calls($0) }.map(\.verb)
    }

    @Test func aStructuredAnswerIsShownOnceAsFormattedJSON() throws {
        // The CLI's serialization of the answer, compact and in no set key
        // order, is the same value as the answer, so it isn't shown again.
        let serialized = String(
            decoding: try JSONSerialization.data(withJSONObject: Self.answer), as: UTF8.self)
        var lines = [try Self.assistant([Self.text("I'll read the runner first.")])]
        lines += try Self.call("t1", "Read", ["file_path": "/workspace/rig/runner.go"])
        lines += try Self.call("t2", "StructuredOutput", Self.answer, output: "Structured output provided successfully")
        lines.append(try Self.result(serialized, structuredOutput: Self.answer))
        let value = Self.read(lines)
        #expect(value.unreadable.isEmpty)
        #expect(
            value.result?.answer == """
                {
                  "reply" : null,
                  "specification" : {
                    "addressals" : [],
                    "body" : "a/b\\nc",
                    "summary" : "S"
                  }
                }
                """)
        #expect(value.result?.message == nil)
        #expect(Self.callNames(value) == ["Read"])
    }

    @Test func aResultWithoutStructuredOutputKeepsItsTextEvenWhenItIsJSON() throws {
        for structuredOutput: Any? in [nil, NSNull()] {
            let value = Self.read([try Self.result(#"{"reply":"hi"}"#, structuredOutput: structuredOutput)])
            #expect(value.result?.message == #"{"reply":"hi"}"#)
            #expect(value.result?.answer == nil)
        }
    }

    @Test func aStructuredOutputCallWithAnotherInputStays() throws {
        let value = Self.read(
            (try Self.call("t1", "StructuredOutput", ["reply": 1], output: "Invalid output", isError: true))
                + (try Self.call("t2", "StructuredOutput", Self.answer))
                + [try Self.result(nil, structuredOutput: Self.answer)])
        let calls = value.entries.flatMap { Self.calls($0) }
        #expect(calls.map(\.verb) == ["StructuredOutput"])
        #expect(calls.first?.failed == true)
        #expect(value.result?.answer != nil)
    }

    @Test func aProseResultBesideStructuredOutputKeepsBoth() throws {
        let value = Self.read([try Self.result("Here is the draft.", structuredOutput: Self.answer)])
        #expect(value.result?.message == "Here is the draft.")
        #expect(value.result?.answer != nil)
    }
}
