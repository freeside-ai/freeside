import FreesideAPI
import SwiftUI

public struct FreesideRootView: View {
    @Environment(\.dynamicTypeSize) private var systemDynamicTypeSize
    @Environment(\.scenePhase) private var scenePhase
    @State private var session: AppSession
    @State private var navigation: NavigationModel
    @State private var feedback: DecisionFeedbackModel
    @State private var flowPreferences: DecisionFlowPreferences
    @State private var listPreferences = ListSectionPreferences()
    private let notificationOpens: NotificationOpenLog
    @State private var technicalDetailsRequest: TechnicalDetailsRevealRequest?
    @State private var showsInboxClearResult: Bool
    @State private var connectionAddress = ""
    @State private var stopRecoveryPresented = false
    @State private var rePairConfirmationPresented = false
    @State private var rePairFailure: String?
    /// Which of the Mac's columns show. Read to tell a collapsed sidebar,
    /// where the standing notices take the window's width again.
    @State private var splitVisibility = NavigationSplitViewVisibility.automatic
    private let launchColorScheme: ColorScheme?
    private let launchProjectID: String?
    private let launchDetailsExpanded: Bool
    private let launchDynamicTypeSize: DynamicTypeSize?
    private let environment: FreesideEnvironment

    /// `environment` defaults to `.prod`, which shows no window subtitle
    /// until a session selects a port other than 7331.
    @MainActor
    public init(
        session: AppSession,
        launchInputs: LaunchInputs = .standard(),
        navigation: NavigationModel? = nil,
        flowPreferences: DecisionFlowPreferences? = nil,
        notificationOpens: NotificationOpenLog? = nil,
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
        self.notificationOpens = notificationOpens ?? NotificationOpenLog()
        _technicalDetailsRequest = State(initialValue: nil)
        _showsInboxClearResult = State(initialValue: false)
        launchColorScheme = launchInputs.colorScheme
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
        .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
        // Displayed text is copyable by default, so the operator can paste a
        // diagnostic instead of retyping it (#1917). Set once here so a new
        // surface inherits it; a view that opts out says why (app/README.md).
        .textSelection(.enabled)
        .preferredColorScheme(launchColorScheme)
        .background(Color.ground)
        .tint(.accentText)
        // The default titlebar renders a bright system material that reads
        // as a hard white, square-cornered band over the ground.
        // Hiding it lets each screen's own ground rise into the titlebar, so
        // the toolbar blends into the body and the window's rounded corners
        // carry the top edge. Applied above the phase switch so it also
        // covers the pairing screen, which renders without platformNavigation.
        #if os(macOS)
            .toolbarBackground(.hidden, for: .windowToolbar)
            // Names the instance under each screen's own title; the icon and
            // menu bar mark name the tier.
            .navigationSubtitle(windowSubtitle ?? "")
        #endif
    }

    private var windowSubtitle: String? {
        WindowSubtitle.text(
            environment: environment, serverURL: session.serverURL,
            readinessDirectory: session.readinessDirectory)
    }

    /// Whether the Mac's detail pane draws `RevokedPane`: the device is
    /// revoked and the pane holds nothing the operator opened. An open card,
    /// timeline, or run stays drawn, since a revoked device still reads its
    /// cache and can still resend a command whose answer was lost (plan
    /// §5.14); Pair Again is then the banner's button, as on iPhone.
    static func showsRevokedPane(
        freshness: InboxStore.Freshness, path: [NavigationModel.Destination]
    ) -> Bool {
        FreshnessBanner.showsRePairAction(for: freshness, hasHandler: true) && path.isEmpty
    }

    /// Pair Again on the Revoked banner. Where the Mac's detail pane holds
    /// it instead (`showsRevokedPane`) the banner only states; iPhone has no
    /// pane beside its list, and always keeps the button under the sentence.
    private func bannerRePair(_ freshness: InboxStore.Freshness) -> (() -> Void)? {
        #if os(macOS)
            if Self.showsRevokedPane(freshness: freshness, path: navigation.path) {
                return nil
            }
        #endif
        return { rePairConfirmationPresented = true }
    }

    /// Whether the standing notices draw at the top of the detail column,
    /// so the sidebar is never under one. That takes a column beside a
    /// list: the Mac with its sidebar shown. An iPhone, and a Mac window
    /// whose sidebar is collapsed, keep the full-width slot above the
    /// navigation.
    private var standingNoticesDrawInDetailColumn: Bool {
        #if os(macOS)
            splitVisibility != .detailOnly
        #else
            false
        #endif
    }

