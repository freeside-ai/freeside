import AppKit
import FreesideCore
import SwiftUI

@main
enum FreesideMacEntryPoint {
    static func main() {
        guard CommandLine.arguments.dropFirst().first == "daemon" else {
            FreesideMacApp.main()
            return
        }
        guard let command = DaemonLifecycleCommand(arguments: Array(CommandLine.arguments.dropFirst())) else {
            print(DaemonLifecycleCommand.usage.line)
            exit(DaemonLifecycleCommand.usage.exitCode)
        }
        Task { @MainActor in
            #if DEBUG
                let isDebugBuild = true
            #else
                let isDebugBuild = false
            #endif
            do {
                let environment = try FreesideEnvironment.current(isDebugBuild: isDebugBuild)
                let result = await command.run(environment: environment)
                print(result.line)
                exit(result.exitCode)
            } catch {
                print("error=invalid_environment")
                exit(78)
            }
        }
        dispatchMain()
    }
}

struct FreesideMacApp: App {
    @State private var session: AppSession
    @State private var daemon: DaemonMenuModel?
    @State private var navigation: NavigationModel
    @State private var flowPreferences: DecisionFlowPreferences
    private let launchInputs: LaunchInputs
    private let environment: FreesideEnvironment

    init() {
        #if DEBUG
            let isDebugBuild = true
        #else
            let isDebugBuild = false
        #endif
        // A tier or launch input this app cannot honour exits before any
        // window or LaunchAgent work, rather than guessing a daemon.
        let environment: FreesideEnvironment
        let session: AppSession
        do {
            environment = try FreesideEnvironment.current(isDebugBuild: isDebugBuild)
            session = try Self.session(environment: environment)
        } catch {
            FileHandle.standardError.write(Data("FreesideMac: \(error)\n".utf8))
            exit(EX_CONFIG)
        }
        let launchInputs = LaunchInputs.standard()
        self.launchInputs = launchInputs
        self.environment = environment
        _session = State(initialValue: session)
        _daemon = State(initialValue: Self.daemonModel(environment: environment))
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
                            flowPreferences: flowPreferences,
                            environment: environment)
                    }
                #else
                    FreesideRootView(
                        session: session,
                        launchInputs: launchInputs,
                        navigation: navigation,
                        flowPreferences: flowPreferences,
                        environment: environment)
                #endif
            }
            .background(MainWindowReader())
            .task { daemon?.startMonitoring() }
            .onChange(of: daemon?.readiness, initial: true) { _, readiness in
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
            Image(
                nsImage: FreesideMenuIcon.image(
                    look: environment.look,
                    badgeColor: daemonMenuState.menuBadgeColor(
                        unattendedStopped: unattendedStopped))
            )
            .renderingMode(.original)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel(
                "\(environment.statusItemName): "
                    + daemonMenuState.accessibilityDescription(
                        unattendedStopped: unattendedStopped)
            )
            .task(id: menuSyncCoordinatorID) {
                guard case .ready(let coordinator) = session.phase else { return }
                coordinator.startReachabilityMonitoring()
                defer { coordinator.stopReachabilityMonitoring() }
                await coordinator.heartbeatLoop(every: SyncCoordinator.heartbeatInterval)
            }
        }
        .menuBarExtraStyle(.window)

        Settings {
            FreesideSettings(
                session: session, navigation: navigation, daemon: daemon, flowPreferences: flowPreferences,
                launchInputs: launchInputs)
        }
    }

    private var daemonMenuState: DaemonMenuState {
        daemon?.state ?? .unsupervised
    }

    /// Whether the synced operating state says unattended operation is
    /// stopped. The menu bar's heartbeat loop keeps it current with the
    /// window closed, so the dot appears without opening the app.
    private var unattendedStopped: Bool {
        guard case .ready(let coordinator) = session.phase else { return false }
        return coordinator.unattendedStoppedMessage() != nil
    }

    private var menuSyncCoordinatorID: ObjectIdentifier? {
        guard case .ready(let coordinator) = session.phase else { return nil }
        return ObjectIdentifier(coordinator)
    }

    @MainActor
    private static func session(environment: FreesideEnvironment) throws -> AppSession {
        #if DEBUG
            if UserDefaults.standard.string(forKey: "FreesideDaemonMenuDemo") != nil {
                return AppSession.mock()
            }
        #endif
        return try AppSession.fromEnvironment(environment: environment)
    }

    @MainActor
    private static func daemonModel(environment: FreesideEnvironment) -> DaemonMenuModel? {
        #if DEBUG
            if let demo = UserDefaults.standard.string(forKey: "FreesideDaemonMenuDemo") {
                return DaemonMenuDemo.model(named: demo)
            }
        #endif
        guard environment.isSupervised else {
            // An ephemeral app has no LaunchAgent of its own, and every
            // supervised one belongs to an installed app, so it builds no
            // model and the menu says so rather than claiming a stopped
            // daemon. This runs before the mock check, so ephemeral mock
            // and pairing-demo launches take it too.
            return nil
        }
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
        return DaemonMenuModel(environment: environment)
    }
}

