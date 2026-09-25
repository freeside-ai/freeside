import CryptoKit
import Foundation
import FreesideAPI
import Observation

/// The app's composition root: which stores back this launch, and
/// whether the device still needs pairing. A session is ready exactly
/// when a device credential exists; everything downstream (the sync
/// coordinator, the command device identity, the bearer middleware's
/// provider) derives from that credential.
@MainActor
@Observable
public final class AppSession {
    public enum PhaseState {
        case needsConnection
        case needsPairing(PairingModel)
        case ready(SyncCoordinator)
    }

    public private(set) var phase: PhaseState

    private struct Connection {
        let client: any APIProtocol
        let credentials: any DeviceCredentialStore
        let cache: any CacheStore
        let deploymentURL: URL?
        let displayName: String?
    }

    private var connection: Connection?
    private let persistServerURL: (URL) -> Void
    /// This tier's same-host supervised daemon, whose readiness file the Mac
    /// app watches; nil when the launch has none (`ephemeral`, iOS).
    private let localDaemonURL: URL?
    /// Where a live connection keeps its disk cache: the tier's state root,
    /// so a non-prod app never writes under the prod root, or nil for an
    /// in-memory cache (`ephemeral` derives no path). `connect(serverURL:)`
    /// reuses it with `persistServerURL`, so a typed server follows the same
    /// rules as the launch.
    private let cacheRoot: URL?
    /// Where a live connection keeps its device credential, per deployment:
    /// the Keychain for a supervised tier or a remote client, memory for
    /// `ephemeral`, which pairs afresh on each run. `connect(serverURL:)`
    /// reuses it like `cacheRoot`.
    private let credentialStore: (URL) -> any DeviceCredentialStore
    /// The last readiness the session was handed (`applyReadiness`),
    /// remembered so `rePair()` can prefill the fresh pairing screen the
    /// same way a launch does: on the Mac readiness is delivered only when
    /// it changes, so a session that re-pairs mid-run would otherwise have
    /// nothing to prefill from.
    private var lastReadiness: DaemonReadiness?

    public init(
        client: any APIProtocol,
        credentials: any DeviceCredentialStore,
        cache: any CacheStore,
        pairingCode: String = "",
        displayName: String? = nil,
        deploymentURL: URL? = nil,
        localDaemonURL: URL? = nil,
        cacheRoot: URL? = AppSession.defaultCacheRoot,
        credentialStore: @escaping (URL) -> any DeviceCredentialStore = AppSession.keychainCredentialStore,
        persistServerURL: @escaping (URL) -> Void = AppSession.persistServerURLToDefaults
    ) {
        self.connection = Connection(
            client: client, credentials: credentials, cache: cache,
            deploymentURL: deploymentURL, displayName: displayName)
        self.persistServerURL = persistServerURL
        self.localDaemonURL = localDaemonURL
        self.cacheRoot = cacheRoot
        self.credentialStore = credentialStore
        // An unreadable credential is indistinguishable from an absent
        // one here, and the recovery is the same either way: pairing
        // mints a new device (#64; a lost token is revoke-and-repair).
        if let credential = try? credentials.load() {
            phase = .ready(
                Self.coordinator(client: client, cache: cache, credential: credential, deploymentURL: deploymentURL))
            // A live session already holding a credential enters `.ready`
            // here without pairing, so this is its only chance to record the
            // deployment URL for a later unadorned relaunch: `completePairing`
            // never runs. This is the reinstall-with-preserved-Keychain (and
            // cleared preferences) case, and the switch back to a previously
            // paired daemon. Mirror `completePairing`'s write; mock and
            // pairing-demo sessions carry no `deploymentURL` and persist
            // nothing.
            if let deploymentURL {
                persistServerURL(deploymentURL)
            }
        } else {
            phase = .needsPairing(
                PairingModel(
                    client: client, credentials: credentials, pairingCode: pairingCode,
                    displayName: displayName))
        }
    }

