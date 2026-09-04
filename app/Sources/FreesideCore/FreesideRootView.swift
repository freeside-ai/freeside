import FreesideAPI
import SwiftUI

public struct FreesideRootView: View {
    @Environment(\.dynamicTypeSize) private var systemDynamicTypeSize
    @Environment(\.scenePhase) private var scenePhase
    @State private var session: AppSession
    @State private var navigation: NavigationModel
    @State private var feedback: DecisionFeedbackModel
    @State private var flowPreferences: DecisionFlowPreferences
    @State private var technicalDetailsRequest: TechnicalDetailsRevealRequest?
    @State private var showsInboxClearResult: Bool
    private let launchColorScheme: ColorScheme?
    private let launchInboxScope: InboxStore.Scope?
    private let launchProjectID: String?
    private let launchDetailsExpanded: Bool
    private let launchDynamicTypeSize: DynamicTypeSize?

    @MainActor
    public init(
        session: AppSession,
        launchInputs: LaunchInputs = .standard(),
        navigation: NavigationModel? = nil,
        flowPreferences: DecisionFlowPreferences? = nil
    ) {
        FreesideFont.registration
        FreesideNavigationChrome.apply()
        _session = State(initialValue: session)
        _navigation = State(
            initialValue: navigation ?? NavigationModel(launchInputs: launchInputs))
        _feedback = State(initialValue: DecisionFeedbackModel())
        _flowPreferences = State(
            initialValue: flowPreferences ?? DecisionFlowPreferences())
        _technicalDetailsRequest = State(initialValue: nil)
        _showsInboxClearResult = State(initialValue: false)
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
            FreshnessBanner(
                freshness: coordinator.store.freshness,
                lastUpdatedAt: coordinator.lastUpdatedAt)
            platformNavigation(
                coordinator,
                selectedTab: operatorSelectedTabBinding,
                inboxPath: operatorInboxPathBinding,
                runsPath: $navigation.runsPath,
                attentionSelection: rawAttentionSelectionBinding,
                runSelection: $navigation.runSelection)
        }
        // The heartbeat is the loss detector (plan §5.14); its first
        // round trip also bootstraps a session with no cursors yet.
        .task {
            #if os(iOS)
                coordinator.startReachabilityMonitoring()
                defer { coordinator.stopReachabilityMonitoring() }
                await coordinator.heartbeatLoop(every: SyncCoordinator.heartbeatInterval)
            #endif
        }
        .onChange(of: scenePhase) {
            guard scenePhase == .active else { return }
            Task { await coordinator.automaticRefresh() }
        }
        .onChange(of: navigation.attentionSelection) {
            technicalDetailsRequest = technicalDetailsRequest?.retained(
                for: navigation.attentionSelection)
        }
        .onChange(of: coordinator.store.openSnapshots.map(\.item.id)) {
            if !coordinator.store.openSnapshots.isEmpty {
                showsInboxClearResult = false
            }
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
                    .badge(coordinator.store.urgentOpenCount)

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
                    .badge(coordinator.store.urgentOpenCount)

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
                            interactiveSelection: operatorAttentionSelectionBinding,
                            onFilterChange: navigation.recordOperatorNavigation,
                            onMoveSelection: {
                                navigation.moveAttentionSelection(
                                    by: $0, store: coordinator.store)
                            },
                            lastUpdatedAt: coordinator.lastUpdatedAt,
                            onRefresh: coordinator.refresh,
                            onRevealTechnicalDetails: revealTechnicalDetails)
                    case .runs:
                        RunsListView(
                            runs: coordinator.runs,
                            schedules: coordinator.schedules,
                            selection: runSelection,
                            onRefresh: coordinator.refresh)
                    }
                }
                .background(Color.sidebarGround)
                .navigationSplitViewColumnWidth(min: 280, ideal: 320)
            } detail: {
                VStack(spacing: 0) {
                    DecisionFeedbackBanner(
                        feedback: feedback,
                        onView: viewConcludedItem)
                    macDetail(
                        coordinator,
                        screen: selectedTab.wrappedValue,
                        attentionSelection: attentionSelection.wrappedValue,
                        runSelection: runSelection.wrappedValue
                    )
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .background(Color.ground)
                }
            }
            .toolbar {
                ToolbarItemGroup {
                    Button {
                        Task { await coordinator.refresh() }
                    } label: {
                        Label("Refresh", systemImage: "arrow.clockwise")
                    }
                    .help("Refresh")
                    LastUpdatedLabel(lastUpdatedAt: coordinator.lastUpdatedAt)
                    Button {
                        navigation.inspectorPresented.toggle()
                    } label: {
                        Label("Inspector", systemImage: "sidebar.trailing")
                    }
                    .help(
                        navigation.inspectorPresented ? "Hide Inspector" : "Show Inspector")
                }
            }
        #endif
    }

    #if os(iOS)
        private func inboxStack(
            _ coordinator: SyncCoordinator,
            path: Binding<[String]>,
            selection: Binding<String?>
        ) -> some View {
            VStack(spacing: 0) {
                DecisionFeedbackBanner(feedback: feedback, onView: viewConcludedItem)
                NavigationStack(path: path) {
                    InboxView(
                        store: coordinator.store,
                        selection: selection,
                        launchScope: launchInboxScope,
                        launchProjectID: launchProjectID,
                        navigationPath: rawInboxPathBinding,
                        onFilterChange: navigation.recordOperatorNavigation,
                        lastUpdatedAt: coordinator.lastUpdatedAt,
                        onRefresh: coordinator.refresh,
                        onRevealTechnicalDetails: revealTechnicalDetails
                    )
                    .navigationDestination(for: String.self) { itemID in
                        DecisionDetailView(
                            store: coordinator.store,
                            itemID: itemID,
                            detailsExpanded: launchDetailsExpanded,
                            detailsRevealRequest: technicalDetailsRequest,
                            onConsumeDetailsRevealRequest: consumeTechnicalDetailsRequest,
                            onSelectItem: { navigation.route(to: .attentionItem($0)) },
                            onConclusion: { conclusion in
                                handleConclusion(conclusion, coordinator: coordinator)
                            })
                    }
                }
                .toolbar {
                    ToolbarItem(placement: .topBarTrailing) {
                        decisionFlowMenu
                    }
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
                    navigationPath: path,
                    onRefresh: coordinator.refresh
                )
                .navigationDestination(for: String.self) { runID in
                    if let run = coordinator.runs.first(where: { $0.run.id == runID }) {
                        RunTimelineView(coordinator: coordinator, snapshot: run)
                    } else {
                        UnavailableStateView(
                            title: "Run unavailable",
                            systemImage: "questionmark.circle",
                            description: "This run is no longer available.")
                    }
                }
            }
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    decisionFlowMenu
                }
            }
        }

        private var decisionFlowMenu: some View {
            @Bindable var preferences = flowPreferences
            return Menu {
                Toggle(
                    "Advance after decisions",
                    isOn: $preferences.advancesAutomatically)
            } label: {
                Label("Decision Flow", systemImage: "gearshape")
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
                        onConsumeDetailsRevealRequest: consumeTechnicalDetailsRequest,
                        inspectorPresented: Bindable(navigation).inspectorPresented,
                        onSelectItem: { navigation.route(to: .attentionItem($0)) },
                        onConclusion: { conclusion in
                            handleConclusion(conclusion, coordinator: coordinator)
                        }
                    )
                    .id(attentionSelection)
                } else if showsInboxClearResult {
                    UnavailableStateView(
                        title: "Inbox clear", systemImage: "checkmark",
                        description: "There are no open attention items.")
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
                    UnavailableStateView(
                        title: "Runs", systemImage: "point.3.connected.trianglepath.dotted",
                        description: "Select a run to inspect its timeline.")
                }
            }
        }
    #endif

    private func revealTechnicalDetails(_ itemID: String) {
        navigation.route(to: .attentionItem(itemID))
        technicalDetailsRequest = .init(itemID: itemID, nonce: UUID())
    }

    private func handleConclusion(
        _ conclusion: DecisionConclusion,
        coordinator: SyncCoordinator
    ) {
        let operatorNavigationRevision = navigation.operatorNavigationRevision
        feedback.present(
            conclusion,
            advancesAutomatically: flowPreferences.advancesAutomatically
        ) {
            switch navigation.advanceAfterConclusion(
                itemID: conclusion.itemID,
                expectedOperatorNavigationRevision: operatorNavigationRevision,
                advancesToNextItem: flowPreferences.advancesAutomatically,
                store: coordinator.store)
            {
            case .advanced, .returnedToInbox:
                showsInboxClearResult = false
            case .inboxClear:
                showsInboxClearResult = true
            case .cancelled:
                break
            }
        }
    }

    private var operatorSelectedTabBinding: Binding<LaunchInputs.Screen> {
        Binding(
            get: { navigation.selectedTab },
            set: { navigation.selectTab($0) })
    }

    private var operatorInboxPathBinding: Binding<[String]> {
        Binding(
            get: { navigation.inboxPath },
            set: { navigation.setInboxPath($0) })
    }

    private var rawInboxPathBinding: Binding<[String]> {
        Binding(
            get: { navigation.inboxPath },
            set: { navigation.inboxPath = $0 })
    }

    private var rawAttentionSelectionBinding: Binding<String?> {
        Binding(
            get: { navigation.attentionSelection },
            set: { navigation.attentionSelection = $0 })
    }

    private var operatorAttentionSelectionBinding: Binding<String?> {
        Binding(
            get: { navigation.attentionSelection },
            set: { navigation.selectAttentionItem($0) })
    }

    private func viewConcludedItem(_ itemID: String) {
        feedback.dismiss()
        showsInboxClearResult = false
        navigation.route(to: .attentionItem(itemID))
    }

    private func consumeTechnicalDetailsRequest(_ nonce: UUID) {
        technicalDetailsRequest = technicalDetailsRequest?.consuming(nonce)
    }

}
