import Foundation
import FreesideAPI
import SwiftUI

enum SpecApprovalReader: String, Identifiable {
    case specification
    case diff

    var id: String { rawValue }

    /// The reader's name in its header row, and the shorter form a narrow
    /// pane falls back to.
    var keyword: (full: String, short: String) {
        switch self {
        case .specification: ("Specification reader", "Specification")
        case .diff: ("Diff reader", "Diff")
        }
    }
}

/// The Mac reader's header row (R16): the reader's keyword with the
/// revision as a chip, and `Close Reader` as a text control fixed in the
/// row, so closing never scrolls away with the content. A pane too narrow
/// for the full keyword draws the short one, and one too narrow for the row
/// stacks the control under the keyword.
struct SpecApprovalReaderHeader: View {
    let reader: SpecApprovalReader
    let revision: Int?
    var rendersInteractiveControls = true
    let close: () -> Void

    var body: some View {
        ViewThatFits(in: .horizontal) {
            row(reader.keyword.full)
            row(reader.keyword.short)
            VStack(alignment: .leading, spacing: 8) {
                identity(reader.keyword.short)
                closeControl
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func row(_ keyword: String) -> some View {
        HStack(alignment: .center, spacing: 12) {
            identity(keyword)
            Spacer(minLength: 0)
            closeControl
        }
    }

    private func identity(_ keyword: String) -> some View {
        HStack(alignment: .center, spacing: 10) {
            KeywordLabel(text: keyword)
                .accessibilityAddTraits(.isHeader)
            if let chip = SpecApprovalReader.revisionChip(revision) { chip }
        }
        .fixedSize()
    }

    @ViewBuilder private var closeControl: some View {
        if rendersInteractiveControls {
            Button("Close Reader", action: close)
                .buttonStyle(.plain)
                .font(FreesideFont.noticeAction)
                .foregroundStyle(Color.accentText)
                .fixedSize()
                .freesideFocusRing(cornerRadius: 4)
                .help("Close reader and show evidence and details")
        } else {
            Text("Close Reader")
                .font(FreesideFont.noticeAction)
                .foregroundStyle(Color.accentText)
                .fixedSize()
        }
    }
}

extension SpecApprovalReader {
    /// The revision a reader shows, as the chip its header carries.
    static func revisionChip(_ revision: Int?) -> StateChip? {
        revision.map { StateChip(label: "Revision \($0)", cut: .ink) }
    }
}

/// The pane or sheet owns vertical scrolling, including notices and digests.
/// Reader bodies retain only their independent horizontal overflow regions.
struct SpecApprovalReaderViewport<Content: View>: View {
    @ViewBuilder var content: Content

    var body: some View {
        ScrollView(.vertical) {
            content
                .frame(maxWidth: .infinity, alignment: .topLeading)
                .padding()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
    }
}

struct SpecificationReaderView: View {
    let preview: DecisionDetailView.NonImagePreview
    let digest: String
    let rendersScrollableContent: Bool
    /// A screenshot surface opens the technical details to capture that state;
    /// live use starts collapsed, matching the timeline sections (#1379).
    let expandsTechnicalDetails: Bool
    let blocks: [SpecificationBlock]

    init(
        text: String, mediaType: Components.Schemas.ClaimText.media_typePayload,
        digest: String, rendersScrollableContent: Bool = true,
        expandsTechnicalDetails: Bool = false
    ) {
        preview = DecisionDetailView.NonImagePreview(bytes: Data(text.utf8))
        self.digest = digest
        self.rendersScrollableContent = rendersScrollableContent
        self.expandsTechnicalDetails = expandsTechnicalDetails
        blocks = Self.blocks(for: preview.text ?? "", mediaType: mediaType)
    }

    static func blocks(
        for text: String, mediaType: Components.Schemas.ClaimText.media_typePayload,
        parse: (String) -> [SpecificationBlock]? = SpecificationMarkdown.blocks(from:)
    ) -> [SpecificationBlock] {
        guard mediaType == .text_sol_markdown else { return [.plainText(text)] }
        return parse(text) ?? [.plainText(text)]
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 22) {
            // What makes this text the one under review: the daemon bound
            // its digest to the approval (the Technical Details row).
            Text("Bound by the daemon to this approval")
                .font(FreesideFont.statement)
                .foregroundStyle(Color.ink)
                .fixedSize(horizontal: false, vertical: true)

            if preview.isTruncated {
                Notice(
                    tone: .wax, keyword: "Truncated",
                    sentence:
                        "Showing the first \(byteCount(DecisionDetailView.NonImagePreview.textByteLimit)) of \(byteCount(preview.byteCount))"
                )
                .accessibilityElement(children: .combine)
            }

            if preview.text != nil {
                VStack(alignment: .leading, spacing: 10) {
                    KeywordLabel(text: "Specification (unverified)")
                        .accessibilityAddTraits(.isHeader)
                    Group {
                        if rendersScrollableContent {
                            LazyVStack(alignment: .leading, spacing: 10) { blockContent }
                        } else {
                            VStack(alignment: .leading, spacing: 10) { blockContent }
                        }
                    }
                    .padding()
                    .freesideCard(dashed: true)
                }
            } else {
                UnavailableStateView(
                    title: "Preview unavailable",
                    description: "This \(byteCount(preview.byteCount)) specification is not text.")
            }

            // The reader's one hairline (R26), above its fold.
            VStack(alignment: .leading, spacing: 18) {
                Rectangle()
                    .fill(Color.rule)
                    .frame(height: 1)
                    .accessibilityHidden(true)
                TechnicalDetailsSection(
                    rows: [.init(label: "Daemon-Bound Digest", value: digest)],
                    startsExpanded: expandsTechnicalDetails)
            }
        }
    }

    private func byteCount(_ count: Int) -> String {
        ByteCountFormatter.string(fromByteCount: Int64(count), countStyle: .file)
    }

    private var blockContent: some View {
        ForEach(blocks.indices, id: \.self) { index in
            blockView(blocks[index])
                .foregroundStyle(Color.ink)
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    @ViewBuilder private func blockView(_ block: SpecificationBlock) -> some View {
        switch block {
        case .heading(let level, let text):
            Text(Self.inlineText(text, style: level == 1 ? .title2 : level == 2 ? .title3 : .headline)).font(
                level == 1
                    ? FreesideFont.title
                    : level == 2
                        ? FreesideFont.sectionTitle
                        : FreesideFont.sans(.headline, weight: .semibold))
        case .paragraph(let text):
            Text(Self.inlineText(text)).font(FreesideFont.body)
        case .plainText(let text):
            Text(verbatim: text).font(.system(.body, design: .monospaced))
        case .listItem(let ordinal, let depth, let text):
            listLine(text, marker: ordinal.map { "\($0)." } ?? "•", depth: depth)
        case .listContinuation(let depth, let text):
            listLine(text, marker: "", depth: depth)
        case .listBlock(let marker, let depth, let block):
            HStack(alignment: .top, spacing: 8) {
                Text(marker).font(FreesideFont.body).frame(minWidth: 24, alignment: .trailing)
                AnyView(blockView(block)).frame(maxWidth: .infinity, alignment: .leading)
            }
            .padding(.leading, CGFloat(16 * depth))
        case .codeBlock(let text), .raw(let text):
            if rendersScrollableContent {
                ScrollView(.horizontal) { literalText(text) }
            } else {
                literalText(text)
            }
        case .quote(let block):
            AnyView(blockView(block))
                .padding(.leading, 12)
                .overlay(alignment: .leading) { Rectangle().fill(Color.inkDim).frame(width: 2) }
        case .thematicBreak:
            Divider()
        }
    }

    private func listLine(_ text: AttributedString, marker: String, depth: Int) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(marker).frame(minWidth: 24, alignment: .trailing)
            Text(Self.inlineText(text)).fixedSize(horizontal: false, vertical: true)
        }
        .font(FreesideFont.body)
        .padding(.leading, CGFloat(16 * depth))
    }

    private func literalText(_ text: String) -> some View {
        Text(verbatim: text)
            .font(FreesideFont.mono(.callout))
            .padding(8)
            .background(Color.ground, in: RoundedRectangle(cornerRadius: 6))
    }

    static func inlineText(_ text: AttributedString, style: Font.TextStyle = .body) -> AttributedString {
        var result = text
        for run in text.runs {
            let intent = run.inlinePresentationIntent ?? []
            let strong = intent.contains(.stronglyEmphasized)
            let code = intent.contains(.code)
            if intent.contains(.emphasized) {
                // The bundled Plex faces have no italic variant. Use the
                // system italic face so emphasis stays visible on both OSes.
                var font = Font.system(
                    style, design: code ? .monospaced : .default,
                    weight: strong ? .semibold : .regular)
                #if canImport(AppKit)
                    if FreesideFont.screenshotDynamicTypeSize != nil {
                        font = .system(
                            size: FreesideFont.size(of: style), weight: strong ? .semibold : .regular,
                            design: code ? .monospaced : .default)
                    }
                #endif
                result[run.range].font = font.italic()
            } else if code {
                result[run.range].font = FreesideFont.mono(style, weight: strong ? .semibold : .regular)
            } else if strong {
                result[run.range].font = FreesideFont.sans(style, weight: .semibold)
            }
        }
        return result
    }
}

struct UnifiedDiffView: View {
    static let truncationMessage =
        "This unified diff is truncated. The line counts cover the full revision."

    enum LineKind: Equatable {
        case hunk
        case addition
        case removal
        case context
    }

    struct Line: Equatable, Identifiable {
        let id: Int
        let text: String
        let kind: LineKind
    }

    struct Hunk: Equatable, Identifiable {
        let id: Int
        let header: String
        let lines: [Line]
    }

    let hunks: [Hunk]
    let linesAdded: Int
    let linesRemoved: Int
    let truncated: Bool
    let rendersScrollableContent: Bool
    /// Every hunk after the first sits under one fold and stays unmounted
    /// until it opens, so a long revision costs one hunk to draw.
    @State private var hunkWidth: CGFloat = 0
    @State private var showsLaterHunks: Bool

    init(
        unified: String,
        linesAdded: Int,
        linesRemoved: Int,
        truncated: Bool,
        rendersScrollableContent: Bool = true,
        expandsLaterHunks: Bool = false
    ) {
        hunks = Self.parse(unified)
        self.linesAdded = linesAdded
        self.linesRemoved = linesRemoved
        self.truncated = truncated
        self.rendersScrollableContent = rendersScrollableContent
        _showsLaterHunks = State(initialValue: expandsLaterHunks)
    }

    /// The fold's label: "1 Later Hunk", "2 Later Hunks".
    static func laterHunksLabel(_ count: Int) -> String {
        count == 1 ? "1 Later Hunk" : "\(count) Later Hunks"
    }

    var body: some View {
        content
    }

    private var content: some View {
        let counts = DiffCounts(added: linesAdded, removed: linesRemoved)
        return VStack(alignment: .leading, spacing: 12) {
            Text("\(counts.text) lines")
                .font(FreesideFont.trailingSummary)
                .foregroundStyle(Color.inkDim)
                .accessibilityLabel("\(counts.spoken) lines")

            if truncated {
                Notice(tone: .wax, keyword: "Truncated", sentence: Self.truncationMessage)
                    .accessibilityElement(children: .combine)
            }

            if let first = hunks.first {
                hunkView(first)
            }
            if hunks.count > 1 {
                SentenceDisclosure(
                    label: Self.laterHunksLabel(hunks.count - 1), isExpanded: $showsLaterHunks
                ) {
                    VStack(alignment: .leading, spacing: 12) {
                        ForEach(hunks.dropFirst()) { hunk in
                            hunkView(hunk)
                        }
                    }
                }
            }
        }
    }

    static func parse(_ unified: String) -> [Hunk] {
        let sourceLines = unified.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        var hunks: [Hunk] = []
        var header = "Diff"
        var lines: [Line] = []
        var lineID = 0

        func appendHunk() {
            guard !lines.isEmpty else { return }
            hunks.append(Hunk(id: hunks.count, header: header, lines: lines))
            lines = []
        }

        for sourceLine in sourceLines {
            if sourceLine.hasPrefix("@@") {
                appendHunk()
                header = sourceLine
            }
            lines.append(Line(id: lineID, text: sourceLine, kind: kind(of: sourceLine)))
            lineID += 1
        }
        appendHunk()
        return hunks
    }

    static func kind(of line: String) -> LineKind {
        if line.hasPrefix("@@") { return .hunk }
        if line.hasPrefix("+") && !line.hasPrefix("+++") { return .addition }
        if line.hasPrefix("-") && !line.hasPrefix("---") { return .removal }
        return .context
    }

    /// One hunk as a bordered block (R3): its header line, then its lines
    /// in the diff cuts.
    private func hunkView(_ hunk: Hunk) -> some View {
        diffRows(hunk.lines)
            .padding(.vertical, 8)
            // A zero minimum keeps the block at the reader's width when a
            // static render has no scroll view to hold a long line.
            .frame(minWidth: 0, maxWidth: .infinity, alignment: .leading)
            .clipShape(RoundedRectangle(cornerRadius: 8))
            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
    }

    @ViewBuilder
    private func diffRows(_ lines: [Line]) -> some View {
        if rendersScrollableContent {
            // A scroll view sizes its content to the widest line, so the
            // block's own width is the floor that carries a short line's
            // wash to the border.
            ScrollView(.horizontal) {
                diffLineStack(lines).frame(minWidth: hunkWidth, alignment: .leading)
            }
            .onGeometryChange(for: CGFloat.self) {
                $0.size.width
            } action: {
                hunkWidth = $0
            }
        } else {
            diffLineStack(lines)
        }
    }

    @ViewBuilder
    private func diffLineStack(_ lines: [Line]) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            if rendersScrollableContent && lines.count > 128 {
                // Bound the number of native text views even for alternating
                // line kinds. Each chunk keeps native width/height measurement.
                ForEach(Array(stride(from: 0, to: lines.count, by: 64)), id: \.self) { start in
                    Text(chunkText(lines[start..<min(start + 64, lines.count)]))
                        .font(FreesideFont.trailingSummary)
                        .lineSpacing(8)
                        .textSelection(.enabled)
                        .fixedSize(horizontal: true, vertical: false)
                        .padding(.horizontal, 10)
                        .padding(.vertical, 4)
                }
            } else {
                diffLines(lines)
            }
        }
    }

    private func chunkText(_ lines: ArraySlice<Line>) -> AttributedString {
        var result = AttributedString()
        for (index, line) in lines.enumerated() {
            if index > 0 { result.append(AttributedString("\n")) }
            var text = Self.cut(line)
            text.backgroundColor = background(for: line.kind)
            result.append(text)
        }
        return result
    }

    @ViewBuilder
    private func diffLines(_ lines: [Line]) -> some View {
        ForEach(lines) { line in
            Text(Self.cut(line))
                .font(FreesideFont.trailingSummary)
                .textSelection(.enabled)
                .fixedSize(horizontal: true, vertical: false)
                .padding(.horizontal, 10)
                .padding(.vertical, 4)
                // The wash runs the width of the widest line, not of its
                // own text, so added and removed lines read as bands.
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(background(for: line.kind))
        }
    }

    /// One line in its diff cut (R28). A hunk header is dim, with its
    /// removed and added ranges in the remove and add colors; the text is
    /// the line as the diff wrote it, so a copy stays exact.
    static func cut(_ line: Line) -> AttributedString {
        var text = AttributedString(line.text.isEmpty ? " " : line.text)
        switch line.kind {
        case .addition: text.foregroundColor = .diffAdd
        case .removal: text.foregroundColor = .diffRemove
        case .context: text.foregroundColor = .ink
        case .hunk:
            text.foregroundColor = .inkDim
            for (range, color) in hunkRanges(in: line.text) {
                if let lower = AttributedString.Index(range.lowerBound, within: text),
                    let upper = AttributedString.Index(range.upperBound, within: text)
                {
                    text[lower..<upper].foregroundColor = color
                }
            }
        }
        return text
    }

    /// The `-a,b` and `+c,d` ranges of a `@@ -a,b +c,d @@` header, each with
    /// the cut it takes. A header in any other shape yields none.
    static func hunkRanges(in header: String) -> [(Range<String.Index>, Color)] {
        let tokens = header.split(separator: " ", maxSplits: 3, omittingEmptySubsequences: false)
        guard tokens.count >= 3, tokens[0] == "@@" else { return [] }
        var ranges: [(Range<String.Index>, Color)] = []
        if tokens[1].hasPrefix("-") { ranges.append((tokens[1].startIndex..<tokens[1].endIndex, .diffRemove)) }
        if tokens[2].hasPrefix("+") { ranges.append((tokens[2].startIndex..<tokens[2].endIndex, .diffAdd)) }
        return ranges
    }

    private func background(for kind: LineKind) -> Color {
        switch kind {
        case .addition: .diffAddWash
        case .removal: .diffRemoveWash
        case .hunk, .context: .clear
        }
    }
}
