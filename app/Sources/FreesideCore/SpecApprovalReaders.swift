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

    init(text: String, digest: String, rendersScrollableContent: Bool = true) {
        preview = DecisionDetailView.NonImagePreview(bytes: Data(text.utf8))
        self.digest = digest
        self.rendersScrollableContent = rendersScrollableContent
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

            if let text = preview.text {
                if rendersScrollableContent {
                    ScrollView(.vertical) {
                        specificationText(text)
                    }
                    .frame(minHeight: 280, idealHeight: 420, maxHeight: 600)
                    .background(Color.ground2, in: RoundedRectangle(cornerRadius: 8))
                } else {
                    specificationText(text)
                        .background(Color.ground2, in: RoundedRectangle(cornerRadius: 8))
                }
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

    private func specificationText(_ text: String) -> some View {
        Text(text)
            .font(.system(.body, design: .monospaced))
            .foregroundStyle(Color.ink)
            .textSelection(.enabled)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding()
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
