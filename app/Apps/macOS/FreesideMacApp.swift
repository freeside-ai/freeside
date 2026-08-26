import AppKit
import FreesideCore
import SwiftUI

@main
struct FreesideMacApp: App {
    @State private var session: AppSession
    @State private var daemon: DaemonMenuModel
    @State private var navigation: NavigationModel
    @State private var flowPreferences: DecisionFlowPreferences
    private let launchInputs: LaunchInputs

    init() {
        let launchInputs = LaunchInputs.standard()
        self.launchInputs = launchInputs
        _session = State(initialValue: Self.session())
        _daemon = State(initialValue: Self.daemonModel())
        _navigation = State(initialValue: NavigationModel(launchInputs: launchInputs))
        _flowPreferences = State(initialValue: DecisionFlowPreferences())
    }

    var body: some Scene {
        WindowGroup("Freeside", id: "main") {
            Group {
                #if DEBUG
                    if UserDefaults.standard.string(forKey: "FreesideDaemonMenuDemo") != nil {
                        DaemonMenu(model: daemon, session: session, navigation: navigation)
                            .frame(width: 280)
                            .padding()
                    } else {
                        FreesideRootView(
                            session: session,
                            launchInputs: launchInputs,
                            navigation: navigation,
                            flowPreferences: flowPreferences)
                    }
                #else
                    FreesideRootView(
                        session: session,
                        launchInputs: launchInputs,
                        navigation: navigation,
                        flowPreferences: flowPreferences)
                #endif
            }
            .task { daemon.startMonitoring() }
            .onChange(of: daemon.readiness, initial: true) { _, readiness in
                session.applyReadiness(readiness)
            }
        }
        .defaultSize(width: 960, height: 640)
        .commands {
            FreesideAppCommands(session: session, navigation: navigation)
        }

        MenuBarExtra {
            DaemonMenu(model: daemon, session: session, navigation: navigation)
        } label: {
            Image(nsImage: FreesideMenuIcon.image(badgeColor: daemon.state.menuBadgeColor))
                .renderingMode(.original)
                .accessibilityElement(children: .ignore)
                .accessibilityLabel("Freeside: \(daemon.state.accessibilityDescription)")
                .task(id: menuSyncCoordinatorID) {
                    guard case .ready(let coordinator) = session.phase else { return }
                    coordinator.startReachabilityMonitoring()
                    defer { coordinator.stopReachabilityMonitoring() }
                    await coordinator.heartbeatLoop(every: .seconds(15))
                }
        }

        Settings {
            DecisionFlowSettingsView(preferences: flowPreferences)
        }
    }

    private var menuSyncCoordinatorID: ObjectIdentifier? {
        guard case .ready(let coordinator) = session.phase else { return nil }
        return ObjectIdentifier(coordinator)
    }

    @MainActor
    private static func session() -> AppSession {
        #if DEBUG
            if UserDefaults.standard.string(forKey: "FreesideDaemonMenuDemo") != nil {
                return AppSession.mock()
            }
        #endif
        return AppSession.fromEnvironment()
    }

    @MainActor
    private static func daemonModel() -> DaemonMenuModel {
        #if DEBUG
            if let demo = UserDefaults.standard.string(forKey: "FreesideDaemonMenuDemo") {
                return DaemonMenuDemo.model(named: demo)
            }
        #endif
        let arguments = UserDefaults.standard.volatileDomain(forName: UserDefaults.argumentDomain)
        let hasExplicitServer =
            (arguments["FreesideServerURL"] as? String)
            .flatMap(URL.init(string:)) != nil
        let usesDevelopmentTransport =
            arguments["FreesidePairingDemo"] as? String == "YES"
            || arguments["FreesideMock"] as? String == "YES"
        if !hasExplicitServer, usesDevelopmentTransport {
            // Mock launches must not mutate the installed LaunchAgent. The
            // demo model keeps the menu inert without touching SMAppService.
            return DaemonMenuDemo.model(named: "stopped")
        }
        return DaemonMenuModel()
    }
}

