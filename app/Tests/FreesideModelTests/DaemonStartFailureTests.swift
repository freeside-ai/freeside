import Foundation
import Testing

@testable import FreesideModel

@Suite struct DaemonStartFailureTests {
    private let loggedAt = Date(timeIntervalSince1970: 1_795_120_200)

    @Test func returnsStartErrorWhenLastNonEmptyLineHasFreesidedPrefix() throws {
        let log = """
            time=2026-11-26T18:00:00Z level=INFO msg=environment tier=prod
            freesided: listen "100.64.0.1:7331": bind: address already in use

            """
        let file = try Self.logFile(log)
        defer { try? FileManager.default.removeItem(at: file.deletingLastPathComponent()) }
        let reader = DaemonStartFailureReader(modificationDate: { _ in loggedAt })

        let failure = reader.read(at: file)
        #expect(
            failure
                == DaemonStartFailure(
                    message: #"listen "100.64.0.1:7331": bind: address already in use"#,
                    loggedAt: loggedAt,
                    logURL: file))
    }

    @Test func returnsNilWhenStructuredLogLinesFollowAnEarlierError() throws {
        let log = """
            freesided: listen "100.64.0.1:7331": bind: address already in use
            time=2026-11-26T18:01:00Z level=INFO msg=environment tier=prod
            time=2026-11-26T18:01:01Z level=INFO msg="serving same-host loopback twin listener"
            """
        let file = try Self.logFile(log)
        defer { try? FileManager.default.removeItem(at: file.deletingLastPathComponent()) }
        let reader = DaemonStartFailureReader(modificationDate: { _ in loggedAt })

        #expect(reader.read(at: file) == nil)
    }

    @Test func returnsNilForMissingEmptyDirectoryOrBlankPrefix() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let reader = DaemonStartFailureReader(modificationDate: { _ in loggedAt })

        #expect(reader.read(at: root.appendingPathComponent("freesided.log")) == nil)
        #expect(reader.read(at: root) == nil)

        for body in ["", "   \n\n\t\n", "freesided:   \n"] {
            let file = root.appendingPathComponent(UUID().uuidString)
            try Data(body.utf8).write(to: file)
            #expect(reader.read(at: file) == nil)
        }
    }

    @Test func readsOnlyTheTailOfALargeFile() throws {
        let paddingLine = "time=2026-11-26T18:00:00Z level=INFO msg=heartbeat revision=42\n"
        let repeatCount = (DaemonStartFailureReader.maxTailBytes / paddingLine.utf8.count) + 50
        let padding = String(repeating: paddingLine, count: repeatCount)

        let errorAtTail = try Self.logFile(padding + "freesided: bind: permission denied\n")
        defer { try? FileManager.default.removeItem(at: errorAtTail.deletingLastPathComponent()) }
        let reader = DaemonStartFailureReader(modificationDate: { _ in loggedAt })
        #expect(reader.read(at: errorAtTail)?.message == "bind: permission denied")

        // An error before the 64 KiB tail followed only by blank lines must not
        // be read, proving only the tail is inspected.
        let blanks = String(repeating: "\n", count: DaemonStartFailureReader.maxTailBytes + 128)
        let errorBeforeTail = try Self.logFile("freesided: stale error\n" + blanks)
        defer { try? FileManager.default.removeItem(at: errorBeforeTail.deletingLastPathComponent()) }
        #expect(reader.read(at: errorBeforeTail) == nil)

        // A single line longer than the tail that contains the prefix mid-line
        // must not match after seeking into the middle of that line.
        let longLine =
            String(repeating: "x", count: DaemonStartFailureReader.maxTailBytes)
            + "freesided: not-at-line-start"
        let truncatedMidLine = try Self.logFile(longLine)
        defer { try? FileManager.default.removeItem(at: truncatedMidLine.deletingLastPathComponent()) }
        #expect(reader.read(at: truncatedMidLine) == nil)
    }

    private static func logFile(_ body: String) throws -> URL {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        let file = DaemonStartFailureReader.fileURL(inStateDirectory: root)
        try Data(body.utf8).write(to: file)
        return file
    }
}
