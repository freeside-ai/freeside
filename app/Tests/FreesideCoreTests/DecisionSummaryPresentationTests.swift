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

    /// What a block draws, without its marker: the text a reader sees.
    private func drawn(_ block: SpecificationBlock) -> String {
        switch block {
        case .heading(_, let text), .paragraph(let text), .listItem(_, _, let text),
            .listContinuation(_, let text):
            String(text.characters)
        case .listBlock(_, _, let block), .quote(let block): drawn(block)
        case .codeBlock(let text), .raw(let text), .plainText(let text): text
        case .thematicBreak: ""
        }
    }

    private func isListItem(_ block: SpecificationBlock) -> Bool {
        if case .listItem = block { return true }
        return false
    }

    /// The observed specification-approval shape (#1461): one long
    /// paragraph is an incomplete excerpt with unknown concerns, never the
    /// whole block, and the original keeps its last sentence.
    @Test func oneLongParagraphIsAnExcerptWithUnknownConcerns() {
        let source = DecisionSummaryFixtures.specificationLegacy
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        #expect(presentation.isExcerpt)
        #expect(presentation.concernsUnknown)
        #expect(presentation.concernBlocks == nil)
        let lead = presentation.leadBlocks.map(drawn).joined(separator: "\n")
        #expect(lead.count <= 800)
        #expect(source.hasPrefix(lead))
        #expect(!lead.contains(DecisionSummaryFixtures.specificationLastConcern))
        #expect(
            DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_markdown, content: source))
                .map(drawn).joined().hasSuffix(DecisionSummaryFixtures.specificationLastConcern))
    }

    /// The specifier's headings: the hard-wrapped paragraph reflows, and
    /// every concern is a list item on the closed card, the last included.
    @Test(arguments: [
        (
            DecisionSummaryFixtures.specificationStructured,
            DecisionSummaryFixtures.specificationStructuredLastConcern, 3
        ),
        (DecisionSummaryFixtures.wrapped, DecisionSummaryFixtures.wrappedLastConcern, 2),
    ])
    func wrappedParagraphsReflowAndConcernsDrawAsItems(source: String, lastConcern: String, count: Int) throws {
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        #expect(!presentation.isExcerpt)
        #expect(!presentation.concernsUnknown)
        #expect(presentation.concernCount == count)

        #expect(presentation.lead.contains("\n"))
        #expect(presentation.leadBlocks.count == 1)
        let lead = try #require(presentation.leadBlocks.first)
        guard case .paragraph = lead else {
            Issue.record("The lead is \(lead), not one paragraph")
            return
        }
        #expect(!drawn(lead).contains("\n"))
        #expect(drawn(lead) == presentation.lead.replacingOccurrences(of: "\n", with: " "))

        let concerns = try #require(presentation.concernBlocks)
        #expect(concerns.count == count)
        #expect(concerns.allSatisfy(isListItem))
        #expect(concerns.last.map(drawn) == lastConcern)
        for text in concerns.map(drawn) {
            #expect(!text.contains("\n"))
            #expect(!text.hasPrefix("- "))
            #expect(!text.contains("**"))
        }
    }

    /// The 800-character cut is made on source text, so it can land inside
    /// a list item or an emphasis span. The excerpt must not then print the
    /// half-open marker.
    @Test(arguments: [
        "Leading line.\n\n"
            + (1...40).map { "- **Item \($0) is strong from its first word to its last one**" }
            .joined(separator: "\n"),
        String(repeating: "word ", count: 150) + "**" + String(repeating: "bold ", count: 60) + "end** tail.",
        String(repeating: "word ", count: 150) + "_" + String(repeating: "soft ", count: 60) + "end_ tail.",
        "## Change\n" + String(repeating: "word ", count: 150) + "**"
            + String(repeating: "bold ", count: 60) + "end**\n## Remaining concerns\n- One",
    ])
    func anExcerptCutInsideAStructureLeavesNoStrayMarkers(source: String) {
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        #expect(presentation.isExcerpt)
        #expect(!presentation.leadBlocks.isEmpty)
        let texts = presentation.leadBlocks.map(drawn)
        #expect(texts.joined().count <= presentation.lead.count)
        for text in texts {
            #expect(!text.isEmpty)
            #expect(!text.contains("**"))
            #expect(!text.contains("_"))
            #expect(!text.contains("`"))
            #expect(!text.hasPrefix("- "))
            #expect(!text.hasPrefix("#"))
        }
        // The cut still lands inside the structure: the source slice holds
        // an opening marker its excerpt never closes.
        let markers = presentation.lead.components(separatedBy: "**").count - 1
        let soft = presentation.lead.filter { $0 == "_" }.count
        #expect(markers % 2 == 1 || soft % 2 == 1 || presentation.lead.contains("\n- "))
    }

    @Test func plainTextKeepsItsLineBreaksAndUnparsedMarkdownKeepsItsSource() {
        let plain = "First line\n- not a list\n\n**not strong**"
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_plain, content: plain))
        #expect(presentation.leadBlocks == [.plainText(plain)])
        #expect(
            DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_plain, content: plain))
                == [.plainText(plain)])
        // A shape the parser cannot represent keeps its exact source.
        let unrepresented = "See []() for the report."
        #expect(
            DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_markdown, content: unrepresented))
                == [.raw(unrepresented)])
    }

    /// A summary is agent Markdown, inert as it is in the specification
    /// reader: a link shows its destination and carries no action.
    @Test func summaryLinksShowTheirDestinationAndCarryNoAction() throws {
        let blocks = DecisionSummaryPresentation(
            .init(media_type: .text_sol_markdown, content: "See [the report](https://example.com/r) first.")
        ).leadBlocks
        guard case .paragraph(let text) = try #require(blocks.first) else {
            Issue.record("The lead is not a paragraph")
            return
        }
        #expect(String(text.characters) == "See the report (https://example.com/r) first.")
        #expect(text.runs.allSatisfy { $0.link == nil })
    }

    /// The parser leaves some Markdown out of what it returns, such as a
    /// link reference definition. A concern or a sentence written in that
    /// shape must still reach the card, so the source draws as written.
    @Test func textTheParserWouldDropDrawsAsItsSource() {
        let concerns = "- [risk]: flaky\n- second"
        let structured = DecisionSummaryPresentation(
            .init(
                media_type: .text_sol_markdown,
                content: "## Change\n\nDid it.\n\n## Remaining concerns\n\n\(concerns)"))
        #expect(structured.concernBlocks == [.plainText(concerns)])
        #expect(structured.concernCount == 2)

        let unstructured = "Did it. Tests pass.\n\n[Caveat]: unverified-on-CI"
        let text = Components.Schemas.ClaimText(media_type: .text_sol_markdown, content: unstructured)
        #expect(DecisionSummaryPresentation(text).leadBlocks == [.plainText(unstructured)])
        #expect(DecisionSummaryPresentation.blocks(text) == [.plainText(unstructured)])

        // Text the parser adds is not text it dropped: a link followed by
        // its destination still formats.
        let link = "See [the **diff**](https://example.com/pull/1) first.\n\n- One"
        let formatted = DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_markdown, content: link))
        #expect(formatted.map(drawn) == ["See the diff (https://example.com/pull/1) first.", "One"])
    }

    /// Dropped text need not hold a letter or digit: a link title or a
    /// reference definition can be a symbol alone. Anything that is not
    /// whitespace or ASCII punctuation is text, and text the parser keeps
    /// still formats.
    @Test func droppedSymbolsDrawAsSourceAndKeptSymbolsStillFormat() {
        let blocks = { (source: String) in
            DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_markdown, content: source))
        }
        for dropped in [
            "See [report](https://example.com \"⚠️\") first.",
            "Did it.\n\n[⚠️]: <> \"→ ✗\"",
            "Did it, [mostly](https://example.com '…—').",
        ] {
            #expect(blocks(dropped) == [.plainText(dropped)])
        }

        let kept =
            "## Résumé ✅\n\n“Done” — mostly… see [the **diff** →](https://example.com/1).\n\n"
            + "- ⚠️ 5 °C ≥ 3 × 1\n- 日本語 🐦\n- కోడ్ మార్పు"
        #expect(
            blocks(kept).map(drawn) == [
                "Résumé ✅", "“Done” — mostly… see the diff → (https://example.com/1).", "⚠️ 5 °C ≥ 3 × 1", "日本語 🐦",
                "కోడ్ మార్పు",
            ])
    }

    /// A hard-wrapped report whose last line continues a list item or a
    /// quote without indentation draws on the card. The shared parser
    /// trapped on that shape (`SpecificationMarkdownTests`), and every
    /// summary now goes through it.
    @Test func aReportEndingOnALazyContinuationDraws() throws {
        let report = "Did it.\n\n- one\n- two wraps\nonto here\n"
        let text = Components.Schemas.ClaimText(media_type: .text_sol_markdown, content: report)
        let presentation = DecisionSummaryPresentation(text)
        #expect(!presentation.isExcerpt)
        #expect(presentation.leadBlocks.map(drawn) == ["Did it.", "one", "two wraps onto here"])
        #expect(DecisionSummaryPresentation.blocks(text) == presentation.leadBlocks)

        let structured = DecisionSummaryPresentation(
            .init(
                media_type: .text_sol_markdown,
                content: "## Change\n\n Did it,\nmostly.\n\n## Remaining concerns\n\n>quoted\nlazy"))
        #expect(structured.leadBlocks.map(drawn) == ["Did it, mostly."])
        #expect(try #require(structured.concernBlocks).map(drawn) == ["quoted lazy"])
    }

    /// The concerns are never bounded: a list far past the lead's bound
    /// draws every item.
    @Test func everyConcernDrawsHoweverLongTheList() throws {
        let items = (1...40).map { "- Concern \($0) names a risk the operator has to weigh before approving" }
        let presentation = DecisionSummaryPresentation(
            .init(
                media_type: .text_sol_markdown,
                content: "## Change\nLead\n## Remaining concerns\n" + items.joined(separator: "\n")))
        let concerns = try #require(presentation.concernBlocks)
        #expect(concerns.count == 40)
        #expect(concerns.allSatisfy(isListItem))
        #expect(concerns.map(drawn).joined(separator: "\n").count > 800)
        #expect(concerns.last.map(drawn) == String(items[39].dropFirst(2)))
    }

    /// The card says "incomplete" exactly when the drawn lead leaves text
    /// out. Markers are not drawn, so a source over the bound whose drawn
    /// text fits it is complete, and concerns outside it are not "unknown".
    @Test func theIncompleteLabelFollowsWhatTheLeadDraws() {
        let whole = { (source: String) in
            DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_markdown, content: source))
        }
        let fits = (1...23).map { "- **Item \($0)** does a thing here ok.." }.joined(separator: "\n")
        #expect(fits.count > 800)
        let complete = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: fits))
        #expect(!complete.isExcerpt)
        #expect(!complete.concernsUnknown)
        #expect(complete.leadBlocks == whole(fits))

        for source in [
            DecisionSummaryFixtures.legacy, DecisionSummaryFixtures.specificationLegacy,
            (1...40).map { "- **Item \($0)** does a thing here ok.." }.joined(separator: "\n"),
        ] {
            let excerpt = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
            #expect(excerpt.isExcerpt)
            #expect(excerpt.concernsUnknown)
            #expect(excerpt.leadBlocks != whole(source))
            #expect(excerpt.leadBlocks.map(drawn).joined().count < whole(source).map(drawn).joined().count)
        }
    }

    /// A block spends more of the bound than the characters it draws: 300
    /// one-letter items draw 300 characters, and a rule draws none. A report
    /// of very short blocks is still cut about where its source prefix ends,
    /// never drawn row after row to the end.
    @Test(arguments: [
        (Array(repeating: "- a", count: 300).joined(separator: "\n"), 200),
        ((1...300).map { "\($0). a" }.joined(separator: "\n"), 200),
        (Array(repeating: "---", count: 300).joined(separator: "\n\n"), 267),
    ])
    func manyShortBlocksStayWithinTheBound(source: String, rows: Int) {
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        let whole = DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_markdown, content: source))
        #expect(whole.count == 300)
        #expect(presentation.isExcerpt)
        #expect(presentation.concernsUnknown)
        #expect(presentation.leadBlocks.count == rows)
        #expect(presentation.leadBlocks == Array(whole.prefix(rows)))
    }

    /// An excerpt ends between words. A cut that would leave the first
    /// letters of a block's first word drops that block instead.
    @Test func anExcerptNeverEndsInsideAWord() throws {
        let source =
            "## Change\n" + String(repeating: "word ", count: 159)
            + "\n- Refactored the importer so that it reads sizes first\n## Remaining concerns\n- One"
        let presentation = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: source))
        #expect(presentation.isExcerpt)
        let last = try #require(presentation.leadBlocks.last.map(drawn))
        #expect(last.hasSuffix("word"))
        #expect(!presentation.leadBlocks.contains(where: isListItem))

        // One unbroken run has no boundary to cut at and is cut at the bound.
        let run = String(repeating: "y", count: 900)
        let unbroken = DecisionSummaryPresentation(.init(media_type: .text_sol_markdown, content: run))
        #expect(unbroken.isExcerpt)
        #expect(unbroken.leadBlocks.map(drawn) == [String(run.prefix(800))])
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
