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
        case needsPairing(PairingModel)
        case ready(SyncCoordinator)
    }

    public private(set) var phase: PhaseState

    private let client: any APIProtocol
    private let cache: any CacheStore
    private let deploymentURL: URL?
    private let persistServerURL: (URL) -> Void

    public init(
        client: any APIProtocol,
        credentials: any DeviceCredentialStore,
        cache: any CacheStore,
        pairingCode: String = "",
        displayName: String? = nil,
        deploymentURL: URL? = nil,
        persistServerURL: @escaping (URL) -> Void = AppSession.persistServerURLToDefaults
    ) {
        self.client = client
        self.cache = cache
        self.deploymentURL = deploymentURL
        self.persistServerURL = persistServerURL
        // An unreadable credential is indistinguishable from an absent
        // one here, and the recovery is the same either way: pairing
        // mints a new device (#64; a lost token is revoke-and-repair).
        if let credential = try? credentials.load() {
            phase = .ready(Self.coordinator(client: client, cache: cache, credential: credential))
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
    /// falling back to the mock. Mock and pairing-demo sessions carry no
    /// `deploymentURL` and persist nothing.
    public func completePairing(_ credential: DeviceCredential) {
        if let deploymentURL {
            persistServerURL(deploymentURL)
        }
        phase = .ready(Self.coordinator(client: client, cache: cache, credential: credential))
    }

    /// A first-run LaunchAgent may publish readiness after the window already
    /// rendered. Replace only an empty or still-unedited readiness suggestion
    /// for the deployment this session already selected; never overwrite
    /// operator input or apply a local code to a persisted remote daemon.
    public func applyReadiness(_ readiness: DaemonReadiness?) {
        guard
            let deploymentURL,
            case .needsPairing(let model) = phase
        else { return }
        guard let readiness else {
            if Self.deploymentKey(for: deploymentURL)
                == Self.deploymentKey(for: DaemonReadinessReader.supervisedAPIURL)
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
        client: any APIProtocol, cache: any CacheStore, credential: DeviceCredential
    ) -> SyncCoordinator {
        SyncCoordinator(
            client: client,
            device: DeviceIdentity(deviceID: credential.deviceID),
            cache: cache
        )
    }

    // MARK: - Launch compositions

    enum LaunchMode: Equatable {
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
        if let argumentServerURL, let url = URL(string: argumentServerURL) {
            return .live(url, pairingCode: "")
        }
        if pairingDemo {
            return .pairingDemo
        }
        if mockMode {
            return .mock
        }
        let persistedURL = persistedServerURL.flatMap { rawValue -> URL? in
            guard
                let url = URL(string: rawValue),
                let scheme = url.scheme?.lowercased(),
                scheme == "http" || scheme == "https",
                url.host?.isEmpty == false
            else { return nil }
            if let port = url.port, !(1...65535).contains(port) {
                return nil
            }
            return url
        }
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
        return .mock
    }

    /// Explicit launch inputs stay the development override. Otherwise a
    /// daemon-host readiness file selects and prefills the local deployment,
    /// unless only the persisted deployment holds a device credential. The
    /// local daemon fallback and today's permissive mock experience follow.
    public static func fromEnvironment() -> AppSession {
        let defaults = UserDefaults.standard
        let arguments = defaults.volatileDomain(forName: UserDefaults.argumentDomain)
        let readiness = DaemonReadinessReader.defaultFileURL()
            .flatMap { DaemonReadinessReader().read(at: $0) }
        let persistedServerURL = Bundle.main.bundleIdentifier
            .flatMap { defaults.persistentDomain(forName: $0)?["FreesideServerURL"] as? String }
        #if os(macOS)
            let localDaemonURL: URL? = DaemonReadinessReader.supervisedAPIURL
        #else
            let localDaemonURL: URL? = nil
        #endif
        switch launchMode(
            argumentServerURL: arguments["FreesideServerURL"] as? String,
            pairingDemo: arguments["FreesidePairingDemo"] as? String == "YES",
            mockMode: arguments["FreesideMock"] as? String == "YES",
            readiness: readiness,
            persistedServerURL: persistedServerURL,
            localDaemonURL: localDaemonURL,
            hasCredential: {
                (try? KeychainCredentialStore(
                    service: "ai.freeside.device-credential/\(deploymentKey(for: $0))"
                ).load()) != nil
            }
        ) {
        case .live(let url, let pairingCode):
            return live(serverURL: url, pairingCode: pairingCode)
        case .pairingDemo:
            return pairingDemo()
        case .mock:
            return mock()
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
    /// else, and the disk cache lives in the app container (plan §5.14).
    /// Both are scoped to the daemon deployment: a device credential is
    /// minted by one daemon, so the Keychain lookup keys on the server
    /// URL and a token can never be attached to a request for another
    /// daemon; the cached rows are likewise one deployment's state.
    public static func live(serverURL: URL, pairingCode: String = "") -> AppSession {
        let credentials = KeychainCredentialStore(
            service: "ai.freeside.device-credential/\(deploymentKey(for: serverURL))")
        return AppSession(
            client: APIClientFactory.live(serverURL: serverURL) {
                (try? credentials.load())?.token
            },
            credentials: credentials,
            cache: DiskCacheStore(directory: cacheDirectory(for: serverURL)),
            pairingCode: pairingCode,
            deploymentURL: serverURL
        )
    }

    /// One stable key per daemon deployment: scheme and host are
    /// case-insensitive and normalize, an explicit port and a non-root
    /// path distinguish deployments, and a bare trailing slash does not.
    /// Do not fall back to the former decoded-path key for encoded paths:
    /// it cannot prove which of two colliding deployments owns a credential.
    public static func deploymentKey(for url: URL) -> String {
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
    public static func cacheDirectory(for url: URL) -> URL {
        let digest = SHA256.hash(data: Data(deploymentKey(for: url).utf8))
            .map { String(format: "%02x", $0) }.joined().prefix(16)
        let host = (url.host?.lowercased() ?? "daemon")
            .replacingOccurrences(of: ":", with: "_")
        return FileManager.default.urls(
            for: .applicationSupportDirectory, in: .userDomainMask
        )[0]
        .appendingPathComponent("Freeside")
        .appendingPathComponent("\(host)-\(digest)")
    }

    /// The default demo surface: a permissive mock and a pre-paired
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
        let server = MockServer(
            authMode: .enforcing,
            pairingCodes: ["483911": .valid],
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
