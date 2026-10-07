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

    /// The Full Report fold counts concerns only where the report's own
    /// list does: a number the source does not state would be a new claim.
    @Test(arguments: [
        ("- First\n- Second", Optional(2)),
        ("1. First\n2) Second\n   continued\n\n3. Third", Optional(3)),
        ("* Only one", Optional(1)),
        ("A question remains.", nil),
        ("- First\nThen some prose.", nil),
        ("  indented prose\n- First", nil),
        ("-not a list item", nil),
    ])
    func concernsAreCountedOnlyWhenTheReportListsThem(concerns: String, count: Int?) {
        let presentation = DecisionSummaryPresentation(
            .init(
                media_type: .text_sol_markdown,
                content: "## Change\nLead\n## Remaining concerns\n\(concerns)"))
        #expect(presentation.concerns == concerns)
        #expect(presentation.concernCount == count)
    }

    @Test func unextractedConcernsAreNeverCounted() {
        let legacy = DecisionSummaryPresentation(
            .init(media_type: .text_sol_markdown, content: DecisionSummaryFixtures.legacy))
        #expect(legacy.concernCount == nil)
        let plain = DecisionSummaryPresentation(
            .init(media_type: .text_sol_plain, content: "- First\n- Second"))
        #expect(plain.concernCount == nil)
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
