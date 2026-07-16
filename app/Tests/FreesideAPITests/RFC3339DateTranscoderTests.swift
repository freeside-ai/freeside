import Foundation
import FreesideAPI
import Testing

/// Regression for the first real-daemon convergence finding: the Go
/// daemon emits RFC 3339 timestamps with fractional seconds and a zone
/// offset, the mock emits whole-second UTC, and the stock runtime
/// transcoders each accept exactly one of those shapes.
@Suite struct RFC3339DateTranscoderTests {
    private let transcoder = RFC3339DateTranscoder()

    @Test func decodesEveryContractLegalShape() throws {
        let epoch = Date(timeIntervalSince1970: 1_800_000_000)
        for string in [
            "2027-01-15T08:00:00Z",
            "2027-01-15T08:00:00.123456Z",
            "2027-01-15T01:00:00-07:00",
            "2027-01-15T01:00:00.5-07:00",
        ] {
            let decoded = try transcoder.decode(string)
            #expect(abs(decoded.timeIntervalSince(epoch)) < 1.0, "decoded \(string)")
        }
    }

    @Test func rejectsNonDates() {
        #expect(throws: Error.self) { try transcoder.decode("not-a-date") }
    }

    @Test func encodesCanonicalWholeSecondUTC() throws {
        let encoded = try transcoder.encode(Date(timeIntervalSince1970: 1_800_000_000.75))
        #expect(encoded == "2027-01-15T08:00:00Z")
        // The canonical output round-trips through its own decoder.
        _ = try transcoder.decode(encoded)
    }
}
