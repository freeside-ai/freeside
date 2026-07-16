import FreesideAPI
import SwiftUI

public struct FreesideRootView: View {
    @State private var session: AppSession
    @State private var selection: String?

    @MainActor
    public init(session: AppSession) {
        _session = State(initialValue: session)
    }

    /// Composes from launch arguments (see AppSession.fromEnvironment);
    /// the bare default remains the permissive mock inbox.
    @MainActor
    public init() {
        self.init(session: .fromEnvironment())
    }

    public var body: some View {
        Group {
            switch session.phase {
            case .needsPairing(let model):
                PairingView(model: model) { credential in
                    session.completePairing(credential)
                }
            case .ready(let coordinator):
                synced(coordinator)
            }
        }
        .preferredColorScheme(Self.forcedColorScheme)
    }

    private func synced(_ coordinator: SyncCoordinator) -> some View {
        // The banner sits above the split view, never over it: a
        // safe-area inset would float over the sidebar list, whose rows
        // bleed through the tinted background.
        VStack(spacing: 0) {
            FreshnessBanner(freshness: coordinator.store.freshness)
            NavigationSplitView {
                InboxView(store: coordinator.store, selection: $selection)
                    .navigationSplitViewColumnWidth(min: 260, ideal: 300)
            } detail: {
                if let selection {
                    DecisionDetailView(store: coordinator.store, itemID: selection)
                        .id(selection)
                } else {
                    ContentUnavailableView(
                        "Freeside",
                        systemImage: "checklist",
                        description: Text("Select an attention item to decide.")
                    )
                }
            }
        }
        // The heartbeat is the loss detector (plan §5.14); its first
        // round trip also bootstraps a session with no cursors yet.
        .task { await coordinator.heartbeatLoop(every: .seconds(15)) }
    }

    /// Screenshot and automation workflows pin the appearance per launch
    /// (`open FreesideMac.app --args -FreesideColorScheme light|dark`)
    /// instead of mutating the user's system appearance setting, which is
    /// host state outside the app under test. Unset or unrecognized means
    /// follow the system, the default for every ordinary launch.
    static var forcedColorScheme: ColorScheme? {
        switch UserDefaults.standard.string(forKey: "FreesideColorScheme") {
        case "light": .light
        case "dark": .dark
        default: nil
        }
    }
}