/// Standard menu items, arranged per the §15 menu-bar spec: a bold state
/// line, its explanation and facts directly under it, the last action's
/// error under a section header, and the actions grouped at the bottom
/// with Quit. The bar and menu stay system chrome.
private struct DaemonMenu: View {
    let model: DaemonMenuModel
    let session: AppSession
    let navigation: NavigationModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Button("Open Freeside") { showApp() }
        Button("Show Inbox · \(coordinator?.store.openSnapshots.count ?? 0)") {
            navigation.selectTab(.inbox)
            showApp()
        }
        if let urgentCount = coordinator?.store.urgentOpenCount, urgentCount > 0 {
            Label("\(urgentCount) urgent", systemImage: "exclamationmark.circle.fill")
        }
        Divider()
        Section("Daemon") {
            daemonSection
        }
        Divider()
        Button("Quit Freeside") { NSApplication.shared.terminate(nil) }
    }

    @ViewBuilder
    private var daemonSection: some View {
        switch model.state {
        case .checking:
            Text("Checking daemon…").bold()
            lastAction
        case .stopped:
            Label("Daemon stopped", systemImage: "stop.circle").bold()
            lastAction
            Button("Start") { Task { await model.start() } }
        case .needsApproval:
            Label("Approval needed", systemImage: "exclamationmark.triangle.fill").bold()
            Text("Allow Freeside in Login Items to start the daemon.")
            Button("Open Login Items…") { model.openApprovalSettings() }
            lastAction
            Button("Stop") { Task { await model.stop() } }
        case .unavailable:
            Label("LaunchAgent unavailable", systemImage: "xmark.circle.fill").bold()
            lastAction
            Button("Start") { Task { await model.start() } }
        case .unreachable:
            Label("Daemon unreachable", systemImage: "xmark.circle.fill").bold()
            Text("launchd is keeping the service enabled, but health is not answering.")
            lastAction
            Button("Stop") { Task { await model.stop() } }
        case .running(let health, let restartObserved):
            Label("Daemon running", systemImage: "checkmark.circle.fill").bold()
            Text("Version \(health.version)")
            Text("Started \(health.startedAt.formatted(date: .abbreviated, time: .standard))")
            if restartObserved {
                Label("Restart observed", systemImage: "arrow.clockwise")
            }
            lastAction
            Button("Stop") { Task { await model.stop() } }
        }
    }

    private var coordinator: SyncCoordinator? {
        guard case .ready(let coordinator) = session.phase else { return nil }
        return coordinator
    }

    private func showApp() {
        if let window = NSApplication.shared.windows.first(where: { $0.title == "Freeside" }) {
            window.makeKeyAndOrderFront(nil)
        } else {
            openWindow(id: "main")
        }
        NSApplication.shared.activate()
    }

    @ViewBuilder
    private var lastAction: some View {
        if let error = model.actionError {
            Divider()
            Section("Last action") {
                Text(error)
            }
        }
    }
}

private struct FreesideAppCommands: Commands {
    let session: AppSession
    let navigation: NavigationModel
    @FocusedValue(\.decisionCommandActions) private var decisionActions

    var body: some Commands {
        CommandMenu("Navigate") {
            ForEach(FreesideCommandDescriptor.all) { descriptor in
                Button(descriptor.title) { perform(descriptor.id) }
                    .keyboardShortcut(descriptor.key, modifiers: descriptor.modifiers)
                    .disabled(isDisabled(descriptor.id))
            }
        }
    }

    private var coordinator: SyncCoordinator? {
        guard case .ready(let coordinator) = session.phase else { return nil }
        return coordinator
    }

    private func isDisabled(_ action: FreesideCommandAction) -> Bool {
        switch action {
        case .showInbox, .showRuns, .toggleInspector:
            false
        case .refresh:
            coordinator == nil
        case .nextItem, .previousItem:
            coordinator?.store.rows.isEmpty != false
        case .takeRecommendation:
            decisionActions?.canTakeRecommendation != true
        case .cancelPendingAction:
            decisionActions == nil
        case .find:
            true
        }
    }

