import FreesideAPI
import Observation

/// The single client-side source of truth for attention item snapshots:
/// the inbox list and every decision card read the same table, so a
/// replacement swap or a revalidation refetch can never leave the two
/// rendering different states. SyncCoordinator drives cache persistence
/// and the §5.14 cursor semantics over this table.
@MainActor
@Observable
public final class InboxStore {
    public enum LoadState: Equatable {
        case idle
        case loading
        case loaded
        case failed(String)
    }

    /// What the UI may claim about the cached view (plan §5.14: cached
    /// read-only view with a freshness banner while unreachable).
    /// Written by the sync coordinator; kept on the store because every
    /// view and model already reads shared client state here.
    public enum Freshness: Equatable, Sendable {
        /// No sync round-trip has settled it yet (launching from cache,
        /// or no coordinator in play): per-item validation decides.
        case unvalidated
        /// The last sync round-trip succeeded; the cache is current.
        case fresh
        /// The daemon is unreachable: cached read-only view.
        case unreachable
        /// The daemon answered 401: this device's credential no longer
        /// authenticates (revoked, or not yet paired).
        case unauthenticated
    }

    public let client: any APIProtocol
    public let device: DeviceIdentity
    /// The card-shared attachment loader over the same client: digests
    /// are content-addressed, so one memory-only table serves every
    /// card instance (plan §5.14 keeps attachment bytes out of the
    /// disk cache).
    public let attachments: AttachmentLoader
    public private(set) var loadState: LoadState = .idle
    public internal(set) var freshness: Freshness = .unvalidated
    /// Reports every canonical `as_of_revision` this store ingests, so
    /// the sync coordinator can advance its observed cursor; a partial
    /// read must never advance the full-snapshot cursor (plan §5.14
    /// sync test 11).
    public var revisionObserver: ((Int64) -> Void)?
    /// Reports every pending-command ledger mutation so the sync
    /// coordinator can persist the ledger as it changes (#115): the
    /// retry affordance survives a relaunch only if each claim, state
    /// move, and release reaches disk when it happens, not at the next
    /// sync round. Returns whether the write reached disk, so a claim can
    /// gate the first send on durability (#163); the post-send state
    /// moves and releases ignore it (their loss only offers a harmless,
    /// idempotent verbatim resend on relaunch).
    public var pendingCommandsObserver: (() -> Bool)?
    public private(set) var snapshotsByID: [String: Components.Schemas.AttentionItemSnapshot] = [:]
    /// A pending command's shared lifecycle: in flight while an attempt
    /// awaits its response (no retry affordance — the request may still
    /// succeed), unresolved once an attempt failed ambiguously (only a
    /// verbatim resend settles it).
    public nonisolated enum PendingCommandState: String, Codable, Equatable, Sendable {
        case inFlight
        case unresolved
    }

    /// One pending entry: the preserved command and where it stands.
    /// Codable because the ledger persists in the disk cache (#115): an
    /// unresolved command's retry affordance must survive a relaunch. A
    /// ClientCommand carries no credential — the token lives in the
    /// Keychain and is attached per-request by the auth middleware — so
    /// persisting the entry adds nothing secret to disk.
    public nonisolated struct PendingCommandEntry: Codable, Equatable, Sendable {
        public let command: Components.Schemas.ClientCommand
        public var state: PendingCommandState

        public init(command: Components.Schemas.ClientCommand, state: PendingCommandState) {
            self.command = command
            self.state = state
        }
    }

    /// Each item's single in-flight or unresolved command. Store-owned
    /// so it survives card navigation and re-created models: the slot is
    /// claimed before a submission's first request leaves the model, and
    /// while an entry exists no new command may be minted for the item —
    /// an in-flight command can still commit after any refetch. A
    /// definitive outcome (200, 409, authoritative 4xx) releases the
    /// slot; a transport loss or 5xx marks it unresolved until a
    /// verbatim resend returns the recorded result or an authoritative
    /// rejection (plan §5.14 sync test 4).
    public private(set) var pendingCommandsByItemID:
        [String: PendingCommandEntry] = [:]
    private var serverOrder: [String] = []
    /// Bumped every time the cache is evicted for a sync-epoch change
    /// (`discardSnapshots`, driven only by `SyncCoordinator.discardCache`).
    /// A per-item validation stamps the generation it certified against,
    /// so a validation from a dead epoch cannot certify the rows a later
    /// bootstrap repopulates (issue #162; plan §5.14 cache eviction on
    /// epoch change). A same-epoch gap bootstrap uses `replaceAll` without
    /// a discard, so it deliberately does not bump.
    public private(set) var cacheGeneration = 0
    /// Overlapping refreshes resolve by recency: only the newest call
    /// may write the load state and rebuild the order, so a stale late
    /// completion cannot clobber a newer one in either direction.
    private var refreshGeneration = 0

