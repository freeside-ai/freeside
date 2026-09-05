import Foundation
import FreesideAPI
import SwiftUI

enum SpecApprovalReader: String, Identifiable {
    case specification
    case diff

    var id: String { rawValue }
}

struct SpecificationReaderView: View {
    let preview: DecisionDetailView.NonImagePreview
    let digest: String
    let rendersScrollableContent: Bool
    let blocks: [SpecificationBlock]

    init(
        text: String, mediaType: Components.Schemas.ClaimText.media_typePayload,
        digest: String, rendersScrollableContent: Bool = true
    ) {
        preview = DecisionDetailView.NonImagePreview(bytes: Data(text.utf8))
        self.digest = digest
        self.rendersScrollableContent = rendersScrollableContent
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
        VStack(alignment: .leading, spacing: 10) {
            if preview.isTruncated {
                Label(
                    "Showing the first \(byteCount(DecisionDetailView.NonImagePreview.textByteLimit)) of \(byteCount(preview.byteCount))",
                    systemImage: "exclamationmark.triangle"
                )
                .font(FreesideFont.caption)
                .foregroundStyle(Color.waxText)
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color.waxWash, in: RoundedRectangle(cornerRadius: 8))
            }

            if preview.text != nil {
                VStack(alignment: .leading, spacing: 10) {
                    VStack(alignment: .leading, spacing: 4) {
                        Text("Specification (unverified)")
                            .font(FreesideFont.caption)
                            .foregroundStyle(Color.inkDim)
                        Text("Written by the agent, not checked by the daemon.")
                            .font(FreesideFont.caption)
                            .foregroundStyle(Color.inkDim)
                    }
                    if rendersScrollableContent {
                        ScrollView(.vertical) {
                            LazyVStack(alignment: .leading, spacing: 10) { blockContent }
                        }
                        .frame(minHeight: 280, idealHeight: 420, maxHeight: 600)
                    } else {
                        VStack(alignment: .leading, spacing: 10) { blockContent }
                    }
                }
                .padding()
                .freesideCard(dashed: true)
            } else {
                UnavailableStateView(
                    title: "Preview unavailable",
                    systemImage: "doc",
                    description: "This \(byteCount(preview.byteCount)) specification is not text.")
            }

            Text("Daemon-bound digest `\(digest)`")
                .font(FreesideFont.monoCaption)
                .foregroundStyle(Color.inkDim)
                .textSelection(.enabled)
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

    init(
        unified: String,
        linesAdded: Int,
        linesRemoved: Int,
        truncated: Bool,
        rendersScrollableContent: Bool = true
    ) {
        hunks = Self.parse(unified)
        self.linesAdded = linesAdded
        self.linesRemoved = linesRemoved
        self.truncated = truncated
        self.rendersScrollableContent = rendersScrollableContent
    }

    var body: some View {
        if rendersScrollableContent {
            ScrollView(.vertical) {
                content
                    .frame(maxWidth: .infinity, alignment: .topLeading)
            }
        } else {
            content
        }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("+\(linesAdded) −\(linesRemoved) lines")
                .font(FreesideFont.mono(.callout))
                .foregroundStyle(Color.inkDim)

            if truncated {
                Label(
                    Self.truncationMessage,
                    systemImage: "exclamationmark.triangle"
                )
                .font(FreesideFont.caption)
                .foregroundStyle(Color.waxText)
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(Color.waxWash, in: RoundedRectangle(cornerRadius: 8))
            }

            ForEach(Array(hunks.enumerated()), id: \.element.id) { index, hunk in
                if index == 0 {
                    hunkView(hunk)
                } else {
                    DisclosureGroup {
                        diffRows(Array(hunk.lines.dropFirst()))
                            .padding(.top, 8)
                    } label: {
                        Text(hunk.header)
                            .font(FreesideFont.mono(.caption))
                            .foregroundStyle(Color.ink)
                    }
                    .padding(10)
                    .background(Color.ground2, in: RoundedRectangle(cornerRadius: 8))
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

    @ViewBuilder
    private func hunkView(_ hunk: Hunk) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            diffRows(hunk.lines)
        }
        .background(Color.ground2, in: RoundedRectangle(cornerRadius: 8))
        .clipShape(RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder
    private func diffRows(_ lines: [Line]) -> some View {
        if rendersScrollableContent {
            ScrollView(.horizontal) {
                diffLineStack(lines)
            }
            .frame(minHeight: max(CGFloat(lines.count) * 24, 44), alignment: .topLeading)
        } else {
            diffLineStack(lines)
        }
    }

    @ViewBuilder
    private func diffLineStack(_ lines: [Line]) -> some View {
        if rendersScrollableContent {
            LazyVStack(alignment: .leading, spacing: 0) {
                diffLines(lines)
            }
        } else {
            VStack(alignment: .leading, spacing: 0) {
                diffLines(lines)
            }
        }
    }

    @ViewBuilder
    private func diffLines(_ lines: [Line]) -> some View {
        ForEach(lines) { line in
            Text(line.text.isEmpty ? " " : line.text)
                .font(FreesideFont.mono(.caption))
                .foregroundStyle(foreground(for: line.kind))
                .textSelection(.enabled)
                .fixedSize(horizontal: true, vertical: false)
                .padding(.horizontal, 10)
                .padding(.vertical, 4)
                .background(background(for: line.kind))
        }
    }

    private func foreground(for kind: LineKind) -> Color {
        switch kind {
        case .hunk: .waterText
        case .addition: .waterText
        case .removal: .waxText
        case .context: .ink
        }
    }

    private func background(for kind: LineKind) -> Color {
        switch kind {
        case .hunk: .neutralWash
        case .addition: .waterWash
        case .removal: .waxWash
        case .context: .ground2
        }
    }
}
