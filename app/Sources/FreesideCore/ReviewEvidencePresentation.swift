import Foundation

/// A reading aid for retained reviewer claims. It supplies no review verdict or evidence authority.
struct ReviewEvidencePresentation: Sendable, Equatable {
    enum Conclusion: Sendable, Equatable {
        case noFindings
        case findings([Finding])
        case absent
        case unreadable
    }

    struct Finding: Decodable, Sendable, Equatable {
        let severity: String
        let location: Location
        let explanation: String

        struct Location: Decodable, Sendable, Equatable {
            let path: String
            let startLine: Int?
            let endLine: Int?
            let wholeFile: Bool?

            enum CodingKeys: String, CodingKey {
                case path
                case startLine = "start_line"
                case endLine = "end_line"
                case wholeFile = "whole_file"
            }

            var label: String {
                if wholeFile == true { return "\(path) · Whole file" }
                return "\(path) · Lines \(startLine ?? 0)–\(endLine ?? 0)"
            }
        }
    }

    struct Entry: Identifiable, Sendable, Equatable {
        let id: Int
        let kind: Kind
        let text: String
        var command: String? = nil
        var exitCode: Int? = nil
        var status: String? = nil
        var invalidUTF8 = false

        enum Kind: String, Sendable {
            case message = "Agent message"
            case command = "Command execution"
            case reasoning = "Reviewer note"
            case error = "Reviewer error"
            case lifecycle = "Turn activity"
            case diagnostic = "Diagnostic or unsupported output"
            case accessCheck = "Freeside access check"
        }

        var isDiagnostic: Bool { kind == .diagnostic || kind == .accessCheck }
    }

    let conclusion: Conclusion
    let entries: [Entry]
    let explanation: String?
    let terminalState: String?
    let exitStatus: Int?

    var messageCount: Int { entries.filter { $0.kind == .message }.count }
    var commandCount: Int { entries.filter { $0.kind == .command }.count }
    var diagnosticCount: Int { entries.filter(\.isDiagnostic).count }

    init(events: [UInt8], result: [UInt8]?, exitStatus: Int?) {
        conclusion = Self.conclusion(from: result)
        self.exitStatus = exitStatus
        var entries: [Entry] = []
        var itemIndices: [String: Int] = [:]
        var explanation: String?
        var terminalState: String?
        let decoder = JSONDecoder()
        for (lineNumber, bytes) in events.split(separator: 10, omittingEmptySubsequences: false).enumerated() {
            // Empty separators have no activity; the raw view preserves them.
            guard !bytes.isEmpty else { continue }
            let text = String(decoding: bytes, as: UTF8.self)
            let invalid = String(bytes: bytes, encoding: .utf8) == nil
            let event =
                invalid || !Self.hasUniqueObjectKeys(Array(bytes))
                ? nil : try? decoder.decode(Event.self, from: Data(bytes))
            var entry = Entry(id: lineNumber, kind: .diagnostic, text: text, invalidUTF8: invalid)
            var itemID: String?
            if let event {
                switch event.type {
                case "thread.started":
                    entry = Entry(id: lineNumber, kind: .lifecycle, text: "Review session started")
                case "turn.started":
                    terminalState = nil
                    entry = Entry(id: lineNumber, kind: .lifecycle, text: "Turn started")
                case "turn.completed":
                    terminalState = "Turn completed"
                    entry = Entry(id: lineNumber, kind: .lifecycle, text: "Turn completed")
                case "turn.failed":
                    let message = event.error?.message ?? "No failure message retained"
                    terminalState = "Turn failed: \(message)"
                    entry = Entry(id: lineNumber, kind: .lifecycle, text: "Turn failed: \(message)")
                case "item.started", "item.updated", "item.completed":
                    if let item = event.item {
                        switch item.type {
                        case "agent_message", "reasoning":
                            if let text = item.text {
                                let structured = Self.conclusion(from: Array(text.utf8))
                                let isResult: Bool
                                let readableText: String
                                switch structured {
                                case .noFindings:
                                    isResult = true
                                    readableText = "The reviewer reported no findings in this message."
                                case .findings(let findings):
                                    isResult = true
                                    readableText = findings.map {
                                        "\($0.severity) · \($0.location.label)\n\($0.explanation)"
                                    }.joined(separator: "\n\n")
                                case .absent, .unreadable:
                                    isResult = false
                                    readableText = text
                                }
                                entry = Entry(
                                    id: lineNumber, kind: item.type == "agent_message" ? .message : .reasoning,
                                    text: readableText)
                                if item.type == "agent_message" {
                                    explanation =
                                        isResult || text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                                        ? nil : text
                                }
                            }
                        case "command_execution":
                            if let command = item.command {
                                entry = Entry(
                                    id: lineNumber, kind: .command, text: item.aggregatedOutput ?? "",
                                    command: command, exitCode: item.exitCode, status: item.status)
                            }
                        case "error":
                            if let message = item.message {
                                entry = Entry(id: lineNumber, kind: .error, text: message)
                            }
                        default: break
                        }
                        if !entry.isDiagnostic { itemID = item.id }
                    }
                default: break
                }
            } else if !invalid,
                text.range(
                    of: #"^freeside-review-access-v1 base=[0-9a-f]{40} head=[0-9a-f]{40} cwd=.+$"#,
                    options: .regularExpression) != nil
            {
                entry = Entry(id: lineNumber, kind: .accessCheck, text: text)
            }
            if let itemID, let index = itemIndices[itemID] {
                entries[index] = entry
            } else {
                if let itemID { itemIndices[itemID] = entries.count }
                entries.append(entry)
            }
        }
        self.entries = entries
        self.explanation = explanation
        self.terminalState = terminalState
    }

