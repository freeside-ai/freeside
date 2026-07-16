import Foundation
import Security

/// The paired device's identity and bearer token, minted once by the
/// pairing exchange (the grant is the token's only appearance, ever).
public struct DeviceCredential: Equatable, Sendable {
    public let deviceID: String
    public let token: String

    public init(deviceID: String, token: String) {
        self.deviceID = deviceID
        self.token = token
    }
}

/// Custody of the device credential — and of nothing else: plan §5.14
/// puts only the device credential in the Keychain, and the credential
/// nowhere else (never the disk cache). Unlike the disposable cache,
/// credential operations fail loud: a save that silently lost the token
/// would strand the device unpaired with no signal.
public protocol DeviceCredentialStore: Sendable {
    func load() throws -> DeviceCredential?
    func save(_ credential: DeviceCredential) throws
    func delete() throws
}

/// The real store: one generic-password item per service name, the
/// device id as the account and the token as the item data, readable
/// after first unlock so a background refresh can authenticate.
public struct KeychainCredentialStore: DeviceCredentialStore {
    public struct KeychainError: Error {
        public let status: OSStatus
    }

    private let service: String

    public init(service: String = "ai.freeside.device-credential") {
        self.service = service
    }

    public func load() throws -> DeviceCredential? {
        var query = baseQuery
        query[kSecReturnAttributes as String] = true
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        switch SecItemCopyMatching(query as CFDictionary, &result) {
        case errSecSuccess:
            guard let item = result as? [String: Any],
                let deviceID = item[kSecAttrAccount as String] as? String,
                let data = item[kSecValueData as String] as? Data,
                let token = String(data: data, encoding: .utf8)
            else { throw KeychainError(status: errSecDecode) }
            return DeviceCredential(deviceID: deviceID, token: token)
        case errSecItemNotFound:
            return nil
        case let status:
            throw KeychainError(status: status)
        }
    }

    public func save(_ credential: DeviceCredential) throws {
        // Pairing replaces the whole identity (a new pairing is a new
        // device, #64), so save is delete-then-add, not an update of
        // token bytes under an old account.
        try delete()
        var attributes = baseQuery
        attributes[kSecAttrAccount as String] = credential.deviceID
        attributes[kSecValueData as String] = Data(credential.token.utf8)
        attributes[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
        let status = SecItemAdd(attributes as CFDictionary, nil)
        guard status == errSecSuccess else {
            throw KeychainError(status: status)
        }
    }

    public func delete() throws {
        let status = SecItemDelete(baseQuery as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw KeychainError(status: status)
        }
    }

    private var baseQuery: [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
        ]
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
