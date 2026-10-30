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

    /// R9: a chip in the value slot stacks by the same rule as the text it
    /// replaces, so the row reads its label for the decision.
    @Test func chipRowCarriesTheChipLabelAsItsValue() {
        let row = FactRow(label: "Posture", chip: StateChip(label: "Degraded", cut: .attention))
        #expect(row.value == "Degraded")
        #expect(row.chip?.label == "Degraded")
        #expect(!FactRow.stacks(row.value, at: .large))
        #expect(FactRow.stacks(row.value, at: .accessibility1))
    }

    @Test func textRowCarriesNoChip() {
        #expect(FactRow(label: "Stage", value: "review").chip == nil)
    }
}
