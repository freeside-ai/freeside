import Foundation

/// Keeps an `ephemeral` app's device credential beside the daemon it paired
/// with: a `0600` file in the `-FreesideReadinessDir` that daemon publishes
/// readiness to, so the pairing lasts as long as that root (plan §10). A
/// relaunch on the same root opens paired; a new or deleted root has no file
/// and pairs afresh.
///
/// Not the Keychain: a Debug build shares prod's Keychain identity, so a
/// probe could prompt for or migrate a prod item. The file sits inside the
/// daemon's own state directory, which already holds the readiness file's
/// pairing code under the same same-user trust (plan §5.2).
///
/// The file names the deployment it belongs to, and `load()` answers nil
/// for any other, so a token is never sent to a daemon that did not mint it.
/// Like the Keychain store, it fails loud: a save that silently lost the
/// credential would strand the device.
public struct ReadinessDirectoryCredentialStore: DeviceCredentialStore {
    public static let fileName = "ephemeral-app-credential.json"

    public let fileURL: URL
    private let deploymentKey: String

    public init(directory: URL, deploymentURL: URL) {
        self.fileURL = directory.appendingPathComponent(Self.fileName, isDirectory: false)
        self.deploymentKey = AppSession.deploymentKey(for: deploymentURL)
    }

    public enum StoreError: Error, Equatable {
        case malformed
        case write(POSIXErrorCode)
    }

    private struct Stored: Codable {
        let formatVersion: Int
        let deployment: String
        let deviceID: String
        let token: String
        let ntfyServerURL: String
        let ntfyTopic: String
    }

    public func load() throws -> DeviceCredential? {
        let data: Data
        do {
            data = try Data(contentsOf: fileURL)
        } catch CocoaError.fileReadNoSuchFile {
            return nil
        }
        guard
            let stored = try? JSONDecoder().decode(Stored.self, from: data),
            stored.formatVersion == 1
        else { throw StoreError.malformed }
        guard stored.deployment == deploymentKey else { return nil }
        guard
            let subscription = DeviceNtfySubscription(serverURL: stored.ntfyServerURL, topic: stored.ntfyTopic),
            let credential = DeviceCredential(
                deviceID: stored.deviceID, token: stored.token, ntfySubscription: subscription)
        else { throw StoreError.malformed }
        return credential
    }

    /// Writes a private temporary file and renames it over the old one, so a
    /// reader sees the old credential or the new one, never a torn file, and
    /// the token never sits in a file other users can read.
    public func save(_ credential: DeviceCredential) throws {
        let data = try JSONEncoder().encode(
            Stored(
                formatVersion: 1, deployment: deploymentKey, deviceID: credential.deviceID,
                token: credential.token, ntfyServerURL: credential.ntfySubscription.serverURL,
                ntfyTopic: credential.ntfySubscription.topic))
        let temporary = fileURL.deletingLastPathComponent()
            .appendingPathComponent(".\(Self.fileName).\(UUID().uuidString)", isDirectory: false)
        let descriptor = open(temporary.path, O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0o600)
        guard descriptor >= 0 else { throw Self.lastError() }
        let handle = FileHandle(fileDescriptor: descriptor, closeOnDealloc: true)
        do {
            try handle.write(contentsOf: data)
            try handle.synchronize()
            try handle.close()
            guard rename(temporary.path, fileURL.path) == 0 else { throw Self.lastError() }
        } catch {
            unlink(temporary.path)
            throw error
        }
    }

    /// Removes the file. Only a session that loaded this deployment's
    /// credential from it re-pairs, so the file it removes is that one.
    public func delete() throws {
        guard unlink(fileURL.path) == 0 || errno == ENOENT else { throw Self.lastError() }
    }

    private static func lastError() -> StoreError {
        .write(POSIXErrorCode(rawValue: errno) ?? .EIO)
    }
}
