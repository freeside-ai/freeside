import Foundation
import Security

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
/// as the account and a versioned encoding of the private grant as item data,
/// stored in the Data Protection Keychain and readable after first unlock so
/// background work can authenticate and subscribe. On macOS, a valid item in
/// the legacy file-based Keychain is copied and verified before removal. A
/// legacy token-only payload fails loud and requires re-pairing; there is no
/// safe way to reconstruct its one-time subscription.
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

    public func load() throws -> DeviceCredential? {
        if let authoritative = try loadItem(from: .dataProtection) {
            if legacyBackendEnabled {
                try delete(from: .legacy)
            }
            return authoritative.credential
        }

        guard legacyBackendEnabled else { return nil }
        guard let legacy = try loadItem(from: .legacy) else { return nil }
        try add(legacy, to: .dataProtection)
        guard let copied = try loadItem(from: .dataProtection),
            copied.credential == legacy.credential,
            copied.data == legacy.data
        else {
            throw KeychainError(status: errSecDecode)
        }
        try delete(from: .legacy)
        return copied.credential
    }

    public func save(_ credential: DeviceCredential) throws {
        // Pairing replaces the whole identity (a new pairing is a new
        // device, #64), so save is delete-then-add, not an update of token
        // bytes under an old account. On macOS, remove legacy material first:
        // a legacy ACL failure must not delete the usable authoritative item.
        let data: Data
        do {
            data = try JSONEncoder().encode(StoredCredential(credential))
        } catch {
            throw KeychainError(status: errSecParam)
        }
        let item = KeychainItem(credential: credential, data: data)
        if legacyBackendEnabled {
            try delete(from: .legacy)
        }
        try delete(from: .dataProtection)
        try add(item, to: .dataProtection)
        guard let saved = try loadItem(from: .dataProtection),
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
        attributes[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
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
        // Always explicit: on macOS a query that omits this key targets both
        // keychains (SecItemCategorizeQuery), so a "legacy" delete without it
        // also removed the authoritative Data Protection item on every load
        // (#997). `false` scopes the legacy backend to the file-based
        // Keychain alone; other platforms ignore the key.
        query[kSecUseDataProtectionKeychain as String] = backend == .dataProtection
        return query
    }
}
