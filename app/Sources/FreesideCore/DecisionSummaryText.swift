import SwiftUI

/// An agent summary's blocks on a card: the card's body face at one size
/// (`FreesideFont.summary`, or the card body's among card-body lines; see
/// `Size`), so a paragraph reflows to the card's width, a list hangs under
/// its markers, and a heading is a heavier line, never a larger one. The specification reader draws the same blocks at reading
/// size (`SpecificationReaderView`). A card is not a reader: nothing here
/// scrolls sideways, and no block outgrows the text beside it. The blocks arrive
/// inert (`SpecificationMarkdown`): links carry no action, images no URL.
struct DecisionSummaryText: View {
    let blocks: [SpecificationBlock]
    var color: Color = .ink
    var size: Size = .statement

    /// The one size the blocks draw at.
    enum Size {
        /// The statement's size (`FreesideFont.summary`), for a summary in
        /// its own quote.
        case statement
        /// The card body's size (`FreesideFont.cardBody(strong:code:)`), for
        /// Markdown among card-body lines, such as a review finding on the
        /// adjudication card or the review-evidence sheet.
        case body

        func font(strong: Bool = false, code: Bool = false) -> Font {
            switch self {
            case .statement: FreesideFont.summary(strong: strong, code: code)
            case .body: FreesideFont.cardBody(strong: strong, code: code)
            }
        }
    }

    /// The bundled Plex faces have no italic, so emphasis takes the system
    /// italic at the drawn size. A sized system font does not follow
    /// Dynamic Type by itself.
    @ScaledMetric(relativeTo: .body) private var statementEmphasisSize = FreesideLadder.current.statement
    @ScaledMetric(relativeTo: .body) private var bodyEmphasisSize = FreesideLadder.current.body

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            ForEach(blocks.indices, id: \.self) { index in
                blockView(blocks[index])
                    .padding(.top, gap(before: index))
            }
        }
        .foregroundStyle(color)
        .textSelection(.enabled)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// Items of one list sit closer than the paragraphs around them.
    private func gap(before index: Int) -> CGFloat {
        guard index > 0 else { return 0 }
        return blocks[index].isListLine && blocks[index - 1].isListLine ? 3 : 8
    }

    @ViewBuilder private func blockView(_ block: SpecificationBlock) -> some View {
        switch block {
        case .heading(_, let text):
            prose(inline(text, heading: true))
        case .paragraph(let text):
            prose(inline(text))
        case .plainText(let text), .raw(let text):
            // Source the parser kept as written is still prose on a card:
            // literal, in the summary's face.
            literal(text, font: size.font())
        case .listItem(let ordinal, let depth, let text):
            listLine(marker: ordinal.map { "\($0)." } ?? "•", depth: depth) { prose(inline(text)) }
        case .listContinuation(let depth, let text):
            listLine(marker: "", depth: depth) { prose(inline(text)) }
        case .listBlock(let marker, let depth, let block):
            listLine(marker: marker, depth: depth) { AnyView(blockView(block)) }
        case .codeBlock(let text):
            literal(text, font: size.font(code: true))
        case .quote(let block):
            AnyView(blockView(block))
                .padding(.leading, 10)
                .overlay(alignment: .leading) { Rectangle().fill(Color.inkDim).frame(width: 2) }
        case .thematicBreak:
            Rectangle().fill(Color.rule).frame(height: 1).accessibilityHidden(true)
        }
    }

    private func prose(_ text: AttributedString) -> some View {
        Text(text)
            .font(size.font())
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func literal(_ text: String, font: Font) -> some View {
        Text(verbatim: text)
            .font(font)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func listLine(
        marker: String, depth: Int, @ViewBuilder content: () -> some View
    ) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Text(marker)
                .font(size.font())
                .frame(minWidth: 12, alignment: .trailing)
                .accessibilityHidden(marker == "•")
            content()
        }
        .padding(.leading, CGFloat(14 * depth))
    }

    private func inline(_ text: AttributedString, heading: Bool = false) -> AttributedString {
        let emphasisSize =
            switch size {
            case .statement: statementEmphasisSize
            case .body: bodyEmphasisSize
            }
        return Self.inline(text, size: size, emphasisSize: emphasisSize, heading: heading)
    }

    /// Inline typography at one size: strong text takes the semibold and
    /// code the mono of that size's face, and emphasis the system italic at
    /// `emphasisSize`, a Dynamic Type scaled point size. A heading is the
    /// semibold of the same face. Shared with a finding's title, which runs
    /// in with its keyword as one `Text`.
    static func inline(
        _ text: AttributedString, size: Size, emphasisSize: CGFloat, heading: Bool = false
    ) -> AttributedString {
        var result = text
        for run in text.runs {
            let intent = run.inlinePresentationIntent ?? []
            let strong = heading || intent.contains(.stronglyEmphasized)
            let code = intent.contains(.code)
            if intent.contains(.emphasized) {
                result[run.range].font = Font.system(
                    size: screenshotMetricBase(emphasisSize, relativeTo: .body),
                    weight: strong ? .semibold : .regular,
                    design: code ? .monospaced : .default
                ).italic()
            } else if strong || code {
                result[run.range].font = size.font(strong: strong, code: code)
            }
        }
        return result
    }
}

extension SpecificationBlock {
    fileprivate var isListLine: Bool {
        switch self {
        case .listItem, .listContinuation, .listBlock: true
        case .heading, .paragraph, .codeBlock, .quote, .thematicBreak, .raw, .plainText: false
        }
    }
}
