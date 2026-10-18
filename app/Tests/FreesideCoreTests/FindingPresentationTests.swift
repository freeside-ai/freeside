import Foundation
import Testing

@testable import FreesideCore

@Suite struct FindingPresentationTests {
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

    @Test func shortFirstSentenceIsTheTitleAndTheRestIsTheBody() {
        let finding = FindingPresentation(
            message: "The retry loop never ends. It resets `attempt` on each pass.\n\n- Cap it\n- Log it")
        #expect(finding.title.map { String($0.characters) } == "The retry loop never ends.")
        #expect(finding.body.map(drawn) == ["It resets attempt on each pass.", "Cap it", "Log it"])
    }

    @Test func titleKeepsItsInlineMarkdownAsTypography() throws {
        let finding = FindingPresentation(message: "`Run` drops the [lease](https://example.com/l). More.")
        let title = try #require(finding.title)
        #expect(String(title.characters) == "Run drops the lease (https://example.com/l).")
        #expect(title.runs.contains { $0.inlinePresentationIntent?.contains(.code) == true })
        #expect(title.runs.allSatisfy { $0.link == nil })
    }

    @Test func longFirstSentenceMakesTheWholeFirstParagraphTheTitle() {
        let sentence = String(repeating: "word ", count: 30) + "ends here."
        let finding = FindingPresentation(message: "\(sentence) Second sentence.\n\nNext paragraph.")
        #expect(sentence.count > FindingPresentation.titleLimit)
        #expect(finding.title.map { String($0.characters) } == "\(sentence) Second sentence.")
        #expect(finding.body.map(drawn) == ["Next paragraph."])
    }

    @Test func oneSentenceMessageIsATitleWithNoBody() {
        let finding = FindingPresentation(message: "  Handle the nil case  ")
        #expect(finding.title.map { String($0.characters) } == "Handle the nil case")
        #expect(finding.body.isEmpty)
    }

    @Test func sentenceEndInsideACodeSpanDoesNotSplit() {
        let finding = FindingPresentation(message: "Call `a. b` first. Then the rest.")
        #expect(finding.title.map { String($0.characters) } == "Call a. b first.")
        #expect(finding.body.map(drawn) == ["Then the rest."])
    }

    @Test func listInterruptingTheFirstParagraphEndsIt() {
        let finding = FindingPresentation(message: "Two problems:\n- One\n- Two")
        #expect(finding.title.map { String($0.characters) } == "Two problems:")
        #expect(finding.body.map(drawn) == ["One", "Two"])
    }

    @Test(arguments: [
        "## Summary\n\nText.", "- First\n- Second", "1. First", "> Quoted.", "```\ncode\n```",
        "| a | b |\n| - | - |\n| 1 | 2 |", "---\n\nAfter a rule.",
    ])
    func messageOpeningABlockHasNoTitle(message: String) {
        let finding = FindingPresentation(message: message)
        #expect(finding.title == nil)
        #expect(!finding.body.isEmpty)
    }

    /// The underline belongs to the heading, so it draws no rule of its own.
    @Test(arguments: ["Heading\n---\n\nText.", "Two-line\nheading\n===", "Heading\n-"])
    func setextHeadingIsABlockWithItsUnderline(message: String) {
        let finding = FindingPresentation(message: message)
        #expect(finding.title == nil)
        guard case .heading = finding.body.first else {
            Issue.record("expected a heading, got \(finding.body)")
            return
        }
        #expect(!finding.body.contains(.thematicBreak))
    }

    @Test func indentedCodeKeepsItsIndentationAtTheStartAndAfterTheTitle() {
        let leading = FindingPresentation(message: "\n    let x = 1\n    let y = 2\n")
        #expect(leading.title == nil)
        guard leading.body.count == 1, case .codeBlock = leading.body[0] else {
            Issue.record("expected one code block, got \(leading.body)")
            return
        }

        let afterTitle = FindingPresentation(message: "Call it once.\n\n    run()")
        #expect(afterTitle.title.map { String($0.characters) } == "Call it once.")
        guard afterTitle.body.count == 1, case .codeBlock = afterTitle.body[0] else {
            Issue.record("expected one code block, got \(afterTitle.body)")
            return
        }
    }

    @Test func emptyMessageHasNeitherTitleNorBody() {
        let finding = FindingPresentation(message: " \n ")
        #expect(finding.title == nil)
        #expect(finding.body.isEmpty)
    }

    /// Markdown that parses by leaving text out draws as written: the body
    /// is the raw remainder as one literal block.
    @Test func bodyTheParserDropsTextFromFallsBackToRawText() {
        let finding = FindingPresentation(message: "Stale link. See [x].\n\n[x]: <> \"⚠️\"")
        #expect(finding.title.map { String($0.characters) } == "Stale link.")
        #expect(finding.body == [.plainText("See [x].\n\n[x]: <> \"⚠️\"")])
    }

    @Test func emptySeverityReadsUnrated() {
        #expect(FindingPresentation.severityLabel("") == "Unrated")
        #expect(FindingPresentation.severityLabel("P2") == "P2")
    }
}
