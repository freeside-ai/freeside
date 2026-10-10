import SwiftUI

/// An agent summary's blocks on a card: the card's body face at one size
/// (`FreesideFont.summary`), so a paragraph reflows to the card's width, a
/// list hangs under its markers, and a heading is a heavier line, never a
/// larger one. The specification reader draws the same blocks at reading
/// size (`SpecificationReaderView`). A card is not a reader: nothing here
/// scrolls sideways, and no block outgrows the text beside it. The blocks arrive
/// inert (`SpecificationMarkdown`): links carry no action, images no URL.
struct DecisionSummaryText: View {
    let blocks: [SpecificationBlock]
    var color: Color = .ink

    /// The bundled Plex faces have no italic, so emphasis takes the system
    /// italic at the summary's size. A sized system font does not follow
    /// Dynamic Type by itself.
    @ScaledMetric(relativeTo: .body) private var emphasisSize = FreesideLadder.current.statement

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
            literal(text, font: FreesideFont.summary())
        case .listItem(let ordinal, let depth, let text):
            listLine(marker: ordinal.map { "\($0)." } ?? "•", depth: depth) { prose(inline(text)) }
        case .listContinuation(let depth, let text):
            listLine(marker: "", depth: depth) { prose(inline(text)) }
        case .listBlock(let marker, let depth, let block):
            listLine(marker: marker, depth: depth) { AnyView(blockView(block)) }
        case .codeBlock(let text):
            literal(text, font: FreesideFont.summary(code: true))
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
            .font(FreesideFont.summary())
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
                .font(FreesideFont.summary())
                .frame(minWidth: 12, alignment: .trailing)
                .accessibilityHidden(marker == "•")
            content()
        }
        .padding(.leading, CGFloat(14 * depth))
    }

    /// Inline typography at the summary's one size. A heading is the
    /// semibold of the same face.
    private func inline(_ text: AttributedString, heading: Bool = false) -> AttributedString {
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
                result[run.range].font = FreesideFont.summary(strong: strong, code: code)
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
