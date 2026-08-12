import FreesideAPI
import SwiftUI

public struct FreesideRootView: View {
    @State private var session: AppSession
    @State private var screen: LaunchInputs.Screen
    @State private var attentionSelection: String?
    @State private var runSelection: String?
    private let launchColorScheme: ColorScheme?
    private let launchInboxScope: InboxStore.Scope?
    private let launchProjectID: String?
    private let launchDetailsExpanded: Bool

    @MainActor
    public init(session: AppSession, launchInputs: LaunchInputs = .standard()) {
        _session = State(initialValue: session)
        _screen = State(initialValue: launchInputs.screen)
        _attentionSelection = State(
            initialValue: launchInputs.screen == .inbox ? launchInputs.selection : nil)
        _runSelection = State(
            initialValue: launchInputs.screen == .runs ? launchInputs.selection : nil)
        launchColorScheme = launchInputs.colorScheme
        launchInboxScope = launchInputs.inboxScope
        launchProjectID = launchInputs.projectID
        launchDetailsExpanded = launchInputs.detailsExpanded
    }

    /// Composes from launch arguments (see AppSession.fromEnvironment
    /// and LaunchInputs); the bare default remains the permissive mock
    /// inbox.
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
        .preferredColorScheme(launchColorScheme)
    }

    private func synced(_ coordinator: SyncCoordinator) -> some View {
        // The banner sits above the split view, never over it: a
        // safe-area inset would float over the sidebar list, whose rows
        // bleed through the tinted background.
        VStack(spacing: 0) {
            FreshnessBanner(freshness: coordinator.store.freshness)
            NavigationSplitView {
                VStack(spacing: 0) {
                    Picker("Section", selection: $screen) {
                        Label("Inbox", systemImage: "tray.full").tag(LaunchInputs.Screen.inbox)
                        Label("Runs", systemImage: "point.3.connected.trianglepath.dotted")
                            .tag(LaunchInputs.Screen.runs)
                    }
                    .pickerStyle(.segmented)
                    .padding()
                    switch screen {
                    case .inbox:
                        InboxView(
                            store: coordinator.store, selection: $attentionSelection,
                            launchScope: launchInboxScope, launchProjectID: launchProjectID)
                    case .runs:
                        RunsListView(
                            runs: coordinator.runs,
                            schedules: coordinator.schedules,
                            selection: $runSelection)
                    }
                }
                .navigationSplitViewColumnWidth(min: 280, ideal: 320)
            } detail: {
                switch screen {
                case .inbox:
                    if let attentionSelection {
                        DecisionDetailView(
                            store: coordinator.store, itemID: attentionSelection,
                            detailsExpanded: launchDetailsExpanded
                        )
                        .id(attentionSelection)
                    } else {
                        ContentUnavailableView(
                            "Freeside",
                            systemImage: "checklist",
                            description: Text("Select an attention item to decide."))
                    }
                case .runs:
                    if let runSelection,
                        let run = coordinator.runs.first(where: { $0.run.id == runSelection })
                    {
                        RunTimelineView(coordinator: coordinator, snapshot: run)
                            .id(runSelection)
                    } else {
                        ContentUnavailableView(
                            "Runs",
                            systemImage: "point.3.connected.trianglepath.dotted",
                            description: Text("Select a run to inspect its timeline."))
                    }
                }
            }
        }
        // The heartbeat is the loss detector (plan §5.14); its first
        // round trip also bootstraps a session with no cursors yet.
        .task { await coordinator.heartbeatLoop(every: .seconds(15)) }
    }
}
