import FreesideAPI
import Observation

/// The client half of plan §5.14's consistency contract. Owns the
/// cursor pair and the disposable disk cache over the InboxStore's
/// table: only a bootstrap (the daemon's one canonical single-read
/// snapshot) advances `lastFullSnapshotRevision`, any canonical read
/// may advance `highestObservedServerRevision`, a heartbeat gap between
/// them triggers a bootstrap (sync test 11), and an epoch change
/// discards the cache outright before resyncing (sync test 8). The
/// daemon is sole authority; everything here is rebuildable from one
/// bootstrap, so every sync failure degrades to the cached read-only
/// view with a freshness banner, never an error the user must resolve.
@MainActor
@Observable
public final class SyncCoordinator {
    public let store: InboxStore
    public private(set) var cursors: SyncCursors?

    private let cache: CacheStore
    /// Overlapping sync rounds resolve by recency, as the store's
    /// refresh and validation generations do: only the newest round may
    /// adopt a snapshot or write freshness, so a bootstrap response
    /// held open across a restore cannot land late and win the cache
    /// back for a dead epoch (or regress the full-snapshot cursor
    /// within one).
    private var syncGeneration = 0

    public init(
        client: any APIProtocol,
        device: DeviceIdentity = .mock,
        cache: CacheStore
    ) {
        store = InboxStore(client: client, device: device)
        self.cache = cache
        if let cached = cache.load() {
            if let cursors = cached.cursors {
                self.cursors = cursors
                store.replaceAll(with: cached.attentionItems)
            }
            // The ledger restores even without cursors: an epoch discard
            // preserves it precisely so an unresolved command's retry
            // survives the relaunch that follows (#115).
            if let pending = cached.pendingCommands {
                store.restorePendingCommands(pending)
            }
        }
        // Freshness stays .unvalidated until a round-trip settles it:
        // the cached view renders immediately, but nothing claims it is
        // current before the first heartbeat or bootstrap says so.
        store.revisionObserver = { [weak self] revision in
            self?.observe(revision: revision)
        }
        // Every ledger mutation persists immediately: a sync round may
        // never come before termination, and the persisted ledger is the
        // §5.14 test-4 guarantee across a restart (#115).
        store.pendingCommandsObserver = { [weak self] in
            self?.persist()
        }
    }

    /// Full resync: the canonical snapshot replaces the cached rows and
    /// both cursors. Bootstrap-on-gap is deliberately coarse for phase
    /// 1 — the plan permits "full bootstrap or refetch of all
    /// potentially affected resources", and the payloads are small.
    public func bootstrap() async {
        syncGeneration += 1
        let generation = syncGeneration
        do {
            let output = try await store.client.getSyncBootstrap()
            guard generation == syncGeneration else { return }
            switch output {
            case .ok(let ok):
                adopt(try ok.body.json)
            case .undocumented(let statusCode, _):
                mark(failureStatus: statusCode)
            }
        } catch {
            guard generation == syncGeneration else { return }
            store.freshness = .unreachable
        }
    }

    /// The periodic loss detector (plan §5.14: push and WebSocket are
    /// latency-only; the heartbeat is what catches a missed
    /// invalidation). An epoch mismatch or a revision past the last
    /// full snapshot resyncs; anything else confirms the cache current.
    public func heartbeat() async {
        syncGeneration += 1
        let generation = syncGeneration
        do {
            let output = try await store.client.getSyncRevision()
            guard generation == syncGeneration else { return }
            switch output {
            case .ok(let ok):
                let server = try ok.body.json
                guard let cursors else {
                    await bootstrap()
                    return
                }
                if server.sync_epoch != cursors.syncEpoch {
                    // The epoch is dead the moment the heartbeat says so:
                    // evict now, not after a successful re-bootstrap, or
                    // an outage window keeps rendering (and would relaunch
                    // into) pre-restore rows (§5.14 cache eviction on
                    // epoch change).
                    discardCache()
                    await bootstrap()
                } else if server.revision > cursors.lastFullSnapshotRevision {
                    // Partial reads may already have shown pieces of
                    // these revisions, but only a bootstrap makes the
                    // whole cache current (test 11).
                    observe(revision: server.revision)
                    await bootstrap()
                } else {
                    observe(revision: server.revision)
                    store.freshness = .fresh
                }
            case .undocumented(let statusCode, _):
                mark(failureStatus: statusCode)
            }
        } catch {
            guard generation == syncGeneration else { return }
            store.freshness = .unreachable
        }
    }

    /// Heartbeats until cancelled; failures already degrade to the
    /// banner, so the loop itself never exits early.
    public func heartbeatLoop(every interval: Duration) async {
        while !Task.isCancelled {
            await heartbeat()
            try? await Task.sleep(for: interval)
        }
    }

    /// A canonical partial read advances only the observed cursor,
    /// never the full-snapshot cursor (test 11). Reads that arrive
    /// before any bootstrap scoped an epoch carry no usable cursor.
    public func observe(revision: Int64) {
        guard var cursors, revision > cursors.highestObservedServerRevision else { return }
        cursors.highestObservedServerRevision = revision
        self.cursors = cursors
        persist()
    }

    private func adopt(_ snapshot: Components.Schemas.BootstrapSnapshot) {
        if let cursors, cursors.syncEpoch != snapshot.sync_epoch {
            // The old epoch's cache and cursors are dead (test 8), even
            // when its revisions ran ahead of the restored daemon's:
            // revisions never compare across epochs.
            discardCache()
        }
        store.replaceAll(with: snapshot.attention_items)
        cursors = SyncCursors(
            syncEpoch: snapshot.sync_epoch,
            lastFullSnapshotRevision: snapshot.revision,
            highestObservedServerRevision: max(
                cursors?.highestObservedServerRevision ?? 0, snapshot.revision)
        )
        store.freshness = .fresh
        persist()
    }

    private func mark(failureStatus: Int) {
        store.freshness = failureStatus == 401 ? .unauthenticated : .unreachable
    }

    private func discardCache() {
        // Evict before re-persisting the ledger: if the save below is
        // lost, an absent cache is honest, while a lingering file of
        // dead-epoch rows is not. The ledger survives the discard (#115):
        // commitment is epoch-independent, and only its verbatim resend
        // can settle an unresolved command against the restored daemon.
        cache.discard()
        store.discardSnapshots()
        cursors = nil
        persist()
    }

    private func persist() {
        let pending = store.pendingCommandsByItemID
        // Nothing worth a file: keeping one would undo an eviction.
        guard cursors != nil || !pending.isEmpty else {
            cache.discard()
            return
        }
        // Rows are meaningless without the cursors that scope them, so a
        // cursor-less save carries the ledger alone.
        cache.save(
            CachedState(
                cursors: cursors,
                attentionItems: cursors == nil ? [] : store.orderedSnapshots,
                pendingCommands: pending
            ))
    }
}
