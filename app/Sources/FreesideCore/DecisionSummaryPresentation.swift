import Foundation
import FreesideAPI

/// Source slices for a card's agent summary, never a new claim or a factual
/// summary. Only the small advisory heading convention is recognized.
/// Ambiguous input stays an explicitly incomplete excerpt with unknown
/// omitted concerns.
struct DecisionSummaryPresentation {
    let original: String
    /// The source prefix that bounds the lead.
    let lead: String
    let concerns: String?
    /// Whether the drawn lead leaves out text the report holds.
    let isExcerpt: Bool
    let concernsUnknown: Bool
    /// The lead and the concerns as the card draws them: Markdown as its
    /// blocks, plain text as written. The same source, formatted; no text
    /// the report does not hold. The concerns are never cut.
    let leadBlocks: [SpecificationBlock]
    let concernBlocks: [SpecificationBlock]?

    init(_ text: Components.Schemas.ClaimText) {
        original = text.content
        if text.media_type == .text_sol_markdown,
            let sections = Self.sections(text.content)
        {
            let change = sections["## Change"] ?? ""
            lead = Self.excerpt(change)
            concerns = sections["## Remaining concerns"]
            (leadBlocks, isExcerpt) = Self.leadBlocks(of: change, lead: lead)
            concernsUnknown = false
            concernBlocks = concerns.map { Self.blocks(.init(media_type: .text_sol_markdown, content: $0)) }
        } else {
            lead = Self.excerpt(text.content)
            concerns = nil
            (leadBlocks, isExcerpt) =
                text.media_type == .text_sol_markdown
                ? Self.leadBlocks(of: text.content, lead: lead)
                : ([.plainText(lead)], lead != text.content)
            concernsUnknown = isExcerpt
            concernBlocks = nil
        }
    }

    /// A claim's whole text as blocks. Plain text stays one literal block.
    /// So does Markdown the parser fails on, and Markdown it parses by
    /// leaving text out (a link reference definition, a link title, an
    /// image's URL): the card shows the source as written before it shows
    /// less than the report holds. The test is that every character of the
    /// source that cannot be a marker is drawn, in order; the parser may add
    /// text (a link's destination after its label) but never has to remove
    /// any. A dropped run of ASCII punctuation alone is not told from the
    /// markers the parser consumes, and passes.
    static func blocks(_ text: Components.Schemas.ClaimText) -> [SpecificationBlock] {
        guard text.media_type == .text_sol_markdown,
            let blocks = SpecificationMarkdown.blocks(from: text.content)
        else { return [.plainText(text.content)] }
        // Characters, not a string: joining what is left after the filter
        // would merge a virama with the consonant across a removed space.
        var drawn = blocks.flatMap { Array($0.drawnText) }.filter(\.isContent)[...]
        for character in text.content where character.isContent {
            guard let match = drawn.firstIndex(of: character) else { return [.plainText(text.content)] }
            drawn = drawn[drawn.index(after: match)...]
        }
        return blocks
    }

    /// The lead's blocks, and whether they leave text out. A source prefix
    /// that ends at a paragraph break parses on its own. One that ends
    /// inside a block would leave a list marker or an emphasis span
    /// half-open, and the parser prints those literally, so that cut is made
    /// in the parsed source instead, after as many drawn characters as the
    /// prefix holds. Inline markers are not drawn, so that cut can fall
    /// later in the source than the prefix ends, and a source whose drawn
    /// text fits the bound is no excerpt at all. Each block after the first
    /// also spends `blockBreakCost`, or a report of very short blocks would
    /// draw far more rows than its source prefix holds.
    private static func leadBlocks(
        of source: String, lead: String
    ) -> (blocks: [SpecificationBlock], isExcerpt: Bool) {
        let parsed = { blocks(.init(media_type: .text_sol_markdown, content: $0)) }
        let whole = parsed(source)
        guard lead != source else { return (whole, false) }
        if source.dropFirst(lead.count).hasPrefix("\n\n") {
            let cut = parsed(lead)
            return (cut, cut != whole)
        }
        var remaining = lead.count
        var result: [SpecificationBlock] = []
        for block in whole {
            let breakCost = result.isEmpty ? 0 : blockBreakCost
            let count = block.characterCount + breakCost
            guard count > remaining else {
                result.append(block)
                remaining -= count
                continue
            }
            if let cut = block.prefix(max(remaining - breakCost, 0), splitsWords: result.isEmpty) {
                result.append(cut)
            }
            return (result, true)
        }
        return (whole, false)
    }

    /// What a block spends of the lead's bound beyond the characters it
    /// draws: the source a line break and the shortest list marker take
    /// (`"\n- "`). A block that draws nothing, such as a rule, costs this
    /// much too, so the bound holds whatever the blocks are.
    private static let blockBreakCost = 3

    /// How many concerns the report lists, when its concerns section is a
    /// flat Markdown list and so counts itself. Prose is not counted: a
    /// number the source does not state would be a new claim.
    var concernCount: Int? {
        guard let concerns else { return nil }
        var count = 0
        for line in concerns.split(separator: "\n", omittingEmptySubsequences: true) {
            guard !line.allSatisfy(\.isWhitespace) else { continue }
            if line.first?.isWhitespace == true {
                // A continuation of the item above; before any item it is
                // prose.
                guard count > 0 else { return nil }
            } else if Self.startsListItem(line) {
                count += 1
            } else {
                return nil
            }
        }
        return count > 0 ? count : nil
    }

