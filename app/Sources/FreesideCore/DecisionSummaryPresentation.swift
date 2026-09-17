import Foundation
import FreesideAPI

/// Source slices for the ready card, never a new claim or a factual summary.
/// Only the small advisory heading convention is recognized. Ambiguous input
/// stays an explicitly incomplete excerpt with unknown omitted concerns.
struct DecisionSummaryPresentation {
    let original: String
    let lead: String
    let concerns: String?
    let isExcerpt: Bool
    let concernsUnknown: Bool

    init(_ text: Components.Schemas.ClaimText) {
        original = text.content
        if text.media_type == .text_sol_markdown,
            let sections = Self.sections(text.content)
        {
            let change = sections["## Change"] ?? ""
            lead = Self.excerpt(change)
            concerns = sections["## Remaining concerns"]
            isExcerpt = lead != change
            concernsUnknown = false
        } else {
            lead = Self.excerpt(text.content)
            concerns = nil
            isExcerpt = lead != text.content
            concernsUnknown = isExcerpt
        }
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

struct DecisionSummaryIdentity: Hashable {
    let itemID: String
    let claim: Components.Schemas.AgentClaim
}
