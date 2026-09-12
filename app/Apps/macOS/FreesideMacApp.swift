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
                    await coordinator.heartbeatLoop(every: SyncCoordinator.heartbeatInterval)
                }
        }
        .menuBarExtraStyle(.window)

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

/// The menu-bar panel bound to the live daemon model and session. The
/// panel itself is `DaemonMenuPanel` in FreesideCore, so the screenshot
/// suite renders every daemon state; this wrapper supplies the counts and
/// the handlers.
private struct DaemonMenu: View {
    let model: DaemonMenuModel
    let session: AppSession
    let navigation: NavigationModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        DaemonMenuPanel(
            state: model.state,
            actionError: model.actionError,
            inbox: coordinator.map {
                DaemonMenuPanel.InboxCounts(
                    open: $0.store.openSnapshots.count,
                    urgent: $0.store.urgentOpenCount)
            },
            actions: DaemonMenuPanel.Actions(
                openApp: showApp,
                showInbox: {
                    navigation.selectTab(.inbox)
                    showApp()
                },
                start: { Task { await model.start() } },
                stop: { Task { await model.stop() } },
                openApprovalSettings: { model.openApprovalSettings() },
                quit: { NSApplication.shared.terminate(nil) }))
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
}

private struct FreesideAppCommands: Commands {
    let session: AppSession
    let navigation: NavigationModel
    @FocusedValue(\.decisionCommandActions) private var decisionActions

    var body: some Commands {
        CommandMenu("Navigate") {
            ForEach(FreesideCommandDescriptor.all) { descriptor in
                Button(descriptor.title) { perform(descriptor.id) }
                    .keyboardShortcut(descriptor.shortcut)
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
        case "mismatch":
            status = .enabled
            health = .contractMismatch
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
        case contractMismatch
        case unreachable
    }

    let result: Result
    let startedAt = Date.now.addingTimeInterval(-5 * 60)

    func health(at serverURL: URL) async throws -> DaemonHealth {
        switch result {
        case .running:
            return DaemonHealth(version: "1.0.0", startedAt: startedAt)
        case .contractMismatch:
            // A daemon built from a different spec, for the menu's
            // contract-mismatch demo/screenshot state (#1265).
            return DaemonHealth(
                version: "1.0.0", startedAt: startedAt,
                contractDigest: "sha256:" + String(repeating: "a", count: 64))
        case .unreachable:
            throw URLError(.cannotConnectToHost)
        }
    }
}