    /// Hands the freshly paired identity to the synced surface; the
    /// pairing model already stored the credential. A live session also
    /// records its deployment URL so a later unadorned relaunch (the iOS
    /// home-screen case, which passes no launch arguments) re-enters live
    /// mode through `fromEnvironment()`'s persisted-URL branch instead of
    /// asking for a server again. Mock and pairing-demo sessions carry no
    /// `deploymentURL` and persist nothing.
    public func completePairing(_ credential: DeviceCredential) {
        guard let connection else {
            preconditionFailure("Pairing requires a configured connection")
        }
        if let deploymentURL = connection.deploymentURL {
            persistServerURL(deploymentURL)
        }
        phase = .ready(
            Self.coordinator(
                client: connection.client, cache: connection.cache, credential: credential,
                deploymentURL: connection.deploymentURL))
    }

    /// The operator's in-app recovery from a rejected credential (the
    /// revoked freshness banner, #1458): delete this deployment's stored
    /// credential and return to pairing for the same daemon. Deleting a
    /// credential cannot be undone, so the caller confirms first; a first
    /// launch pairs without a bearer token, so the pairing endpoints still
    /// answer once it is gone. Only meaningful while `.ready`; anything
    /// else is a no-op. `delete()` can throw after it has already removed the
    /// authoritative credential (the macOS store deletes the Data Protection
    /// item before the legacy one and reports a later legacy-Keychain error),
    /// so a thrown error returns to pairing only on a confirmed absence: a
    /// credential that still loads, or a reload that itself throws (an
    /// indeterminate result, since the credential may remain), keeps the phase
    /// unchanged. The app never shows pairing over a credential that still
    /// exists or might, nor claims "still paired" over one that is gone. The
    /// cache, the saved deployment URL, and `changeServer()` are left
    /// untouched (Non-goals): the next sync discards a stale cache on its own
    /// when the daemon's epoch changed.
    public func rePair() throws {
        guard case .ready = phase, let connection else { return }
        do {
            try connection.credentials.delete()
        } catch let deleteError {
            // Reload to tell a completed delete from a real failure. Proceed
            // to pairing only on a confirmed absence: a load that itself
            // throws is indeterminate (the credential may remain, e.g. the
            // legacy item read back and re-promoted before a cleanup error),
            // so surface the delete failure and stay ready rather than show
            // pairing over a credential that can still answer requests.
            let credentialGone: Bool
            do {
                credentialGone = try connection.credentials.load() == nil
            } catch {
                throw deleteError
            }
            if !credentialGone { throw deleteError }
        }
        phase = .needsPairing(
            PairingModel(
                client: connection.client, credentials: connection.credentials,
                displayName: connection.displayName))
        // Prefill the fresh screen from the last readiness, exactly as a
        // launch would; an operator-typed code is never overwritten.
        applyReadiness(lastReadiness)
    }

    private init(
        localDaemonURL: URL?, cacheRoot: URL?,
        credentialStore: @escaping (URL) -> any DeviceCredentialStore,
        persistServerURL: @escaping (URL) -> Void
    ) {
        connection = nil
        self.persistServerURL = persistServerURL
        self.localDaemonURL = localDaemonURL
        self.cacheRoot = cacheRoot
        self.credentialStore = credentialStore
        phase = .needsConnection
    }

    public func connect(serverURL: URL) {
        let selected = Self.live(
            serverURL: serverURL, localDaemonURL: localDaemonURL, cacheRoot: cacheRoot,
            credentialStore: credentialStore, persistServerURL: persistServerURL)
        connection = selected.connection
        phase = selected.phase
    }

    /// Returns to address entry without changing saved deployments or credentials.
    func changeServer() {
        connection = nil
        phase = .needsConnection
    }

    /// A first-run LaunchAgent may publish readiness after the window already
    /// rendered. Replace only an empty or still-unedited readiness suggestion
    /// for the deployment this session already selected; never overwrite
    /// operator input or apply a local code to a persisted remote daemon.
    public func applyReadiness(_ readiness: DaemonReadiness?) {
        // Remember it even while `.ready`, where the prefill below is a
        // no-op, so a later `rePair()` can replay it onto the fresh screen.
        lastReadiness = readiness
        guard
            let deploymentURL = connection?.deploymentURL,
            case .needsPairing(let model) = phase
        else { return }
        guard let readiness else {
            if let localDaemonURL,
                Self.deploymentKey(for: deploymentURL) == Self.deploymentKey(for: localDaemonURL)
            {
                model.clearPairingCodePrefill()
            }
            return
        }
        guard Self.deploymentKey(for: deploymentURL) == Self.deploymentKey(for: readiness.apiURL)
        else { return }
        model.prefillPairingCode(readiness.pairingCode)
    }

