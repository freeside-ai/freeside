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
    /// When the pairing code stops redeeming, or nil for a daemon that
    /// predates `expires_at`; the reader then dates the code from the file.
    public let expiresAt: Date?

    public init(
        apiURL: URL, pairingCode: String, environment: FreesideEnvironment, runID: String,
        expiresAt: Date? = nil
    ) {
        self.apiURL = apiURL
        self.pairingCode = pairingCode
        self.environment = environment
        self.runID = runID
        self.expiresAt = expiresAt
    }

    private static let stampedKeys: Set<String> = ["api_url", "pairing_code", "environment", "run_id"]

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
        let keys = Set(fields.keys)
        // A daemon from before `expires_at` writes the four stamped keys;
        // a present expiry must be a valid RFC 3339 date.
        let expiresAt: Date?
        if keys == stampedKeys {
            expiresAt = nil
        } else if keys == stampedKeys.union(["expires_at"]),
            let raw = fields["expires_at"] as? String,
            let date = parseRFC3339(raw)
        {
            expiresAt = date
        } else {
            throw ParseError.invalidFields
        }
        guard
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
            apiURL: apiURL, pairingCode: pairingCode, environment: environment, runID: runID,
            expiresAt: expiresAt)
    }

    /// RFC 3339 with or without fractional seconds; Go's encoder writes the
    /// fraction only when it is non-zero.
    private static func parseRFC3339(_ raw: String) -> Date? {
        let whole = ISO8601DateFormatter()
        whole.formatOptions = [.withInternetDateTime]
        if let date = whole.date(from: raw) { return date }
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return fractional.date(from: raw)
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
    /// A file for this environment whose pairing code has expired. Its URL
    /// still names the daemon, which an `ephemeral` app that already holds
    /// a credential for it follows (`AppSession.launchMode`); every other
    /// reader treats it as absent.
    case expired(DaemonReadiness)

    /// The readiness to follow, or nil when the file is absent, refused, or
    /// its code has expired.
    public var readiness: DaemonReadiness? {
        if case .ready(let readiness) = self { readiness } else { nil }
    }
}

public struct DaemonReadinessReader: Sendable {
    public static let fileName = "readiness.json"
    // Keep this aligned with daemon/internal/signet/pairing.go. It dates a
    // file with no `expires_at`: the daemon mints immediately before atomic
    // publication, so the file timestamp is the app-visible start of this
    // one-shot code's lifetime.
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
    /// only a matching file ages into `.expired`. The lifetime ends at the
    /// file's `expires_at` when it has one, and otherwise
    /// `pairingCodeLifetime` after the file was written.
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
        let now = now()
        let live: Bool
        if let expiresAt = readiness.expiresAt {
            live = now < expiresAt
        } else {
            let age = now.timeIntervalSince(publishedAt)
            live = age >= 0 && age < Self.pairingCodeLifetime
        }
        return live ? .ready(readiness) : .expired(readiness)
    }

    /// The readiness file a daemon publishes in its `-state-dir`: the
    /// supervised tier's derived directory, or an `ephemeral` run's
    /// `-FreesideReadinessDir`.
    public static func fileURL(inStateDirectory directory: URL) -> URL {
        directory.appendingPathComponent(fileName, isDirectory: false)
    }
}
