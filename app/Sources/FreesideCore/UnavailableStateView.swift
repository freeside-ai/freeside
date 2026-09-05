import SwiftUI

struct UnavailableStateView: View {
    let title: String
    let systemImage: String
    let description: String

    @ScaledMetric(relativeTo: .title) private var glyphSize: CGFloat = screenshotMetricBase(
        28, relativeTo: .title)

    var body: some View {
        ContentUnavailableView {
            Label {
                Text(title).font(FreesideFont.title)
            } icon: {
                // ContentUnavailableView imposes its own image configuration;
                // an inline symbol keeps the explicit font metrics in control.
                Text(Image(systemName: systemImage))
                    .font(.system(size: glyphSize, weight: .regular))
                    .foregroundStyle(Color.inkDim)
            }
        } description: {
            Text(description).font(FreesideFont.callout)
        }
        .foregroundStyle(Color.inkDim)
    }
}

/// A sidebar column's empty state, centered in the content area below the
/// filters. The filters stay pinned at the top; only this block centers in
/// the space beneath them, the way the detail pane's ContentUnavailableView
/// centers in its own area. The call sites frame it with zero-minimum
/// spacers so the empty column still collapses and never holds the sidebar,
/// or the window, open the way a fixed-height empty state did.
struct SidebarEmptyState: View {
    let title: String
    let systemImage: String
    let description: String

    @ScaledMetric(relativeTo: .title) private var glyphSize: CGFloat = screenshotMetricBase(
        28, relativeTo: .title)

    var body: some View {
        VStack(spacing: 8) {
            // Decorative: the title and description carry the meaning, so keep
            // this glyph out of the VoiceOver order the way the Label icon slot
            // in ContentUnavailableView does.
            Text(Image(systemName: systemImage))
                .font(.system(size: glyphSize, weight: .regular))
                .accessibilityHidden(true)
            Text(title).font(FreesideFont.title)
            Text(description)
                .font(FreesideFont.callout)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
        }
        .foregroundStyle(Color.inkDim)
        .frame(maxWidth: .infinity)
        .padding(.horizontal)
        .padding(.top, 32)
    }
}
