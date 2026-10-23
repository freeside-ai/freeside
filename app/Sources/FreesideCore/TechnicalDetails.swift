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

extension View {
    /// A context menu that keeps its gesture. Selectable text claims the
    /// long press on iOS for its own Copy, which would leave this menu
    /// reachable only from the view's padding, so text under it opts out of
    /// the window's selection default and the menu offers Copy for that
    /// text instead (app/README.md, Copyable Text).
    func copyMenu<MenuItems: View>(@ViewBuilder _ menuItems: () -> MenuItems) -> some View {
        textSelection(.disabled).contextMenu(menuItems: menuItems)
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
    /// Stacks the value under its label whatever its length, for a pane
    /// too narrow for a trailing column (`FactRow.stacksAlways`).
    var stacksAlways = false
    /// Whose value this is, spoken after the label, where one section
    /// holds several owners' rows of the same label.
    var owner: String? = nil

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            FactRow(
                label: row.label, value: row.value, valueColor: .inkDim,
                stacksAlways: stacksAlways
            )
            .textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: .leading)
            if rendersInteractiveControls {
                Button {
                    Clipboard.copy(row.value)
                } label: {
                    Image(systemName: "doc.on.doc")
                        .font(FreesideFont.keyword)
                        .foregroundStyle(Color.accentText)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Copy \(row.label)\(owner.map { ", \($0)" } ?? "")")
            }
        }
    }
}

/// A run of identifiers under the name of the thing they belong to, for a
/// page whose one technical-details section gathers several owners' rows.
struct IdentifierGroup: Equatable {
    let owner: String
    let rows: [AttentionDisplay.BindingRow]
}

/// The one disclosure (R2), collapsed by default and labeled "Identifiers",
/// holding a surface's exact identifiers, digests, and producer
/// coordinates, each with a copy control. The primary views name the work;
/// the values a reader occasionally needs to compare or paste live one
/// disclosure from reach, unchanged from the daemon (#1379). Every surface
/// that draws it says the same word (#2117).
struct TechnicalDetailsSection: View {
    /// Owner nil draws the rows with no heading: a surface with one owner.
    private let groups: [(owner: String?, rows: [AttentionDisplay.BindingRow])]
    /// What the closed disclosure says it holds: the surface's identity
    /// line, so the facts a header used to print stay readable without
    /// opening it. Nil draws the label alone.
    var summary: String? = nil
    var rendersInteractiveControls = true
    /// The caller's open state when it persists the fold; nil keeps it here.
    private let isExpanded: Binding<Bool>?
    /// A screenshot captures the expanded state by starting open; live use
    /// starts collapsed, as the contract requires.
    @State private var expanded: Bool

    init(
        rows: [AttentionDisplay.BindingRow],
        summary: String? = nil,
        rendersInteractiveControls: Bool = true,
        startsExpanded: Bool = false
    ) {
        groups = [(nil, rows)]
        self.summary = summary
        self.rendersInteractiveControls = rendersInteractiveControls
        isExpanded = nil
        _expanded = State(initialValue: startsExpanded)
    }

    /// One section for a page with several owners, each group's rows under
    /// its owner's name, its open state held by the caller.
    init(groups: [IdentifierGroup], summary: String? = nil, isExpanded: Binding<Bool>) {
        self.groups = groups.map { ($0.owner, $0.rows) }
        self.summary = summary
        self.isExpanded = isExpanded
        _expanded = State(initialValue: false)
    }

    var body: some View {
        if groups.contains(where: { !$0.rows.isEmpty }) {
            SentenceDisclosure(label: "Identifiers", summary: summary, isExpanded: isExpanded ?? $expanded) {
                VStack(alignment: .leading, spacing: 14) {
                    ForEach(Array(groups.enumerated()), id: \.offset) { _, group in
                        VStack(alignment: .leading, spacing: 6) {
                            if let owner = group.owner {
                                KeywordLabel(text: owner)
                                    .accessibilityAddTraits(.isHeader)
                            }
                            ForEach(Array(group.rows.enumerated()), id: \.offset) { _, row in
                                TechnicalDetailRow(
                                    row: row, rendersInteractiveControls: rendersInteractiveControls,
                                    owner: group.owner)
                            }
                        }
                    }
                }
                // A stacked row hugs its text, and a disclosure centers
                // content narrower than itself.
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }
}
