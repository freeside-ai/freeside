import AppKit
import FreesideCore
import SwiftUI

@main
struct FreesideMacApp: App {
    @State private var session: AppSession
    @State private var daemon: DaemonMenuModel

    init() {
        _session = State(initialValue: Self.session())
        _daemon = State(initialValue: Self.daemonModel())
    }

    var body: some Scene {
        WindowGroup {
            Group {
                #if DEBUG
                    if UserDefaults.standard.string(forKey: "FreesideDaemonMenuDemo") != nil {
                        DaemonMenu(model: daemon)
                            .frame(width: 280)
                            .padding()
                    } else {
                        FreesideRootView(session: session)
                    }
                #else
                    FreesideRootView(session: session)
                #endif
            }
            .task { daemon.startMonitoring() }
            .onChange(of: daemon.readiness, initial: true) { _, readiness in
                session.applyReadiness(readiness)
            }
        }
        .defaultSize(width: 960, height: 640)

        MenuBarExtra {
            DaemonMenu(model: daemon)
        } label: {
            Image(nsImage: FreesideMenuIcon.image(badgeColor: daemon.state.menuBadgeColor))
                .renderingMode(.original)
                .accessibilityElement(children: .ignore)
                .accessibilityLabel("Freeside: \(daemon.state.accessibilityDescription)")
        }
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

private struct DaemonMenu: View {
    let model: DaemonMenuModel

    var body: some View {
        switch model.state {
        case .checking:
            Text("Checking daemon…")
        case .stopped:
            Label("Daemon stopped", systemImage: "stop.circle")
            Button("Start") { Task { await model.start() } }
        case .needsApproval:
            Label("Approval needed", systemImage: "exclamationmark.triangle.fill")
            Text("Allow Freeside in Login Items to start the daemon.")
            Button("Open Login Items") { model.openApprovalSettings() }
            Button("Stop") { Task { await model.stop() } }
        case .unavailable:
            Label("LaunchAgent unavailable", systemImage: "xmark.circle.fill")
            Button("Start") { Task { await model.start() } }
        case .unreachable:
            Label("Daemon unreachable", systemImage: "xmark.circle.fill")
            Text("launchd is keeping the service enabled, but health is not answering.")
            Button("Stop") { Task { await model.stop() } }
        case .running(let health, let restartObserved):
            Label("Daemon running", systemImage: "checkmark.circle.fill")
            Text("Version \(health.version)")
            Text("Started \(health.startedAt.formatted(date: .abbreviated, time: .standard))")
            if restartObserved {
                Label("Restart observed", systemImage: "arrow.clockwise")
            }
            Button("Stop") { Task { await model.stop() } }
        }
        if let error = model.actionError {
            Divider()
            Text(error)
        }
        Divider()
        Button("Quit Freeside") { NSApplication.shared.terminate(nil) }
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
                badgeColor.setFill()
                NSBezierPath(ovalIn: NSRect(x: 13, y: 0, width: 7, height: 7)).fill()
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
