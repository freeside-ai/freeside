import SwiftUI

struct UnavailableStateView: View {
    let title: String
    let systemImage: String
    let description: String

    var body: some View {
        ContentUnavailableView {
            Label {
                Text(title).font(FreesideFont.title)
            } icon: {
                Image(systemName: systemImage)
            }
        } description: {
            Text(description).font(FreesideFont.callout)
        }
        .foregroundStyle(Color.inkDim)
    }
}
