import Foundation

/// The daemon's same-user startup handoff (plan §5.2). Parsing is strict so
/// a malformed or stale file never redirects credentials to an unintended
/// endpoint; the reader deliberately turns every file-system or parse failure
/// into absence because a daemon that has not started yet is an ordinary state.
/// The one exception is the environment stamp (#1504): a file stamped for
/// another tier, or with no stamp, is a refusal the reader reports apart from
/// absence, so an app never silently follows another tier's daemon.
public struct DaemonReadiness: Equatable, Sendable {
    public enum ParseError: Error, Equatable {
        case malformedDocument
        case invalidFields
        case invalidAPIURL
        /// Exactly the pre-stamp keys: a daemon older than the stamp.
        case missingStamp
    }

    public let apiURL: URL
    public let pairingCode: String
    public let environment: FreesideEnvironment
    /// Names one daemon process start, not a workflow run. The app requires
    /// it but compares nothing against it; pairing with this start's one-time
    /// code already ties the app to the run that wrote the file.
    public let runID: String

    public init(apiURL: URL, pairingCode: String, environment: FreesideEnvironment, runID: String) {
        self.apiURL = apiURL
        self.pairingCode = pairingCode
        self.environment = environment
        self.runID = runID
    }

    public static func parse(_ data: Data) throws -> DaemonReadiness {
        guard
            let object = try? JSONSerialization.jsonObject(with: data),
            let fields = object as? [String: Any]
        else {
            throw ParseError.malformedDocument
        }
        if Set(fields.keys) == ["api_url", "pairing_code"] {
            throw ParseError.missingStamp
        }
        guard
            Set(fields.keys) == ["api_url", "pairing_code", "environment", "run_id"],
            let apiURLRaw = fields["api_url"] as? String,
            let pairingCode = fields["pairing_code"] as? String,
            !pairingCode.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
            let environment = (fields["environment"] as? String).flatMap(FreesideEnvironment.init(rawValue:)),
            let runID = fields["run_id"] as? String,
            !runID.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        else {
            throw ParseError.invalidFields
        }
        guard
            let components = URLComponents(string: apiURLRaw),
            components.scheme?.lowercased() == "http",
            components.host == "127.0.0.1" || components.host == "::1",
            let port = components.port, (1...65_535).contains(port),
            components.user == nil,
            components.password == nil,
            components.query == nil,
            components.fragment == nil,
            components.path.isEmpty || components.path == "/",
            let apiURL = components.url
        else {
            throw ParseError.invalidAPIURL
        }
        return DaemonReadiness(
            apiURL: apiURL, pairingCode: pairingCode, environment: environment, runID: runID)
    }
}

/// Why the app refuses to follow a readiness file. Shown on the connect
/// screen, where typing an address stays the operator's explicit override.
public enum DaemonReadinessRefusal: Equatable, Sendable, CustomStringConvertible {
    case environmentMismatch(app: FreesideEnvironment, daemon: FreesideEnvironment)
    case daemonTooOld(app: FreesideEnvironment)

    public var description: String {
        switch self {
        case .environmentMismatch(let app, let daemon):
            "This \(app.rawValue) app found a readiness file from a \(daemon.rawValue) daemon and will not connect to it."
        case .daemonTooOld(let app):
            "This \(app.rawValue) app found a readiness file with no environment stamp. Upgrade freesided (re-run the installer with --daemon-path) and restart it."
        }
    }
}

/// A readiness read for one app environment. A refusal is reported apart from
/// absence so the app can say why it will not follow the file.
public enum DaemonReadinessOutcome: Equatable, Sendable {
    case absent
    case refused(DaemonReadinessRefusal)
    case ready(DaemonReadiness)

    /// The readiness to follow, or nil when the file is absent or refused.
    public var readiness: DaemonReadiness? {
        if case .ready(let readiness) = self { readiness } else { nil }
    }
}

public struct DaemonReadinessReader: Sendable {
    public static let fileName = "readiness.json"
    // Keep this aligned with daemon/internal/signet/pairing.go. The daemon
    // mints immediately before atomic publication; the file timestamp is
    // therefore the app-visible start of this one-shot code's lifetime.
    public static let pairingCodeLifetime: TimeInterval = 10 * 60

    private let now: @Sendable () -> Date
    private let modificationDate: @Sendable (URL) -> Date?

    public init(
        now: @escaping @Sendable () -> Date = Date.init,
        modificationDate: @escaping @Sendable (URL) -> Date? = { url in
            try? url.resourceValues(forKeys: [.contentModificationDateKey])
                .contentModificationDate
        }
    ) {
        self.now = now
        self.modificationDate = modificationDate
    }

    /// Reads the file as `environment`'s app. The stamp is checked before the
    /// pairing-code lifetime, so a refusal persists after the code expires;
    /// only a matching file ages into absence.
    public func read(at url: URL, expecting environment: FreesideEnvironment) -> DaemonReadinessOutcome {
        guard
            let publishedAt = modificationDate(url),
            let data = try? Data(contentsOf: url, options: .mappedIfSafe)
        else { return .absent }
        let readiness: DaemonReadiness
        do {
            readiness = try DaemonReadiness.parse(data)
        } catch DaemonReadiness.ParseError.missingStamp {
            return .refused(.daemonTooOld(app: environment))
        } catch {
            return .absent
        }
        guard readiness.environment == environment else {
            return .refused(.environmentMismatch(app: environment, daemon: readiness.environment))
        }
        let age = now().timeIntervalSince(publishedAt)
        guard age >= 0, age < Self.pairingCodeLifetime else { return .absent }
        return .ready(readiness)
    }

    /// The readiness file a daemon publishes in its `-state-dir`: the
    /// supervised tier's derived directory, or an `ephemeral` run's
    /// `-FreesideReadinessDir`.
    public static func fileURL(inStateDirectory directory: URL) -> URL {
        directory.appendingPathComponent(fileName, isDirectory: false)
    }
}
