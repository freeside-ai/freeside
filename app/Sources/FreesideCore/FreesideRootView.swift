import FreesideAPI
import SwiftUI

public struct FreesideRootView: View {
    @Environment(\.dynamicTypeSize) private var systemDynamicTypeSize
    @State private var session: AppSession
    @State private var screen: LaunchInputs.Screen
    @State private var attentionSelection: String?
    @State private var runSelection: String?
    private let launchColorScheme: ColorScheme?
    private let launchInboxScope: InboxStore.Scope?
    private let launchProjectID: String?
    private let launchDetailsExpanded: Bool
    private let launchDynamicTypeSize: DynamicTypeSize?

    @MainActor
    public init(session: AppSession, launchInputs: LaunchInputs = .standard()) {
        FreesideFont.registration
        FreesideNavigationChrome.apply()
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
        launchDynamicTypeSize = launchInputs.dynamicTypeSize
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
        .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
        .preferredColorScheme(launchColorScheme)
        .background(Color.ground)
        .tint(.accentText)
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
                .background(Color.sidebarGround)
                .navigationSplitViewColumnWidth(min: 280, ideal: 320)
            } detail: {
                detail(coordinator)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .background(Color.ground)
            }
        }
        // The heartbeat is the loss detector (plan §5.14); its first
        // round trip also bootstraps a session with no cursors yet.
        .task { await coordinator.heartbeatLoop(every: .seconds(15)) }
    }

    @ViewBuilder
    private func detail(_ coordinator: SyncCoordinator) -> some View {
        switch screen {
        case .inbox:
            if let attentionSelection {
                DecisionDetailView(
                    store: coordinator.store, itemID: attentionSelection,
                    detailsExpanded: launchDetailsExpanded
                )
                .id(attentionSelection)
            } else {
                emptyDetail("Freeside", systemImage: "checklist", description: "Select an attention item to decide.")
            }
        case .runs:
            if let runSelection,
                let run = coordinator.runs.first(where: { $0.run.id == runSelection })
            {
                RunTimelineView(coordinator: coordinator, snapshot: run)
                    .id(runSelection)
            } else {
                emptyDetail(
                    "Runs", systemImage: "point.3.connected.trianglepath.dotted",
                    description: "Select a run to inspect its timeline.")
            }
        }
    }

    private func emptyDetail(_ title: String, systemImage: String, description: String) -> some View {
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
