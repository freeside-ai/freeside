import Foundation

/// The instance tier this app belongs to (plan "Environments: Prod, Dev, and
/// Ephemeral"). The supervised tiers derive every local identifier from the
/// tier, so this enum is the only Swift owner of those strings;
/// `install-mac-app.sh` is the only other writer. `ephemeral` derives nothing:
/// its daemon is whatever run the launch names explicitly.
public enum FreesideEnvironment: String, CaseIterable, Sendable {
    case prod
    case dev
    case ephemeral

    public struct ResolutionError: Error, Equatable, CustomStringConvertible {
        public let source: String
        public let value: String

        public var description: String {
            let allowed = FreesideEnvironment.allCases.map(\.rawValue).joined(separator: ", ")
            return "\(source) is \"\(value)\", not one of \(allowed)"
        }
    }

    public static let environmentVariable = "FREESIDE_ENV"
    public static let infoPlistKey = "FreesideEnvironment"

    /// The plan's precedence: the `FREESIDE_ENV` variable, then the installed
    /// bundle's `FreesideEnvironment` key, then the build configuration. A
    /// source that is present but unknown fails instead of falling through,
    /// so a typo never lands an app on another tier's daemon.
    public static func resolve(
        environmentVariable: String?, infoPlistValue: String?, isDebugBuild: Bool
    ) throws -> FreesideEnvironment {
        for (source, value) in [
            (Self.environmentVariable, environmentVariable),
            ("Info.plist \(infoPlistKey)", infoPlistValue),
        ] {
            guard let value else { continue }
            guard let environment = FreesideEnvironment(rawValue: value) else {
                throw ResolutionError(source: source, value: value)
            }
            return environment
        }
        return isDebugBuild ? .ephemeral : .prod
    }

    /// Resolves from this process. The build configuration is the caller's,
    /// because `DEBUG` is a property of the app target, not of this package.
    public static func current(isDebugBuild: Bool) throws -> FreesideEnvironment {
        try resolve(
            environmentVariable: ProcessInfo.processInfo.environment[environmentVariable],
            infoPlistValue: Bundle.main.object(forInfoDictionaryKey: infoPlistKey) as? String,
            isDebugBuild: isDebugBuild)
    }

    public var isSupervised: Bool {
        switch self {
        case .prod, .dev: true
        case .ephemeral: false
        }
    }

    /// The installed bundle's display name, which also names the state root.
    public var displayName: String? {
        switch self {
        case .prod: "Freeside"
        case .dev: "Freeside Dev"
        case .ephemeral: nil
        }
    }

    /// The window badge; production shows none.
    public var badgeTitle: String? {
        switch self {
        case .prod: nil
        case .dev: "Dev"
        case .ephemeral: "Ephemeral"
        }
    }

    public var bundleIdentifier: String? {
        switch self {
        case .prod: "ai.freeside.app.macos"
        case .dev: "ai.freeside.app.macos.dev"
        case .ephemeral: nil
        }
    }

    public var launchdLabel: String? {
        switch self {
        case .prod: "ai.freeside.daemon"
        case .dev: "ai.freeside.daemon.dev"
        case .ephemeral: nil
        }
    }

    /// The LaunchAgent plist bundled under `Contents/Library/LaunchAgents`.
    public var launchAgentPlistName: String? {
        launchdLabel.map { "\($0).plist" }
    }

    /// The same-host daemon URL. Supervised ports are fixed because the
    /// Keychain device credential is keyed by this URL.
    public var supervisedAPIURL: URL? {
        switch self {
        case .prod: URL(string: "http://127.0.0.1:7331")
        case .dev: URL(string: "http://127.0.0.1:7332")
        case .ephemeral: nil
        }
    }

    public func stateRoot(fileManager: FileManager = .default) -> URL? {
        guard let displayName else { return nil }
        return fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask).first?
            .appendingPathComponent(displayName, isDirectory: true)
    }

    /// The daemon's `-state-dir`, which holds its readiness file.
    public func daemonStateDirectory(fileManager: FileManager = .default) -> URL? {
        stateRoot(fileManager: fileManager)?.appendingPathComponent("daemon", isDirectory: true)
    }
}
