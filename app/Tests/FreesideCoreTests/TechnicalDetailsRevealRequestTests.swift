import Foundation
import Testing

@testable import FreesideCore

@Suite struct TechnicalDetailsRevealRequestTests {
    @Test func revealRequestExpiresWhenSelectionLeavesItsItem() {
        let request = TechnicalDetailsRevealRequest(itemID: "item-a", nonce: UUID())

        #expect(request.retained(for: "item-a") == request)
        #expect(request.retained(for: "item-b") == nil)
        #expect(request.retained(for: nil) == nil)
    }

    @Test func onlyTheHandledRevealEventIsConsumed() {
        let request = TechnicalDetailsRevealRequest(itemID: "item-a", nonce: UUID())

        #expect(request.consuming(request.nonce) == nil)
        #expect(request.consuming(UUID()) == request)
    }
}
