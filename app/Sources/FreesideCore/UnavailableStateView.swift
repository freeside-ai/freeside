import SwiftUI

/// The system glyph an empty state leads with (R13, revised 8 Oct 2026):
/// one per section, so an empty pane still says which section it is.
enum EmptyStateGlyph {
    /// The inbox and what opens from it: everything filed.
    case inbox
    /// Tasks, runs, and their timelines: the tab's symbol with every item
    /// ticked.
    case tasks
    /// A device whose pairing was revoked.
    case revoked
    /// An app and daemon too far apart to talk (#3109).
    case incompatible

    var symbolName: String {
        switch self {
        case .inbox: "archivebox"
        case .tasks: "checklist.checked"
        case .revoked: "lock.slash"
        case .incompatible: "arrow.down.app"
        }
    }
}

/// A pane's empty or unavailable state (R13): its section's glyph, a serif
/// line, and a dim sans line, centered where its seat puts it.
struct UnavailableStateView: View {
    /// Where an empty state centers. Both center it in the height offered.
    enum Seat {
        /// Across the pane: a list, a reader, a sheet, a card's own body.
        case pane
        /// In the column a card takes in the detail pane (R18), at the
        /// card's x and no wider than the column's cap, so a wide pane
        /// holds the state over the place its cards draw. A pane no wider
        /// than the column and its margins centers it across its own
        /// width, as a phone does.
        case detailColumn
    }

    let glyph: EmptyStateGlyph
    let title: String
    let description: String
    var seat = Seat.pane

    var body: some View {
        EmptyStateBlock(glyph: glyph, title: title, description: description)
            .emptyStateSeat(seat)
    }
}

extension View {
    @ViewBuilder
    func emptyStateSeat(_ seat: UnavailableStateView.Seat) -> some View {
        switch seat {
        case .pane:
            padding(.horizontal, 24)
                .padding(.vertical, 40)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        case .detailColumn:
            padding(.vertical, 40)
                .frame(maxWidth: DecisionCardComposition.Scale.columnCap, maxHeight: .infinity)
                .padding(.horizontal, 24)
                .frame(maxWidth: .infinity, alignment: DecisionCardComposition.Scale.columnAlignment)
        }
    }
}

/// The empty state itself (R13): the section's glyph at 28pt in faint ink,
/// the statement in the serif, and one dim line under it, centered. The
/// glyph is decoration, and the two lines read to VoiceOver as one element,
/// the way the system's unavailable view reads its label and description.
struct EmptyStateBlock: View {
    let glyph: EmptyStateGlyph
    let title: String
    let description: String

    @ScaledMetric(relativeTo: .body) private var glyphSize: CGFloat = screenshotMetricBase(
        28, relativeTo: .body)

    var body: some View {
        VStack(spacing: 6) {
            Image(systemName: glyph.symbolName)
                .font(.system(size: glyphSize))
                .foregroundStyle(Color.inkFaint)
                .padding(.bottom, 6)
                .accessibilityHidden(true)
            Text(title)
                .font(FreesideFont.statement)
                .foregroundStyle(Color.ink)
            Text(description)
                .font(FreesideFont.cardBody)
                .foregroundStyle(Color.inkDim)
        }
        .multilineTextAlignment(.center)
        .accessibilityElement(children: .combine)
    }
}
