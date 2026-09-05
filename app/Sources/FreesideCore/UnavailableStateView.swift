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
