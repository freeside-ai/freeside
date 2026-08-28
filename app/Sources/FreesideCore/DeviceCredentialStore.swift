import Foundation
import Network
import Security

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
            guard let address = parts.first, IPv6Address(String(address)) != nil else {
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
        if let address = IPv6Address(literal) {
            if address == .loopback { return true }
            let bytes = Array(address.rawValue)
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

struct KeychainSecurityOperations: @unchecked Sendable {
    let copyMatching: ([String: Any]) -> (OSStatus, Any?)
    let add: ([String: Any]) -> OSStatus
    let delete: ([String: Any]) -> OSStatus

    static let live = KeychainSecurityOperations(
        copyMatching: { query in
            var result: CFTypeRef?
            let status = SecItemCopyMatching(query as CFDictionary, &result)
            return (status, result)
        },
        add: { attributes in
            SecItemAdd(attributes as CFDictionary, nil)
        },
        delete: { query in
            SecItemDelete(query as CFDictionary)
        })
}

/// The real store: one generic-password item per service name, the device id
/// as the account and a versioned encoding of the private grant as item data.
/// macOS uses the persistent file-based login Keychain; iOS uses the Data
/// Protection Keychain with after-first-unlock accessibility. Reads consult
/// only that platform's authoritative backend. A save best-effort clears a
/// stale copy from the other macOS backend before replacing the authoritative
/// item, while explicit deletion attempts both backends and reports failure.
public struct KeychainCredentialStore: DeviceCredentialStore {
    public struct KeychainError: Error {
        public let status: OSStatus
    }

    private let service: String
    private let operations: KeychainSecurityOperations
    private let legacyBackendEnabled: Bool

    private enum Backend {
        case dataProtection
        case legacy
    }

    private struct KeychainItem {
        let credential: DeviceCredential
        let data: Data
    }

    private struct StoredCredential: Codable {
        let formatVersion: Int
        let deviceID: String
        let token: String
        let ntfyServerURL: String
        let ntfyTopic: String

        init(_ credential: DeviceCredential) {
            formatVersion = 1
            deviceID = credential.deviceID
            token = credential.token
            ntfyServerURL = credential.ntfySubscription.serverURL
            ntfyTopic = credential.ntfySubscription.topic
        }
    }

    public init(service: String = "ai.freeside.device-credential") {
        self.init(service: service, operations: .live)
    }

    init(service: String, operations: KeychainSecurityOperations) {
        #if os(macOS)
            let legacyBackendEnabled = true
        #else
            let legacyBackendEnabled = false
        #endif
        self.init(
            service: service,
            operations: operations,
            legacyBackendEnabled: legacyBackendEnabled)
    }

    init(
        service: String,
        operations: KeychainSecurityOperations,
        legacyBackendEnabled: Bool
    ) {
        self.service = service
        self.operations = operations
        self.legacyBackendEnabled = legacyBackendEnabled
    }

    // The persistent backend for this platform. The Data Protection Keychain
    // is the iOS-native store, but on macOS it requires a sandbox/app-group
    // container; a non-sandboxed macOS app's DP add returns errSecSuccess yet
    // the item vanishes (errSecItemNotFound) within about a second, so the
    // credential never survives to the next request (#960 regression). The
    // file-based login keychain persists there, so macOS reads and writes it.
    private var primaryBackend: Backend {
        legacyBackendEnabled ? .legacy : .dataProtection
    }

    public func load() throws -> DeviceCredential? {
        try loadItem(from: primaryBackend)?.credential
    }

    public func save(_ credential: DeviceCredential) throws {
        // Pairing replaces the whole identity (a new pairing is a new
        // device, #64), so save is delete-then-add, not an update of token
        // bytes under an old account.
        let data: Data
        do {
            data = try JSONEncoder().encode(StoredCredential(credential))
        } catch {
            throw KeychainError(status: errSecParam)
        }
        let item = KeychainItem(credential: credential, data: data)
        // Best-effort clear any copy in the non-authoritative backend so a
        // stale credential there can never shadow the new one; its failure
        // must not block the real save into the persistent backend.
        if primaryBackend != .dataProtection {
            try? delete(from: .dataProtection)
        }
        if legacyBackendEnabled && primaryBackend != .legacy {
            try? delete(from: .legacy)
        }
        try delete(from: primaryBackend)
        try add(item, to: primaryBackend)
        guard let saved = try loadItem(from: primaryBackend),
            saved.credential == credential,
            saved.data == data
        else {
            throw KeychainError(status: errSecDecode)
        }
    }

    public func delete() throws {
        var firstError: KeychainError?
        let backends: [Backend] =
            legacyBackendEnabled ? [.dataProtection, .legacy] : [.dataProtection]
        for backend in backends {
            do {
                try delete(from: backend)
            } catch let error as KeychainError {
                if firstError == nil { firstError = error }
            }
        }
        if let firstError { throw firstError }
    }

    private func loadItem(from backend: Backend) throws -> KeychainItem? {
        var query = baseQuery(for: backend)
        query[kSecReturnAttributes as String] = true
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        let (status, result) = operations.copyMatching(query)
        switch status {
        case errSecSuccess:
            guard let item = result as? [String: Any],
                let deviceID = item[kSecAttrAccount as String] as? String,
                let data = item[kSecValueData as String] as? Data,
                let stored = try? JSONDecoder().decode(StoredCredential.self, from: data),
                stored.formatVersion == 1,
                stored.deviceID == deviceID,
                let subscription = DeviceNtfySubscription(
                    serverURL: stored.ntfyServerURL, topic: stored.ntfyTopic),
                let credential = DeviceCredential(
                    deviceID: deviceID,
                    token: stored.token,
                    ntfySubscription: subscription)
            else { throw KeychainError(status: errSecDecode) }
            return KeychainItem(credential: credential, data: data)
        case errSecItemNotFound:
            return nil
        default:
            throw KeychainError(status: status)
        }
    }

    private func add(_ item: KeychainItem, to backend: Backend) throws {
        var attributes = baseQuery(for: backend)
        attributes[kSecAttrAccount as String] = item.credential.deviceID
        attributes[kSecValueData as String] = item.data
        // kSecAttrAccessible is a Data Protection Keychain attribute; the
        // file-based backend uses an ACL (default: trust the creating app) and
        // ignores or rejects an accessibility class, so scope it to the DP
        // backend only.
        if backend == .dataProtection {
            attributes[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
        }
        let status = operations.add(attributes)
        guard status == errSecSuccess else {
            throw KeychainError(status: status)
        }
    }

    private func delete(from backend: Backend) throws {
        let status = operations.delete(baseQuery(for: backend))
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw KeychainError(status: status)
        }
    }

    private func baseQuery(for backend: Backend) -> [String: Any] {
        var query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
        ]
        if backend == .dataProtection {
            query[kSecUseDataProtectionKeychain as String] = true
        }
        return query
    }
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