    private static func coordinator(
        client: any APIProtocol, cache: any CacheStore, credential: DeviceCredential, deploymentURL: URL?
    ) -> SyncCoordinator {
        SyncCoordinator(
            client: client,
            device: DeviceIdentity(deviceID: credential.deviceID),
            cache: cache,
            submissionDaemonID: deploymentURL.map { deploymentKey(for: $0) } ?? "mock"
        )
    }

    // MARK: - Launch compositions

    enum LaunchMode: Equatable {
        case needsConnection
        case live(URL, pairingCode: String)
        case pairingDemo
        case mock
    }

    static func launchMode(
        argumentServerURL: String?,
        pairingDemo: Bool,
        mockMode: Bool,
        readiness: DaemonReadiness?,
        persistedServerURL: String?,
        localDaemonURL: URL?,
        hasCredential: (URL) -> Bool
    ) -> LaunchMode {
        if let argumentServerURL {
            guard let url = serverURL(from: argumentServerURL) else { return .needsConnection }
            return .live(url, pairingCode: "")
        }
        if pairingDemo {
            return .pairingDemo
        }
        if mockMode {
            return .mock
        }
        let persistedURL = persistedServerURL.flatMap { serverURL(from: $0) }
        if let readiness {
            let matchesLocalDaemon =
                localDaemonURL.map {
                    Self.deploymentKey(for: readiness.apiURL) == Self.deploymentKey(for: $0)
                } ?? true
            if matchesLocalDaemon {
                if !hasCredential(readiness.apiURL),
                    let persistedURL,
                    hasCredential(persistedURL)
                {
                    return .live(persistedURL, pairingCode: "")
                }
                return .live(readiness.apiURL, pairingCode: readiness.pairingCode)
            }
        }
        if let persistedURL {
            return .live(persistedURL, pairingCode: "")
        }
        if let localDaemonURL {
            return .live(localDaemonURL, pairingCode: "")
        }
        return .needsConnection
    }

    public enum LaunchError: Error, Equatable, CustomStringConvertible {
        case readinessDirectoryInSupervisedTier(FreesideEnvironment)
        case readinessDirectoryNotAbsolute(String)
        case readinessDirectoryUnderSupervisedRoot(String, FreesideEnvironment)

        public var description: String {
            switch self {
            case .readinessDirectoryInSupervisedTier(let environment):
                "-FreesideReadinessDir is only for an ephemeral app; this app is \(environment.rawValue)"
            case .readinessDirectoryNotAbsolute(let path):
                "-FreesideReadinessDir must be an absolute path, not \"\(path)\""
            case .readinessDirectoryUnderSupervisedRoot(let path, let environment):
                "-FreesideReadinessDir \"\(path)\" is inside the \(environment.rawValue) state root; an ephemeral app never reads a supervised daemon's readiness"
            }
        }
    }

