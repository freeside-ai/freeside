import Foundation

/// A reading aid for a writer's retained transcript: the Claude CLI's
/// stream JSON, one record per line. It grants nothing. Everything in it is
/// the agent's own account, and the raw bytes stay the evidence of record,
/// so the sheet that draws this keeps them one link away.
///
/// The parse accepts a stream that is still growing (#1926): a last line
/// with no newline that doesn't decode is held as partial, not unreadable,
/// and a stream with no final message is a valid stream. Once the stream
/// has ended, that line is a record cut off by a killed writer, and it
/// counts as unreadable, so the conversation still names it.
///
/// A run launched with a JSON Schema (#3568) answers through the CLI's
/// `StructuredOutput` tool, and its `result` record carries that answer in
/// `structured_output`; the presentation shows it once, formatted, in place
/// of the serialized copy in the record's text and the tool call.
package struct AgentTranscriptPresentation: Sendable, Equatable {
    /// One tool call and, once its result arrives, the result.
    package struct Call: Identifiable, Sendable, Equatable {
        package enum Kind: Sendable, Equatable {
            case read, edit, run, search, other
        }

        /// Its place among the conversation's items, in stream order. One
        /// record can hold several blocks, so a line number would repeat.
        package let id: Int
        package let kind: Kind
        /// `Ran`, `Read`, `Edited`, `Searched`, or the tool's own name.
        package let verb: String
        /// The command, the path relative to the repository, or the
        /// pattern; nil for a tool this parser has no identifier for.
        package let identifier: String?
        package fileprivate(set) var output: Output?
        package fileprivate(set) var failed = false
        /// A failed command's exit code, read from its result's
        /// `Exit code N` first line.
        package fileprivate(set) var exitCode: Int?
    }

    /// A call's result text, with its line count, so the sheet can show
    /// the end of a long one without splitting it on every draw.
    package struct Output: Sendable, Equatable {
        package let text: String
        package let lineCount: Int
        /// The last `tailLineLimit` lines, set only when the output is
        /// longer than `longOutputLineCount`.
        package let tail: String?

        package static let longOutputLineCount = 8
        package static let tailLineLimit = 4

        init(_ text: String) {
            self.text = text
            let lines = text.split(separator: "\n", omittingEmptySubsequences: false)
            lineCount = lines.count
            tail =
                lines.count > Self.longOutputLineCount
                ? lines.suffix(Self.tailLineLimit).joined(separator: "\n") : nil
        }
    }

    package struct Reasoning: Identifiable, Sendable, Equatable {
        package let id: Int
        package let text: String

        /// The first line with text in it, for the one-line preview.
        package var preview: String {
            text.split(separator: "\n").lazy
                .map { $0.trimmingCharacters(in: .whitespaces) }
                .first { !$0.isEmpty } ?? ""
        }
    }

    /// A run of calls with no agent message between them. Reasoning that
    /// falls between two of its calls stays inside it, in order.
    package struct Work: Identifiable, Sendable, Equatable {
        package enum Step: Identifiable, Sendable, Equatable {
            case call(Call)
            case reasoning(Reasoning)

            package var id: Int {
                switch self {
                case .call(let call): call.id
                case .reasoning(let reasoning): reasoning.id
                }
            }
        }

        package let steps: [Step]

        package var id: Int { steps[0].id }

        package var calls: [Call] {
            steps.compactMap { step in
                if case .call(let call) = step { return call }
                return nil
            }
        }

        /// The dim sentence a group of more than one call collapses to,
        /// `Read 3 files, ran 2 commands, 4 other actions`; nil for a group
        /// of one call, which shows that call's own line.
        package var summary: String? {
            let calls = calls
            guard calls.count > 1 else { return nil }
            func distinct(_ kind: Call.Kind) -> Int {
                Set(calls.filter { $0.kind == kind }.map { $0.identifier ?? "" }).count
            }
            func count(_ kind: Call.Kind) -> Int { calls.filter { $0.kind == kind }.count }
            func phrase(_ number: Int, _ verb: String, _ noun: String) -> String? {
                guard number > 0 else { return nil }
                return "\(verb) \(number) \(noun)\(number == 1 ? "" : "s")"
            }
            let others = count(.other)
            let parts = [
                phrase(distinct(.read), "read", "file"),
                phrase(distinct(.edit), "edited", "file"),
                phrase(count(.run), "ran", "command"),
                phrase(count(.search), "searched", "pattern"),
                others > 0 ? "\(others) other action\(others == 1 ? "" : "s")" : nil,
            ].compactMap { $0 }
            let sentence = parts.joined(separator: ", ")
            return sentence.prefix(1).uppercased() + sentence.dropFirst()
        }
    }

    package enum Entry: Identifiable, Sendable, Equatable {
        case message(id: Int, text: String)
        case reasoning(Reasoning)
        case work(Work)

        package var id: Int {
            switch self {
            case .message(let id, _): id
            case .reasoning(let reasoning): reasoning.id
            case .work(let work): work.id
            }
        }
    }

    /// The stream's closing `result` record.
    package struct Result: Sendable, Equatable {
        /// The agent's final message; nil when the record carries none, or
        /// when its text is only the structured answer serialized.
        package let message: String?
        /// A schema-constrained run's answer, the record's non-null
        /// `structured_output`, pretty-printed with sorted keys, because
        /// Foundation's parse doesn't keep the model's key order. Nil for a
        /// prose run, even when its text happens to be JSON.
        package let answer: String?
        package let isError: Bool
        /// The CLI's subtype, such as `success` or `error_max_turns`.
        package let subtype: String?
    }

    /// A line kept as written, with its 1-based number.
    package struct Line: Identifiable, Sendable, Equatable {
        package let id: Int
        package let text: String
    }

    /// The conversation before the final message, oldest first.
    package let entries: [Entry]
    /// While the stream is still growing, the call the agent is making now:
    /// the stream's last item, when it is a call with no result yet. It is
    /// left out of `entries` and carries a present-tense verb (`Running`,
    /// `Reading`). Always nil once the stream has ended.
    package let underWay: Call?
    /// Nil while the stream is still growing, or when it ended without one.
    package let result: Result?
    /// Every line that isn't a known conversation or bookkeeping record.
    package let unreadable: [Line]
    /// Every line of the stream, for the raw view; a trailing newline adds
    /// no empty line.
    package let lines: [String]
    package let byteCount: Int
    /// True when the stream is still growing and its last line has no
    /// newline yet and doesn't decode: a record still being written.
    package let endsWithPartialLine: Bool

    /// The dim sentence under a failed run's `Failed after`, saying how
    /// the stream ended.
    package var failedEnding: String {
        guard let result else { return "The run ended without a final message." }
        if !result.isError { return "The agent sent a final message, but the invocation failed." }
        if result.subtype == "error_max_turns" { return "The agent stopped at its turn limit." }
        return "The agent's final record reports an error."
    }

    /// Where the writer's workspace is mounted (`workspaceDir` in
    /// `daemon/internal/exec/claude/spec.go`), stripped so paths read
    /// relative to the repository.
    static let workspacePrefix = "/workspace"

    /// Records the stream carries for the CLI's own bookkeeping, not the
    /// conversation. `usage_driver.cjs` adds the control lines.
    private static let bookkeepingTypes: Set<String> = [
        "system", "control_request", "control_response", "rate_limit_event",
    ]

    /// `streamEnded` says the writer is done, as it is for every
    /// transcript a card carries; only a running agent's stream (#1926)
    /// passes false.
    package init(bytes: [UInt8], streamEnded: Bool) {
        byteCount = bytes.count
        var segments = bytes.split(separator: 10, omittingEmptySubsequences: false)
        if bytes.last == 10 { segments.removeLast() }
        lines = segments.map { String(decoding: $0, as: UTF8.self) }

        var builder = Builder()
        var unreadable: [Line] = []
        var partial = false
        for (index, segment) in segments.enumerated() {
            let number = index + 1
            // An empty line carries nothing; the raw view still shows it.
            guard !segment.isEmpty else { continue }
            let object =
                (try? JSONSerialization.jsonObject(with: Data(segment))) as? [String: Any]
            guard let object else {
                if !streamEnded, index == segments.count - 1, bytes.last != 10 {
                    partial = true
                } else {
                    unreadable.append(Line(id: number, text: lines[index]))
                }
                continue
            }
            if !builder.apply(object) {
                unreadable.append(Line(id: number, text: lines[index]))
            }
        }
        let (entries, result, underWay) = builder.finish(streamEnded: streamEnded)
        self.entries = entries
        self.underWay = underWay
        self.result = result
        self.unreadable = unreadable
        endsWithPartialLine = partial
    }

    /// Accumulates records in stream order; each `apply` either takes a
    /// whole record or none of it.
    private struct Builder {
        enum Item {
            case message(id: Int, text: String)
            case reasoning(Reasoning)
            case call(index: Int)
        }

        var items: [Item] = []
        var calls: [Call] = []
        var callIndexByToolUseID: [String: Int] = [:]
        /// The calls a result has answered, an empty one included.
        var answered: Set<Int> = []
        /// Each `StructuredOutput` call's input, formatted as an answer is,
        /// by call index, so the call that carried the answer is dropped.
        var structuredInputs: [Int: String] = [:]
        var result: Result?
        /// The next item's id: an ordinal, so blocks that share a record
        /// keep distinct ids and fold keys.
        var nextID = 0

        /// False when the record isn't one this parser reads, so its line
        /// is counted as unreadable.
        mutating func apply(_ object: [String: Any]) -> Bool {
            guard let type = object["type"] as? String else { return false }
            if AgentTranscriptPresentation.bookkeepingTypes.contains(type) { return true }
            // A subagent's own turns: its parent call's result carries what
            // it found, so the conversation shows that call alone.
            if object["parent_tool_use_id"] is String { return true }
            switch type {
            case "assistant": return applyAssistant(object)
            case "user": return applyUser(object)
            case "result":
                guard let isError = object["is_error"] as? Bool else { return false }
                let answer = Self.formattedJSON(object["structured_output"])
                var message = object["result"] as? String
                // The CLI serializes the answer into the text too; the sheet
                // shows it once, as the answer.
                if let answer, let text = message,
                    Self.formattedJSON(
                        try? JSONSerialization.jsonObject(with: Data(text.utf8), options: .fragmentsAllowed))
                        == answer
                {
                    message = nil
                }
                result = Result(
                    message: message, answer: answer, isError: isError, subtype: object["subtype"] as? String)
                return true
            default: return false
            }
        }

        private mutating func applyAssistant(_ object: [String: Any]) -> Bool {
            guard let message = object["message"] as? [String: Any],
                let content = message["content"] as? [[String: Any]]
            else { return false }
            var newItems: [Item] = []
            var newCalls: [(toolUseID: String, call: Call)] = []
            var newStructuredInputs: [Int: String] = [:]
            var id = nextID
            for block in content {
                defer { id += 1 }
                switch block["type"] as? String {
                case "text":
                    guard let text = block["text"] as? String else { return false }
                    if !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                        newItems.append(.message(id: id, text: text))
                    }
                case "thinking":
                    guard let text = block["thinking"] as? String else { return false }
                    if !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                        newItems.append(.reasoning(Reasoning(id: id, text: text)))
                    }
                case "redacted_thinking":
                    continue
                case "tool_use":
                    guard let toolUseID = block["id"] as? String, let name = block["name"] as? String
                    else { return false }
                    let input = block["input"] as? [String: Any] ?? [:]
                    let index = calls.count + newCalls.count
                    if name == "StructuredOutput" {
                        newStructuredInputs[index] = Self.formattedJSON(block["input"])
                    }
                    newItems.append(.call(index: index))
                    newCalls.append((toolUseID, Self.call(name: name, input: input, id: id)))
                default:
                    return false
                }
            }
            for (toolUseID, call) in newCalls {
                callIndexByToolUseID[toolUseID] = calls.count
                calls.append(call)
            }
            items.append(contentsOf: newItems)
            structuredInputs.merge(newStructuredInputs) { _, new in new }
            nextID = id
            return true
        }

        private mutating func applyUser(_ object: [String: Any]) -> Bool {
            guard let message = object["message"] as? [String: Any] else { return false }
            // A plain string is the prompt; the sheet doesn't draw it yet (#3506).
            if message["content"] is String { return true }
            guard let content = message["content"] as? [[String: Any]] else { return false }
            var results: [(index: Int, output: String, failed: Bool)] = []
            for block in content {
                switch block["type"] as? String {
                case "tool_result":
                    guard let toolUseID = block["tool_use_id"] as? String,
                        let index = callIndexByToolUseID[toolUseID]
                    else { return false }
                    results.append(
                        (index, Self.resultText(block["content"]), block["is_error"] as? Bool ?? false))
                case "text", "image":
                    continue
                default:
                    return false
                }
            }
            for (index, output, failed) in results {
                answered.insert(index)
                calls[index].output = output.isEmpty ? nil : Output(output)
                calls[index].failed = failed
                if failed, calls[index].kind == .run { calls[index].exitCode = Self.exitCode(output) }
            }
            return true
        }

        func finish(streamEnded: Bool) -> ([Entry], Result?, Call?) {
            var items = items
            var underWay: Call?
            if !streamEnded, case .call(let index)? = items.last, !answered.contains(index) {
                items.removeLast()
                underWay = Self.underWay(calls[index])
            }
            // The answer is drawn once, as the answer: a `StructuredOutput`
            // call that carried it goes, and one with any other input, such
            // as an attempt the CLI rejected, stays.
            if let answer = result?.answer {
                items.removeAll { item in
                    if case .call(let index) = item { structuredInputs[index] == answer } else { false }
                }
            }
            // The result repeats the agent's last message; the sheet draws
            // it once, as the final message.
            if let final = result?.message?.trimmingCharacters(in: .whitespacesAndNewlines),
                case .message(_, let last)? = items.last,
                last.trimmingCharacters(in: .whitespacesAndNewlines) == final
            {
                items.removeLast()
            }
            var entries: [Entry] = []
            var run: [Work.Step] = []
            func flush() {
                let firstCall = run.firstIndex { if case .call = $0 { true } else { false } }
                let lastCall = run.lastIndex { if case .call = $0 { true } else { false } }
                for (offset, step) in run.enumerated() {
                    guard case .reasoning(let reasoning) = step else { continue }
                    if let firstCall, offset > firstCall { break }
                    entries.append(.reasoning(reasoning))
                }
                if let firstCall, let lastCall {
                    entries.append(.work(Work(steps: Array(run[firstCall...lastCall]))))
                    for case .reasoning(let reasoning) in run[(lastCall + 1)...] {
                        entries.append(.reasoning(reasoning))
                    }
                }
                run = []
            }
            for item in items {
                switch item {
                case .message(let id, let text):
                    flush()
                    entries.append(.message(id: id, text: text))
                case .reasoning(let reasoning):
                    run.append(.reasoning(reasoning))
                case .call(let index):
                    run.append(.call(calls[index]))
                }
            }
            flush()
            return (entries, result, underWay)
        }

        /// The call with its verb in the present tense, as the call the
        /// agent is making now. A tool with no verb keeps its own name.
        private static func underWay(_ call: Call) -> Call {
            let verb =
                switch call.kind {
                case .run: "Running"
                case .read: "Reading"
                case .edit: "Editing"
                case .search: "Searching"
                case .other: call.verb
                }
            return Call(id: call.id, kind: call.kind, verb: verb, identifier: call.identifier)
        }

        private static func call(name: String, input: [String: Any], id: Int) -> Call {
            func string(_ key: String) -> String? { input[key] as? String }
            switch name {
            case "Bash":
                return Call(id: id, kind: .run, verb: "Ran", identifier: string("command"))
            case "Read":
                return Call(id: id, kind: .read, verb: "Read", identifier: relative(string("file_path")))
            case "Edit", "MultiEdit", "Write":
                return Call(id: id, kind: .edit, verb: "Edited", identifier: relative(string("file_path")))
            case "NotebookEdit":
                return Call(id: id, kind: .edit, verb: "Edited", identifier: relative(string("notebook_path")))
            case "Grep", "Glob":
                return Call(id: id, kind: .search, verb: "Searched", identifier: string("pattern"))
            default:
                return Call(id: id, kind: .other, verb: name, identifier: nil)
            }
        }

        private static func relative(_ path: String?) -> String? {
            guard let path else { return nil }
            if path == workspacePrefix { return "." }
            if path.hasPrefix(workspacePrefix + "/") { return String(path.dropFirst(workspacePrefix.count + 1)) }
            return path
        }

        /// A result's content is a string or a list of blocks; an image
        /// block has no text to show, so it reads as `[Image]`.
        private static func resultText(_ content: Any?) -> String {
            if let text = content as? String { return text }
            guard let blocks = content as? [[String: Any]] else { return "" }
            return blocks.compactMap { block in
                switch block["type"] as? String {
                case "text": block["text"] as? String
                case "image": "[Image]"
                default: nil
                }
            }.joined(separator: "\n")
        }

        /// A parsed JSON value pretty-printed with sorted keys; nil for a
        /// missing value or JSON null. Only values `JSONSerialization` parsed
        /// reach it, and those always encode, so the write can't raise.
        private static func formattedJSON(_ value: Any?) -> String? {
            guard let value, !(value is NSNull),
                let data = try? JSONSerialization.data(
                    withJSONObject: value,
                    options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes, .fragmentsAllowed])
            else { return nil }
            // Foundation writes an empty array or object over three lines,
            // the middle one blank. A raw newline never occurs inside a JSON
            // string, so an opener followed by one is structural.
            return String(decoding: data, as: UTF8.self).replacingOccurrences(
                of: #"([\[{])\n\s*([\]}])"#, with: "$1$2", options: .regularExpression)
        }

        /// The CLI starts a failed command's result with `Exit code N`.
        private static func exitCode(_ output: String) -> Int? {
            let prefix = "Exit code "
            guard output.hasPrefix(prefix) else { return nil }
            return Int(output.dropFirst(prefix.count).prefix { $0 == "-" || $0.isNumber })
        }
    }
}
