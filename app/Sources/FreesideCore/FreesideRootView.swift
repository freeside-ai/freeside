import FreesideAPI
import SwiftUI

public struct FreesideRootView: View {
    /// The macOS sidebar's section switcher; iOS keeps its tab bar.
    static let sectionSegments: [FreesideSegmentedControl<LaunchInputs.Screen>.Segment] = [
        .init(value: .inbox, label: "Inbox"),
        .init(value: .tasks, label: "Tasks"),
    ]

    @Environment(\.dynamicTypeSize) private var systemDynamicTypeSize
    @Environment(\.scenePhase) private var scenePhase
    @State private var session: AppSession
    @State private var navigation: NavigationModel
    @State private var feedback: DecisionFeedbackModel
    @State private var flowPreferences: DecisionFlowPreferences
    @State private var technicalDetailsRequest: TechnicalDetailsRevealRequest?
    @State private var showsInboxClearResult: Bool
    @State private var connectionAddress = ""
    @State private var stopRecoveryPresented = false
    @State private var rePairConfirmationPresented = false
    @State private var rePairFailure: String?
    private let launchColorScheme: ColorScheme?
    private let launchInboxScope: InboxStore.Scope?
    private let launchProjectID: String?
    private let launchDetailsExpanded: Bool
    private let launchDynamicTypeSize: DynamicTypeSize?
    private let environment: FreesideEnvironment

    /// `environment` defaults to `.prod`, which draws no badge, so every
    /// screenshot surface renders as it did before tiers existed.
    @MainActor
    public init(
        session: AppSession,
        launchInputs: LaunchInputs = .standard(),
        navigation: NavigationModel? = nil,
        flowPreferences: DecisionFlowPreferences? = nil,
        environment: FreesideEnvironment = .prod
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
        self.environment = environment
    }

    /// Composes from launch arguments (see AppSession.fromEnvironment
    /// and LaunchInputs); an unconfigured device asks for its daemon address.
    @MainActor
    public init() {
        self.init(session: .fromEnvironment())
    }

    public var body: some View {
        Group {
            switch session.phase {
            case .needsConnection:
                DaemonConnectionView(address: $connectionAddress, refusal: session.connectionRefusal) { url in
                    connectionAddress = url.absoluteString
                    session.connect(serverURL: url)
                }
            case .needsPairing(let model):
                PairingView(
                    model: model,
                    onChangeServer: connectionAddress.isEmpty ? nil : { session.changeServer() },
                    onPaired: { credential in session.completePairing(credential) })
            case .ready(let coordinator):
                synced(coordinator)
            }
        }
        // Above the phase switch, so the connect, pairing, and synced
        // screens all say which daemon tier the operator is looking at.
        .overlay(alignment: .topTrailing) {
            if let badgeTitle = environment.badgeTitle {
                EnvironmentBadge(title: badgeTitle)
                    .padding(8)
            }
        }
        .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
        .preferredColorScheme(launchColorScheme)
        .background(Color.ground)
        .tint(.accentText)
        // The default titlebar renders a bright system material that reads
        // as a hard white, square-cornered band over the warm ground.
        // Hiding it lets each screen's own ground rise into the titlebar, so
        // the toolbar blends into the body and the window's rounded corners
        // carry the top edge. Applied above the phase switch so it also
        // covers the pairing screen, which renders without platformNavigation.
        #if os(macOS)
            .toolbarBackground(.hidden, for: .windowToolbar)
        #endif
    }