/// The main window as AppKit holds it, recorded by the main scene's
/// content. Each screen's `.navigationTitle` renames the window, so its
/// title can't find it: a task page's window isn't titled "Freeside".
@MainActor
private enum MainWindow {
    static weak var current: NSWindow?
}

private struct MainWindowReader: NSViewRepresentable {
    func makeNSView(context: Context) -> NSView { Recorder() }
    func updateNSView(_ nsView: NSView, context: Context) {}

    private final class Recorder: NSView {
        override func viewDidMoveToWindow() {
            super.viewDidMoveToWindow()
            if let window { MainWindow.current = window }
        }
    }
}

/// The main window brought forward, or opened when it was closed, with the
/// app activated: from the menu bar panel or the Settings window, another
/// window or app is in front.
@MainActor
private func showMainWindow(_ openWindow: OpenWindowAction) {
    if let window = MainWindow.current, window.isVisible || window.isMiniaturized {
        window.deminiaturize(nil)
        window.makeKeyAndOrderFront(nil)
    } else {
        openWindow(id: "main")
    }
    NSApplication.shared.activate()
}

/// The Settings window, with a seated finding card's Task link showing the
/// task page in the main window.
private struct FreesideSettings: View {
    let session: AppSession
    let navigation: NavigationModel
    let daemon: DaemonMenuModel?
    let flowPreferences: DecisionFlowPreferences
    let launchInputs: LaunchInputs
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        SettingsWindow(
            session: session, navigation: navigation, daemon: daemon, flowPreferences: flowPreferences,
            launchInputs: launchInputs,
            openTask: { taskID in
                navigation.route(to: .task(taskID))
                showMainWindow(openWindow)
            })
    }
}

/// The menu-bar panel bound to the live daemon model and session. The
/// panel itself is `DaemonMenuPanel` in FreesideCore, so the screenshot
/// suite renders every daemon state; this wrapper supplies the counts and
/// the handlers.
private struct DaemonMenu: View {
    /// Nil for an app with no supervised daemon; the panel then shows no
    /// lifecycle control, so the optional handlers below never fire.
    let model: DaemonMenuModel?
    let session: AppSession
    let navigation: NavigationModel
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        DaemonMenuPanel(
            state: model?.state ?? .unsupervised,
            actionError: model?.actionError,
            needsYou: coordinator.flatMap(DaemonMenuPanel.NeedsYouCount.init(coordinator:)),
            systemHealth: coordinator.flatMap(DaemonMenuPanel.SystemHealthCount.init(coordinator:)),
            unattendedStopped: coordinator?.unattendedStoppedMessage(),
            actions: DaemonMenuPanel.Actions(
                openApp: showApp,
                showNeedsYou: showApp,
                showSystemHealth: showSystemHealth,
                start: { Task { await model?.start() } },
                stop: { Task { await model?.stop() } },
                openApprovalSettings: { model?.openApprovalSettings() },
                quit: { NSApplication.shared.terminate(nil) }))
    }

    private var coordinator: SyncCoordinator? {
        guard case .ready(let coordinator) = session.phase else { return nil }
        return coordinator
    }

    private func showApp() {
        showMainWindow(openWindow)
    }

    /// The Settings window on System Health, the way the sidebar gear
    /// opens it. The app activates first: from the menu bar panel it is
    /// not active, so Settings would otherwise open behind the frontmost
    /// app.
    private func showSystemHealth() {
        navigation.openSystemHealth()
        NSApplication.shared.activate()
        openSettings()
    }
}

private struct FreesideAppCommands: Commands {
    let session: AppSession
    let navigation: NavigationModel
    @FocusedValue(\.decisionCommandActions) private var decisionActions

