import Foundation

enum SpecificationBlock: Equatable {
    case heading(level: Int, AttributedString)
    case paragraph(AttributedString)
    case listItem(ordinal: Int?, depth: Int, AttributedString)
    case listContinuation(depth: Int, AttributedString)
    indirect case listBlock(marker: String, depth: Int, SpecificationBlock)
    case codeBlock(String)
    indirect case quote(SpecificationBlock)
    case thematicBreak
    case raw(String)
    case plainText(String)
}

/// Agent-authored Markdown stays inside the reader's labeled claim frame.
/// Links lose their action and show their destination so labels cannot hide it.
/// HTML stays literal, and images lose their URL so claim text cannot fetch content.
enum SpecificationMarkdown {
    static func blocks(from text: String) -> [SpecificationBlock]? {
        var options = AttributedString.MarkdownParsingOptions(
            interpretedSyntax: .full, failurePolicy: .returnPartiallyParsedIfPossible)
        options.appliesSourcePositionAttributes = true
        guard let parsed = try? AttributedString(markdown: text, options: options) else {
            return nil
        }
        let sourceLines = text.split(omittingEmptySubsequences: false) {
            $0 == "\n" || $0 == "\r" || $0 == "\r\n"
        }
        if hasUnrepresentedEmptyLink(in: text, parsed: parsed)
            || hasUnrepresentedEmptyList(in: text, lines: sourceLines, parsed: parsed, options: options)
        {
            return [.raw(text)]
        }
        var groups: [AttributedString] = []
        var previousIdentity: Int?
        for run in parsed.runs {
            let components = run.presentationIntent?.components ?? []
            // A table is one source fallback; ordinary blocks use their leaf
            // identity because sibling list items share the outer list identity.
            let identity =
                components.first(where: {
                    if case .table = $0.kind { return true }
                    return false
                })?.identity ?? components.first?.identity
            if let identity, identity == previousIdentity, !groups.isEmpty {
                groups[groups.count - 1].append(AttributedString(parsed[run.range]))
            } else {
                groups.append(AttributedString(parsed[run.range]))
            }
            previousIdentity = identity
        }

        var seenListItems = Set<Int>()
        var blocks: [SpecificationBlock] = []
        for group in groups {
            let components = group.runs.first?.presentationIntent?.components ?? []
            let item = components.first {
                if case .listItem = $0.kind { return true }
                return false
            }
            var ancestors: [SpecificationBlock] = []
            // An item may begin with a nested list and have no paragraph of
            // its own. Emit its marker before the child and remember it when
            // a later parent paragraph resumes after that child.
            for (index, component) in components.enumerated().reversed() {
                guard component.identity != item?.identity,
                    case .listItem(let ordinal) = component.kind,
                    seenListItems.insert(component.identity).inserted
                else { continue }
                let containers = components.dropFirst(index + 1).filter {
                    $0.kind == .orderedList || $0.kind == .unorderedList
                }
                var ancestor: SpecificationBlock = .listItem(
                    ordinal: containers.first?.kind == .orderedList ? ordinal : nil,
                    depth: max(0, containers.count - 1), AttributedString(""))
                for container in components.dropFirst(index + 1) where container.kind == .blockQuote {
                    ancestor = .quote(ancestor)
                }
                ancestors.append(ancestor)
            }
            let lists = components.filter { $0.kind == .orderedList || $0.kind == .unorderedList }
            let depth = max(0, lists.count - 1)
            // Raw fallbacks also consume their item: a later paragraph must
            // remain a continuation instead of acquiring a second marker.
            let startsItem = item.map { seenListItems.insert($0.identity).inserted } ?? false
            let leaf: SpecificationBlock = {
                if components.contains(where: {
                    if case .table = $0.kind { return true }
                    return false
                }) {
                    return .raw(source(of: group, in: text, lines: sourceLines, table: true))
                }
                if group.runs.contains(where: {
                    $0.inlinePresentationIntent?.contains(.blockHTML) == true
                }) {
                    // Foundation's HTML end position can omit the closing line.
                    // Its literal payload retains all lines, with one final LF.
                    let literal = String(group.characters)
                    let lineCount = literal.components(separatedBy: "\n").count - (literal.hasSuffix("\n") ? 1 : 0)
                    return .raw(source(of: group, in: text, lines: sourceLines, lineCount: lineCount))
                }
                if group.runs.contains(where: {
                    guard $0.link != nil, let position = $0.markdownSourcePosition else { return false }
                    return position.startLine != position.endLine
                }) {
                    // Foundation both joins wrapped link words and reports a
                    // shortened label range. Preserve this block's complete source
                    // instead of guessing missing label characters or formatting.
                    return .raw(source(of: group, in: text, lines: sourceLines))
                }
                guard let content = neutralized(group, source: text) else {
                    return .raw(source(of: group, in: text, lines: sourceLines))
                }
                var block: SpecificationBlock
                switch components.first?.kind {
                case .header(let level): block = .heading(level: level, content)
                case .paragraph: block = .paragraph(content)
                case .codeBlock: block = .codeBlock(String(group.characters))
                case .thematicBreak: block = .thematicBreak
                default: return .raw(source(of: group, in: text, lines: sourceLines))
                }
                // Components run from leaf outward. Keep that order so a
                // quote around a list includes its marker, while a quoted
                // list-item body keeps the marker outside the quote.
                for component in components {
                    if component.kind == .blockQuote {
                        block = .quote(block)
                    } else if component.identity == item?.identity,
                        case .listItem(let ordinal) = component.kind
                    {
                        let displayedOrdinal = lists.first?.kind == .orderedList ? ordinal : nil
                        if case .paragraph = block {
                            block =
                                startsItem
                                ? .listItem(ordinal: displayedOrdinal, depth: depth, content)
                                : .listContinuation(depth: depth, content)
                        } else {
                            let marker = startsItem ? displayedOrdinal.map { "\($0)." } ?? "•" : ""
                            block = .listBlock(marker: marker, depth: depth, block)
                        }
                    }
                }
                return block
            }()
            if case .raw = leaf, !ancestors.isEmpty {
                // A literal child may already include its ancestors' markers
                // on the same source line. Preserve the complete input rather
                // than synthesize duplicate markers or omit an empty parent.
                return [.raw(text)]
            }
            blocks.append(contentsOf: ancestors)
            blocks.append(leaf)
        }
        return blocks
    }

