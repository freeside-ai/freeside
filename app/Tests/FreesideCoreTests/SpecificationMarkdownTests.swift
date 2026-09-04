import Foundation
import FreesideAPI
import SwiftUI
import Testing

@testable import FreesideCore

@Suite struct SpecificationMarkdownTests {
    @Test func revisedSpecificationHasSeparateHeadingsAndItems() throws {
        let claim = try #require(
            AttentionFixtures.revisedSpecification().item.agent_claims.first {
                $0.label == "Specification"
            })
        let text = try #require(claim.text)
        let blocks = try #require(SpecificationMarkdown.blocks(from: text.content))
        #expect(blocks.first == .heading(level: 1, AttributedString("Authentication Migration")))
        let headings = blocks.compactMap { block -> String? in
            if case .heading(level: 2, let text) = block { return String(text.characters) }
            return nil
        }
        #expect(headings == ["Intent", "Order", "Rollback", "Verification"])
        let ordinals = blocks.compactMap { block -> Int? in
            if case .listItem(let ordinal, _, _) = block { return ordinal }
            return nil
        }
        #expect(ordinals == [1, 2, 3])
        #expect(blocks.filter { if case .listItem(ordinal: nil, _, _) = $0 { true } else { false } }.count == 2)
    }

    @Test func siblingItemsNeverMerge() {
        #expect(
            SpecificationMarkdown.blocks(from: "1. Alpha\n2. Beta\n3. Gamma") == [
                .listItem(ordinal: 1, depth: 0, AttributedString("Alpha")),
                .listItem(ordinal: 2, depth: 0, AttributedString("Beta")),
                .listItem(ordinal: 3, depth: 0, AttributedString("Gamma")),
            ])
    }

    @Test func nestedListsAndContinuationKeepTheirDepth() {
        #expect(
            SpecificationMarkdown.blocks(from: "1. Parent\n   - Child\n   - Next\n\n   Continuation") == [
                .listItem(ordinal: 1, depth: 0, AttributedString("Parent")),
                .listItem(ordinal: nil, depth: 1, AttributedString("Child")),
                .listItem(ordinal: nil, depth: 1, AttributedString("Next")),
                .listContinuation(depth: 0, AttributedString("Continuation")),
            ])
        #expect(
            SpecificationMarkdown.blocks(from: "- Parent\n\n  3. Child\n  4. Next") == [
                .listItem(ordinal: nil, depth: 0, AttributedString("Parent")),
                .listItem(ordinal: 3, depth: 1, AttributedString("Child")),
                .listItem(ordinal: 4, depth: 1, AttributedString("Next")),
            ])
    }

    @Test func inlineFormattingSurvives() throws {
        let blocks = try #require(SpecificationMarkdown.blocks(from: "*emphasis* **strong** `code`"))
        guard case .paragraph(let text) = try #require(blocks.first) else {
            Issue.record("Expected paragraph")
            return
        }
        for (word, intent) in [
            ("emphasis", InlinePresentationIntent.emphasized), ("strong", .stronglyEmphasized), ("code", .code),
        ] {
            #expect(
                text.runs.contains {
                    String(text[$0.range].characters) == word && $0.inlinePresentationIntent?.contains(intent) == true
                })
        }
    }

    @Test(arguments: ["-", "+", "*", "1.", "3)", "123456789."])
    func emptyListItemsRetainExactSource(marker: String) {
        for newline in ["\n", "\r", "\r\n"] {
            for source in [
                marker, "  \(marker) \t", "\(marker) Before\(newline)\(marker)\(newline)\(marker) After",
                "\(marker)\(newline)\(marker) After", "\(marker) Before\(newline)\(marker)",
                "\(marker)\(newline)\(marker)", "- Parent\(newline)\(newline)    \(marker)",
                "> \(marker)", "- > \(marker)", "- \(marker)", "before\(newline)\(newline)\(marker)",
            ] {
                #expect(SpecificationMarkdown.blocks(from: source) == [.raw(source)])
            }
        }
    }

    @Test func emptyMarkerDetectionKeepsRepresentedBlocks() {
        #expect(SpecificationMarkdown.blocks(from: "Heading\n-") == [.heading(level: 2, AttributedString("Heading"))])
        #expect(
            SpecificationMarkdown.blocks(from: "## Heading\n-\n- After") == [.raw("## Heading\n-\n- After")])
        #expect(SpecificationMarkdown.blocks(from: "* * *") == [.thematicBreak])
        #expect(SpecificationMarkdown.blocks(from: "> - - -") == [.quote(.thematicBreak)])
        #expect(
            SpecificationMarkdown.blocks(from: "-\n  * * *") == [.listBlock(marker: "•", depth: 0, .thematicBreak)])
        #expect(SpecificationMarkdown.blocks(from: "```\n-\n```") == [.codeBlock("-\n")])
        #expect(SpecificationMarkdown.blocks(from: "    -") == [.codeBlock("-\n")])
        #expect(SpecificationMarkdown.blocks(from: "<pre>\n-\n</pre>") == [.raw("<pre>\n-\n</pre>")])
        #expect(
            SpecificationMarkdown.blocks(from: "Before\n\nAfter") == [
                .paragraph(AttributedString("Before")), .paragraph(AttributedString("After")),
            ])
    }

    @Test func nestedCodeKeepsWhitespaceAndListPlacement() {
        #expect(
            SpecificationMarkdown.blocks(from: "- Parent\n\n  ```swift\n  a  b\n    c\n  ```\n\n  Tail") == [
                .listItem(ordinal: nil, depth: 0, AttributedString("Parent")),
                .listBlock(marker: "", depth: 0, .codeBlock("a  b\n  c\n")),
                .listContinuation(depth: 0, AttributedString("Tail")),
            ])
        #expect(
            SpecificationMarkdown.blocks(from: "- Parent\n  -     a  b\n        c") == [
                .listItem(ordinal: nil, depth: 0, AttributedString("Parent")),
                .listBlock(marker: "•", depth: 1, .codeBlock("a  b\nc\n")),
            ])
    }

    @Test func nestedLeavesKeepTheirKindAndFirstItemMarker() {
        #expect(
            SpecificationMarkdown.blocks(from: "3. # Heading\n\n   > Quote\n\n   ---\n\n   Tail") == [
                .listBlock(marker: "3.", depth: 0, .heading(level: 1, AttributedString("Heading"))),
                .listBlock(marker: "", depth: 0, .quote(.paragraph(AttributedString("Quote")))),
                .listBlock(marker: "", depth: 0, .thematicBreak),
                .listContinuation(depth: 0, AttributedString("Tail")),
            ])
    }

    @Test func rawFirstItemStillConsumesItsMarker() {
        #expect(
            SpecificationMarkdown.blocks(from: "- <script>x</script>\n\n  Tail") == [
                .raw("- <script>x</script>"), .listContinuation(depth: 0, AttributedString("Tail")),
            ])
    }

    @Test func itemsBeginningWithNestedListsKeepTheirMarkerAndContinuation() {
        #expect(
            SpecificationMarkdown.blocks(from: "3.\n   - Child\n\n   Tail\n4. Sibling") == [
                .listItem(ordinal: 3, depth: 0, AttributedString("")),
                .listItem(ordinal: nil, depth: 1, AttributedString("Child")),
                .listContinuation(depth: 0, AttributedString("Tail")),
                .listItem(ordinal: 4, depth: 0, AttributedString("Sibling")),
            ])
        #expect(
            SpecificationMarkdown.blocks(from: "-\n  -\n    - Child\n\n  Tail") == [
                .listItem(ordinal: nil, depth: 0, AttributedString("")),
                .listItem(ordinal: nil, depth: 1, AttributedString("")),
                .listItem(ordinal: nil, depth: 2, AttributedString("Child")),
                .listContinuation(depth: 0, AttributedString("Tail")),
            ])
        #expect(
            SpecificationMarkdown.blocks(from: "-\n  -     a  b\n        c") == [
                .listItem(ordinal: nil, depth: 0, AttributedString("")),
                .listBlock(marker: "•", depth: 1, .codeBlock("a  b\nc\n")),
            ])
    }

    @Test(arguments: ["-\n  - <script>x</script>", "- - <script>x</script>"])
    func rawChildrenOfEmptyAncestorsKeepTheWholeSource(list: String) {
        let source = "before\n\n\(list)\n\n  Tail\n\nafter"
        #expect(SpecificationMarkdown.blocks(from: source) == [.raw(source)])
    }

    @Test(arguments: ["https://example.com", "javascript:alert(1)", "file:///tmp/private", "mailto:agent@example.com"])
    func linksAreLiteralAndInert(destination: String) throws {
        let blocks = try #require(SpecificationMarkdown.blocks(from: "[text](\(destination))"))
        guard case .paragraph(let text) = try #require(blocks.first) else {
            Issue.record("Expected paragraph")
            return
        }
        #expect(String(text.characters) == "text (\(destination))")
        #expect(text.runs.allSatisfy { $0.link == nil && $0.imageURL == nil })
    }

    @Test func htmlRemainsLiteral() throws {
        let html = "<script>alert(1)</script>"
        let blocks = try #require(SpecificationMarkdown.blocks(from: "a <b>x</b>\n\n\(html)"))
        guard case .paragraph(let text) = blocks[0] else {
            Issue.record("Expected paragraph")
            return
        }
        #expect(String(text.characters) == "a <b>x</b>")
        #expect(text.runs.allSatisfy { $0.inlinePresentationIntent?.contains(.stronglyEmphasized) != true })
        #expect(blocks[1] == .raw(html))
    }

    @Test func adjacentLinksEachShowTheirDestination() {
        #expect(
            SpecificationMarkdown.blocks(from: "[safe](https://safe.invalid)[admin approved](https://safe.invalid)")
                == [
                    .paragraph(AttributedString("safe (https://safe.invalid)admin approved (https://safe.invalid)"))
                ])
    }

    @Test func linkLabelsKeepInlineStyles() throws {
        let blocks = try #require(
            SpecificationMarkdown.blocks(from: "[*Migration* **guide** `code` reference](https://example.invalid)"))
        guard case .paragraph(let text) = try #require(blocks.first) else {
            Issue.record("Expected paragraph")
            return
        }
        #expect(String(text.characters) == "Migration guide code reference (https://example.invalid)")
        for intent in [InlinePresentationIntent.emphasized, .stronglyEmphasized, .code] {
            #expect(text.runs.contains { $0.inlinePresentationIntent?.contains(intent) == true })
        }
        #expect(text.runs.allSatisfy { $0.link == nil && $0.imageURL == nil })
    }

    @Test(arguments: ["*Migration* **guide** `code`", "``a ` b``"])
    func incompleteLinkLabelSourceKeepsTheWholeBlock(label: String) {
        let paragraph = "Read [\(label)](https://example.invalid) first."
        #expect(SpecificationMarkdown.blocks(from: paragraph) == [.raw(paragraph)])
    }

    @Test(arguments: ["\n", "\r", "\r\n"])
    func wrappedLinkLabelsKeepTheirWholeSource(newline: String) {
        let paragraph = "Read [*Migration*\(newline)**guide** `code`](https://example.invalid) first."
        #expect(
            SpecificationMarkdown.blocks(from: "before\(newline)\(newline)\(paragraph)\(newline)\(newline)after") == [
                .paragraph(AttributedString("before")), .raw(paragraph), .paragraph(AttributedString("after")),
            ])
    }

    @Test(arguments: [
        "[](https://example.invalid)", "[\n](https://example.invalid)", "[ \n ](https://example.invalid)",
        "[\r\n](https://example.invalid)", "[][target]\n\n[target]: https://example.invalid",
        "[![](https://image.invalid)](https://example.invalid)",
        "[![][image]][target]\n\n[image]: https://image.invalid\n[target]: https://example.invalid",
    ])
    func emptyLinkLabelsPreserveTheWholeSource(link: String) {
        let source = "# Before\n\n\(link)\n\nAfter"
        #expect(SpecificationMarkdown.blocks(from: source) == [.raw(source)])
    }

    @Test func emptyLinkPrefixDetectionKeepsRepresentedContent() throws {
        let syntax = "[](https://example.invalid)"
        #expect(SpecificationMarkdown.blocks(from: "```md\n\(syntax)\n```") == [.codeBlock("\(syntax)\n")])
        let inline = try #require(SpecificationMarkdown.blocks(from: "`\(syntax)`"))
        guard case .paragraph(let code) = try #require(inline.first) else {
            Issue.record("Expected inline code")
            return
        }
        #expect(String(code.characters) == syntax)
        #expect(code.runs.allSatisfy { $0.inlinePresentationIntent?.contains(.code) == true })
        for label in [" ", "\t"] {
            let blocks = try #require(SpecificationMarkdown.blocks(from: "See [\(label)](https://example.invalid)"))
            guard case .paragraph(let text) = try #require(blocks.first) else {
                Issue.record("Expected represented whitespace label")
                continue
            }
            #expect(String(text.characters).contains("(https://example.invalid)"))
        }
        let image = "![](https://image.invalid)"
        #expect(SpecificationMarkdown.blocks(from: image) != [.raw(image)])
        let mixed = "`\(syntax)` then \(syntax)"
        #expect(SpecificationMarkdown.blocks(from: mixed) == [.raw(mixed)])
    }

    @Test func imagesHaveOnlyAltText() throws {
        let blocks = try #require(SpecificationMarkdown.blocks(from: "![alt](https://example.com/i.png)"))
        #expect(blocks == [.paragraph(AttributedString("alt"))])
        guard case .paragraph(let text) = blocks[0] else { return }
        #expect(text.runs.allSatisfy { $0.imageURL == nil && $0.link == nil })
    }

    @Test(arguments: [
        ("[café 👋](https://example.invalid)", "café 👋 (https://example.invalid)"),
        ("[a\\]b](https://example.invalid)", "a]b (https://example.invalid)"),
        ("[![alt](https://example.invalid/i.png)](https://example.invalid)", "alt (https://example.invalid)"),
    ])
    func linkLabelEdgeCasesStayLiteral(markdown: String, visible: String) throws {
        let blocks = try #require(SpecificationMarkdown.blocks(from: markdown))
        guard case .paragraph(let text) = try #require(blocks.first) else {
            Issue.record("Expected paragraph")
            return
        }
        #expect(String(text.characters) == visible)
        #expect(text.runs.allSatisfy { $0.link == nil && $0.imageURL == nil })
    }

    @Test func codeAndQuotesKeepTheirContent() {
        #expect(
            SpecificationMarkdown.blocks(from: "```swift\na\nb\n```\n\n> Quote\n\n---") == [
                .codeBlock("a\nb\n"), .quote(.paragraph(AttributedString("Quote"))), .thematicBreak,
            ])
    }

    @Test func quotesPreserveEverySupportedLeafAndNesting() {
        #expect(
            SpecificationMarkdown.blocks(from: "> # Heading\n>\n> ```swift\n> a  b\n>   c\n> ```\n>\n> ---") == [
                .quote(.heading(level: 1, AttributedString("Heading"))),
                .quote(.codeBlock("a  b\n  c\n")),
                .quote(.thematicBreak),
            ])
        #expect(
            SpecificationMarkdown.blocks(from: "> > Nested") == [
                .quote(.quote(.paragraph(AttributedString("Nested"))))
            ])
    }

    @Test func quotesAndListMarkersKeepTheirContainerOrder() {
        #expect(
            SpecificationMarkdown.blocks(from: "> - Item") == [
                .quote(.listItem(ordinal: nil, depth: 0, AttributedString("Item")))
            ])
        #expect(
            SpecificationMarkdown.blocks(from: "- > Quote") == [
                .listBlock(marker: "•", depth: 0, .quote(.paragraph(AttributedString("Quote"))))
            ])
        #expect(
            SpecificationMarkdown.blocks(from: "> 3.\n>    - Child") == [
                .quote(.listItem(ordinal: 3, depth: 0, AttributedString(""))),
                .quote(.listItem(ordinal: nil, depth: 1, AttributedString("Child"))),
            ])
    }

    @Test(arguments: ["| A | B |\n|---|---|\n|x|y|", "| A | B |\n|---|---|"])
    func tablesRoundTripTheirSource(table: String) {
        #expect(
            SpecificationMarkdown.blocks(from: "before\n\n\(table)\n\nafter") == [
                .paragraph(AttributedString("before")), .raw(table), .paragraph(AttributedString("after")),
            ])
    }

    @Test @MainActor func plainTextAndParseFailureKeepRawText() {
        let text = "# Literal\n<b>source</b>"
        #expect(SpecificationReaderView.blocks(for: text, mediaType: .text_sol_plain) == [.plainText(text)])
        #expect(
            SpecificationReaderView.blocks(for: text, mediaType: .text_sol_markdown, parse: { _ in nil }) == [
                .plainText(text)
            ])
    }

    @Test(arguments: ["\n", "\r", "\r\n"])
    func rawBlocksKeepOriginalLineEndings(newline: String) {
        let table = ["| A | B |", "|---|---|", "|x|y|"].joined(separator: newline)
        let html = ["<script>", "alert(1)", "</script>"].joined(separator: newline)
        #expect(
            SpecificationMarkdown.blocks(
                from: "before\(newline)\(newline)\(table)\(newline)\(newline)\(html)\(newline)\(newline)after") == [
                    .paragraph(AttributedString("before")), .raw(table), .raw(html),
                    .paragraph(AttributedString("after")),
                ])
    }

    @Test @MainActor func inlineTypographyUsesVisibleFontVariants() throws {
        let blocks = try #require(SpecificationMarkdown.blocks(from: "*emphasis* **strong** `code`"))
        guard case .paragraph(let text) = try #require(blocks.first) else {
            Issue.record("Expected paragraph")
            return
        }
        let rendered = SpecificationReaderView.inlineText(text)
        for run in rendered.runs where run.inlinePresentationIntent != nil {
            #expect(run.font != nil)
            #expect(run.link == nil && run.imageURL == nil)
        }
    }

    @Test func rawBlocksKeepMixedLineEndings() {
        let table = "| A | B |\r\n|---|---|\n|x|y|"
        let html = "<script>\ralert(1)\r\n</script>"
        #expect(
            SpecificationMarkdown.blocks(from: "before\n\n\(table)\r\r\(html)\n\nafter") == [
                .paragraph(AttributedString("before")), .raw(table), .raw(html), .paragraph(AttributedString("after")),
            ])
    }

    @Test @MainActor func readerParsesOnlyTheTruncatedPrefix() throws {
        let reader = SpecificationReaderView(
            text: String(repeating: "x", count: 70 * 1024), mediaType: .text_sol_markdown, digest: "fixture")
        #expect(reader.preview.isTruncated)
        let prefix = try #require(reader.preview.text)
        #expect(prefix.utf8.count == DecisionDetailView.NonImagePreview.textByteLimit)
        #expect(reader.blocks == [.paragraph(AttributedString(prefix))])
    }
}
