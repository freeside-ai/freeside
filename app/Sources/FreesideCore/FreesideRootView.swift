import SwiftUI

public struct FreesideRootView: View {
    public init() {}

    public var body: some View {
        ContentUnavailableView(
            "Freeside",
            systemImage: "checklist",
            description: Text("Attention items will appear here.")
        )
        .frame(minWidth: 320, minHeight: 240)
    }
}
