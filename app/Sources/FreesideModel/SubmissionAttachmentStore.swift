import Foundation
import FreesideAPI

/// The bytes of attachments a saved, unconfirmed task submission names, by
/// digest, so Retry can upload them again after a lost response or a
/// relaunch (#2590). It holds nothing else: the coordinator deletes bytes as
/// soon as no saved submission names them, so attachments are not cached
/// long-term (plan §5.14).
public protocol SubmissionAttachmentStore: Sendable {
    func save(digest: String, bytes: Data) throws
    func load(digest: String) -> Data?
    func delete(digest: String)
    func storedDigests() -> Set<String>
}

/// One file per digest under `submission-attachments/` in a deployment's
/// cache directory, beside `cache.json`, written atomically. Protected on
/// iOS like the cache and excluded from backup.
public struct DiskSubmissionAttachmentStore: SubmissionAttachmentStore {
    struct MalformedDigest: Error {}

    private let directory: URL

    public init(directory: URL) {
        self.directory = directory.appendingPathComponent("submission-attachments", isDirectory: true)
    }

    public func save(digest: String, bytes: Data) throws {
        guard let url = fileURL(digest) else { throw MalformedDigest() }
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        #if canImport(Darwin)
            var values = URLResourceValues()
            values.isExcludedFromBackup = true
            var excluded = directory
            try? excluded.setResourceValues(values)
        #endif
        var options: Data.WritingOptions = [.atomic]
        #if os(iOS)
            options.insert(.completeFileProtectionUntilFirstUserAuthentication)
        #endif
        try bytes.write(to: url, options: options)
    }

    public func load(digest: String) -> Data? {
        fileURL(digest).flatMap { try? Data(contentsOf: $0) }
    }

    public func delete(digest: String) {
        guard let url = fileURL(digest) else { return }
        try? FileManager.default.removeItem(at: url)
    }

    public func storedDigests() -> Set<String> {
        let names = (try? FileManager.default.contentsOfDirectory(atPath: directory.path)) ?? []
        return Set(names.map { "sha256:" + $0 }.filter(TaskAttachmentLimits.validDigest))
    }

    /// The file for a well-formed digest; nil otherwise, so no digest can
    /// name a path outside the directory.
    private func fileURL(_ digest: String) -> URL? {
        guard TaskAttachmentLimits.validDigest(digest) else { return nil }
        return directory.appendingPathComponent(String(digest.dropFirst("sha256:".count)))
    }
}

/// Keeps nothing across instances; for mock, ephemeral, and test sessions.
public final class InMemorySubmissionAttachmentStore: SubmissionAttachmentStore, @unchecked Sendable {
    private let lock = NSLock()
    private var bytesByDigest: [String: Data] = [:]

    public init() {}

    public func save(digest: String, bytes: Data) throws {
        lock.withLock { bytesByDigest[digest] = bytes }
    }

    public func load(digest: String) -> Data? {
        lock.withLock { bytesByDigest[digest] }
    }

    public func delete(digest: String) {
        lock.withLock { _ = bytesByDigest.removeValue(forKey: digest) }
    }

    public func storedDigests() -> Set<String> {
        lock.withLock { Set(bytesByDigest.keys) }
    }
}
