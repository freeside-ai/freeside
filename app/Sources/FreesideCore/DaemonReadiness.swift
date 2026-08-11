import Foundation

/// The daemon's same-user startup handoff (plan §5.2). Parsing is strict so
/// a malformed or stale file never redirects credentials to an unintended
/// endpoint; the reader deliberately turns every file-system or parse failure
/// into absence because a daemon that has not started yet is an ordinary state.
public struct DaemonReadiness: Equatable, Sendable {
    public enum ParseError: Error, Equatable {
        case malformedDocument
        case invalidFields
        case invalidAPIURL
    }

    public let apiURL: URL
    public let pairingCode: String

    public static func parse(_ data: Data) throws -> DaemonReadiness {
        guard
            let object = try? JSONSerialization.jsonObject(with: data),
            let fields = object as? [String: Any]
        else {
            throw ParseError.malformedDocument
        }
        guard
            Set(fields.keys) == ["api_url", "pairing_code"],
            let apiURLRaw = fields["api_url"] as? String,
            let pairingCode = fields["pairing_code"] as? String,
            !pairingCode.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
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
        return DaemonReadiness(apiURL: apiURL, pairingCode: pairingCode)
    }
}

public struct DaemonReadinessReader: Sendable {
    public static let fileName = "readiness.json"
    // Keep this aligned with daemon/internal/signet/pairing.go. The daemon
    // mints immediately before atomic publication; the file timestamp is
    // therefore the app-visible start of this one-shot code's lifetime.
    public static let pairingCodeLifetime: TimeInterval = 10 * 60
    public static var supervisedAPIURL: URL {
        guard let url = URL(string: "http://127.0.0.1:7331") else {
            preconditionFailure("the fixed supervised daemon URL is invalid")
        }
        return url
    }

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

    public func read(at url: URL) -> DaemonReadiness? {
        guard let publishedAt = modificationDate(url) else { return nil }
        let age = now().timeIntervalSince(publishedAt)
        guard age >= 0, age < Self.pairingCodeLifetime else { return nil }
        guard let data = try? Data(contentsOf: url, options: .mappedIfSafe) else { return nil }
        return try? DaemonReadiness.parse(data)
    }

    public static func stateDirectory(fileManager: FileManager = .default) -> URL? {
        fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask).first?
            .appendingPathComponent("Freeside", isDirectory: true)
            .appendingPathComponent("daemon", isDirectory: true)
    }

    public static func defaultFileURL(fileManager: FileManager = .default) -> URL? {
        stateDirectory(fileManager: fileManager)?
            .appendingPathComponent(fileName, isDirectory: false)
    }
}