    private static func hasUnrepresentedEmptyList(
        in source: String, lines: [Substring], parsed: AttributedString,
        options: AttributedString.MarkdownParsingOptions
    ) -> Bool {
        // Empty leaves have no runs, while empty ancestors still occur in a
        // child's components. Populate marker-only lines in a detection-only
        // parse: newly visible items mean the original needs exact source.
        let pattern = #"^[ \t]*(?:>[ \t]*|(?:[-+*]|[0-9]{1,9}[.)])[ \t]+)*(?:[-+*]|[0-9]{1,9}[.)])[ \t]*$"#
        var setextHeaders: [Int: Int] = [:]
        var seenHeaders = Set<Int>()
        for run in parsed.runs {
            guard let header = run.presentationIntent?.components.first,
                case .header(2) = header.kind, let position = run.markdownSourcePosition
            else { continue }
            if seenHeaders.insert(header.identity).inserted,
                let range = Range(position, in: source)
            {
                let prefix = source[lines[position.startLine - 1].startIndex..<range.lowerBound]
                if !prefix.trimmingCharacters(in: .whitespaces).hasSuffix("#") {
                    setextHeaders[header.identity] = position.endLine
                }
            } else if setextHeaders[header.identity] != nil {
                setextHeaders[header.identity] = position.endLine
            }
        }
        let underlines = Set(setextHeaders.values.map { $0 + 1 })
        var emptyLines: [String: Bool] = [:]
        var probe = source
        for (index, line) in lines.enumerated().reversed() {
            guard !underlines.contains(index + 1), line.range(of: pattern, options: .regularExpression) != nil else {
                continue
            }
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if emptyLines[trimmed] == nil {
                // A spaced thematic break resembles nested empty markers.
                // Foundation, not a second Markdown grammar, distinguishes it.
                emptyLines[trimmed] = (try? AttributedString(markdown: trimmed, options: options))?.characters.isEmpty
            }
            if emptyLines[trimmed] == true {
                probe.insert(contentsOf: " x", at: line.endIndex)
            }
        }
        guard probe != source else { return false }
        guard let populated = try? AttributedString(markdown: probe, options: options) else { return true }
        return listItemCount(in: populated) != listItemCount(in: parsed)
    }

