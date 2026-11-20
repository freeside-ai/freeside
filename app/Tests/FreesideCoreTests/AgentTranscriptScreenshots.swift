#if os(macOS)
    import Foundation
    import SwiftUI

    @testable import FreesideCore
    @testable import FreesideModel

    /// The agent transcript sheet's states for the screenshot suite (#1131),
    /// drawn from a synthetic transcript shaped like the Claude CLI's stream
    /// JSON. The messages, calls, outputs, and times follow the design
    /// frames (`proposal-agent-transcript-*` in `freeside-design`) and are
    /// illustrative. No real transcript is committed: one is sensitive
    /// evidence.
    @MainActor enum AgentTranscriptScreenshots {
        enum State: CaseIterable {
            /// A finished run folded behind `Worked for`.
            case finished
            /// The same run with the work opened: one group to its calls and
            /// one call to its output.
            case open
            /// A failed run with no final message: nothing folds.
            case failed
            /// A long finished run, opened and clipped at its end, its last
            /// call's long output showing its last lines.
            case end
            /// A finished run with two lines Freeside could not read, opened.
            case unreadable
            /// The raw stream, from its head.
            case raw

            var surface: String {
                switch self {
                case .finished: "agent-transcript-finished"
                case .open: "agent-transcript-open"
                case .failed: "agent-transcript-failed"
                case .end: "agent-transcript-end"
                case .unreadable: "agent-transcript-unreadable"
                case .raw: "agent-transcript-raw"
                }
            }
        }

        static let digest = "sha256:" + String(repeating: "a1", count: 32)

        /// The sheet in `state`, laid out in place of its scroll view.
        static func sheet(_ state: State) -> AnyView {
            let bytes = Array(transcript(state).utf8)
            let presentation = AgentTranscriptPresentation(bytes: bytes, streamEnded: true)
            let sheet = AgentTranscriptSheet(
                digest: digest, bytes: bytes, mediaType: "application/jsonl",
                title: "Implementation · Attempt 1", outcome: state == .failed ? .failed : .finished,
                workingTime: state == .failed ? 9 * 60 : 16 * 60, prompt: state == .raw ? nil : prompt,
                folds: AgentTranscriptFoldPreferences(defaults: nil, open: [digest: openFolds(state, presentation)]),
                presentation: presentation, showsRaw: state == .raw, rendersScrollableContent: false,
                screenshotHeight: state == .end || state == .raw ? 560 : nil)
            return AnyView(sheet)
        }

        private static func openFolds(_ state: State, _ presentation: AgentTranscriptPresentation) -> Set<String> {
            let works = presentation.entries.compactMap { entry -> AgentTranscriptPresentation.Work? in
                if case .work(let work) = entry { return work }
                return nil
            }
            switch state {
            case .finished, .failed, .raw:
                return []
            case .unreadable:
                return ["unreadable"]
            case .open:
                // The second group, and its failed test run's output.
                guard works.count > 1, let failed = works[1].calls.first(where: \.failed) else { return ["worked"] }
                return ["worked", "work-\(works[1].id)", "call-\(failed.id)"]
            case .end:
                guard let last = works.last, let call = last.calls.last else { return ["worked"] }
                return ["worked", "work-\(last.id)", "call-\(call.id)"]
            }
        }

        // MARK: The Transcript

        /// The prompt the writer received, which the daemon serves beside
        /// the stream (rule 2). Raw is the stream only, so it shows none.
        private static let prompt =
            "Fix the flaky acceptance rig: the probe fires before the fixture server listens. Keep the change to rig/."
        private static let m1 = "I’ll read the rig’s runner and the failing acceptance test first."
        private static let m2 =
            "The rig fails because the probe fires before the fixture server is listening. I’ll move the bind ahead of the probe and add a test for the order."
        private static let m3 =
            "The new test fails as expected before the fix and passes after it. Checking formatting and the rest of the package."
        private static let final =
            "The acceptance rig now starts its fixture server before the first probe, and the three rig tests pass. I did not run the full suite, because it needs the staging database."
        private static let thought =
            "The retry loop hides the race on a fast machine, so the test needs to hold the listener back until the probe has fired once."
        private static let testFailure = """
            Exit code 1
            --- FAIL: TestProbeWaitsForListener (2.01s)
                runner_test.go:88: probe returned before listen:
                    dial tcp 127.0.0.1:41873: connect: connection refused
            FAIL
            FAIL    freeside.example/rig    2.214s
            """

        static func transcript(_ state: State) -> String {
            var lines = [
                record([
                    "type": "system", "subtype": "init", "cwd": "/workspace", "model": "claude-opus-5-5",
                    "tools": ["Task", "Bash", "Edit", "Read", "Write", "Grep", "Glob"],
                ]),
                say(m1),
            ]
            lines += call("t1", "Read", ["file_path": "/workspace/rig/runner.go"], "package rig")
            lines += call("t2", "Read", ["file_path": "/workspace/rig/probe.go"], "package rig")
            lines += call("t3", "Read", ["file_path": "/workspace/rig/runner_test.go"], "package rig")
            lines += call("t4", "Bash", ["command": "go test ./rig/..."], testFailure, isError: true)
            lines += call("t5", "Bash", ["command": "git log --oneline -3 rig/"], "4f1c2e9 Move fixture server")
            lines += [say(m2), think(thought)]
            lines += call("t6", "Edit", ["file_path": "/workspace/rig/runner.go"], "The file has been updated.")
            lines += call("t7", "Edit", ["file_path": "/workspace/rig/runner_test.go"], "The file has been updated.")
            lines += call("t8", "Bash", ["command": "go test ./rig/..."], testFailure, isError: true)
            if state == .failed { return lines.joined(separator: "\n") + "\n" }
            lines += call("t9", "Bash", ["command": "gofmt -l rig/"], "")
            lines += call("t10", "Bash", ["command": "go test ./rig/..."], "ok  \tfreeside.example/rig\t2.281s")
            lines += call("t11", "Bash", ["command": "git status --short"], " M rig/runner.go\n M rig/runner_test.go")
            lines += [say(m3)]
            if state == .end {
                for round in 1...12 {
                    lines += call("r\(round)a", "Bash", ["command": "go vet ./rig/..."], "")
                    lines += call("r\(round)b", "Grep", ["pattern": "listen"], "rig/runner.go:41")
                    lines += [say("Round \(round) of checks is clean; tightening the probe’s retry next.")]
                }
                let long =
                    (1...2_114).map { "=== RUN   TestCase\($0)" } + [
                        "ok      freeside.example/rig/probe      0.412s",
                        "ok      freeside.example/rig/runner     2.281s",
                        "ok      freeside.example/rig/fixture    0.118s",
                        "PASS",
                    ]
                lines += call("e1", "Bash", ["command": "gofmt -l rig/"], "")
                lines += call("e2", "Bash", ["command": "go test ./... -count=1"], long.joined(separator: "\n"))
            } else {
                lines += call("t12", "Bash", ["command": "go vet ./rig/..."], "")
                lines += call("t13", "Bash", ["command": "go test ./rig/... -run Probe"], "ok")
                lines += call("t14", "Bash", ["command": "go test ./rig/..."], "ok")
                lines += call("t15", "Grep", ["pattern": "probe.Start"], "rig/runner.go:52")
                lines += call("t16", "Glob", ["pattern": "rig/**/*_test.go"], "rig/runner_test.go")
                for (index, tool) in ["TodoWrite", "TodoWrite", "WebFetch", "TodoWrite"].enumerated() {
                    lines += call("o\(index)", tool, [:], "ok")
                }
            }
            if state == .unreadable {
                lines.insert("claude: stream interrupted, reconnecting", at: 12)
                lines.append(record(["type": "stream_event", "event": ["type": "message_delta"]]))
            }
            lines += [
                say(final), record(["type": "result", "subtype": "success", "is_error": false, "result": final]),
            ]
            return lines.joined(separator: "\n") + "\n"
        }

        private static func record(_ object: [String: Any]) -> String {
            // Every fixture object is plain JSON, so this cannot fail.
            let data = (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])) ?? Data()
            return String(decoding: data, as: UTF8.self)
        }

        private static func assistant(_ block: [String: Any]) -> String {
            record([
                "type": "assistant", "parent_tool_use_id": NSNull(),
                "message": ["role": "assistant", "content": [block]],
            ])
        }

        private static func say(_ text: String) -> String { assistant(["type": "text", "text": text]) }

        private static func think(_ text: String) -> String {
            assistant(["type": "thinking", "thinking": text, "signature": "sig"])
        }

        private static func call(
            _ id: String, _ name: String, _ input: [String: Any], _ output: String, isError: Bool = false
        ) -> [String] {
            [
                assistant(["type": "tool_use", "id": id, "name": name, "input": input]),
                record([
                    "type": "user", "parent_tool_use_id": NSNull(),
                    "message": [
                        "role": "user",
                        "content": [
                            ["type": "tool_result", "tool_use_id": id, "content": output, "is_error": isError]
                        ],
                    ],
                ]),
            ]
        }
    }
#endif
