import Foundation
import Testing

@testable import FreesideModel

/// The daemon's environment stamp, spliced into every well-formed fixture.
private let stamp = #""environment":"dev","run_id":"0123456789abcdef0123456789abcdef""#

@Suite struct DaemonReadinessTests {
    @Test func validLoopbackReadinessParses() throws {
        let readiness = try DaemonReadiness.parse(
            Data(#"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp)}"#.utf8))

        #expect(readiness.apiURL == URL(string: "http://127.0.0.1:7331"))
        #expect(readiness.pairingCode == "483911")
        #expect(readiness.environment == .dev)
        #expect(readiness.runID == "0123456789abcdef0123456789abcdef")
        #expect(readiness.expiresAt == nil)
    }

    @Test(
        arguments: [
            ("2026-11-19T20:30:00Z", 1_795_120_200.0),
            ("2026-11-19T20:30:00.25Z", 1_795_120_200.25),
            ("2026-11-19T21:30:00+01:00", 1_795_120_200.0),
        ])
    func aFileWithAnExpiryParsesIt(raw: String, epoch: TimeInterval) throws {
        let readiness = try DaemonReadiness.parse(
            Data(
                #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp),"expires_at":"\#(raw)"}"#
                    .utf8))

        #expect(readiness.pairingCode == "483911")
        #expect(readiness.expiresAt == Date(timeIntervalSince1970: epoch))
    }

    @Test func preStampReadinessIsReportedAsMissingStamp() {
        #expect(throws: DaemonReadiness.ParseError.missingStamp) {
            try DaemonReadiness.parse(
                Data(#"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911"}"#.utf8))
        }
    }

    @Test(
        arguments: [
            "",
            "{",
            "[]",
            #"{"api_url":"http://127.0.0.1:7331"}"#,
            #"{"api_url":7331,"pairing_code":"483911",\#(stamp)}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":483911,\#(stamp)}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp),"extra":true}"#,
            #"{"api_url":"https://127.0.0.1:7331","pairing_code":"483911",\#(stamp)}"#,
            #"{"api_url":"http://daemon.example:7331","pairing_code":"483911",\#(stamp)}"#,
            #"{"api_url":"http://127.0.0.1","pairing_code":"483911",\#(stamp)}"#,
            #"{"api_url":"http://user@127.0.0.1:7331","pairing_code":"483911",\#(stamp)}"#,
            #"{"api_url":"http://127.0.0.1:7331/path","pairing_code":"483911",\#(stamp)}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"   ",\#(stamp)}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","environment":"dev"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","run_id":"r"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","environment":"staging","run_id":"r"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","environment":"dev","run_id":""}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","environment":"dev","run_id":7}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp),"expires_at":"soon"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp),"expires_at":1795638600}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp),"expires_at":"2026-11-19"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","environment":"dev","expires_at":"2026-11-19T20:30:00Z"}"#,
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

        #expect(reader.read(at: root.appendingPathComponent("absent.json"), expecting: .dev) == .absent)
        #expect(reader.read(at: root, expecting: .dev) == .absent)
        let malformed = root.appendingPathComponent("readiness.json")
        try Data(#"{"api_url":false}"#.utf8).write(to: malformed)
        #expect(reader.read(at: malformed, expecting: .dev) == .absent)
    }

    @Test func onlyAnUnexpiredCodeIsReady() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let file = root.appendingPathComponent("readiness.json")
        try Data(
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp)}"#.utf8
        ).write(to: file)
        let now = Date(timeIntervalSince1970: 1_786_400_000)

        let fresh = DaemonReadinessReader(
            now: { now },
            modificationDate: { _ in now.addingTimeInterval(-599) })
        #expect(fresh.read(at: file, expecting: .dev).readiness?.pairingCode == "483911")

        for publishedAt in [
            now.addingTimeInterval(-DaemonReadinessReader.pairingCodeLifetime),
            now.addingTimeInterval(1),
        ] {
            let invalid = DaemonReadinessReader(
                now: { now }, modificationDate: { _ in publishedAt })
            let outcome = invalid.read(at: file, expecting: .dev)
            #expect(outcome.readiness == nil)
            guard case .expired(let expired) = outcome else {
                Issue.record("outcome = \(outcome), want expired")
                continue
            }
            #expect(expired.apiURL == URL(string: "http://127.0.0.1:7331"))
        }
    }

    /// A file's `expires_at` decides the code's lifetime, not the file's
    /// age: a rewritten file keeps its code's expiry, and a code past its
    /// expiry is expired however recently the file was written.
    @Test func aFilesExpiryOverridesItsAge() throws {
        let now = Date(timeIntervalSince1970: 1_795_119_600)
        let file = try Self.readinessFile(
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp),"expires_at":"2026-11-19T20:30:00Z"}"#
        )
        defer { try? FileManager.default.removeItem(at: file.deletingLastPathComponent()) }

        let oldFile = DaemonReadinessReader(
            now: { now }, modificationDate: { _ in now.addingTimeInterval(-3_600) })
        #expect(oldFile.read(at: file, expecting: .dev).readiness?.pairingCode == "483911")

        let pastExpiry = DaemonReadinessReader(
            now: { now.addingTimeInterval(600) }, modificationDate: { _ in now })
        guard case .expired = pastExpiry.read(at: file, expecting: .dev) else {
            Issue.record("a code past its expires_at must read as expired")
            return
        }
    }

    @Test func aMismatchedEnvironmentIsRefusedFreshOrStale() throws {
        let file = try Self.readinessFile(
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911",\#(stamp)}"#)
        defer { try? FileManager.default.removeItem(at: file.deletingLastPathComponent()) }
        let now = Date(timeIntervalSince1970: 1_786_400_000)

        for age in [TimeInterval(1), DaemonReadinessReader.pairingCodeLifetime + 1] {
            let reader = DaemonReadinessReader(
                now: { now }, modificationDate: { _ in now.addingTimeInterval(-age) })
            let outcome = reader.read(at: file, expecting: .prod)
            #expect(outcome == .refused(.environmentMismatch(app: .prod, daemon: .dev)))
            #expect(outcome.readiness == nil)
        }
        let refusal = DaemonReadinessRefusal.environmentMismatch(app: .prod, daemon: .dev)
        #expect(refusal.description.contains("prod") && refusal.description.contains("dev"))
    }

    @Test func aPreStampFileIsRefusedWithAnUpgradeMessageFreshOrStale() throws {
        let file = try Self.readinessFile(
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911"}"#)
        defer { try? FileManager.default.removeItem(at: file.deletingLastPathComponent()) }
        let now = Date(timeIntervalSince1970: 1_786_400_000)

        for age in [TimeInterval(1), DaemonReadinessReader.pairingCodeLifetime + 1] {
            let reader = DaemonReadinessReader(
                now: { now }, modificationDate: { _ in now.addingTimeInterval(-age) })
            #expect(reader.read(at: file, expecting: .ephemeral) == .refused(.daemonTooOld(app: .ephemeral)))
        }
        #expect(DaemonReadinessRefusal.daemonTooOld(app: .prod).description.contains("Upgrade freesided"))
    }

    private static func readinessFile(_ body: String) throws -> URL {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        let file = root.appendingPathComponent("readiness.json")
        try Data(body.utf8).write(to: file)
        return file
    }
}
