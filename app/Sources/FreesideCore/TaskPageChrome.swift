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

/// The sections the page keeps the reader's place by, in page order: each
/// draws a jump target the page reads its place from and lands moves on.
enum TaskPageSection: Hashable {
    case decisions
    case conversation
    case timeline
}

/// The page's one pinned element, over a hairline: the task's documents as
/// quiet outlined buttons, wrapping where the column can't hold them, and
/// the trailing slot (the task's menu on the Mac), which stays on the
/// first line. It draws, and holds its hairline, even with nothing in it.
struct OutlineRow<Documents: View, Trailing: View>: View {
    @ViewBuilder let documents: () -> Documents
    /// Drawn at the row's trailing end, on its first line.
    @ViewBuilder let trailing: () -> Trailing

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            WrappingHStack(horizontalSpacing: 7, verticalSpacing: 6) {
                documents()
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            trailing()
        }
        .padding(.horizontal, Self.bleed)
        .padding(.vertical, 6)
        .background(Color.ground)
        .overlay(alignment: .bottom) {
            Color.rule.frame(height: 1)
        }
        .padding(.horizontal, -Self.bleed)
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Task documents and actions")
    }

    /// How far the ground and hairline reach past the column, so a card
    /// scrolling under the pinned row never shows at its edges.
    private static var bleed: CGFloat { 8 }
}

/// The pinned row's quiet outlined button (issue #3377): a `ground2` fill
/// and a 1pt `secondaryBorder` outline at a 5pt radius, quieter than R6's
/// `ruleStrong` outlined action, which the owner chose for the row. A
/// disabled button fades its outline to `rule`, as an outlined action's does.
struct PinnedRowButtonFrame: ViewModifier {
    var isDisabled = false

    static let cornerRadius: CGFloat = 5

    func body(content: Content) -> some View {
        content
            .padding(.vertical, 3)
            .padding(.leading, 10)
            .padding(.trailing, 9)
            .background(Color.ground2, in: RoundedRectangle(cornerRadius: Self.cornerRadius))
            .overlay(
                RoundedRectangle(cornerRadius: Self.cornerRadius)
                    .strokeBorder(isDisabled ? Color.rule : Color.secondaryBorder, lineWidth: 1)
            )
            .contentShape(Rectangle())
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
