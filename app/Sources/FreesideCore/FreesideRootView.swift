import FreesideAPI
import SwiftUI

public struct FreesideRootView: View {
    @Environment(\.dynamicTypeSize) private var systemDynamicTypeSize
    @State private var session: AppSession
    @State private var navigation: NavigationModel
    @State private var technicalDetailsRequest: TechnicalDetailsRevealRequest?
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
        _navigation = State(initialValue: NavigationModel(launchInputs: launchInputs))
        _technicalDetailsRequest = State(initialValue: nil)
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
        @Bindable var navigation = navigation
        return VStack(spacing: 0) {
            FreshnessBanner(freshness: coordinator.store.freshness)
            platformNavigation(
                coordinator,
                selectedTab: $navigation.selectedTab,
                inboxPath: $navigation.inboxPath,
                runsPath: $navigation.runsPath,
                attentionSelection: $navigation.attentionSelection,
                runSelection: $navigation.runSelection)
        }
        // The heartbeat is the loss detector (plan §5.14); its first
        // round trip also bootstraps a session with no cursors yet.
        .task { await coordinator.heartbeatLoop(every: .seconds(15)) }
        .onChange(of: navigation.attentionSelection) {
            technicalDetailsRequest = technicalDetailsRequest?.retained(
                for: navigation.attentionSelection)
        }
    }

    @ViewBuilder
    private func platformNavigation(
        _ coordinator: SyncCoordinator,
        selectedTab: Binding<LaunchInputs.Screen>,
        inboxPath: Binding<[String]>,
        runsPath: Binding<[String]>,
        attentionSelection: Binding<String?>,
        runSelection: Binding<String?>
    ) -> some View {
        #if os(iOS)
            if #available(iOS 18.0, *) {
                TabView(selection: selectedTab) {
                    Tab("Inbox", systemImage: "tray.full", value: LaunchInputs.Screen.inbox) {
                        inboxStack(
                            coordinator,
                            path: inboxPath,
                            selection: attentionSelection)
                    }
                    .badge(coordinator.store.openSnapshots.count)

                    Tab(
                        "Runs", systemImage: "point.3.connected.trianglepath.dotted",
                        value: LaunchInputs.Screen.runs
                    ) {
                        runsStack(coordinator, path: runsPath, selection: runSelection)
                    }
                }
                .tabViewStyle(.sidebarAdaptable)
            } else {
                TabView(selection: selectedTab) {
                    inboxStack(
                        coordinator,
                        path: inboxPath,
                        selection: attentionSelection
                    )
                    .tabItem { Label("Inbox", systemImage: "tray.full") }
                    .tag(LaunchInputs.Screen.inbox)
                    .badge(coordinator.store.openSnapshots.count)

                    runsStack(coordinator, path: runsPath, selection: runSelection)
                        .tabItem {
                            Label("Runs", systemImage: "point.3.connected.trianglepath.dotted")
                        }
                        .tag(LaunchInputs.Screen.runs)
                }
            }
        #else
            NavigationSplitView {
                VStack(spacing: 0) {
                    Picker("Section", selection: selectedTab) {
                        Label("Inbox", systemImage: "tray.full").tag(LaunchInputs.Screen.inbox)
                        Label("Runs", systemImage: "point.3.connected.trianglepath.dotted")
                            .tag(LaunchInputs.Screen.runs)
                    }
                    .pickerStyle(.segmented)
                    .padding()
                    switch selectedTab.wrappedValue {
                    case .inbox:
                        InboxView(
                            store: coordinator.store, selection: attentionSelection,
                            launchScope: launchInboxScope, launchProjectID: launchProjectID,
                            onRevealTechnicalDetails: revealTechnicalDetails)
                    case .runs:
                        RunsListView(
                            runs: coordinator.runs,
                            schedules: coordinator.schedules,
                            selection: runSelection)
                    }
                }
                .background(Color.sidebarGround)
                .navigationSplitViewColumnWidth(min: 280, ideal: 320)
            } detail: {
                macDetail(
                    coordinator,
                    screen: selectedTab.wrappedValue,
                    attentionSelection: attentionSelection.wrappedValue,
                    runSelection: runSelection.wrappedValue
                )
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Color.ground)
            }
        #endif
    }

    #if os(iOS)
        private func inboxStack(
            _ coordinator: SyncCoordinator,
            path: Binding<[String]>,
            selection: Binding<String?>
        ) -> some View {
            NavigationStack(path: path) {
                InboxView(
                    store: coordinator.store,
                    selection: selection,
                    launchScope: launchInboxScope,
                    launchProjectID: launchProjectID,
                    navigationPath: path,
                    onRevealTechnicalDetails: revealTechnicalDetails
                )
                .navigationDestination(for: String.self) { itemID in
                    DecisionDetailView(
                        store: coordinator.store,
                        itemID: itemID,
                        detailsExpanded: launchDetailsExpanded,
                        detailsRevealRequest: technicalDetailsRequest,
                        onConsumeDetailsRevealRequest: consumeTechnicalDetailsRequest)
                }
            }
        }

        private func runsStack(
            _ coordinator: SyncCoordinator,
            path: Binding<[String]>,
            selection: Binding<String?>
        ) -> some View {
            NavigationStack(path: path) {
                RunsListView(
                    runs: coordinator.runs,
                    schedules: coordinator.schedules,
                    selection: selection,
                    navigationPath: path
                )
                .navigationDestination(for: String.self) { runID in
                    if let run = coordinator.runs.first(where: { $0.run.id == runID }) {
                        RunTimelineView(coordinator: coordinator, snapshot: run)
                    } else {
                        ContentUnavailableView(
                            "Run unavailable",
                            systemImage: "questionmark.circle",
                            description: Text("This run is no longer available."))
                    }
                }
            }
        }
    #else
        @ViewBuilder
        private func macDetail(
            _ coordinator: SyncCoordinator,
            screen: LaunchInputs.Screen,
            attentionSelection: String?,
            runSelection: String?
        ) -> some View {
            switch screen {
            case .inbox:
                if let attentionSelection {
                    DecisionDetailView(
                        store: coordinator.store,
                        itemID: attentionSelection,
                        detailsExpanded: launchDetailsExpanded,
                        detailsRevealRequest: technicalDetailsRequest,
                        onConsumeDetailsRevealRequest: consumeTechnicalDetailsRequest
                    )
                    .id(attentionSelection)
                } else {
                    OperationalSummaryView(
                        summary: OperationalSummary(
                            openSnapshots: coordinator.store.openSnapshots,
                            runs: coordinator.runs,
                            freshness: coordinator.store.freshness))
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
    #endif

    private func revealTechnicalDetails(_ itemID: String) {
        navigation.route(to: .attentionItem(itemID))
        technicalDetailsRequest = .init(itemID: itemID, nonce: UUID())
    }

    private func consumeTechnicalDetailsRequest(_ nonce: UUID) {
        technicalDetailsRequest = technicalDetailsRequest?.consuming(nonce)
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
