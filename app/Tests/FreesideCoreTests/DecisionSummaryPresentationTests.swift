import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct DecisionSummaryPresentationTests {
    @Test func lateConcernRemainsVisibleAndOriginalIsUnchanged() {
        let source = DecisionSummaryFixtures.structured
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        #expect(presentation.original == source)
        #expect(presentation.lead == DecisionSummaryFixtures.change)
        #expect(presentation.concerns == DecisionSummaryFixtures.concern)
        #expect(!presentation.concernsUnknown)
        #expect(!presentation.lead.contains(DecisionSummaryFixtures.details))
    }

    @Test func legacyExcerptNeverClaimsToExtractConcerns() {
        let source = DecisionSummaryFixtures.legacy
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        #expect(source.hasPrefix(presentation.lead))
        #expect(presentation.isExcerpt)
        #expect(presentation.concernsUnknown)
        #expect(presentation.concerns == nil)
        #expect(presentation.original.hasSuffix(DecisionSummaryFixtures.concern))
    }

    @Test(arguments: [
        "Short complete report.", "A café changed. 🐦 Uncertainty remains.", "",
        "## Change\nSmall change\n## Remaining concerns\nA question remains.",
    ])
    func plainTextIsLiteralAndShortTextIsComplete(source: String) {
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_plain, content: source))
        #expect(presentation.lead == source)
        #expect(presentation.original == source)
        #expect(!presentation.isExcerpt)
        #expect(presentation.concerns == nil)
    }

    @Test(arguments: [
        "## Change\nLead\n## Details\nSupporting text",
        "## Change\nLead\n## Remaining concerns\nFirst\n## Remaining concerns\nLast",
        "## Change\nLead\n```\n## Remaining concerns\nNot a heading\n```",
        "Preamble\n## Change\nLead\n## Remaining concerns\nQuestion",
        "## Change\nLead\n## Remaining concerns\nQuestion\n## Unknown\nText",
        "## Change\nLead\n## Remaining concerns\n\n## Details\nText",
        "## Change\nChanged docs.\n## Remaining concerns\nMigration remains untested.\n\n    ## Details\n    example\n\nProduction migration still needs review.",
    ])
    func ambiguousLongReportsKeepUnknownConcernsVisible(source: String) {
        let source = source + "\n" + DecisionSummaryFixtures.details
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        #expect(presentation.original == source)
        #expect(presentation.isExcerpt)
        #expect(presentation.concernsUnknown)
    }

    @Test func concernsHaveNoLengthLimitAndUnicodeExcerptIsASourcePrefix() {
        let concerns = String(repeating: "Unresolved 🐦. ", count: 200)
        let change = String(repeating: "é🐦", count: 500)
        let source = "## Change\n\(change)\n## Remaining concerns\n\(concerns)"
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        #expect(change.hasPrefix(presentation.lead))
        #expect(presentation.isExcerpt)
        #expect(presentation.concerns == concerns)
        #expect(!presentation.concernsUnknown)
    }

    @Test func expansionIdentityBindsItemClaimAndProvenance() throws {
        let item = DecisionSummaryFixtures.snapshot(content: DecisionSummaryFixtures.structured).item
        let claim = try #require(item.agent_claims.first { $0.label == "freeside.summary" })
        let identity = DecisionSummaryIdentity(itemID: item.id, claim: claim)
        #expect(identity != DecisionSummaryIdentity(itemID: "another-item", claim: claim))
        var changed = claim
        changed.digest = "different-digest"
        #expect(identity != DecisionSummaryIdentity(itemID: item.id, claim: changed))
        changed = claim
        changed.artifact_id = "another-report"
        #expect(identity != DecisionSummaryIdentity(itemID: item.id, claim: changed))
        #expect(identity.claim == claim)
        #expect(identity.claim.text?.content == DecisionSummaryFixtures.structured)
    }
}