    private static func startsListItem(_ line: Substring) -> Bool {
        if line.hasPrefix("- ") || line.hasPrefix("* ") || line.hasPrefix("+ ") { return true }
        let digits = line.prefix { $0.isASCII && $0.isNumber }
        guard !digits.isEmpty else { return false }
        let rest = line.dropFirst(digits.count)
        return rest.hasPrefix(". ") || rest.hasPrefix(") ")
    }

    private static func excerpt(_ source: String) -> String {
        guard source.count > 800 else { return source }
        let prefix = source.prefix(800)
        if let paragraph = prefix.range(of: "\n\n"),
            prefix[..<paragraph.lowerBound].count >= 80
        {
            return String(prefix[..<paragraph.lowerBound])
        }
        if let space = prefix.lastIndex(where: \.isWhitespace) {
            return String(prefix[..<space])
        }
        return String(prefix)
    }

    private static func sections(_ source: String) -> [String: String]? {
        let allowed: Set<String> = ["## Change", "## Remaining concerns", "## Details"]
        var result: [String: String] = [:]
        var heading: String?
        // Keep line endings and body bytes intact. A fenced block, unknown
        // heading, or repeated heading makes section boundaries ambiguous.
        for line in source.split(separator: "\n", omittingEmptySubsequences: false) {
            let trimmed = line.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !trimmed.hasPrefix("```"), !trimmed.hasPrefix("~~~") else { return nil }
            if trimmed.hasPrefix("#") {
                guard !line.hasPrefix(" "), !line.hasPrefix("\t") else { return nil }
                guard allowed.contains(trimmed), result[trimmed] == nil else { return nil }
                if heading == nil, trimmed != "## Change" { return nil }
                heading = trimmed
                result[trimmed] = ""
            } else if let heading {
                result[heading, default: ""] += String(line) + "\n"
            } else if !trimmed.isEmpty {
                return nil
            }
        }
        guard let change = result["## Change"], let concerns = result["## Remaining concerns"],
            !change.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
            !concerns.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        else { return nil }
        return result.mapValues { $0.trimmingCharacters(in: .newlines) }
    }
}

extension SpecificationBlock {
    /// The characters the block draws, without its marker.
    fileprivate var characterCount: Int {
        switch self {
        case .heading(_, let text), .paragraph(let text), .listItem(_, _, let text),
            .listContinuation(_, let text):
            text.characters.count
        case .listBlock(_, _, let block), .quote(let block):
            block.characterCount
        case .codeBlock(let text), .raw(let text), .plainText(let text):
            text.count
        case .thematicBreak:
            0
        }
    }

    /// The block's text as drawn, with the marker a list line draws.
    fileprivate var drawnText: String {
        switch self {
        case .heading(_, let text), .paragraph(let text), .listContinuation(_, let text):
            String(text.characters)
        case .listItem(let ordinal, _, let text):
            (ordinal.map { "\($0)." } ?? "") + String(text.characters)
        case .listBlock(let marker, _, let block):
            marker + block.drawnText
        case .quote(let block):
            block.drawnText
        case .codeBlock(let text), .raw(let text), .plainText(let text):
            text
        case .thematicBreak:
            ""
        }
    }

    /// The block cut to its first `limit` characters at a word boundary, as
    /// `excerpt` cuts source text. Nil when nothing would be left to draw,
    /// which includes a limit that ends inside the block's first word unless
    /// `splitsWords` allows the cut there (a lead that is one long word).
    fileprivate func prefix(_ limit: Int, splitsWords: Bool) -> SpecificationBlock? {
        func cut(_ text: AttributedString) -> AttributedString? {
            let characters = text.characters
            let end = characters.index(characters.startIndex, offsetBy: min(limit, characters.count))
            guard let stop = characters[..<end].lastIndex(where: \.isWhitespace) ?? (splitsWords ? end : nil)
            else { return nil }
            return stop > characters.startIndex ? AttributedString(text[..<stop]) : nil
        }
        func cut(_ text: String) -> String? {
            let end = text.prefix(limit)
            guard let stop = end.lastIndex(where: \.isWhitespace) ?? (splitsWords ? end.endIndex : nil)
            else { return nil }
            return stop > text.startIndex ? String(text[..<stop]) : nil
        }
        switch self {
        case .heading(let level, let text):
            return cut(text).map { .heading(level: level, $0) }
        case .paragraph(let text):
            return cut(text).map { .paragraph($0) }
        case .listItem(let ordinal, let depth, let text):
            return cut(text).map { .listItem(ordinal: ordinal, depth: depth, $0) }
        case .listContinuation(let depth, let text):
            return cut(text).map { .listContinuation(depth: depth, $0) }
        case .listBlock(let marker, let depth, let block):
            return block.prefix(limit, splitsWords: splitsWords).map {
                .listBlock(marker: marker, depth: depth, $0)
            }
        case .quote(let block):
            return block.prefix(limit, splitsWords: splitsWords).map { .quote($0) }
        case .codeBlock(let text):
            return cut(text).map { .codeBlock($0) }
        case .raw(let text):
            return cut(text).map { .raw($0) }
        case .plainText(let text):
            return cut(text).map { .plainText($0) }
        case .thematicBreak:
            return nil
        }
    }
}

extension Character {
    /// Whether Markdown syntax cannot account for the character. Every
    /// marker is whitespace or ASCII punctuation, so anything else (a
    /// letter, a digit, an emoji, a dash) is text the report holds.
    fileprivate var isContent: Bool { !isWhitespace && !(isASCII && (isPunctuation || isSymbol)) }
}

struct DecisionSummaryIdentity: Hashable {
    let itemID: String
    let claim: Components.Schemas.AgentClaim
}