    private func synced(_ coordinator: SyncCoordinator) -> some View {
        @Bindable var navigation = navigation
        let pendingUnderOldPairing = Self.unsentActionCount(
            pendingCommands: coordinator.store.pendingCommandsByItemID.count,
            pendingTaskSubmissions: coordinator.pendingTaskSubmissions.count,
            taskStops: coordinator.pendingTaskStops.values)
        return VStack(spacing: 0) {
            FreshnessBanner(
                freshness: coordinator.store.freshness,
                lastUpdatedAt: coordinator.lastUpdatedAt,
                onRePair: { rePairConfirmationPresented = true })
            platformNavigation(
                coordinator,
                selectedTab: operatorSelectedTabBinding,
                inboxPath: operatorInboxPathBinding,
                tasksPath: operatorTasksPathBinding,
                attentionSelection: rawAttentionSelectionBinding,
                taskSelection: operatorTaskSelectionBinding)
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
        // Re-pair is offered only while revoked. If a transient failure clears
        // while the dialog is open, freshness returns to a non-revoked state
        // and the banner's action goes away; dismiss the open confirmation so
        // its destructive button cannot delete a credential that just
        // re-authenticated.
        .onChange(of: FreshnessBanner.showsRePairAction(for: coordinator.store.freshness, hasHandler: true)) {
            if !FreshnessBanner.showsRePairAction(for: coordinator.store.freshness, hasHandler: true) {
                rePairConfirmationPresented = false
            }
        }
        // Deleting the credential cannot be undone, so the revoked banner's
        // "Pair Again" confirms first (#1458). A 401 can be the wrong daemon
        // answering, so the operator, not the app, decides.
        .confirmationDialog(
            "Pair this device again?",
            isPresented: $rePairConfirmationPresented,
            titleVisibility: .visible
        ) {
            Button("Pair Again", role: .destructive) {
                performRePair(for: coordinator.store.freshness)
            }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text(Self.rePairConfirmationMessage(pendingUnderOldPairing: pendingUnderOldPairing))
        }
        .alert(
            "Couldn't pair again",
            isPresented: Binding(
                get: { rePairFailure != nil },
                set: { if !$0 { rePairFailure = nil } })
        ) {
            Button("OK", role: .cancel) { rePairFailure = nil }
        } message: {
            Text(rePairFailure ?? "")
        }
    }

    /// Deletes this deployment's credential and returns to pairing. A failed
    /// delete leaves the synced view in place and surfaces the failure, so the
    /// app never shows pairing over a credential it could not confirm removing.
    ///
    /// Revalidates revocation at commit time against `freshness` (the same
    /// predicate that offered the action): deleting the credential is
    /// irreversible, so a freshness recovery that raced an open dialog must not
    /// delete a credential that just re-authenticated.
    private func performRePair(for freshness: InboxStore.Freshness) {
        guard FreshnessBanner.showsRePairAction(for: freshness, hasHandler: true) else { return }
        do {
            try session.rePair()
        } catch {
            rePairFailure = Self.rePairFailureMessage
        }
    }

    /// The delete-failed alert copy. `rePair()` throws both when the credential
    /// definitely remains and when removal is indeterminate (the reload also
    /// failed), so the copy says removal could not be confirmed rather than
    /// asserting the device is still paired.
    static let rePairFailureMessage =
        "This device's stored credential couldn't be removed, so removal isn't confirmed and it may still be paired. Try again."

    /// Actions under the old pairing whose delivery is unresolved, for the
    /// re-pair confirmation copy. A Stop keeps its entry with a non-nil
    /// receipt after the daemon accepts it; that one is confirmed sent, so it
    /// is excluded. The inbox and submission ledgers release an entry once its
    /// outcome is definitive, so their remaining entries are unresolved (a
    /// lost response may already have committed), not confirmed unsent.
    static func unsentActionCount(
        pendingCommands: Int,
        pendingTaskSubmissions: Int,
        taskStops: some Sequence<PendingTaskStop>
    ) -> Int {
        pendingCommands + pendingTaskSubmissions
            + taskStops.filter { $0.receipt == nil }.count
    }

    /// The confirmation copy. A counted command's delivery is unresolved, not
    /// necessarily unsent: a lost response or 5xx may already have committed
    /// on the daemon (the ledgers keep such entries for verbatim replay). So
    /// the copy says these actions won't be retried, since a re-paired device
    /// gets a new id and the app drops the entries, rather than claiming they
    /// were never sent.
    static func rePairConfirmationMessage(pendingUnderOldPairing: Int) -> String {
        let base =
            "This removes this device's stored credential and returns to pairing for the same daemon. Cached items are kept and become readable again after pairing."
        guard pendingUnderOldPairing > 0 else { return base }
        let actions =
            pendingUnderOldPairing == 1
            ? "1 unresolved action" : "\(pendingUnderOldPairing) unresolved actions"
        return
            "\(base) \(actions) made under the old pairing won't be retried."
    }

    @ViewBuilder
    private func platformNavigation(
        _ coordinator: SyncCoordinator,
        selectedTab: Binding<LaunchInputs.Screen>,
        inboxPath: Binding<[String]>,
        tasksPath: Binding<[String]>,
        attentionSelection: Binding<String?>,
        taskSelection: Binding<String?>
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

                    Tab("Tasks", systemImage: "checklist", value: LaunchInputs.Screen.tasks) {
                        tasksStack(coordinator, path: tasksPath)
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

                    tasksStack(coordinator, path: tasksPath)
                        .tabItem { Label("Tasks", systemImage: "checklist") }
                        .tag(LaunchInputs.Screen.tasks)
                }
            }
        #else
            NavigationSplitView {
                VStack(spacing: 0) {
                    FreesideSegmentedControl(
                        accessibilityLabel: "Section",
                        segments: Self.sectionSegments,
                        selection: selectedTab
                    )
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
                    case .tasks:
                        TasksListView(
                            tasks: coordinator.tasks,
                            runs: coordinator.runs,
                            schedules: coordinator.schedules,
                            attentionItems: coordinator.store.orderedSnapshots,
                            taskTimelines: coordinator.taskTimelinesByTaskID,
                            cursors: coordinator.cursors,
                            onLoadTimeline: coordinator.refreshTaskTimeline,
                            selection: taskSelection,
                            onRefresh: coordinator.refresh,
                            newTaskBlockedReason: TaskSubmissionModel.composeBlockedReason(
                                freshness: coordinator.store.freshness))
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
                        taskSelection: taskSelection.wrappedValue,
                        runSelection: navigation.runSelection
                    )
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                    .background(Color.ground)
                }
            }
            .toolbar {
                ToolbarItem(placement: .navigation) {
                    // The composer lives on the Tasks screen, so its action
                    // shows only there. Setting the shared flag is the one way
                    // the sheet opens, so this gate covers ⌘N and the menu too.
                    if selectedTab.wrappedValue == .tasks {
                        Button {
                            navigation.newTaskComposerPresented = true
                        } label: {
                            Label("New task", systemImage: "plus")
                        }
                        .help(
                            TaskSubmissionModel.composeBlockedReason(
                                freshness: coordinator.store.freshness) ?? "New task"
                        )
                        .disabled(
                            !TaskSubmissionModel.canCompose(freshness: coordinator.store.freshness))
                    }
                }
                ToolbarItemGroup {
                    if selectedTab.wrappedValue == .tasks {
                        submissionRecoveryButton(coordinator)
                    }
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
            .sheet(isPresented: Bindable(navigation).newTaskComposerPresented) {
                NewTaskSheet(
                    projects: TaskDisplay.knownProjects(in: coordinator.tasks),
                    model: TaskSubmissionModel(coordinator: coordinator),
                    onSubmitted: { routeToSubmittedTask($0, coordinator: coordinator) }
                )
                .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
            }
            .sheet(isPresented: Bindable(navigation).submissionRecoveryPresented) {
                submissionRecoverySheet(coordinator)
            }
        #endif
    }

    @ViewBuilder
    private func submissionRecoveryButton(_ coordinator: SyncCoordinator) -> some View {
        if !coordinator.pendingTaskStops.isEmpty {
            Button("Pending Stops (\(coordinator.pendingTaskStops.count))") { stopRecoveryPresented = true }
                .sheet(isPresented: $stopRecoveryPresented) { TaskStopRecoveryView(coordinator: coordinator) }
        }
        if !coordinator.pendingTaskSubmissions.isEmpty {
            Button {
                navigation.submissionRecoveryPresented = true
            } label: {
                Text("Unconfirmed (\(coordinator.pendingTaskSubmissions.count))")
            }
            .accessibilityLabel("Unconfirmed submissions (\(coordinator.pendingTaskSubmissions.count))")
            .help("Unconfirmed submissions")
        }
    }

    private func submissionRecoverySheet(_ coordinator: SyncCoordinator) -> some View {
        TaskSubmissionRecoverySheet(
            model: TaskSubmissionModel(coordinator: coordinator),
            onRecovered: { routeToSubmittedTask($0, coordinator: coordinator) }
        )
        .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
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
                    // The toolbar rides the stack's root content, not the
                    // NavigationStack, so the gear renders in the inbox's
                    // navigation bar and stays off the pushed decision detail.
                    // iOS drops a toolbar attached to the NavigationStack.
                    .toolbar {
                        ToolbarItem(placement: .topBarTrailing) {
                            decisionFlowMenu
                        }
                    }
                }
            }
        }

        /// The stack is rooted at a task and may hold that task's run above
        /// it, so one destination resolves either id: task ids and run ids
        /// never collide.
        private func tasksStack(
            _ coordinator: SyncCoordinator,
            path: Binding<[String]>
        ) -> some View {
            NavigationStack(path: path) {
                TasksListView(
                    tasks: coordinator.tasks,
                    runs: coordinator.runs,
                    schedules: coordinator.schedules,
                    attentionItems: coordinator.store.orderedSnapshots,
                    taskTimelines: coordinator.taskTimelinesByTaskID,
                    cursors: coordinator.cursors,
                    onLoadTimeline: coordinator.refreshTaskTimeline,
                    selection: rawTaskSelectionBinding,
                    navigationPath: rawTasksPathBinding,
                    onRefresh: coordinator.refresh,
                    newTaskBlockedReason: TaskSubmissionModel.composeBlockedReason(
                        freshness: coordinator.store.freshness)
                )
                // The toolbar and sheet ride the stack's root content, not the
                // NavigationStack, so the trailing items render in the list's
                // navigation bar and stay off the pushed task and run detail.
                .toolbar {
                    ToolbarItemGroup(placement: .topBarTrailing) {
                        Button {
                            navigation.newTaskComposerPresented = true
                        } label: {
                            Label("New task", systemImage: "plus")
                        }
                        .disabled(
                            !TaskSubmissionModel.canCompose(freshness: coordinator.store.freshness))
                        submissionRecoveryButton(coordinator)
                        decisionFlowMenu
                    }
                }
                .sheet(isPresented: Bindable(navigation).newTaskComposerPresented) {
                    NewTaskSheet(
                        projects: TaskDisplay.knownProjects(in: coordinator.tasks),
                        model: TaskSubmissionModel(coordinator: coordinator),
                        onSubmitted: { routeToSubmittedTask($0, coordinator: coordinator) }
                    )
                    .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
                }
                .sheet(isPresented: Bindable(navigation).submissionRecoveryPresented) {
                    submissionRecoverySheet(coordinator)
                }
                .navigationDestination(for: String.self) { id in
                    if let task = coordinator.tasks.first(where: { $0.task.id == id }) {
                        TaskTimelineView(
                            coordinator: coordinator, snapshot: task,
                            onOpenRun: { navigation.route(to: .run(taskID: task.task.id, runID: $0)) },
                            onOpenInboxItem: { navigation.route(to: .attentionItem($0)) })
                    } else if let run = coordinator.runs.first(where: { $0.run.id == id }) {
                        RunTimelineView(coordinator: coordinator, snapshot: run)
                    } else {
                        UnavailableStateView(
                            title: "Not available",
                            systemImage: "questionmark.circle",
                            description: "This task or run is no longer available.")
                    }
                }
            }
        }

        private var decisionFlowMenu: some View {
            @Bindable var preferences = flowPreferences
            return Menu {
                Toggle(
                    "Advance to the next item",
                    isOn: $preferences.advancesToNextItem)
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
            taskSelection: String?,
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
                            tasks: coordinator.tasks,
                            freshness: coordinator.store.freshness),
                        onSelectItem: { navigation.route(to: .attentionItem($0)) },
                        onShowTasks: { navigation.showActiveTasks() }
                    )
                    // Pinned to the column's top-leading corner, where the
                    // decision card it stands in for begins, instead of
                    // floating at its center.
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
                }
            case .tasks:
                if let runSelection {
                    // A run opened under its task: the column has no stack to
                    // pop, so a return row leads back to the task.
                    VStack(spacing: 0) {
                        runReturnRow
                        if let run = coordinator.runs.first(where: { $0.run.id == runSelection }) {
                            RunTimelineView(coordinator: coordinator, snapshot: run)
                        } else {
                            UnavailableStateView(
                                title: "Run unavailable", systemImage: "questionmark.circle",
                                description: "This run is no longer available.")
                        }
                    }
                    .id(runSelection)
                } else if let taskSelection,
                    let task = coordinator.tasks.first(where: { $0.task.id == taskSelection })
                {
                    TaskTimelineView(
                        coordinator: coordinator, snapshot: task,
                        onOpenRun: { navigation.route(to: .run(taskID: task.task.id, runID: $0)) },
                        onOpenInboxItem: { navigation.route(to: .attentionItem($0)) }
                    )
                    .id(taskSelection)
                } else {
                    UnavailableStateView(
                        title: "Tasks", systemImage: "checklist",
                        description: "Select a task to inspect its history.")
                }
            }
        }

        private var runReturnRow: some View {
            HStack {
                Button {
                    navigation.closeRun()
                } label: {
                    Label("Back to task", systemImage: "chevron.left")
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.accentText)
                }
                .buttonStyle(.plain)
                .help("Back to task")
                Spacer()
            }
            .padding(.horizontal, 24)
            .padding(.top, 16)
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
        feedback.present(conclusion) {
            switch navigation.advanceAfterConclusion(
                itemID: conclusion.itemID,
                expectedOperatorNavigationRevision: operatorNavigationRevision,
                advancesToNextItem: flowPreferences.advancesToNextItem,
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

    private var operatorTasksPathBinding: Binding<[String]> {
        Binding(
            get: { navigation.tasksPath },
            set: { navigation.setTasksPath($0) })
    }

    private var rawTasksPathBinding: Binding<[String]> {
        Binding(
            get: { navigation.tasksPath },
            set: { navigation.applyTasksPath($0) })
    }

    private var operatorTaskSelectionBinding: Binding<String?> {
        Binding(
            get: { navigation.taskSelection },
            set: { navigation.selectTask($0) })
    }

    /// The iOS list's selection binding: the stack carries the selection
    /// there, so the list's own writes need no routing.
    private var rawTaskSelectionBinding: Binding<String?> {
        Binding(
            get: { navigation.taskSelection },
            set: { navigation.taskSelection = $0 })
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

    /// Routes to the newly created task after a submit. The model confirms the
    /// task is synced before reporting success, but a failed post-submit
    /// refresh can still leave it absent from the cache; fall back to the Tasks
    /// list so a successful submission never lands on a "Not available" detail.
    private func routeToSubmittedTask(_ taskID: String, coordinator: SyncCoordinator) {
        if coordinator.tasks.contains(where: { $0.task.id == taskID }) {
            navigation.route(to: .task(taskID))
        } else {
            navigation.showActiveTasks()
        }
    }

}

/// The non-production tier marker; production draws nothing.
struct EnvironmentBadge: View {
    let title: String

    var body: some View {
        KeywordLabel(text: title, color: .accentText)
            .padding(.horizontal, 8)
            .padding(.vertical, 3)
            .background(Capsule().fill(Color.accentWash))
            .overlay(Capsule().strokeBorder(Color.accentBorder))
            .allowsHitTesting(false)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel("Environment: \(title)")
    }
}