    /// The Mac's tier-aware resolution, which feeds the generic one above. A
    /// supervised tier reads its own derived readiness file and falls back to
    /// its own fixed port. `ephemeral` has no local daemon of its own: it
    /// follows the run named by `-FreesideReadinessDir` or asks for a
    /// connection, and never takes the persisted URL, which an earlier launch
    /// may have pointed at a supervised daemon. It never probes the Keychain
    /// either: its credentials live in memory (`credentialStore(for:)`), and
    /// a Debug build shares prod's Keychain identity, so a probe could prompt
    /// or migrate a prod item. The readiness directory is
    /// refused in a supervised tier so a stray argument cannot point the
    /// operator's app at another daemon, and refused under any supervised
    /// state root so an ephemeral run never consumes a supervised daemon's
    /// one-time pairing code (plan: a non-prod instance never names a prod
    /// root; `freesided` refuses both supervised roots the same way).
    static func launchMode(
        environment: FreesideEnvironment,
        argumentServerURL: String?,
        pairingDemo: Bool,
        mockMode: Bool,
        readinessDirectory: String?,
        persistedServerURL: String?,
        readReadiness: (URL) -> DaemonReadiness?,
        hasCredential: (URL) -> Bool,
        fileManager: FileManager = .default
    ) throws(LaunchError) -> LaunchMode {
        let readinessDirectoryURL: URL?
        if let readinessDirectory {
            guard !environment.isSupervised else {
                throw .readinessDirectoryInSupervisedTier(environment)
            }
            guard readinessDirectory.hasPrefix("/") else {
                throw .readinessDirectoryNotAbsolute(readinessDirectory)
            }
            let directory = URL(fileURLWithPath: readinessDirectory, isDirectory: true)
            if let owner = supervisedRootOwner(of: directory, fileManager: fileManager) {
                throw .readinessDirectoryUnderSupervisedRoot(readinessDirectory, owner)
            }
            readinessDirectoryURL = directory
        } else {
            readinessDirectoryURL = environment.daemonStateDirectory()
        }
        return launchMode(
            argumentServerURL: argumentServerURL,
            pairingDemo: pairingDemo,
            mockMode: mockMode,
            readiness: readinessDirectoryURL.flatMap {
                readReadiness(DaemonReadinessReader.fileURL(inStateDirectory: $0))
            },
            persistedServerURL: environment.isSupervised ? persistedServerURL : nil,
            localDaemonURL: environment.supervisedAPIURL,
            hasCredential: environment.isSupervised ? hasCredential : { _ in false })
    }

    /// The supervised tier whose state root contains `directory`, compared
    /// after resolving symlinks on both sides so an alias cannot slip past.
    private static func supervisedRootOwner(
        of directory: URL, fileManager: FileManager
    ) -> FreesideEnvironment? {
        let path = directory.resolvingSymlinksInPath().standardizedFileURL.path
        return FreesideEnvironment.allCases.first { environment in
            guard let root = environment.stateRoot(fileManager: fileManager) else { return false }
            let rootPath = root.resolvingSymlinksInPath().standardizedFileURL.path
            return path == rootPath || path.hasPrefix(rootPath + "/")
        }
    }

    static func serverURL(from value: String) -> URL? {
        guard
            let components = URLComponents(string: value.trimmingCharacters(in: .whitespacesAndNewlines)),
            let scheme = components.scheme?.lowercased(),
            scheme == "http" || scheme == "https",
            components.host?.isEmpty == false,
            components.user == nil, components.password == nil,
            components.query == nil, components.fragment == nil
        else { return nil }
        if let port = components.port, !(1...65535).contains(port) { return nil }
        return components.url
    }

    /// Explicit launch inputs stay the development override. Otherwise a
    /// daemon-host readiness file selects and prefills the local deployment,
    /// unless only the persisted deployment holds a device credential. The
    /// Mac's local daemon, readiness file, and persisted-URL rule come from
    /// its tier (`launchMode(environment:...)`). Sample data requires an
    /// explicit mock or pairing-demo launch argument.
    public static func fromEnvironment(
        environment: FreesideEnvironment
    ) throws(LaunchError) -> AppSession {
        let arguments = argumentDomain()
        let mode = try launchMode(
            environment: environment,
            argumentServerURL: arguments["FreesideServerURL"] as? String,
            pairingDemo: arguments["FreesidePairingDemo"] as? String == "YES",
            mockMode: arguments["FreesideMock"] as? String == "YES",
            readinessDirectory: arguments["FreesideReadinessDir"] as? String,
            persistedServerURL: persistedServerURL(),
            readReadiness: { DaemonReadinessReader().read(at: $0) },
            hasCredential: hasStoredCredential)
        // The ephemeral app shares the production bundle ID, and so its
        // preferences domain; recording a throwaway run there would send the
        // installed app to a dead daemon on its next launch.
        let persist: (URL) -> Void =
            environment.isSupervised ? { persistServerURLToDefaults($0) } : { _ in }
        return session(
            for: mode, localDaemonURL: environment.supervisedAPIURL,
            cacheRoot: environment.stateRoot(), credentialStore: credentialStore(for: environment),
            persistServerURL: persist)
    }