    public init(client: any APIProtocol, device: DeviceIdentity = .mock) {
        self.client = client
        self.device = device
        attachments = AttachmentLoader(client: client)
    }

    /// The inbox rows: open items first, urgent-to-low within a status,
    /// server order as the stable tiebreak.
    public var rows: [Components.Schemas.AttentionItemSnapshot] {
        let ordered = serverOrder.enumerated().compactMap {
            index, id in snapshotsByID[id].map { (index, $0) }
        }
        return ordered.sorted { lhs, rhs in
            let (lhsKey, rhsKey) = (sortKey(lhs.1, index: lhs.0), sortKey(rhs.1, index: rhs.0))
            return lhsKey < rhsKey
        }.map(\.1)
    }

    /// Rebuilds the inbox from the canonical list (plan §5.14 sync test 3:
    /// a foreground refresh reconstructs the inbox with no notifications).
    public func refresh() async {
        refreshGeneration += 1
        let generation = refreshGeneration
        loadState = .loading
        do {
            let snapshots = try await client.listAttentionItems(.init()).ok.body.json
            // Canonical data always applies (per-item monotonicity); the
            // order rewrite and load state belong to the newest call.
            for snapshot in snapshots {
                apply(snapshot)
            }
            guard generation == refreshGeneration else { return }
            // The listed ids lead, but ids only this store knows stay:
            // overlapping refreshes can return out of order, and an older
            // list must never hide a newer snapshot from the rows.
            let listed = snapshots.map(\.item.id)
            serverOrder = listed + serverOrder.filter { !listed.contains($0) }
            loadState = .loaded
        } catch {
            guard generation == refreshGeneration else { return }
            loadState = .failed(String(describing: error))
        }
    }

    /// Upserts a canonical snapshot from any read or rejection: a detail
    /// refetch, or the replacement item a stale submission returned.
    /// Per-resource version monotonicity: concurrent reads can complete
    /// out of order, and an older snapshot must never downgrade newer
    /// state the cards gate their actions on.
    ///
    /// Returns whether `snapshot` is now the rendered row: `false` when a
    /// cached higher `entity_version` outranked it and the write was
    /// refused. A certifying caller must not mark a rejected snapshot
    /// validated — `entity_version` is monotonic only within a sync
    /// epoch, so across a restore the shadowing higher version is a dead
    /// pre-restore row, not newer state (issue #162). The snapshot itself
    /// carries no epoch, so the rejection is the only local signal.
    @discardableResult
    public func apply(_ snapshot: Components.Schemas.AttentionItemSnapshot) -> Bool {
        if let existing = snapshotsByID[snapshot.item.id],
            existing.entity_version > snapshot.entity_version
        {
            return false
        }
        snapshotsByID[snapshot.item.id] = snapshot
        if !serverOrder.contains(snapshot.item.id) {
            serverOrder.append(snapshot.item.id)
        }
        revisionObserver?(snapshot.as_of_revision)
        return true
    }

    /// Ingests a bootstrap or the persisted cache: the canonical full
    /// snapshot replaces rows and order wholesale (per-item version
    /// monotonicity still holds against a racing partial read), while
    /// the pending-command ledger survives — it is client mutation
    /// state, not readable cache, and an in-flight command can still
    /// commit whatever the read side does.
    public func replaceAll(with snapshots: [Components.Schemas.AttentionItemSnapshot]) {
        var replaced: [String: Components.Schemas.AttentionItemSnapshot] = [:]
        for snapshot in snapshots {
            if let existing = snapshotsByID[snapshot.item.id],
                existing.entity_version > snapshot.entity_version
            {
                replaced[snapshot.item.id] = existing
            } else {
                replaced[snapshot.item.id] = snapshot
            }
        }
        snapshotsByID = replaced
        serverOrder = snapshots.map(\.item.id)
        loadState = .loaded
        for snapshot in snapshots {
            revisionObserver?(snapshot.as_of_revision)
        }
    }

    /// Drops every cached row (an epoch change made them meaningless,
    /// plan §5.14 sync test 8). The pending-command ledger survives:
    /// commitment is epoch-independent, and only a verbatim resend can
    /// settle an ambiguous command against the restored daemon.
    public func discardSnapshots() {
        snapshotsByID = [:]
        serverOrder = []
        loadState = .idle
        // A new epoch: every prior per-item validation is now stale, even
        // for rows a subsequent bootstrap repopulates (issue #162).
        cacheGeneration += 1
    }

