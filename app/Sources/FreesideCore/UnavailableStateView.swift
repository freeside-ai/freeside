import SwiftUI

/// A pane's empty or unavailable state (R13): the key, a serif line, and a
/// dim sans line, centered in the pane.
struct UnavailableStateView: View {
    let title: String
    let description: String

    var body: some View {
        KeyMarkEmptyState(title: title, description: description)
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
    let title: String
    let description: String

    var body: some View {
        KeyMarkEmptyState(title: title, description: description)
            .frame(maxWidth: .infinity)
            .padding(.horizontal)
            .padding(.top, 32)
    }
}

/// The empty state itself (R13): the Freeside key at 32pt in faint ink, the
/// statement in the serif, and one dim line under it, centered. The key is
/// decoration, and the two lines read to VoiceOver as one element, the way
/// the system's unavailable view reads its label and description.
private struct KeyMarkEmptyState: View {
    let title: String
    let description: String

    @ScaledMetric(relativeTo: .body) private var markHeight: CGFloat = screenshotMetricBase(
        32, relativeTo: .body)

    var body: some View {
        VStack(spacing: 6) {
            KeyMark()
                .fill(Color.inkFaint, style: FillStyle(eoFill: true))
                .frame(width: markHeight * KeyMark.aspectRatio, height: markHeight)
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
