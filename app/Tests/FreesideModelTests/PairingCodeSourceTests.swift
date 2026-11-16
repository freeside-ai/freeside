import Foundation
import Testing

@testable import FreesideModel

@Suite struct MintedPairingCodeParseTests {
    @Test func theCLIsLineParses() throws {
        // Go's encoder writes nanoseconds, a zone offset, and a newline.
        let minted = try #require(
            MintedPairingCode.parse(
                Data(
                    #"{"api_url":"http://127.0.0.1:7331","pairing_code":"7KQ4M2XP","expires_at":"2026-11-16T15:35:16.123456789-08:00"}"#
                        .utf8 + [0x0A])))

        #expect(minted.code == "7KQ4M2XP")
        #expect(minted.apiURL == URL(string: "http://127.0.0.1:7331"))
        #expect(abs(minted.expiresAt.timeIntervalSince1970 - 1_794_872_116.123) < 0.001)
    }

    @Test func aWholeSecondExpiryParses() throws {
        let minted = try #require(
            MintedPairingCode.parse(
                Data(
                    #"{"api_url":"http://100.101.102.103:7331","pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z"}"#
                        .utf8)))

        #expect(minted.expiresAt == Date(timeIntervalSince1970: 1_794_872_116))
        #expect(minted.apiURL == URL(string: "http://100.101.102.103:7331"))
    }

    @Test(
        arguments: [
            "",
            "not json",
            "[]",
            // Each key missing.
            #"{"pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"http://127.0.0.1:7331","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911"}"#,
            // An extra key.
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z","extra":1}"#,
            // An empty or non-string code.
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"  ","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":483911,"expires_at":"2026-11-16T23:35:16Z"}"#,
            // A bad or zero time.
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","expires_at":"tomorrow"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","expires_at":"0001-01-01T00:00:00Z"}"#,
            #"{"api_url":"http://127.0.0.1:7331","pairing_code":"483911","expires_at":1763336116}"#,
            // An address that isn't an http origin with a port.
            #"{"api_url":"https://127.0.0.1:7331","pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"http://127.0.0.1","pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"http://user@127.0.0.1:7331","pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"http://127.0.0.1:7331/path","pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"http://127.0.0.1:7331?q=1","pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z"}"#,
            #"{"api_url":"127.0.0.1:7331","pairing_code":"483911","expires_at":"2026-11-16T23:35:16Z"}"#,
        ])
    func anythingElseIsRejected(_ line: String) {
        #expect(MintedPairingCode.parse(Data(line.utf8)) == nil)
    }

    @Test func descriptionsLeaveTheCodeOut() {
        let minted = MintedPairingCode(
            code: "7KQ4M2XP", expiresAt: Date(timeIntervalSince1970: 1_794_872_116),
            apiURL: URL(string: "http://127.0.0.1:7331")!)

        #expect(!"\(minted)".contains("7KQ4M2XP"))
        #expect(!String(reflecting: minted).contains("7KQ4M2XP"))
    }
}