    /// Rows in server order, for cache persistence.
    public var orderedSnapshots: [Components.Schemas.AttentionItemSnapshot] {
        serverOrder.compactMap { snapshotsByID[$0] }
    }

    /// The outcome of claiming an item's in-flight slot for a first send.
    /// `registered` alone clears the send: the command_id is both claimed
    /// in memory and durably recorded. `slotOccupied` means another
    /// command already holds the item (a racing card instance), and
    /// `notPersisted` means the durable write failed, so the claim was
    /// rolled back and the caller must not send (#163).
    public nonisolated enum PendingCommandRegistration: Equatable, Sendable {
        case registered
        case slotOccupied
        case notPersisted
    }

    /// Claims the item's single in-flight slot and durably records the
    /// command before the caller sends. The durable write is a
    /// precondition, not a side effect: an in-memory-only claim whose
    /// disk write is lost would let a committed command's reusable
    /// command_id vanish on relaunch, defeating the lost-response replay
    /// (plan §5.14 sync test 4, #163). If the observer reports the write
    /// failed, the just-claimed slot is rolled back and `notPersisted`
    /// returned; with no observer wired (a bare store in tests) there is
    /// no cache to gate on and the in-memory claim stands.
    public func registerPendingCommand(
        _ command: Components.Schemas.ClientCommand
    ) -> PendingCommandRegistration {
        let itemID = command.payload.item_id
        guard pendingCommandsByItemID[itemID] == nil else { return .slotOccupied }
        pendingCommandsByItemID[itemID] =
            PendingCommandEntry(command: command, state: .inFlight)
        if let observer = pendingCommandsObserver, observer() == false {
            // The write failed and left disk untouched, so dropping the
            // in-memory entry restores the pre-claim state exactly.
            pendingCommandsByItemID[itemID] = nil
            return .notPersisted
        }
        return .registered
    }

    /// Moves the slot between in-flight and unresolved, only while it
    /// still holds the named command. Best-effort persistence: this runs
    /// after the send, and a lost write only offers an idempotent
    /// verbatim resend on relaunch (#163).
    public func setPendingCommandState(
        itemID: String, commandID: String, state: PendingCommandState
    ) {
        guard pendingCommandsByItemID[itemID]?.command.command_id == commandID else { return }
        pendingCommandsByItemID[itemID]?.state = state
        _ = pendingCommandsObserver?()
    }

    /// Clears the slot only while it still holds the command that
    /// settled: a late completion from an older replay must never
    /// release a newer command's slot.
    public func clearPendingCommand(itemID: String, commandID: String) {
        guard pendingCommandsByItemID[itemID]?.command.command_id == commandID else { return }
        pendingCommandsByItemID[itemID] = nil
        _ = pendingCommandsObserver?()
    }

    /// Restores a persisted ledger at relaunch (#115). Only empty slots
    /// fill — a live entry is newer truth — and every restored entry
    /// lands unresolved: no task awaits a restored command's response,
    /// so even one persisted in flight has failed ambiguously by now,
    /// and only a verbatim resend settles it (plan §5.14 sync test 4
    /// across a restart). Entries whose item is absent from the restored
    /// rows stay, as replaceAll keeps them in-process: commitment is
    /// client mutation state, not readable cache, and the resend
    /// converges them either way. No observer fire — the ledger came
    /// from disk, so there is nothing new to persist.
    public func restorePendingCommands(_ entries: [String: PendingCommandEntry]) {
        for (itemID, entry) in entries where pendingCommandsByItemID[itemID] == nil {
            // Decoded fields are re-gated at this reconstruction
            // boundary, never trusted: an entry minted by another device
            // (a re-pair after a lost credential, same deployment cache)
            // must not occupy this device's slots — its verbatim resend
            // would die at the daemon's device gate as an authoritative
            // rejection and clear a possibly committed outcome as "not
            // recorded" — and a key naming a different item than its
            // command would block one item with another's command.
            guard entry.command.device_id == device.deviceID,
                entry.command.payload.item_id == itemID
            else { continue }
            pendingCommandsByItemID[itemID] =
                PendingCommandEntry(command: entry.command, state: .unresolved)
        }
    }

    private func sortKey(
        _ snapshot: Components.Schemas.AttentionItemSnapshot, index: Int
    ) -> (Int, Int, Int) {
        let statusRank = snapshot.item.status == .open ? 0 : 1
        let priorityRank: Int
        switch snapshot.item.priority {
        case .urgent: priorityRank = 0
        case .high: priorityRank = 1
        case .normal: priorityRank = 2
        case .low: priorityRank = 3
        }
        return (statusRank, priorityRank, index)
    }
}
