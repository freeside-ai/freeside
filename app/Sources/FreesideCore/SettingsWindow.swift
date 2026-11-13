#if os(macOS)
    import FreesideAPI
    import SwiftUI

    /// The Mac's Settings window (`⌘,`, the App's `Settings` scene): a source
    /// list of the eight panes with the daemon's host and version at its foot,
    /// and the selected pane beside it, as the 4 Nov 2026 handoff draws it
    /// (`docs/design/handoff-2026-11-04-settings-usage-new-task/`, canvas
    /// band 1). The selection is `NavigationModel.settingsPane`, so the
    /// sidebar's gear and a launch argument open the window on it.
    public struct SettingsWindow: View {
        private let session: AppSession
        private let navigation: NavigationModel
        private let daemon: DaemonMenuModel?
        private let flowPreferences: DecisionFlowPreferences
        private let launchColorScheme: ColorScheme?
        private let launchDynamicTypeSize: DynamicTypeSize?
        private let openTask: (String) -> Void
        @Environment(\.dynamicTypeSize) private var systemDynamicTypeSize

        /// `openTask` shows a task's page in the main window, for a seated
        /// finding card's Task link: this window has no page of its own to
        /// show it in.
        public init(
            session: AppSession, navigation: NavigationModel, daemon: DaemonMenuModel?,
            flowPreferences: DecisionFlowPreferences, launchInputs: LaunchInputs,
            openTask: @escaping (String) -> Void
        ) {
            FreesideFont.registration
            self.session = session
            self.navigation = navigation
            self.daemon = daemon
            self.flowPreferences = flowPreferences
            self.openTask = openTask
            launchColorScheme = launchInputs.colorScheme
            launchDynamicTypeSize = launchInputs.dynamicTypeSize
        }

        public var body: some View {
            Group {
                if case .ready(let coordinator) = session.phase {
                    // Keyed to the coordinator, so a new pairing builds its
                    // models afresh.
                    SyncedSettingsWindow(
                        session: session, coordinator: coordinator, navigation: navigation,
                        flowPreferences: flowPreferences, daemon: macDaemon, openTask: openTask
                    )
                    .id(ObjectIdentifier(coordinator))
                } else {
                    SettingsWindowContent(
                        navigation: navigation, sync: nil, flowPreferences: flowPreferences,
                        host: SettingsHost.line(session.serverURL), daemon: macDaemon)
                }
            }
            // The Settings scene sits outside FreesideRootView, so the main
            // window root's defaults don't reach it: the launch appearance
            // and text size apply here too, for captures, and displayed text
            // stays copyable (app/README.md, Copyable Text).
            .dynamicTypeSize(launchDynamicTypeSize ?? systemDynamicTypeSize)
            .preferredColorScheme(launchColorScheme)
            .textSelection(.enabled)
            .background(Color.ground)
            .tint(.accentText)
        }

        /// The menu-bar panel's daemon state and the same actions it calls,
        /// or nil with no supervised daemon.
        private var macDaemon: DaemonPane.MacDaemon? {
            guard let daemon else { return nil }
            return DaemonPane.MacDaemon(
                state: daemon.state, actionError: daemon.actionError,
                start: { Task { await daemon.start() } },
                stop: { Task { await daemon.stop() } },
                openApprovalSettings: { daemon.openApprovalSettings() })
        }
    }

    /// The window once paired: the models the panes read, built once for the
    /// coordinator.
    private struct SyncedSettingsWindow: View {
        let session: AppSession
        let coordinator: SyncCoordinator
        let navigation: NavigationModel
        let flowPreferences: DecisionFlowPreferences
        let daemon: DaemonPane.MacDaemon?
        let openTask: (String) -> Void
        @State private var devices: DevicesModel
        @State private var routing: RoutingPolicyModel
        @State private var workflow: WorkflowPolicyModel
        @State private var usage: AccountUsageModel

        init(
            session: AppSession, coordinator: SyncCoordinator, navigation: NavigationModel,
            flowPreferences: DecisionFlowPreferences, daemon: DaemonPane.MacDaemon?,
            openTask: @escaping (String) -> Void
        ) {
            self.session = session
            self.coordinator = coordinator
            self.navigation = navigation
            self.flowPreferences = flowPreferences
            self.daemon = daemon
            self.openTask = openTask
            // The Mac takes no notification link, so it holds no
            // subscription; revoking this device signs out to pairing.
            _devices = State(
                initialValue: DevicesModel(
                    coordinator: coordinator,
                    signOut: { try session.rePair(endingPairingOf: coordinator) }))
            _routing = State(initialValue: RoutingPolicyModel(coordinator: coordinator))
            _workflow = State(initialValue: WorkflowPolicyModel(coordinator: coordinator))
            _usage = State(initialValue: AccountUsageModel(coordinator: coordinator))
        }

        var body: some View {
            if case .incompatible(let incompatibility) = coordinator.store.freshness {
                // The Settings scene sits outside FreesideRootView, so it
                // takes the same refusal (#3109): its panes read devices,
                // resend routing and usage commands, and open System Health
                // cards, none of which may reach a daemon this app can't
                // talk to. The window returns once a round finds the two
                // compatible.
                CompatibilityRefusalView(incompatibility: incompatibility)
                    .frame(width: SettingsWindowContent.width)
                    .frame(minHeight: 520)
            } else {
                SettingsWindowContent(
                    navigation: navigation,
                    sync: SettingsSync(
                        devices: devices, routing: routing, workflow: workflow, usage: usage,
                        freshness: coordinator.store.freshness, lastUpdatedAt: coordinator.lastUpdatedAt,
                        revision: coordinator.cursors?.highestObservedServerRevision,
                        projects: coordinator.unifiedList.knownProjects,
                        deviceID: coordinator.store.device.deviceID,
                        unresolvedActionCount: FreesideRootView.unsentActionCount(
                            pendingCommands: coordinator.store.pendingCommandsByItemID.count,
                            pendingTaskSubmissions: coordinator.pendingTaskSubmissions.count,
                            taskStops: coordinator.pendingTaskStops.values),
                        store: coordinator.store, unattendedOperation: coordinator.unattendedOperation),
                    flowPreferences: flowPreferences,
                    host: SettingsHost.line(session.serverURL),
                    daemon: daemon,
                    tasks: coordinator.tasks,
                    openTask: openTask
                )
                // The Agents pane names devices and the Devices pane lists them:
                // on open, and again when the heartbeat observes a newer revision.
                .task(id: devices.observedRevision) {
                    await devices.loadIfStale()
                }
            }
        }
    }

    /// The window's drawing, which the screenshot suite renders with fixed
    /// models: the source list and the selected pane, or a System Health
    /// finding's card seated in the pane.
    struct SettingsWindowContent: View {
        static let width: CGFloat = 760
        static let sourceListWidth: CGFloat = 180
        /// The pane's width beside the source list.
        static var paneWidth: CGFloat { width - sourceListWidth }

        let navigation: NavigationModel
        let sync: SettingsSync?
        let flowPreferences: DecisionFlowPreferences
        let host: String?
        let daemon: DaemonPane.MacDaemon?
        /// The synced tasks a seated finding card's Task link opens, and
        /// what opens one.
        var tasks: [Components.Schemas.TaskSnapshot] = []
        var openTask: (String) -> Void = { _ in }
        /// Where the System Health pane keeps its History fold's state.
        var listDefaults: UserDefaults = .standard
        var rendersInteractiveControls = true

        var body: some View {
            HStack(alignment: .top, spacing: 0) {
                sourceList
                Group {
                    if navigation.settingsPane == .systemHealth, let itemID = navigation.settingsFinding,
                        let sync
                    {
                        SystemHealthCardSeat(
                            itemID: itemID, store: sync.store, onBack: navigation.closeFinding,
                            onSelectItem: navigation.openFinding, tasks: tasks, onSelectTask: openTask,
                            rendersInteractiveControls: rendersInteractiveControls)
                    } else if rendersInteractiveControls {
                        // The pane scrolls, never the source list: Usage with
                        // several pools outgrows the window.
                        ScrollView { pane }
                    } else {
                        pane
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            }
            .frame(width: Self.width)
            .frame(minHeight: 520)
            .background(Color.ground)
        }

        private var pane: some View {
            SettingsPaneView(
                page: navigation.settingsPane, sync: sync, flowPreferences: flowPreferences, host: host,
                client: .mac, mac: daemon, openFinding: navigation.openFinding, listDefaults: listDefaults,
                rendersInteractiveControls: rendersInteractiveControls)
        }

        private var sourceList: some View {
            VStack(alignment: .leading, spacing: 2) {
                ForEach(NavigationModel.SettingsPage.allCases, id: \.self) { page in
                    sourceItem(page)
                }
                Spacer(minLength: 16)
                VStack(alignment: .leading, spacing: 2) {
                    if let host {
                        Text(host)
                            .font(FreesideFont.trailingSummary)
                            .foregroundStyle(Color.inkDim)
                    }
                    if case .running(let health, _)? = daemon?.state {
                        Text("freesided \(health.version)")
                            .font(FreesideFont.trailingSummary)
                            .foregroundStyle(Color.inkFaint)
                    }
                }
                .padding(.horizontal, 10)
                .padding(.vertical, 6)
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 12)
            .frame(width: Self.sourceListWidth, alignment: .leading)
            .frame(maxHeight: .infinity, alignment: .top)
            .background(Color.sidebarGround)
            .overlay(alignment: .trailing) {
                Rectangle().fill(Color.rule).frame(width: 1)
            }
        }

        private func sourceItem(_ page: NavigationModel.SettingsPage) -> some View {
            let selected = navigation.settingsPane == page
            return Button {
                navigation.selectSettingsPane(page)
            } label: {
                HStack(spacing: 8) {
                    Text(page.title)
                        .font(FreesideFont.factLabel)
                        .foregroundStyle(selected ? Color.ink : Color.inkDim)
                    Spacer(minLength: 0)
                    if let summary = summary(page) {
                        Text(summary)
                            .font(FreesideFont.trailingSummary)
                            .foregroundStyle(Color.inkDim)
                    }
                }
                .padding(.horizontal, 10)
                .padding(.vertical, 5)
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(RoundedRectangle(cornerRadius: 6).fill(selected ? Color.accentWash : .clear))
                .contentShape(RoundedRectangle(cornerRadius: 6))
            }
            .buttonStyle(.plain)
            .accessibilityAddTraits(selected ? .isSelected : [])
        }

        /// What trails a source-list row: System Health's open count.
        private func summary(_ page: NavigationModel.SettingsPage) -> String? {
            guard page == .systemHealth, let sync else { return nil }
            return SettingsSummary.systemHealthCount(sync.findings)
        }
    }
#endif