    /// A supervised tier keeps its device credential in the Keychain, keyed
    /// by its fixed daemon URL. `ephemeral` keeps it in memory: a run may
    /// reuse a fixed port with a fresh credential database, so a Keychain
    /// credential from an earlier run would skip that run's pairing and send
    /// a stale token (plan: `ephemeral` pairs afresh on each run).
    static func credentialStore(
        for environment: FreesideEnvironment
    ) -> (URL) -> any DeviceCredentialStore {
        if environment.isSupervised {
            return { keychainCredentialStore(for: $0) }
        }
        return { _ in InMemoryCredentialStore() }
    }

    /// A remote client (iOS) with no same-host daemon: explicit inputs, then
    /// the persisted deployment, then the connect screen.
    public static func fromEnvironment() -> AppSession {
        let arguments = argumentDomain()
        let mode = launchMode(
            argumentServerURL: arguments["FreesideServerURL"] as? String,
            pairingDemo: arguments["FreesidePairingDemo"] as? String == "YES",
            mockMode: arguments["FreesideMock"] as? String == "YES",
            readiness: nil,
            persistedServerURL: persistedServerURL(),
            localDaemonURL: nil,
            hasCredential: hasStoredCredential)
        return session(
            for: mode, localDaemonURL: nil, cacheRoot: defaultCacheRoot,
            credentialStore: keychainCredentialStore, persistServerURL: persistServerURLToDefaults)
    }

    private static func argumentDomain() -> [String: Any] {
        UserDefaults.standard.volatileDomain(forName: UserDefaults.argumentDomain)
    }

    private static func persistedServerURL() -> String? {
        Bundle.main.bundleIdentifier.flatMap {
            UserDefaults.standard.persistentDomain(forName: $0)?["FreesideServerURL"] as? String
        }
    }

    private static func hasStoredCredential(_ url: URL) -> Bool {
        (try? keychainCredentialStore(for: url).load()) != nil
    }

    /// The deployment's Keychain credential. `nonisolated` so it satisfies
    /// the plain closure default without a main-actor hop, like
    /// `persistServerURLToDefaults`.
    public nonisolated static func keychainCredentialStore(for url: URL) -> any DeviceCredentialStore {
        KeychainCredentialStore(service: "ai.freeside.device-credential/\(deploymentKey(for: url))")
    }

    private static func session(
        for mode: LaunchMode, localDaemonURL: URL?, cacheRoot: URL?,
        credentialStore: @escaping (URL) -> any DeviceCredentialStore,
        persistServerURL: @escaping (URL) -> Void
    ) -> AppSession {
        switch mode {
        case .needsConnection:
            AppSession(
                localDaemonURL: localDaemonURL, cacheRoot: cacheRoot, credentialStore: credentialStore,
                persistServerURL: persistServerURL)
        case .live(let url, let pairingCode):
            live(
                serverURL: url, pairingCode: pairingCode, localDaemonURL: localDaemonURL,
                cacheRoot: cacheRoot, credentialStore: credentialStore, persistServerURL: persistServerURL)
        case .pairingDemo:
            pairingDemo()
        case .mock:
            mock()
        }
    }

    /// Writes the live deployment URL into the app's persistent domain
    /// under the same `FreesideServerURL` key the Mac installer seeds
    /// externally (`install-mac-app.sh`) and `fromEnvironment()` reads, so
    /// an unadorned relaunch resolves back to this deployment. The write
    /// mirrors that read: keyed on `Bundle.main.bundleIdentifier`, storing
    /// `url.absoluteString`. `nonisolated` so it satisfies the plain
    /// closure default without a main-actor hop; `Bundle`/`UserDefaults`
    /// are their own synchronization.
    public nonisolated static func persistServerURLToDefaults(_ url: URL) {
        guard let bundleID = Bundle.main.bundleIdentifier else { return }
        let defaults = UserDefaults.standard
        var domain = defaults.persistentDomain(forName: bundleID) ?? [:]
        domain["FreesideServerURL"] = url.absoluteString
        defaults.setPersistentDomain(domain, forName: bundleID)
    }

