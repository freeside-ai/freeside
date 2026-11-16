import Foundation

/// A one-time pairing code the daemon minted for a caller on its own host
/// (plan §5.14 devices). It is shown to the operator and never stored: the
/// app keeps it in memory until the screen showing it closes or a newer
/// code replaces it. `description` leaves the code out, so interpolating
/// one into a log line or an error leaks nothing.
public struct MintedPairingCode: Equatable, Sendable, CustomStringConvertible,
    CustomDebugStringConvertible
{
    public let code: String
    public let expiresAt: Date
    /// The daemon's own listen address, as it reported it. On a supervised
    /// tier this is loopback; `PhoneAddress` says what a phone should enter.
    public let apiURL: URL

    public init(code: String, expiresAt: Date, apiURL: URL) {
        self.code = code
        self.expiresAt = expiresAt
        self.apiURL = apiURL
    }

    public var description: String {
        "MintedPairingCode(expiresAt: \(expiresAt.formatted(.iso8601)), apiURL: \(apiURL.absoluteString))"
    }

    public var debugDescription: String { description }

    /// Reads the one line `freesided pairing-code` prints on success
    /// (`pairingCodeResult` in `daemon/cmd/freesided/pairing_control.go`):
    /// exactly `api_url`, `pairing_code`, and `expires_at`. Parsing is strict,
    /// as `DaemonReadiness.parse` is, so a changed CLI shows up as unreadable
    /// output rather than a code with a wrong address or expiry.
    public static func parse(_ data: Data) -> MintedPairingCode? {
        guard
            let object = try? JSONSerialization.jsonObject(with: data),
            let fields = object as? [String: Any],
            Set(fields.keys) == ["api_url", "pairing_code", "expires_at"],
            let code = fields["pairing_code"] as? String,
            !code.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
            let expiresAt = (fields["expires_at"] as? String).flatMap(parseInstant),
            let apiURL = (fields["api_url"] as? String).flatMap(parseAPIURL)
        else { return nil }
        return MintedPairingCode(code: code, expiresAt: expiresAt, apiURL: apiURL)
    }

    /// RFC 3339 as Go's `time.Time` encodes it: fractional seconds when the
    /// instant has any, none otherwise. Go's zero time, and anything at or
    /// before the Unix epoch, is not an expiry the daemon would mint.
    private static func parseInstant(_ value: String) -> Date? {
        let date =
            (try? Date.ISO8601FormatStyle(includingFractionalSeconds: true).parse(value))
            ?? (try? Date.ISO8601FormatStyle().parse(value))
        guard let date, date.timeIntervalSince1970 > 0 else { return nil }
        return date
    }

    /// An `http` origin with an explicit port: the shape the daemon's
    /// listen address always has.
    private static func parseAPIURL(_ value: String) -> URL? {
        guard
            let components = URLComponents(string: value),
            components.scheme?.lowercased() == "http",
            let host = components.host, !host.isEmpty,
            let port = components.port, (1...65_535).contains(port),
            components.user == nil, components.password == nil,
            components.query == nil, components.fragment == nil,
            components.path.isEmpty || components.path == "/"
        else { return nil }
        return components.url
    }
}

/// Why no code came back. Each message is fixed text: none carries the
/// CLI's output, which on success holds the code.
public enum PairingCodeFailure: Error, Equatable, Sendable {
    /// The bundled `freesided` is missing or would not launch, as in an
    /// Xcode build, which bundles none.
    case unavailable
    /// `freesided pairing-code` exited non-zero: the daemon isn't running,
    /// refused, or couldn't mint.
    case refused(exitStatus: Int32)
    case timedOut
    /// It exited zero but printed something other than one code.
    case unreadableOutput

    public var message: String {
        switch self {
        case .unavailable:
            "This app can't find its bundled freesided, so it can't ask the daemon for a code."
        case .refused:
            "The daemon didn't give a code. Check that it's running, then try again."
        case .timedOut:
            "The daemon took too long to give a code."
        case .unreadableOutput:
            "The daemon's answer couldn't be read. This app and the daemon may be different versions."
        }
    }
}

/// Mints a pairing code on this device's own daemon host. Only the Mac has
/// one (`BundledPairingCodeSource`); the iPhone never mints.
public protocol PairingCodeSource: Sendable {
    func mint() async throws(PairingCodeFailure) -> MintedPairingCode
}
