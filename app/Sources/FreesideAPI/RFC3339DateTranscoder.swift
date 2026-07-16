import Foundation
import OpenAPIRuntime

/// Decodes any RFC 3339 `date-time` the contract permits; the runtime's
/// stock transcoders each accept exactly one shape (`.iso8601` rejects
/// fractional seconds, `.iso8601WithFractionalSeconds` requires them),
/// and the daemon legitimately emits sub-second timestamps while the
/// in-process mock emits whole seconds. Encoding stays canonical
/// whole-second UTC: the client never needs to claim more precision
/// than that, and one output shape keeps request bodies deterministic.
public struct RFC3339DateTranscoder: DateTranscoder {
    private static let whole: ISO8601DateTranscoder = .iso8601
    private static let fractional: ISO8601DateTranscoder = .iso8601WithFractionalSeconds

    public init() {}

    public func encode(_ date: Date) throws -> String {
        try Self.whole.encode(date)
    }

    public func decode(_ dateString: String) throws -> Date {
        if let date = try? Self.fractional.decode(dateString) {
            return date
        }
        return try Self.whole.decode(dateString)
    }
}

extension DateTranscoder where Self == RFC3339DateTranscoder {
    /// The transcoder every Freeside client uses (see APIClientFactory).
    public static var rfc3339: Self { RFC3339DateTranscoder() }
}