    private static func listItemCount(in parsed: AttributedString) -> Int {
        Set(
            parsed.runs.flatMap { run in
                (run.presentationIntent?.components ?? []).compactMap { component -> Int? in
                    if case .listItem = component.kind { return component.identity }
                    return nil
                }
            }
        ).count
    }

    private static func hasUnrepresentedEmptyLink(in source: String, parsed: AttributedString) -> Bool {
        // Empty labels, including an empty image used as a link label, can
        // disappear without a run. Inspect only their source prefixes: never
        // reconstruct targets. Covered prefixes are literal code/text or
        // represented whitespace labels and need no fallback.
        let pattern = #"(?<!!)\[[ \t\r\n]*\](?:\(|\[)|\[[ \t\r\n]*!\[[ \t\r\n]*\]"#
        let represented = parsed.runs.compactMap { run in
            run.markdownSourcePosition.flatMap { Range($0, in: source) }
        }
        var remaining = source.startIndex..<source.endIndex
        while let prefix = source.range(of: pattern, options: .regularExpression, range: remaining) {
            if !represented.contains(where: { $0.overlaps(prefix) }) {
                return true
            }
            remaining = prefix.upperBound..<source.endIndex
        }
        return false
    }

    private static func source(
        of group: AttributedString, in text: String, lines: [Substring], table: Bool = false,
        lineCount: Int? = nil
    ) -> String {
        let positions = group.runs.compactMap(\.markdownSourcePosition)
        guard let start = positions.map(\.startLine).min(),
            let end = positions.map(\.endLine).max(), start > 0, start <= lines.count
        else { return String(group.characters) }
        // Header-only tables have no cell run on the separator line.
        let last = min(lines.count, lineCount.map { start + $0 - 1 } ?? max(end, table ? start + 1 : end))
        return String(text[lines[start - 1].startIndex..<lines[last - 1].endIndex])
    }

    private static func neutralized(_ text: AttributedString, source: String) -> AttributedString? {
        var result = AttributedString()
        var pendingLink: URL?
        var pendingSource: AttributedString.MarkdownSourcePosition?
        for run in text.runs {
            if pendingLink != run.link || pendingSource != run.markdownSourcePosition,
                let destination = pendingLink
            {
                result.append(AttributedString(" (\(destination.absoluteString))"))
            }
            pendingLink = run.link
            pendingSource = run.markdownSourcePosition
            if run.link != nil, let position = run.markdownSourcePosition,
                let range = Range(position, in: source)
            {
                // The full parser flattens inline styles in link labels.
                // Reparse only the label, then retain typography alone.
                // A span ending before trailing formatting delimiters is
                // incomplete; preserve the whole block instead of guessing.
                guard range.upperBound < source.endIndex,
                    source[range.upperBound] == "]" || source[range.upperBound] == ">"
                else { return nil }
                let label = String(source[range])
                let inline =
                    (try? AttributedString(
                        markdown: label, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)))
                    ?? AttributedString(label)
                guard String(inline.characters) == String(text[run.range].characters) else { return nil }
                result.append(typographyOnly(inline))
            } else {
                result.append(typographyOnly(AttributedString(text[run.range])))
            }
        }
        if let destination = pendingLink {
            result.append(AttributedString(" (\(destination.absoluteString))"))
        }
        return result
    }

    private static func typographyOnly(_ text: AttributedString) -> AttributedString {
        var result = AttributedString()
        for run in text.runs {
            // The view owns block layout. Carry only inline typography into
            // Text, never link, image, source, or list-layout attributes.
            var literal = AttributedString(String(text[run.range].characters))
            literal.inlinePresentationIntent = run.inlinePresentationIntent
            result.append(literal)
        }
        return result
    }
}
