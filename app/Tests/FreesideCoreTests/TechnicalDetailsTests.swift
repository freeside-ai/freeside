import Foundation
import Testing

@testable import FreesideCore

@Suite struct ShortIdentifierTests {
    @Test func keepsPrefixAndShortensLongIds() {
        let hex = String(repeating: "0", count: 64)
        #expect(ShortIdentifier.short("run-\(hex)") == "run-00000000…")
        #expect(ShortIdentifier.short("campaign-\(hex)") == "campaign-00000000…")
        #expect(ShortIdentifier.short("task-\(String(repeating: "a", count: 26))") == "task-aaaaaaaa…")
        #expect(ShortIdentifier.short("sha256:\(hex)") == "sha256:000000000000…")
    }

    @Test func shortIdsAndDigestsPrintWhole() {
        // A short fixture run id and a short digest are under the thresholds.
        #expect(ShortIdentifier.short("run-freeside-657") == "run-freeside-657")
        #expect(ShortIdentifier.short("sha256:abc") == "sha256:abc")
        // Exactly 24 characters print whole; 25 shorten (with a known prefix).
        #expect(ShortIdentifier.short(String(repeating: "x", count: 24)) == String(repeating: "x", count: 24))
        #expect(
            ShortIdentifier.short("run-\(String(repeating: "z", count: 21))")
                == "run-zzzzzzzz…")
    }

    @Test func anUnknownLongIdIsLeftWhole() {
        // No run-/campaign-/task-/sha256: prefix: there is no meaningful point
        // to truncate at, so the value is returned unchanged.
        let value = "workitem-\(String(repeating: "q", count: 40))"
        #expect(ShortIdentifier.short(value) == value)
    }
}