    var body: some Commands {
        // New Task takes the File > New slot (⌘N) instead of a Navigate row,
        // so it reads as document creation rather than navigation.
        CommandGroup(replacing: .newItem) {
            if let descriptor = FreesideCommandDescriptor.all.first(where: { $0.id == .newTask }) {
                Button(descriptor.title) { perform(.newTask) }
                    .keyboardShortcut(descriptor.shortcut)
                    .disabled(isDisabled(.newTask))
            }
        }
        CommandMenu("Navigate") {
            ForEach(FreesideCommandDescriptor.all.filter { $0.id != .newTask }) { descriptor in
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
        case .refresh:
            coordinator == nil
        case .nextItem, .previousItem:
            coordinator?.hasNeedsYouRows != true
        case .takeRecommendation:
            decisionActions?.canTakeRecommendation != true
        case .cancelPendingAction:
            decisionActions == nil
        case .newTask:
            coordinator.map { !TaskSubmissionModel.canCompose(freshness: $0.store.freshness) } ?? true
        }
    }

    private func perform(_ action: FreesideCommandAction) {
        switch action {
        case .refresh:
            guard let coordinator else { return }
            Task { await coordinator.refresh() }
        case .nextItem:
            guard let coordinator else { return }
            navigation.moveAttentionSelection(by: 1, coordinator: coordinator)
        case .previousItem:
            guard let coordinator else { return }
            navigation.moveAttentionSelection(by: -1, coordinator: coordinator)
        case .takeRecommendation:
            decisionActions?.takeRecommendation()
        case .cancelPendingAction:
            decisionActions?.cancelPendingAction()
        case .newTask:
            navigation.newTaskComposerPresented = true
        }
    }
}

@MainActor
private enum FreesideMenuIcon {
    private static let ink = NSColor(srgbRed: 0x17 / 255, green: 0x14 / 255, blue: 0x0F / 255, alpha: 1)
    private static let lightTile = NSColor(
        srgbRed: 0xFF / 255, green: 0xCB / 255, blue: 0x05 / 255, alpha: 1)
    private static let darkTile = NSColor(
        srgbRed: 0xF0 / 255, green: 0xBB / 255, blue: 0x00 / 255, alpha: 1)

    static func image(look: FreesideLook, badgeColor: NSColor?) -> NSImage {
        guard
            let url = Bundle.main.url(forResource: "FreesideMenuMark", withExtension: "png"),
            let mark = NSImage(contentsOf: url)
        else {
            preconditionFailure("FreesideMenuMark.png is missing from the app bundle")
        }
        let image = NSImage(size: NSSize(width: 20, height: 20), flipped: false) { rect in
            let dot = NSRect(x: 13, y: 13, width: 7, height: 7)
            switch look {
            case .prod:
                tinted(mark, NSColor.labelColor).draw(in: rect)
            case .dev:
                // Mark 2: the black key on a yellow tile. The tile follows the
                // menu bar's appearance, read when the handler draws.
                let isDark =
                    NSAppearance.currentDrawing().bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
                (isDark ? darkTile : lightTile).setFill()
                NSBezierPath(roundedRect: rect.insetBy(dx: 1, dy: 1), xRadius: 4.5, yRadius: 4.5)
                    .fill()
                tinted(mark, ink).draw(in: rect.insetBy(dx: 2, dy: 2))
                if badgeColor != nil {
                    // A real cutout, not a painted ring: the menu bar behind
                    // it is translucent, and the brass "stopped" dot would
                    // vanish against the yellow without the gap.
                    NSGraphicsContext.current?.compositingOperation = .clear
                    NSBezierPath(ovalIn: dot.insetBy(dx: -1.5, dy: -1.5)).fill()
                    NSGraphicsContext.current?.compositingOperation = .sourceOver
                }
            }
            if let badgeColor {
                // Top-right, over the key's bar; the dot is the status
                // channel at this size, where the key's own dot is retired.
                badgeColor.setFill()
                NSBezierPath(ovalIn: dot).fill()
            }
            return true
        }
        image.cacheMode = .never
        image.isTemplate = false
        return image
    }

    /// The mark in one color, drawn offscreen so the tint never paints over
    /// anything already on the canvas.
    private static func tinted(_ mark: NSImage, _ color: NSColor) -> NSImage {
        NSImage(size: mark.size, flipped: false) { rect in
            mark.draw(in: rect)
            color.set()
            rect.fill(using: .sourceIn)
            return true
        }
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
        case "incompatible":
            status = .enabled
            health = .appBelowFloor
        default:
            status = .notRegistered
            health = .unreachable
        }
        return DaemonMenuModel(
            service: DemoDaemonService(status: status),
            healthChecker: DemoHealthChecker(result: health),
            // The demo checker ignores the URL; no daemon is contacted.
            daemonURL: URL(fileURLWithPath: "/dev/null"),
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
        case appBelowFloor
        case unreachable
    }

    let result: Result
    let startedAt = Date.now.addingTimeInterval(-5 * 60)

    func health(at serverURL: URL) async throws -> DaemonHealth {
        switch result {
        case .running:
            return DaemonHealth(version: "1.0.0", startedAt: startedAt)
        case .appBelowFloor:
            // A daemon whose floor is far above any app's level, for the
            // menu's update-needed demo state (#3109). This target reads no
            // FreesideAPI constant, so the level is a literal.
            return DaemonHealth(
                version: "1.0.0", startedAt: startedAt,
                compatibilityLevel: 1_000, compatibilityFloor: 1_000)
        case .unreachable:
            throw URLError(.cannotConnectToHost)
        }
    }
}
