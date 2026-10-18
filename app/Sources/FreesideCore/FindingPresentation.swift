import Foundation
import FreesideAPI

/// A review finding as one readable unit: a short title and the rest of the
/// message as Markdown blocks. No finding on the wire carries a title, so
/// every surface derives it from the message by one rule (#2060):
///
/// - The title is the first sentence of the message's first paragraph when
///   that sentence is at most `titleLimit` characters, else the whole first
///   paragraph.
/// - A message whose first line opens a Markdown block (a heading, list,
///   quote, fence, table, or rule) has no title; the whole message is the
///   body.
/// - An empty message has neither.
///
/// The sentence split is a heuristic: an abbreviation such as "e.g." ends
/// the title early. Nothing is lost when it does, since what follows the
/// title is drawn as the body.
struct FindingPresentation: Equatable {
    static let titleLimit = 120

    /// The title with its inline Markdown, links neutralized as the body's
    /// are. Raw text when the title does not parse as one paragraph.
    let title: AttributedString?
    let body: [SpecificationBlock]

    init(message: String) {
        let (titleSource, rest) = Self.split(message)
        title = titleSource.map(Self.inlineTitle)
        body =
            rest.isEmpty ? [] : DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_markdown, content: rest))
    }

    /// The title as a strong line above the body, for a surface that draws
    /// the whole finding with one block renderer.
    var titledBlocks: [SpecificationBlock] {
        (title.map { [.heading(level: 3, $0)] } ?? []) + body
    }

    /// A severity as a finding's header names it.
    static func severityLabel(_ severity: String) -> String {
        severity.isEmpty ? "Unrated" : severity
    }

    /// The severity chip's cut, by what the daemon does with the finding
    /// (`daemon/internal/engine/per_finding_acceptance.go`): P0 and P1 hold
    /// for a decision, so they draw in ink; P2 and P3 are advisory, so they
    /// draw faint. An empty severity is a finding with no badge, dashed as
    /// not rated. No accent: R23 keeps it for the fill, a link, and the
    /// attention mark.
    static func severityCut(_ severity: String) -> (cut: StateChip.Cut, dashed: Bool) {
        switch severity {
        case "P0", "P1": return (.ink, false)
        case "": return (.faint, true)
        default: return (.faint, false)
        }
    }

    /// The message's title source and the source left for the body.
    static func split(_ message: String) -> (title: String?, rest: String) {
        // Blank lines go, but not a line's indentation: four leading spaces
        // make an indented code line, at the start or after the title.
        let lines = withoutBlankEdges(message.split(separator: "\n", omittingEmptySubsequences: false)[...])
        let whole = lines.joined(separator: "\n")
        guard let first = lines.first, !opensBlock(first), !isIndentedCode(first) else { return (nil, whole) }
        // The first paragraph ends at a blank line or at a line that opens a
        // block, as a list or a fence can interrupt a paragraph.
        let paragraphEnd =
            lines.dropFirst().firstIndex {
                isBlank($0) || isSetextUnderline($0) || opensBlock($0)
            } ?? lines.endIndex
        // An underline makes the paragraph a setext heading, so the message
        // opens a block.
        if paragraphEnd < lines.endIndex, isSetextUnderline(lines[paragraphEnd]) { return (nil, whole) }
        let paragraph = lines[..<paragraphEnd].joined(separator: "\n").trimmingCharacters(in: .whitespaces)
        let after = withoutBlankEdges(lines[paragraphEnd...]).joined(separator: "\n")
        guard let end = firstSentenceEnd(paragraph),
            paragraph[..<end].count <= titleLimit
        else { return (paragraph, after) }
        let remainder = paragraph[end...].trimmingCharacters(in: .whitespacesAndNewlines)
        return (
            String(paragraph[..<end]),
            [remainder, after].filter { !$0.isEmpty }.joined(separator: "\n\n")
        )
    }

    private static func withoutBlankEdges(_ lines: ArraySlice<Substring>) -> ArraySlice<Substring> {
        let start = lines.firstIndex { !isBlank($0) } ?? lines.endIndex
        let end = lines.lastIndex { !isBlank($0) }.map { $0 + 1 } ?? start
        return lines[start..<max(start, end)]
    }

    private static func isBlank(_ line: Substring) -> Bool {
        line.allSatisfy(\.isWhitespace)
    }

    /// The end of the paragraph's first sentence: past a `.`, `!`, or `?`
    /// that whitespace follows, outside a code span. Nil when the sentence
    /// is the whole paragraph.
    private static func firstSentenceEnd(_ paragraph: String) -> String.Index? {
        var inCode = false
        var index = paragraph.startIndex
        while index < paragraph.endIndex {
            let character = paragraph[index]
            let next = paragraph.index(after: index)
            if character == "`" {
                inCode.toggle()
            } else if !inCode, ".!?".contains(character), next < paragraph.endIndex,
                paragraph[next].isWhitespace
            {
                return next
            }
            index = next
        }
        return nil
    }

    /// Whether a line opens a Markdown block rather than a paragraph's
    /// prose. Up to three leading spaces still open one; four make an
    /// indented code line, which cannot interrupt a paragraph.
    private static func opensBlock(_ line: Substring) -> Bool {
        let leading = line.prefix { $0 == " " }.count
        guard leading <= 3 else { return false }
        let content = String(line.dropFirst(leading))
        let patterns = [
            #"^#{1,6}(\s|$)"#,  // ATX heading
            #"^[-*+](\s|$)"#,  // bullet item
            #"^[0-9]{1,9}[.)](\s|$)"#,  // ordered item
            #"^>"#,  // quote
            #"^(```|~~~)"#,  // fence
            #"^\|"#,  // table row
            #"^([-*_])[ \t]*(\1[ \t]*){2,}$"#,  // thematic break
        ]
        return patterns.contains { content.range(of: $0, options: .regularExpression) != nil }
    }

    /// A line of `=` or `-` alone, which turns the paragraph above it into
    /// a heading. Checked before `opensBlock`, since `---` under prose is
    /// an underline, not a rule.
    private static func isSetextUnderline(_ line: Substring) -> Bool {
        line.range(of: #"^ {0,3}(=+|-+)[ \t]*$"#, options: .regularExpression) != nil
    }

    /// A non-blank line indented four columns, by spaces or a tab.
    private static func isIndentedCode(_ line: Substring) -> Bool {
        let content = line.drop { $0 == " " }
        return !isBlank(line) && (line.count - content.count >= 4 || content.first == "\t")
    }

    private static func inlineTitle(_ source: String) -> AttributedString {
        let blocks = DecisionSummaryPresentation.blocks(.init(media_type: .text_sol_markdown, content: source))
        guard blocks.count == 1, case .paragraph(let text) = blocks[0] else { return AttributedString(source) }
        return text
    }
}
