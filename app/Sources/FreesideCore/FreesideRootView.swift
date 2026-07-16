import FreesideAPI
import SwiftUI

public struct FreesideRootView: View {
    @State private var store: InboxStore
    @State private var selection: String?

    @MainActor
    public init(client: any APIProtocol = APIClientFactory.mock()) {
        _store = State(initialValue: InboxStore(client: client))
    }

    public var body: some View {
        NavigationSplitView {
            InboxView(store: store, selection: $selection)
                .navigationSplitViewColumnWidth(min: 260, ideal: 300)
        } detail: {
            if let selection {
                DecisionDetailView(store: store, itemID: selection)
                    .id(selection)
            } else {
                ContentUnavailableView(
                    "Freeside",
                    systemImage: "checklist",
                    description: Text("Select an attention item to decide.")
                )
            }
        }
        .task { await store.refresh() }
    }
}