    private struct Event: Decodable {
        let type: String
        var item: Item?
        var error: Failure?

        struct Failure: Decodable { let message: String }
        struct Item: Decodable {
            let type: String
            var id: String?
            var text: String?
            var command: String?
            var aggregatedOutput: String?
            var exitCode: Int?
            var status: String?
            var message: String?

            enum CodingKeys: String, CodingKey {
                case type, id, text, command, status, message
                case aggregatedOutput = "aggregated_output"
                case exitCode = "exit_code"
            }
        }
    }

    private static func conclusion(from bytes: [UInt8]?) -> Conclusion {
        guard let bytes, !bytes.isEmpty else { return .absent }
        let data = Data(bytes)
        // Match the findings schema, including its closed object shapes. An unfamiliar
        // result remains unreadable rather than becoming a claim of a clean review.
        guard String(bytes: bytes, encoding: .utf8) != nil,
            let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
            hasUniqueObjectKeys(bytes),
            Set(object.keys) == ["findings"],
            let rawFindings = object["findings"] as? [[String: Any]],
            rawFindings.allSatisfy({ finding in
                guard Set(finding.keys) == ["severity", "location", "explanation"],
                    let location = finding["location"] as? [String: Any]
                else { return false }
                return Set(location.keys) == ["path", "start_line", "end_line"]
                    || Set(location.keys) == ["path", "whole_file"]
            }),
            let decoded = try? JSONDecoder().decode(Result.self, from: data),
            decoded.findings.allSatisfy({ finding in
                guard ["P0", "P1", "P2", "P3"].contains(finding.severity) else { return false }
                let location = finding.location
                if location.wholeFile == true { return true }
                guard let start = location.startLine, let end = location.endLine else { return false }
                return start >= 1 && end >= start
            })
        else { return .unreadable }
        return decoded.findings.isEmpty ? .noFindings : .findings(decoded.findings)
    }

    private struct Result: Decodable { let findings: [Finding] }

    /// Foundation handles the JSON grammar, but silently discards duplicate keys.
    /// Scan only object keys before interpreting claims, including escaped spellings.
    private static func hasUniqueObjectKeys(_ bytes: [UInt8]) -> Bool {
        var objects: [Set<String>] = []
        var index = 0
        while index < bytes.count {
            switch bytes[index] {
            case 123: objects.append([])
            case 125:
                if !objects.isEmpty { objects.removeLast() }
            case 34:
                let start = index
                index += 1
                while index < bytes.count, bytes[index] != 34 {
                    if bytes[index] == 92 { index += 1 }
                    index += 1
                }
                guard index < bytes.count else { return false }
                let end = index
                var next = index + 1
                while next < bytes.count, [9, 10, 13, 32].contains(bytes[next]) { next += 1 }
                if next < bytes.count, bytes[next] == 58, !objects.isEmpty {
                    guard let key = try? JSONDecoder().decode(String.self, from: Data(bytes[start...end])),
                        objects[objects.count - 1].insert(key).inserted
                    else { return false }
                }
            default: break
            }
            index += 1
        }
        return true
    }
}
