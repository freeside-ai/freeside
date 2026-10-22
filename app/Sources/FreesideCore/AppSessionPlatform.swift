import Foundation

/// The launch entry points the app targets call, composed with the
/// Keychain: kept apart from `AppSession.swift` so the session model
/// names no Apple-only store.
extension AppSession {
    /// `fromEnvironment(environment:keychainStore:)` with the Keychain as
    /// the supervised tiers' credential store.
    public static func fromEnvironment(
        environment: FreesideEnvironment
    ) throws(LaunchError) -> AppSession {
        try fromEnvironment(environment: environment, keychainStore: keychainCredentialStore)
    }

    /// `fromEnvironment(keychainStore:)` with the Keychain as the remote
    /// client's credential store.
    public static func fromEnvironment() -> AppSession {
        fromEnvironment(keychainStore: keychainCredentialStore)
    }

    /// The deployment's Keychain credential. `nonisolated` so it satisfies
    /// the plain closure parameters without a main-actor hop, like
    /// `persistServerURLToDefaults`.
    public nonisolated static func keychainCredentialStore(for url: URL) -> any DeviceCredentialStore {
        KeychainCredentialStore(service: "ai.freeside.device-credential/\(deploymentKey(for: url))")
    }
}
