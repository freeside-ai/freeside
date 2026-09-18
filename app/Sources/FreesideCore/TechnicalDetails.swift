import SwiftUI

#if os(macOS)
    import AppKit
#elseif os(iOS)
    import UIKit
#endif

/// The clipboard write the task timeline, run timeline, and decision detail
/// each repeated per platform. One helper so a copy control anywhere writes
/// the exact string it was handed.
enum Clipboard {
    static func copy(_ string: String) {
        #if os(macOS)
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(string, forType: .string)
        #elseif os(iOS)
            UIPasteboard.general.string = string
        #endif
    }
}

/// Shortens an opaque identifier for primary text, where it appears only to
/// disambiguate one row from another. A real run or campaign id is its kind
/// prefix plus 64 hex characters and a task id its prefix plus 26 characters
/// (`daemon/internal/store/production_attempt.go`, `task.go`); a digest is
/// `sha256:` plus 64 hex characters. The exact value stays in the technical
/// details section, so a short form never has to round-trip.
enum ShortIdentifier {
    private static let digestPrefix = "sha256:"
    private static let kindPrefixes = ["run-", "campaign-", "task-"]

    /// An id of 24 characters or fewer prints whole. A longer one keeps its
    /// kind prefix and the next 8 characters; a digest keeps `sha256:` and its
    /// first 12 hex characters. An id with no known kind prefix is left whole
    /// rather than truncated at a meaningless point.
    static func short(_ identifier: String) -> String {
        if identifier.hasPrefix(digestPrefix) {
            let hex = identifier.dropFirst(digestPrefix.count)
            return hex.count > 12 ? "\(digestPrefix)\(hex.prefix(12))…" : identifier
        }
        guard identifier.count > 24 else { return identifier }
        for prefix in kindPrefixes where identifier.hasPrefix(prefix) {
            return "\(prefix)\(identifier.dropFirst(prefix.count).prefix(8))…"
        }
        return identifier
    }
}

/// One technical-details row: the shared `FactRow` value under its label, with
/// a copy control that writes the exact, full value. The label names the value
/// for VoiceOver.
///
/// `rendersInteractiveControls` is false only where a non-interactive copy of a
/// card draws the row (the decision inspector's screenshot), matching the flag
/// the composers already take; every live surface, and every screenshot of
/// these sections, shows the button, because a touch platform has no hover to
/// reveal it.
struct TechnicalDetailRow: View {
    let row: AttentionDisplay.BindingRow
    var rendersInteractiveControls = true

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            FactRow(label: row.label, value: row.value, valueColor: .inkDim)
                .font(FreesideFont.monoCaption)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            if rendersInteractiveControls {
                Button {
                    Clipboard.copy(row.value)
                } label: {
                    Image(systemName: "doc.on.doc")
                        .font(FreesideFont.caption)
                        .foregroundStyle(Color.accentText)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Copy \(row.label)")
            }
        }
    }
}

/// A disclosure, collapsed by default and labeled "Technical details", holding
/// a surface's exact identifiers, digests, and producer coordinates, each with
/// a copy control. The primary views name the work; the values a reader
/// occasionally needs to compare or paste live one disclosure from reach,
/// unchanged from the daemon (#1379).
struct TechnicalDetailsSection: View {
    let rows: [AttentionDisplay.BindingRow]
    var rendersInteractiveControls = true
    /// A screenshot captures the expanded state by starting open; live use
    /// starts collapsed, as the contract requires.
    @State private var expanded: Bool

    init(
        rows: [AttentionDisplay.BindingRow],
        rendersInteractiveControls: Bool = true,
        startsExpanded: Bool = false
    ) {
        self.rows = rows
        self.rendersInteractiveControls = rendersInteractiveControls
        _expanded = State(initialValue: startsExpanded)
    }

    var body: some View {
        if !rows.isEmpty {
            DisclosureGroup(isExpanded: $expanded) {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(Array(rows.enumerated()), id: \.offset) { _, row in
                        TechnicalDetailRow(row: row, rendersInteractiveControls: rendersInteractiveControls)
                    }
                }
                .padding(.top, 6)
            } label: {
                KeywordLabel(text: "Technical details")
            }
            .tint(.accentText)
        }
    }
}
