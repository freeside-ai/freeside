import SwiftUI
import Testing

@testable import FreesideCore

@MainActor
struct FactRowTests {
    private let fortyCharacters = String(repeating: "a", count: 40)
    private let fortyOneCharacters = String(repeating: "a", count: 41)

    @Test func valueAtTheThresholdKeepsTheTrailingLayout() {
        #expect(!FactRow.stacks(fortyCharacters, at: .large))
        #expect(!FactRow.stacks(fortyCharacters, at: .xSmall))
    }

    @Test func valueOverTheThresholdStacksAtEverySize() {
        #expect(FactRow.stacks(fortyOneCharacters, at: .xSmall))
        #expect(FactRow.stacks(fortyOneCharacters, at: .accessibility5))
    }

    @Test func accessibilitySizeStacksAShortValue() {
        #expect(FactRow.stacks("ok", at: .accessibility1))
        #expect(!FactRow.stacks("ok", at: .xxxLarge))
    }
}