    private func perform(_ action: FreesideCommandAction) {
        switch action {
        case .showInbox:
            navigation.selectTab(.inbox)
        case .showRuns:
            navigation.selectTab(.runs)
        case .refresh:
            guard let coordinator else { return }
            Task { await coordinator.refresh() }
        case .toggleInspector:
            navigation.inspectorPresented.toggle()
        case .nextItem:
            guard let coordinator else { return }
            navigation.moveAttentionSelection(by: 1, store: coordinator.store)
        case .previousItem:
            guard let coordinator else { return }
            navigation.moveAttentionSelection(by: -1, store: coordinator.store)
        case .takeRecommendation:
            decisionActions?.takeRecommendation()
        case .cancelPendingAction:
            decisionActions?.cancelPendingAction()
        case .find:
            break
        }
    }
}

extension DaemonMenuState {
    fileprivate var menuBadgeColor: NSColor? {
        switch self {
        case .checking, .running:
            nil
        case .stopped, .needsApproval:
            .systemOrange
        case .unavailable, .unreachable:
            .systemRed
        }
    }

    fileprivate var accessibilityDescription: String {
        switch self {
        case .checking:
            "checking daemon"
        case .stopped:
            "daemon stopped"
        case .needsApproval:
            "approval needed"
        case .unavailable:
            "LaunchAgent unavailable"
        case .unreachable:
            "daemon unreachable"
        case .running:
            "daemon running"
        }
    }
}

@MainActor
private enum FreesideMenuIcon {
    static func image(badgeColor: NSColor?) -> NSImage {
        guard
            let url = Bundle.main.url(forResource: "FreesideMenuMark", withExtension: "png"),
            let mark = NSImage(contentsOf: url)
        else {
            preconditionFailure("FreesideMenuMark.png is missing from the app bundle")
        }
        let image = NSImage(size: NSSize(width: 20, height: 20), flipped: false) { rect in
            mark.draw(in: rect)
            guard let context = NSGraphicsContext.current else { return false }
            context.saveGraphicsState()
            context.compositingOperation = .sourceIn
            NSColor.labelColor.setFill()
            NSBezierPath(rect: rect).fill()
            context.restoreGraphicsState()
            if let badgeColor {
                // Top-right, over the key's bar; the dot is the status
                // channel at this size, where the key's own dot is retired.
                badgeColor.setFill()
                NSBezierPath(ovalIn: NSRect(x: 13, y: 13, width: 7, height: 7)).fill()
            }
            return true
        }
        image.cacheMode = .never
        image.isTemplate = false
        return image
    }
}

@MainActor
private enum DaemonMenuDemo {
    static func model(named name: String) -> DaemonMenuModel {
        let status: DaemonServiceStatus
        let health: DemoHealthChecker.Result
        switch name {
        case "running":
            status = .enabled
            health = .running
        case "approval":
            status = .requiresApproval
            health = .unreachable
        case "unreachable":
            status = .enabled
            health = .unreachable
        default:
            status = .notRegistered
            health = .unreachable
        }
        return DaemonMenuModel(
            service: DemoDaemonService(status: status),
            healthChecker: DemoHealthChecker(result: health),
            registerOnFirstRun: false,
            readReadiness: { nil })
    }
}

@MainActor
private final class DemoDaemonService: DaemonServiceControlling {
    var status: DaemonServiceStatus
    let needsAutomaticStart = false

    init(status: DaemonServiceStatus) {
        self.status = status
    }

    func start() async { status = .enabled }
    func stop() async { status = .notRegistered }
    func openApprovalSettings() {}
}

private struct DemoHealthChecker: DaemonHealthChecking {
    enum Result: Sendable {
        case running
        case unreachable
    }

    let result: Result
    let startedAt = Date.now.addingTimeInterval(-5 * 60)

    func health(at serverURL: URL) async throws -> DaemonHealth {
        switch result {
        case .running:
            return DaemonHealth(version: "1.0.0", startedAt: startedAt)
        case .unreachable:
            throw URLError(.cannotConnectToHost)
        }
    }
}