    private func standingNotices(
        _ coordinator: SyncCoordinator, placement: StandingNoticePlacement
    ) -> StandingNotices {
        StandingNotices(
            saveWarning: coordinator.promptHistory.saveWarning,
            onDismissSaveWarning: { coordinator.promptHistory.saveWarning = nil },
            freshness: coordinator.store.freshness,
            lastUpdatedAt: coordinator.lastUpdatedAt,
            onRePair: bannerRePair(coordinator.store.freshness),
            operation: coordinator.unattendedOperation,
            reason: { coordinator.store.openItemReason($0) },
            onOpenItem: { navigation.route(to: .attentionItem($0)) },
            placement: placement)
    }

    private func synced(_ coordinator: SyncCoordinator) -> some View {
        @Bindable var navigation = navigation
        let pendingUnderOldPairing = Self.unsentActionCount(
            pendingCommands: coordinator.store.pendingCommandsByItemID.count,
            pendingTaskSubmissions: coordinator.pendingTaskSubmissions.count,
            taskStops: coordinator.pendingTaskStops.values)
        return VStack(spacing: 0) {
            if !standingNoticesDrawInDetailColumn {
                standingNotices(coordinator, placement: .window)
            }
            platformNavigation(coordinator)
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
        // One sheet for both platforms, above the navigation containers, so
        // the Mac toolbar and the iPhone stack open the same one.
        .sheet(isPresented: $navigation.devicesPresented) {
            DevicesView(
                model: DevicesModel(
                    coordinator: coordinator,
                    notificationSubscription: phoneNotificationSubscription,
                    notificationOpens: notificationOpens,
                    signOut: {
                        try session.rePair(endingPairingOf: coordinator)
                        // The sheet leaves with the synced surface, but the
                        // flag outlives it: left set, the sheet would open
                        // again by itself once the operator pairs.
                        if case .needsPairing = session.phase { navigation.devicesPresented = false }
                    }),
                unresolvedActionCount: pendingUnderOldPairing
            )
            .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
        }
        // Offered once after a pairing, on the iPhone only.
        .sheet(
            isPresented: Binding(
                get: { session.offersNotificationSetup && phoneNotificationSubscription != nil },
                set: { session.offersNotificationSetup = $0 })
        ) {
            if let subscription = phoneNotificationSubscription {
                PhoneNotificationSetupSheet(
                    subscription: subscription, opens: notificationOpens,
                    deviceID: coordinator.store.device.deviceID
                )
                .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
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

    /// The Devices sheet shows this subscription on the iPhone only: the
    /// notification's link opens the iPhone app, and the Mac app registers
    /// no link scheme, so a topic shown there would invite a subscription
    /// whose notifications open nothing.
    private var phoneNotificationSubscription: DeviceNtfySubscription? {
        #if os(iOS)
            session.ntfySubscription
        #else
            nil
        #endif
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
    private func platformNavigation(_ coordinator: SyncCoordinator) -> some View {
        #if os(iOS)
            listStack(coordinator)
        #else
            NavigationSplitView(columnVisibility: $splitVisibility) {
                unifiedList(coordinator)
                    .background(Color.sidebarGround)
                    .navigationSplitViewColumnWidth(min: 280, ideal: 320)
            } detail: {
                VStack(spacing: 0) {
                    DecisionFeedbackBanner(
                        feedback: feedback,
                        onView: viewConcludedItem)
                    let notices =
                        standingNoticesDrawInDetailColumn
                        ? standingNotices(coordinator, placement: .detailColumn) : nil
                    StandingDetailColumn(notices: notices) { topMargin in
                        Group {
                            if Self.showsRevokedPane(
                                freshness: coordinator.store.freshness, path: navigation.path)
                            {
                                RevokedPane { rePairConfirmationPresented = true }
                            } else {
                                macDetail(coordinator, topMargin: topMargin)
                            }
                        }
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                    }
                    .background(Color.ground)
                }
            }
            .toolbar {
                ToolbarItemGroup {
                    submissionRecoveryButton(coordinator)
                    Button {
                        Task { await coordinator.refresh() }
                    } label: {
                        Label("Refresh", systemImage: "arrow.clockwise")
                    }
                    .help("Refresh")
                    LastUpdatedLabel(lastUpdatedAt: coordinator.lastUpdatedAt)
                    devicesButton
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

    /// A decision card, alone in the pane or seated in `page`.
    private func decisionCard(
        _ itemID: String, coordinator: SyncCoordinator, topMargin: CGFloat? = nil,
        page: DecisionDetailView.Page? = nil
    ) -> DecisionDetailView {
        DecisionDetailView(
            store: coordinator.store,
            itemID: itemID,
            detailsExpanded: launchDetailsExpanded,
            detailsRevealRequest: technicalDetailsRequest,
            onConsumeDetailsRevealRequest: consumeTechnicalDetailsRequest,
            launchComposer: navigation.launchComposer(for: itemID),
            onConsumeLaunchComposer: navigation.consumeLaunchComposer,
            topMargin: topMargin,
            page: page,
            onSelectItem: { navigation.route(to: .attentionItem($0)) },
            tasks: coordinator.tasks,
            onSelectTask: { navigation.route(to: .task($0)) },
            onConclusion: { conclusion in
                handleConclusion(conclusion, coordinator: coordinator)
            })
    }

    private func taskPage(
        _ task: Components.Schemas.TaskSnapshot, leadingItemID: String?, coordinator: SyncCoordinator,
        topMargin: CGFloat? = nil
    ) -> TaskPageHost {
        TaskPageHost(
            coordinator: coordinator, snapshot: task, leadingItemID: leadingItemID, topMargin: topMargin,
            onOpenRun: { navigation.openRun(taskID: task.task.id, runID: $0) },
            onOpenItem: { navigation.route(to: .attentionItem($0)) },
            card: { itemID, page in decisionCard(itemID, coordinator: coordinator, page: page) })
    }

    private func unifiedList(_ coordinator: SyncCoordinator) -> some View {
        UnifiedListView(
            store: coordinator.store,
            tasks: coordinator.tasks,
            runs: coordinator.runs,
            taskTimelines: coordinator.taskTimelinesByTaskID,
            runTimelines: coordinator.timelinesByRunID,
            cursors: coordinator.cursors,
            cacheGeneration: coordinator.cacheGeneration,
            onLoadTimeline: coordinator.refreshTaskTimeline,
            navigation: navigation,
            preferences: listPreferences,
            launchProjectID: launchProjectID,
            newTaskBlockedReason: TaskSubmissionModel.composeBlockedReason(
                freshness: coordinator.store.freshness),
            lastUpdatedAt: coordinator.lastUpdatedAt,
            onRefresh: coordinator.refresh,
            onRevealTechnicalDetails: revealTechnicalDetails)
    }

    private var devicesButton: some View {
        Button {
            navigation.devicesPresented = true
        } label: {
            Label("Devices", systemImage: "laptopcomputer.and.iphone")
        }
        .help("Devices")
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
        private func listStack(_ coordinator: SyncCoordinator) -> some View {
            VStack(spacing: 0) {
                DecisionFeedbackBanner(feedback: feedback, onView: viewConcludedItem)
                NavigationStack(
                    path: Binding(get: { navigation.path }, set: { navigation.setPath($0) })
                ) {
                    unifiedList(coordinator)
                        // The toolbar and sheets ride the stack's root content,
                        // not the NavigationStack, so the trailing items render
                        // in the list's navigation bar and stay off the pushed
                        // detail. iOS drops a toolbar attached to the
                        // NavigationStack.
                        .toolbar {
                            ToolbarItemGroup(placement: .topBarTrailing) {
                                submissionRecoveryButton(coordinator)
                                devicesButton
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
                        .navigationDestination(for: NavigationModel.Destination.self) { destination in
                            phoneDetail(destination, coordinator: coordinator)
                        }
                }
            }
        }

        @ViewBuilder
        private func phoneDetail(
            _ destination: NavigationModel.Destination, coordinator: SyncCoordinator
        ) -> some View {
            switch NavigationModel.surface(for: destination, in: coordinator.unifiedList) {
            case .card(let itemID):
                decisionCard(itemID, coordinator: coordinator)
            case .taskPage(let taskID, let leadingItemID):
                if let task = coordinator.tasks.first(where: { $0.task.id == taskID }) {
                    taskPage(task, leadingItemID: leadingItemID, coordinator: coordinator)
                } else {
                    unavailableDetail
                }
            case .run(_, let runID):
                if let run = coordinator.runs.first(where: { $0.run.id == runID }) {
                    RunTimelineView(coordinator: coordinator, snapshot: run)
                } else {
                    unavailableDetail
                }
            }
        }

        private var unavailableDetail: some View {
            UnavailableStateView(
                glyph: .tasks, title: "Not available",
                description: "This task or run is no longer available.",
                seat: .detailColumn)
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
            topMargin: CGFloat?
        ) -> some View {
            let surface = navigation.detailSurface(in: coordinator.unifiedList)
            if case .card(let itemID) = surface {
                decisionCard(itemID, coordinator: coordinator, topMargin: topMargin)
                    .id(itemID)
            } else if case .run(_, let runID) = surface {
                // A run opened under its task: the column has no stack to
                // pop, so a return row leads back to the task.
                VStack(spacing: 0) {
                    runReturnRow(topMargin: topMargin)
                    if let run = coordinator.runs.first(where: { $0.run.id == runID }) {
                        RunTimelineView(coordinator: coordinator, snapshot: run, topMargin: 10)
                    } else {
                        UnavailableStateView(
                            glyph: .tasks, title: "Run unavailable",
                            description: "This run is no longer available.",
                            seat: .detailColumn)
                    }
                }
                .id(runID)
            } else if case .taskPage(let taskID, let leadingItemID) = surface,
                let task = coordinator.tasks.first(where: { $0.task.id == taskID })
            {
                taskPage(task, leadingItemID: leadingItemID, coordinator: coordinator, topMargin: topMargin)
                    .id(taskID)
            } else if showsInboxClearResult {
                // The same two lines the empty Needs you draws in the
                // sidebar, so the two panes agree.
                let empty = UnifiedListView.emptyNeedsYou(projectID: coordinator.store.projectID)
                UnavailableStateView(
                    glyph: .inbox, title: empty.title, description: empty.description,
                    seat: .detailColumn)
            } else {
                OperationalSummaryView(
                    summary: OperationalSummary(
                        list: coordinator.unifiedList, freshness: coordinator.store.freshness),
                    onSelectItem: { navigation.route(to: .attentionItem($0)) },
                    onShowTasks: showWorking,
                    topMargin: topMargin
                )
                // Pinned to the column's top, where the decision card
                // it stands in for begins, instead of floating at its
                // center.
                .frame(maxHeight: .infinity, alignment: .top)
            }
        }

        private func runReturnRow(topMargin: CGFloat?) -> some View {
            HStack {
                Button {
                    navigation.closeRun()
                } label: {
                    Label("Back to Task", systemImage: "chevron.left")
                        .font(FreesideFont.callout)
                        .foregroundStyle(Color.accentText)
                }
                .buttonStyle(.plain)
                .help("Back to Task")
                Spacer()
            }
            // The detail column's x and top margin (R18): the row is the
            // column's first line here, under any standing notice, and the
            // run's card sits under it.
            .padding(.horizontal, 24)
            .padding(.top, topMargin ?? 20)
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
                list: coordinator.unifiedList)
            {
            case .advanced, .returnedToList:
                showsInboxClearResult = false
            case .inboxClear:
                showsInboxClearResult = true
            case .cancelled:
                break
            }
        }
    }

    private func viewConcludedItem(_ itemID: String) {
        feedback.dismiss()
        showsInboxClearResult = false
        // The operator asked for this card, whether or not it's still open.
        navigation.route(toLinkedItem: itemID)
    }

    /// Clears the selection and opens Working: where the active tasks are,
    /// and where a task the cache hasn't caught up to will land.
    private func showWorking() {
        navigation.select(nil)
        listPreferences.workingExpanded = true
    }

    private func consumeTechnicalDetailsRequest(_ nonce: UUID) {
        technicalDetailsRequest = technicalDetailsRequest?.consuming(nonce)
    }

    /// Routes to the newly created task after a submit, with Working open so
    /// its row shows. The model confirms the task is synced before reporting
    /// success, but a failed post-submit refresh can still leave it absent
    /// from the cache; fall back to the list so a successful submission never
    /// lands on a "Not available" detail.
    private func routeToSubmittedTask(_ taskID: String, coordinator: SyncCoordinator) {
        if coordinator.tasks.contains(where: { $0.task.id == taskID }) {
            listPreferences.workingExpanded = true
            navigation.route(to: .task(taskID))
        } else {
            showWorking()
        }
    }

}

/// The Mac's detail pane while this device's access is revoked and nothing
/// is open in it (R14): the pane has nothing else to offer, so Pair Again
/// is its one filled control and the banner above only states. Cached rows
/// stay in the sidebar, and opening one draws it in place of this pane. The
/// two lines read to VoiceOver as one element, and the button after them.
struct RevokedPane: View {
    let onRePair: () -> Void

    var body: some View {
        VStack(spacing: 6) {
            EmptyStateBlock(
                glyph: .revoked, title: "This device is no longer paired",
                description: "Pair again to act on items. Cached items stay readable.")
            Button("Pair Again", action: onRePair)
                .buttonStyle(FreesideActionButtonStyle(tone: .primary, expands: false))
                .padding(.top, 10)
        }
        .multilineTextAlignment(.center)
        .emptyStateSeat(.detailColumn)
    }
}
