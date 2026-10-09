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

    var symbolName: String {
        switch self {
        case .inbox: "archivebox"
        case .tasks: "checklist.checked"
        case .revoked: "lock.slash"
        }
    }
}

/// A pane's empty or unavailable state (R13): its section's glyph, a serif
/// line, and a dim sans line, centered in the pane.
struct UnavailableStateView: View {
    let glyph: EmptyStateGlyph
    let title: String
    let description: String

    var body: some View {
        EmptyStateBlock(glyph: glyph, title: title, description: description)
            .padding(.horizontal, 24)
            .padding(.vertical, 40)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

/// A sidebar column's empty state, centered in the content area below the
/// filters. The filters stay pinned at the top; only this block centers in
/// the space beneath them, the way the detail pane's unavailable state
/// centers in its own area. The call sites frame it with zero-minimum
/// spacers so the empty column still collapses and never holds the sidebar,
/// or the window, open the way a fixed-height empty state did.
struct SidebarEmptyState: View {
    let glyph: EmptyStateGlyph
    let title: String
    let description: String

    var body: some View {
        EmptyStateBlock(glyph: glyph, title: title, description: description)
            .frame(maxWidth: .infinity)
            .padding(.horizontal)
            .padding(.top, 32)
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
