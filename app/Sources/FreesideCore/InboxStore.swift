import FreesideAPI
import Observation

/// The single client-side source of truth for attention item snapshots:
/// the inbox list and every decision card read the same table, so a
/// replacement swap or a revalidation refetch can never leave the two
/// rendering different states. In-memory only; cache persistence and
/// revision-gap semantics are the next unit's scope (plan §5.14).
@MainActor
@Observable
public final class InboxStore {
    public enum LoadState: Equatable {
        case idle
        case loading
        case loaded
        case failed(String)
    }

    public let client: any APIProtocol
    public let device: DeviceIdentity
    public private(set) var loadState: LoadState = .idle
    public private(set) var snapshotsByID: [String: Components.Schemas.AttentionItemSnapshot] = [:]
    /// A pending command's shared lifecycle: in flight while an attempt
    /// awaits its response (no retry affordance — the request may still
    /// succeed), unresolved once an attempt failed ambiguously (only a
    /// verbatim resend settles it).
    public enum PendingCommandState: Equatable, Sendable {
        case inFlight
        case unresolved
    }

    /// One pending entry: the preserved command and where it stands.
    public struct PendingCommandEntry: Equatable, Sendable {
        public let command: Components.Schemas.ClientCommand
        public var state: PendingCommandState
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
    /// Overlapping refreshes resolve by recency: only the newest call
    /// may write the load state and rebuild the order, so a stale late
    /// completion cannot clobber a newer one in either direction.
    private var refreshGeneration = 0

    public init(client: any APIProtocol, device: DeviceIdentity = .mock) {
        self.client = client
        self.device = device
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
    public func apply(_ snapshot: Components.Schemas.AttentionItemSnapshot) {
        if let existing = snapshotsByID[snapshot.item.id],
            existing.entity_version > snapshot.entity_version
        {
            return
        }
        snapshotsByID[snapshot.item.id] = snapshot
        if !serverOrder.contains(snapshot.item.id) {
            serverOrder.append(snapshot.item.id)
        }
    }

    /// Claims the item's single in-flight slot; false when another
    /// command already occupies it, so a racing card instance can never
    /// replace or duplicate an unresolved submission.
    public func registerPendingCommand(
        _ command: Components.Schemas.ClientCommand
    ) -> Bool {
        guard pendingCommandsByItemID[command.payload.item_id] == nil else { return false }
        pendingCommandsByItemID[command.payload.item_id] =
            PendingCommandEntry(command: command, state: .inFlight)
        return true
    }

    /// Moves the slot between in-flight and unresolved, only while it
    /// still holds the named command.
    public func setPendingCommandState(
        itemID: String, commandID: String, state: PendingCommandState
    ) {
        guard pendingCommandsByItemID[itemID]?.command.command_id == commandID else { return }
        pendingCommandsByItemID[itemID]?.state = state
    }

    /// Clears the slot only while it still holds the command that
    /// settled: a late completion from an older replay must never
    /// release a newer command's slot.
    public func clearPendingCommand(itemID: String, commandID: String) {
        guard pendingCommandsByItemID[itemID]?.command.command_id == commandID else { return }
        pendingCommandsByItemID[itemID] = nil
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
