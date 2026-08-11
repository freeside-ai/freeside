#if os(macOS)
    import Foundation
    import ServiceManagement

    public enum DaemonServiceStatus: Equatable, Sendable {
        case notRegistered
        case enabled
        case requiresApproval
        case notFound
    }

    @MainActor
    protocol DaemonServiceRegistering: AnyObject {
        var status: DaemonServiceStatus { get }
        func register() throws
        func unregister() async throws
    }

    @MainActor
    private final class SystemDaemonServiceRegistration: DaemonServiceRegistering {
        private let service: SMAppService

        init(plistName: String) {
            service = .agent(plistName: plistName)
        }

        var status: DaemonServiceStatus {
            switch service.status {
            case .notRegistered:
                return .notRegistered
            case .enabled:
                return .enabled
            case .requiresApproval:
                return .requiresApproval
            case .notFound:
                return .notFound
            @unknown default:
                return .notFound
            }
        }

        func register() throws {
            try service.register()
        }

        func unregister() async throws {
            try await service.unregister()
        }
    }

    /// The app's launchd boundary. Menu state depends only on this protocol;
    /// production registration stays inside ServiceManagement and tests use a
    /// fake without touching the user's launchd domain.
    @MainActor
    public protocol DaemonServiceControlling: AnyObject {
        var status: DaemonServiceStatus { get }
        var needsAutomaticStart: Bool { get }
        func start() async throws
        func stop() async throws
        func openApprovalSettings()
    }

    @MainActor
    public final class SMAppDaemonService: DaemonServiceControlling {
        public static let plistName = "ai.freeside.daemon.plist"
        public static let registrationCurrentKey = "FreesideLaunchAgentRegistrationCurrent"
        public static let operatorDisabledKey = "FreesideLaunchAgentOperatorDisabled"

        private let service: any DaemonServiceRegistering
        private let defaults: UserDefaults

        public init(plistName: String = plistName, defaults: UserDefaults = .standard) {
            service = SystemDaemonServiceRegistration(plistName: plistName)
            self.defaults = defaults
        }

        init(service: any DaemonServiceRegistering, defaults: UserDefaults) {
            self.service = service
            self.defaults = defaults
        }

        public var status: DaemonServiceStatus {
            service.status
        }

        public var needsAutomaticStart: Bool {
            !defaults.bool(forKey: Self.registrationCurrentKey)
                && !defaults.bool(forKey: Self.operatorDisabledKey)
        }

        public func start() async throws {
            defaults.removeObject(forKey: Self.operatorDisabledKey)
            switch status {
            case .enabled where defaults.bool(forKey: Self.registrationCurrentKey),
                .requiresApproval where defaults.bool(forKey: Self.registrationCurrentKey):
                return
            case .enabled, .requiresApproval:
                try await service.unregister()
                try registerCurrentService()
            case .notRegistered, .notFound:
                try registerCurrentService()
            }
        }

        public func stop() async throws {
            switch status {
            case .notRegistered:
                break
            case .enabled, .requiresApproval, .notFound:
                try await service.unregister()
            }
            defaults.removeObject(forKey: Self.registrationCurrentKey)
            defaults.set(true, forKey: Self.operatorDisabledKey)
        }

        public func openApprovalSettings() {
            SMAppService.openSystemSettingsLoginItems()
        }

        private func registerCurrentService() throws {
            do {
                try service.register()
                defaults.set(true, forKey: Self.registrationCurrentKey)
            } catch {
                // Registration can be current but awaiting the user's Login
                // Items approval. Remember that generation so an ordinary app
                // relaunch does not unregister the pending request.
                if status == .requiresApproval {
                    defaults.set(true, forKey: Self.registrationCurrentKey)
                }
                throw error
            }
        }
    }
#endif
