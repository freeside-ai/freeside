import Foundation
import FreesideAPI

/// The client's §5.14 cursor pair plus the epoch that scopes it. A
/// partial fetch never advances the whole cache: only a bootstrap moves
/// `lastFullSnapshotRevision`, while any observed server revision may
/// move `highestObservedServerRevision`; a gap between the two means
/// the cache is not current (sync test 11), and a different epoch means
/// it is dead (sync test 8).
public struct SyncCursors: Codable, Equatable, Sendable {
    public var syncEpoch: String
    public var lastFullSnapshotRevision: Int64
    public var highestObservedServerRevision: Int64

    public init(
        syncEpoch: String,
        lastFullSnapshotRevision: Int64,
        highestObservedServerRevision: Int64
    ) {
        self.syncEpoch = syncEpoch
        self.lastFullSnapshotRevision = lastFullSnapshotRevision
        self.highestObservedServerRevision = highestObservedServerRevision
    }
}

/// What the disposable read cache holds: the cursors and the metadata
/// snapshots they scope, plus the pending-command ledger (#115) — an
/// unresolved command's retry affordance must survive a relaunch (plan
/// §5.14 sync test 4), so the ledger persists alongside the rows but
/// outlives them: cursors are absent after an epoch discard while an
/// unsettled command still needs its verbatim resend. Attachment bytes
/// are deliberately absent — the cache persists item metadata only, so
/// nothing high-sensitivity can reach disk through it; when attachment
/// rendering lands, bytes whose sensitivity_class is high_sensitivity
/// stay memory-only by default (plan §5.14). The device credential
/// never belongs here; it lives in the Keychain alone, and a
/// ClientCommand carries no token, so the ledger adds no credential.
public struct CachedState: Codable, Equatable, Sendable {
    public var cursors: SyncCursors?
    public var attentionItems: [Components.Schemas.AttentionItemSnapshot]
    /// The persisted ledger by item id; nil when absent or unreadable.
    /// The ledger is client mutation state, not readable cache, so it
    /// degrades independently: a corrupt section costs the retry
    /// affordance, never the rows and cursors around it.
    public var pendingCommands: [String: InboxStore.PendingCommandEntry]?

    public init(
        cursors: SyncCursors?,
        attentionItems: [Components.Schemas.AttentionItemSnapshot],
        pendingCommands: [String: InboxStore.PendingCommandEntry]? = nil
    ) {
        self.cursors = cursors
        self.attentionItems = attentionItems
        self.pendingCommands = pendingCommands
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        cursors = try container.decodeIfPresent(SyncCursors.self, forKey: .cursors)
        attentionItems = try container.decode(
            [Components.Schemas.AttentionItemSnapshot].self, forKey: .attentionItems)
        // The ledger section is forgiving on its own: an undecodable
        // section loads as absent without failing the whole file.
        pendingCommands = try? container.decodeIfPresent(
            [String: InboxStore.PendingCommandEntry].self, forKey: .pendingCommands)
    }
}

/// Persistence for the disposable read cache (plan §5.14: the daemon is
/// sole authority; client databases are caches a client can always
/// rebuild by bootstrapping). Loading is therefore forgiving — anything
/// unreadable is absent — while saving is best-effort: a lost save
/// costs one bootstrap, never correctness.
public protocol CacheStore: Sendable {
    func load() -> CachedState?
    func save(_ state: CachedState)
    func discard()
}

/// One atomic JSON file in the app's protected container. Rejected
/// heavier stores (SwiftData, SQLite): the whole cache is a small
/// Codable payload rebuilt wholesale on every bootstrap, and epoch
/// discard is `rm`; a database earns nothing here.
public struct DiskCacheStore: CacheStore {
    /// Bumped when the persisted shape changes incompatibly; an old
    /// file then loads as absent and the client bootstraps. 2: cursors
    /// became optional and the pending-command ledger joined (#115).
    static let format = 2

    private struct CacheFile: Codable {
        var format: Int
        var state: CachedState
    }

    private let fileURL: URL

    public init(directory: URL) {
        fileURL = directory.appendingPathComponent("cache.json")
    }

    public func load() -> CachedState? {
        guard let data = try? Data(contentsOf: fileURL),
            let file = try? Self.decoder.decode(CacheFile.self, from: data),
            file.format == Self.format
        else { return nil }
        return file.state
    }

    public func save(_ state: CachedState) {
        guard let data = try? Self.encoder.encode(CacheFile(format: Self.format, state: state))
        else { return }
        try? FileManager.default.createDirectory(
            at: fileURL.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        var options: Data.WritingOptions = [.atomic]
        #if os(iOS)
            // Cached metadata is part of the threat model (plan §5.14):
            // encrypted at rest, readable after first unlock so a
            // background refresh can still persist.
            options.insert(.completeFileProtectionUntilFirstUserAuthentication)
        #endif
        try? data.write(to: fileURL, options: options)
    }

    public func discard() {
        try? FileManager.default.removeItem(at: fileURL)
    }

    private static let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        return encoder
    }()

    private static let decoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return decoder
    }()
}

/// Keeps nothing across instances; for tests and previews.
public final class InMemoryCacheStore: CacheStore, @unchecked Sendable {
    private let lock = NSLock()
    private var state: CachedState?

    public init() {}

    public func load() -> CachedState? {
        lock.withLock { state }
    }

    public func save(_ state: CachedState) {
        lock.withLock { self.state = state }
    }

    public func discard() {
        lock.withLock { state = nil }
    }
}
