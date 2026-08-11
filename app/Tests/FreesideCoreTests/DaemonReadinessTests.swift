import Foundation
import Testing

@testable import FreesideCore

@Suite struct DaemonReadinessTests {
    @Test func validLoopbackReadinessParses() throws {
        let readiness = try DaemonReadiness.parse(
            Data(#"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911"}"#.utf8))

        #expect(readiness.apiURL == URL(string: "http://127.0.0.1:7331"))
        #expect(readiness.pairingCode == "483911")
    }

    @Test(
        arguments: [
            "",
            "{",
            "[]",
            #"{"api_url":"http://127.0.0.1:7331"}"#,
            #"{"api_url":7331,"pairing_code":"483911"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":483911}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","extra":true}"#,
            #"{"api_url":"https://127.0.0.1:7331","pairing_code":"483911"}"#,
            #"{"api_url":"http://daemon.example:7331","pairing_code":"483911"}"#,
            #"{"api_url":"http://127.0.0.1","pairing_code":"483911"}"#,
            #"{"api_url":"http://user@127.0.0.1:7331","pairing_code":"483911"}"#,
            #"{"api_url":"http://127.0.0.1:7331/path","pairing_code":"483911"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"   "}"#,
        ])
    func malformedOrUnsupervisedReadinessIsRejected(raw: String) {
        #expect(throws: DaemonReadiness.ParseError.self) {
            try DaemonReadiness.parse(Data(raw.utf8))
        }
    }

    @Test func absentUnreadableAndMalformedFilesAreNormalAbsence() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let reader = DaemonReadinessReader()

        #expect(reader.read(at: root.appendingPathComponent("absent.json")) == nil)
        #expect(reader.read(at: root) == nil)
        let malformed = root.appendingPathComponent("readiness.json")
        try Data(#"{"api_url":false}"#.utf8).write(to: malformed)
        #expect(reader.read(at: malformed) == nil)
    }

    @Test func onlyUnexpiredReadinessIsReturned() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let file = root.appendingPathComponent("readiness.json")
        try Data(
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911"}"#.utf8
        ).write(to: file)
        let now = Date(timeIntervalSince1970: 1_786_400_000)

        let fresh = DaemonReadinessReader(
            now: { now },
            modificationDate: { _ in now.addingTimeInterval(-599) })
        #expect(fresh.read(at: file)?.pairingCode == "483911")

        for publishedAt in [
            now.addingTimeInterval(-DaemonReadinessReader.pairingCodeLifetime),
            now.addingTimeInterval(1),
        ] {
            let invalid = DaemonReadinessReader(
                now: { now }, modificationDate: { _ in publishedAt })
            #expect(invalid.read(at: file) == nil)
        }
    }
}