    /// A real daemon: the credential lives in the Keychain and nowhere
    /// else (or only in memory for `ephemeral`), and the disk cache lives
    /// in the app container (plan §5.14). Both are scoped to the daemon
    /// deployment: a device credential is minted by one daemon, so the
    /// credential store keys on the server URL and a token can never be
    /// attached to a request for another daemon; the cached rows are
    /// likewise one deployment's state.
    public static func live(
        serverURL: URL, pairingCode: String = "", localDaemonURL: URL? = nil,
        cacheRoot: URL? = AppSession.defaultCacheRoot,
        credentialStore: @escaping (URL) -> any DeviceCredentialStore = AppSession.keychainCredentialStore,
        persistServerURL: @escaping (URL) -> Void = AppSession.persistServerURLToDefaults
    ) -> AppSession {
        let credentials = credentialStore(serverURL)
        return AppSession(
            client: APIClientFactory.live(serverURL: serverURL) {
                (try? credentials.load())?.token
            },
            credentials: credentials,
            cache: cacheRoot.map { DiskCacheStore(directory: cacheDirectory(for: serverURL, in: $0)) }
                ?? InMemoryCacheStore(),
            pairingCode: pairingCode,
            deploymentURL: serverURL,
            localDaemonURL: localDaemonURL,
            cacheRoot: cacheRoot,
            credentialStore: credentialStore,
            persistServerURL: persistServerURL
        )
    }

    /// One stable key per daemon deployment: scheme and host are
    /// case-insensitive and normalize, an explicit port and a non-root
    /// path distinguish deployments, and a bare trailing slash does not.
    /// Do not fall back to the former decoded-path key for encoded paths:
    /// it cannot prove which of two colliding deployments owns a credential.
    public nonisolated static func deploymentKey(for url: URL) -> String {
        let scheme = url.scheme?.lowercased() ?? "http"
        let host = url.host?.lowercased() ?? ""
        let port = url.port.map { ":\($0)" } ?? ""
        let encodedPath =
            URLComponents(url: url, resolvingAgainstBaseURL: false)?.percentEncodedPath ?? url.path
        let path = encodedPath == "/" ? "" : encodedPath
        return "\(scheme)://\(host)\(port)\(path)"
    }

    /// The path component must distinguish deployments exactly as the
    /// key does, so identity comes from a digest of the key (a lossy
    /// separator replacement collapsed distinct keys into one
    /// directory); the host prefix exists only for a human reading the
    /// container.
    public static func cacheDirectory(for url: URL, in root: URL = defaultCacheRoot) -> URL {
        let digest = SHA256.hash(data: Data(deploymentKey(for: url).utf8))
            .map { String(format: "%02x", $0) }.joined().prefix(16)
        let host = (url.host?.lowercased() ?? "daemon")
            .replacingOccurrences(of: ":", with: "_")
        return root.appendingPathComponent("\(host)-\(digest)")
    }

    /// The cache root before tiers existed; the prod state root on the Mac
    /// (so existing prod caches stay put) and the iOS container's.
    public static var defaultCacheRoot: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Freeside")
    }

    /// An explicitly requested demo: a permissive mock and a pre-paired
    /// mock identity, so the inbox renders immediately.
    public static func mock() -> AppSession {
        let secretSegment = Data(repeating: 0, count: 32).base64EncodedString()
            .replacingOccurrences(of: "=", with: "")
        // swift-format-ignore: NeverForceUnwrap
        return AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(
                credential: DeviceCredential(
                    deviceID: DeviceIdentity.mock.deviceID,
                    token: "fsd1.ZGV2aWNlLW1vY2s.\(secretSegment)",
                    ntfySubscription: .mock)!),
            cache: InMemoryCacheStore()
        )
    }

    /// The pairing flow end to end against an enforcing mock; nothing
    /// persists across launches.
    public static func pairingDemo() -> AppSession {
        var facts = MockServer.pairingFacts
        facts.code_expires_at = Date().addingTimeInterval(600)
        let server = MockServer(
            authMode: .enforcing,
            pairingCodes: ["483911": .valid],
            pairingFacts: facts,
            automaticallyCompletesAgentWork: true
        )
        let credentials = InMemoryCredentialStore()
        #if os(iOS)
            let displayName = "Review iPhone"
        #else
            let displayName = "Studio Mac"
        #endif
        return AppSession(
            client: APIClientFactory.mock(server: server) {
                (try? credentials.load())?.token
            },
            credentials: credentials,
            cache: InMemoryCacheStore(),
            displayName: displayName
        )
    }
}
