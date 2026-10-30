import FreesideAPI
import SwiftUI

/// The task page's opening: eyebrow, serif title, one status line, and what
/// the task's own state adds under it. It scrolls away with the page.
struct PageHeader<Detail: View>: View {
    let eyebrow: String
    let title: TaskNameLabel
    let status: String
    @ViewBuilder let detail: () -> Detail

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            CardEyebrow(keyword: eyebrow)
            title
            Text(status)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
                .fixedSize(horizontal: false, vertical: true)
            detail()
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// The places the outline row jumps to, in page order.
enum TaskPageSection: Hashable {
    case decisions
    case task
    case conversation
    case timeline
    case runs
}

/// The page's one pinned element: a row of keyword chips over a hairline.
/// Selection records the last chip chosen and doesn't follow scrolling.
/// Decisions draws at one open item or more, and Decisions and Runs are
/// native popups when they hold several. One line at every text size: the
/// row scrolls sideways where the column can't hold it.
struct OutlineRow: View {
    struct Entry: Identifiable, Equatable {
        let id: String
        let title: String
    }

    let decisions: [Entry]
    /// The decision whose card leads, which its popup checks.
    let leadingItemID: String?
    let conversationMessages: Int
    let runs: [Entry]
    let selected: TaskPageSection
    var rendersInteractiveControls = true
    let onSelect: (TaskPageSection) -> Void
    let onChooseDecision: (String) -> Void
    let onChooseRun: (String) -> Void

    var body: some View {
        Group {
            if rendersInteractiveControls {
                ViewThatFits(in: .horizontal) {
                    chips
                    ScrollView(.horizontal, showsIndicators: false) { chips }
                }
            } else {
                // A renderer can't draw a scroll view: the drawn row clips
                // where the live row scrolls.
                chips
                    .fixedSize(horizontal: true, vertical: false)
                    // No minimum, so the column can't grow to fit the chips.
                    .frame(minWidth: 0, maxWidth: .infinity, alignment: .leading)
                    .clipped()
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, Self.bleed)
        .padding(.vertical, 6)
        .background(Color.ground)
        .overlay(alignment: .bottom) {
            Color.rule.frame(height: 1)
        }
        .padding(.horizontal, -Self.bleed)
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Page outline")
    }

    /// How far the ground and hairline reach past the column, so a card
    /// scrolling under the pinned row never shows at its edges.
    private static let bleed: CGFloat = 8

    private var chips: some View {
        HStack(spacing: 4) {
            if !decisions.isEmpty {
                menuChip(
                    "Decisions", section: .decisions, entries: decisions, checked: leadingItemID,
                    choose: onChooseDecision)
            }
            chip("Task", section: .task)
            chip("Conversation", count: conversationMessages, section: .conversation)
            chip("Timeline", section: .timeline)
            menuChip("Runs", section: .runs, entries: runs, checked: nil, choose: onChooseRun)
        }
    }

    private func chip(_ title: String, count: Int? = nil, section: TaskPageSection) -> some View {
        Button {
            onSelect(section)
        } label: {
            OutlineChip(title: title, count: count, isSelected: selected == section)
        }
        .buttonStyle(.plain)
        .freesideFocusRing(cornerRadius: 4)
    }

    /// A chip that counts `entries`, and with more than one is a popup of
    /// them. With one it jumps to its section, where that entry is.
    @ViewBuilder
    private func menuChip(
        _ title: String, section: TaskPageSection, entries: [Entry], checked: String?,
        choose: @escaping (String) -> Void
    ) -> some View {
        if entries.count > 1 {
            let label = OutlineChip(
                title: title, count: entries.count, isSelected: selected == section, opensMenu: true)
            if rendersInteractiveControls {
                Menu {
                    ForEach(entries) { entry in
                        Button {
                            choose(entry.id)
                        } label: {
                            if entry.id == checked {
                                Label(entry.title, systemImage: "checkmark")
                            } else {
                                Text(entry.title)
                            }
                        }
                    }
                } label: {
                    label
                }
                .menuStyle(.button)
                .buttonStyle(.plain)
                .menuIndicator(.hidden)
                .fixedSize()
            } else {
                label
            }
        } else {
            chip(title, count: entries.count, section: section)
        }
    }
}

/// One outline chip: a keyword on the selected fill, with its count, and the
/// popup mark where it opens one.
private struct OutlineChip: View {
    let title: String
    var count: Int? = nil
    let isSelected: Bool
    var opensMenu = false

    var body: some View {
        let tone = isSelected ? Color.ink : Color.inkDim
        HStack(spacing: 6) {
            KeywordLabel(text: title, color: tone)
            if let count {
                Text("\(count)")
                    .font(FreesideFont.keyword)
                    .foregroundStyle(tone)
            }
            if opensMenu {
                Image(systemName: "arrowtriangle.down.fill")
                    .font(FreesideFont.disclosureGlyph)
                    .foregroundStyle(Color.accentText)
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 5)
        .background {
            if isSelected {
                RoundedRectangle(cornerRadius: 4).fill(Color.segmentSelected)
            }
        }
        .contentShape(Rectangle())
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(count.map { "\(title), \($0)" } ?? title)
        .accessibilityAddTraits(isSelected ? .isSelected : [])
    }
}

/// An open item that doesn't lead the page: its ask on one line and its type
/// chip. Choosing it swaps which card leads.
struct OneLineItemRow: View {
    let ask: String
    let type: String
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 12) {
                Text(ask)
                    .font(FreesideFont.rowTitle)
                    .foregroundStyle(Color.ink)
                    .lineLimit(1)
                    .truncationMode(.tail)
                Spacer(minLength: 0)
                StateChip(label: type, cut: .attention)
                    .fixedSize()
            }
            .padding(.vertical, 9)
            .padding(.horizontal, 14)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.ground2, in: RoundedRectangle(cornerRadius: 8))
            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.itemBorder, lineWidth: 1))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .freesideFocusRing(cornerRadius: 8)
        .accessibilityLabel("\(ask), \(type)")
        .accessibilityHint("Leads the page with this decision.")
    }
}
