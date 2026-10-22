import Foundation

/// The private ntfy read capability minted for one paired device. Validation
/// mirrors the daemon boundary so a malformed grant never becomes durable
/// client authority.
public struct DeviceNtfySubscription: Equatable, Sendable {
    public let serverURL: String
    public let topic: String

    public init?(serverURL: String, topic: String) {
        guard let components = URLComponents(string: serverURL),
            let scheme = components.scheme?.lowercased(),
            let host = components.host?.lowercased(),
            !host.isEmpty,
            components.user == nil,
            components.password == nil,
            components.query == nil,
            components.fragment == nil,
            Self.hasValidAuthority(components, raw: serverURL),
            scheme == "https" || (scheme == "http" && Self.isLoopback(host)),
            Self.isValidTopic(topic)
        else { return nil }
        self.serverURL = serverURL
        self.topic = topic
    }

    /// Whether the server is ntfy's own hosted one. The ntfy app subscribes
    /// there unless told otherwise, so setup then needs only the topic.
    public var usesHostedServer: Bool {
        guard let components = URLComponents(string: serverURL) else { return false }
        return components.scheme?.lowercased() == "https" && components.host?.lowercased() == "ntfy.sh"
            && (components.port ?? 443) == 443 && (components.path.isEmpty || components.path == "/")
    }

    /// Valid non-secret fixture material for previews and tests.
    // swift-format-ignore: NeverForceUnwrap
    public static let mock = DeviceNtfySubscription(
        serverURL: "https://ntfy.example",
        topic: "fs-00000000000000000000000000000000"
    )!

    private static func hasValidAuthority(_ components: URLComponents, raw: String) -> Bool {
        let port = components.port
        if let port, !(1...65535).contains(port) { return false }
        guard let schemeEnd = raw.range(of: "://")?.upperBound else { return false }
        let remainder = raw[schemeEnd...]
        let authority = remainder.prefix { !"/?#".contains($0) }
        if authority.hasPrefix("[") {
            guard let bracket = authority.lastIndex(of: "]") else { return false }
            let suffix = authority[authority.index(after: bracket)...]
            guard matchesPortSuffix(suffix, port: port) else { return false }
            let rawLiteral = authority[authority.index(after: authority.startIndex)..<bracket]
            let zoneMarker = rawLiteral.range(of: "%25")
            let rawAddress = zoneMarker.map { rawLiteral[..<$0.lowerBound] } ?? rawLiteral[...]
            guard !containsPercentEncodedASCII(Substring(rawAddress)),
                zoneMarker.map({ isValidZone(rawLiteral[$0.upperBound...]) }) ?? true,
                let decodedHost = components.host,
                decodedHost.hasPrefix("["), decodedHost.hasSuffix("]")
            else { return false }
            let decodedLiteral = decodedHost.dropFirst().dropLast()
            let parts = decodedLiteral.split(
                separator: "%", maxSplits: 1, omittingEmptySubsequences: false)
            guard let address = parts.first, ipv6Bytes(String(address)) != nil else {
                return false
            }
            if zoneMarker == nil {
                guard parts.count == 1 else { return false }
            } else {
                guard parts.count == 2, !parts[1].isEmpty else { return false }
            }
            return true
        }
        let rawHost: Substring
        if let port {
            guard let colon = authority.lastIndex(of: ":"),
                matchesPortSuffix(authority[colon...], port: port)
            else { return false }
            rawHost = authority[..<colon]
        } else {
            guard !authority.contains(":") else { return false }
            rawHost = authority
        }
        return !containsPercentEncodedASCII(rawHost)
            && components.host?.contains(":") == false
    }

    private static func matchesPortSuffix(_ suffix: Substring, port: Int?) -> Bool {
        guard let port else { return suffix.isEmpty }
        guard suffix.first == ":" else { return false }
        let digits = suffix.dropFirst()
        return !digits.isEmpty
            && digits.utf8.allSatisfy({ (48...57).contains($0) })
            && Int(digits) == port
    }

    private static func containsPercentEncodedASCII(_ authority: Substring) -> Bool {
        let bytes = Array(authority.utf8)
        var index = 0
        while index < bytes.count {
            guard bytes[index] == 37 else {
                index += 1
                continue
            }
            guard index + 2 < bytes.count,
                let high = hexValue(bytes[index + 1]),
                let low = hexValue(bytes[index + 2])
            else { return true }
            if high * 16 + low < 128 { return true }
            index += 3
        }
        return false
    }

    private static func isValidZone(_ zone: Substring) -> Bool {
        guard !zone.isEmpty else { return false }
        let bytes = Array(zone.utf8)
        var index = 0
        while index < bytes.count {
            let byte = bytes[index]
            guard byte == 37 else {
                if byte < 128 && !isHostByte(byte) { return false }
                index += 1
                continue
            }
            guard index + 2 < bytes.count,
                let high = hexValue(bytes[index + 1]),
                let low = hexValue(bytes[index + 2])
            else { return false }
            let value = high * 16 + low
            guard value == 37 || value == 32 || isHostByte(value) else { return false }
            index += 3
        }
        return true
    }

    private static func isHostByte(_ byte: UInt8) -> Bool {
        (48...57).contains(byte) || (65...90).contains(byte) || (97...122).contains(byte)
            || [
                33, 34, 36, 38, 39, 40, 41, 42, 43, 44, 45, 46, 58, 59, 60, 61, 62,
                91, 93, 95, 126,
            ].contains(byte)
    }

    private static func hexValue(_ byte: UInt8) -> UInt8? {
        switch byte {
        case 48...57: byte - 48
        case 65...70: byte - 55
        case 97...102: byte - 87
        default: nil
        }
    }

    private static func isLoopback(_ host: String) -> Bool {
        if host == "localhost" { return true }
        let literal =
            host.hasPrefix("[") && host.hasSuffix("]")
            ? String(host.dropFirst().dropLast())
            : host
        guard !literal.contains("%") else { return false }
        if let bytes = ipv6Bytes(literal) {
            if bytes == Array(repeating: 0, count: 15) + [1] { return true }
            guard bytes.prefix(10).allSatisfy({ $0 == 0 }),
                bytes[10] == 0xff,
                bytes[11] == 0xff,
                bytes[12] == 127
            else { return false }
            guard let colon = literal.lastIndex(of: ":"), literal.contains(".")
            else { return true }
            return isCanonicalIPv4(String(literal[literal.index(after: colon)...]))
        }
        return isCanonicalIPv4(literal) && literal.hasPrefix("127.")
    }

    /// The 16 network-order bytes of an IPv6 literal, or nil when the text
    /// is not one. Callers strip any zone first: Darwin's inet_pton accepts
    /// `%zone` and folds it into the address bytes. POSIX inet_pton keeps
    /// the parse free of the Apple-only Network framework.
    private static func ipv6Bytes(_ literal: String) -> [UInt8]? {
        var address = in6_addr()
        guard inet_pton(AF_INET6, literal, &address) == 1 else { return nil }
        return withUnsafeBytes(of: address) { Array($0) }
    }

    private static func isCanonicalIPv4(_ host: String) -> Bool {
        let octets = host.split(separator: ".", omittingEmptySubsequences: false)
        return octets.count == 4
            && octets.allSatisfy { octet in
                guard !octet.isEmpty,
                    octet.utf8.allSatisfy({ (48...57).contains($0) }),
                    octet.count == 1 || octet.first != "0"
                else { return false }
                guard let value = Int(octet) else { return false }
                return (0...255).contains(value)
            }
    }

    private static func isValidTopic(_ topic: String) -> Bool {
        guard topic.hasPrefix("fs-") else { return false }
        let suffix = topic.dropFirst(3).utf8
        return suffix.count == 32
            && suffix.allSatisfy { byte in
                (48...57).contains(byte) || (97...102).contains(byte)
            }
    }
}

/// The paired device's private grant material, minted once by the pairing
/// exchange. Both the bearer token and ntfy subscription appear only there,
/// so custody moves into one Keychain record before the app presents as paired.
public struct DeviceCredential: Equatable, Sendable {
    public let deviceID: String
    public let token: String
    public let ntfySubscription: DeviceNtfySubscription

    public init?(
        deviceID: String,
        token: String,
        ntfySubscription: DeviceNtfySubscription
    ) {
        guard Self.token(token, belongsTo: deviceID) else { return nil }
        self.deviceID = deviceID
        self.token = token
        self.ntfySubscription = ntfySubscription
    }

    private static func token(_ token: String, belongsTo deviceID: String) -> Bool {
        let segments = token.split(separator: ".", omittingEmptySubsequences: false)
        guard segments.count == 3,
            segments[0] == "fsd1",
            !deviceID.isEmpty,
            let decodedDeviceID = decodeBase64URL(segments[1]),
            let secret = decodeBase64URL(segments[2]),
            secret.count == 32,
            String(data: decodedDeviceID, encoding: .utf8) == deviceID
        else { return false }

        return true
    }

    private static func decodeBase64URL(_ segment: Substring) -> Data? {
        guard !segment.isEmpty,
            segment.utf8.allSatisfy({ byte in
                (48...57).contains(byte) || (65...90).contains(byte)
                    || (97...122).contains(byte) || byte == 45 || byte == 95
            })
        else { return nil }

        let encoded = String(segment)
        var padded =
            encoded
            .replacingOccurrences(of: "-", with: "+")
            .replacingOccurrences(of: "_", with: "/")
        padded += String(repeating: "=", count: (4 - padded.count % 4) % 4)
        guard let decoded = Data(base64Encoded: padded) else { return nil }

        let canonical = decoded.base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
        return canonical == encoded ? decoded : nil
    }
}

/// Custody of the private pairing grant — and of nothing else. The bearer
/// token and ntfy capability stay together in Keychain and never enter the
/// disposable disk cache. Unlike that cache, credential operations fail loud:
/// a save that silently lost either value would strand the device.
public protocol DeviceCredentialStore: Sendable {
    func load() throws -> DeviceCredential?
    func save(_ credential: DeviceCredential) throws
    func delete() throws
}

/// Keeps the credential in memory only; for tests and previews.
public final class InMemoryCredentialStore: DeviceCredentialStore, @unchecked Sendable {
    private let lock = NSLock()
    private var credential: DeviceCredential?

    public init(credential: DeviceCredential? = nil) {
        self.credential = credential
    }

    public func load() throws -> DeviceCredential? {
        lock.withLock { credential }
    }

    public func save(_ credential: DeviceCredential) throws {
        lock.withLock { self.credential = credential }
    }

    public func delete() throws {
        lock.withLock { credential = nil }
    }
}
