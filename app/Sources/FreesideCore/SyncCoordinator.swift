import Foundation
import FreesideAPI
import Network
import Observation
import OpenAPIRuntime

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
    public static let stalenessThreshold: TimeInterval = 60

    public enum TimelineLoadState: Equatable, Sendable {
        case idle
        case loading
        case loaded
        case unavailable
    }

    public let store: InboxStore
    public private(set) var cursors: SyncCursors?
    public private(set) var runs: [Components.Schemas.RunSnapshot] = []
    public private(set) var schedules: [Components.Schemas.ScheduleSnapshot] = []
    public private(set) var timelinesByRunID: [String: Components.Schemas.RunTimeline] = [:]
    public private(set) var timelineLoadStates: [String: TimelineLoadState] = [:]
    public private(set) var lastUpdatedAt: Date?

    private let cache: CacheStore
    /// Overlapping sync rounds resolve by recency, as the store's
    /// refresh and validation generations do: only the newest round may
    /// adopt a snapshot or write freshness, so a bootstrap response
    /// held open across a restore cannot land late and win the cache
    /// back for a dead epoch (or regress the full-snapshot cursor
    /// within one).
    private var syncGeneration = 0
    // A successful canonical replacement invalidates partial responses that
    // were issued against the prior cache image. Unlike syncGeneration this
    // token does not advance for an ordinary same-epoch heartbeat, so a lazy
    // timeline fetch may finish while liveness is being confirmed.
    private var cacheGeneration = 0
    private var runListGeneration = 0
    private var timelineGenerations: [String: Int] = [:]
    private var heartbeatTask: Task<Void, Never>?
    private var heartbeatToken: UUID?
    private var refreshTask: Task<Void, Never>?
    private var refreshToken: UUID?
    private var reachabilityMonitor: NWPathMonitor?
    private var lastReachabilitySatisfied: Bool?

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
                runs = cached.runs
                schedules = cached.schedules
                timelinesByRunID = cached.runTimelines.reduce(into: [:]) { timelines, timeline in
                    timelines[timeline.run_id] = timeline
                }
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
        // §5.14 test-4 guarantee across a restart (#115). The observer
        // reports whether the write reached disk so registration can gate
        // the first send on durability (#163); a coordinator torn down
        // mid-registration reports false and fails the send closed.
        store.pendingCommandsObserver = { [weak self] in
            self?.persist() ?? false
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
                if !adopt(try ok.body.json) {
                    // A same-epoch partial read completed while this
                    // bootstrap response was in flight. Fetch one new
                    // canonical snapshot rather than replacing that newer
                    // observation with an older full image.
                    await bootstrap()
                }
            case .undocumented(let statusCode, _):
                mark(failureStatus: statusCode)
            }
        } catch {
            guard generation == syncGeneration else { return }
            store.freshness = freshnessForReadError(error)
        }
    }

    /// The periodic loss detector (plan §5.14: push and WebSocket are
    /// latency-only; the heartbeat is what catches a missed
    /// invalidation). An epoch mismatch or a revision past the last
    /// full snapshot resyncs; anything else confirms the cache current.
    public func heartbeat() async {
        if let heartbeatTask {
            await heartbeatTask.value
            return
        }

        let token = UUID()
        let task = Task { @MainActor [weak self] in
            guard let self else { return }
            await performHeartbeat()
        }
        heartbeatToken = token
        heartbeatTask = task
        await task.value
        if heartbeatToken == token {
            heartbeatTask = nil
            heartbeatToken = nil
        }
    }

    private func performHeartbeat() async {
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
                } else {
                    // The response's revision is only a lower bound on the
                    // live cursor: a partial read may have completed while
                    // this heartbeat was in flight. Re-read that cursor
                    // after observing the response before claiming freshness.
                    observe(revision: server.revision)
                    guard let current = self.cursors else {
                        await bootstrap()
                        return
                    }
                    if current.highestObservedServerRevision > current.lastFullSnapshotRevision {
                        // Partial reads may already have shown pieces of
                        // these revisions, but only a bootstrap makes the
                        // whole cache current (test 11).
                        await bootstrap()
                    } else {
                        store.freshness = .fresh
                        lastUpdatedAt = .now
                    }
                }
            case .undocumented(let statusCode, _):
                mark(failureStatus: statusCode)
            }
        } catch {
            guard generation == syncGeneration else { return }
            store.freshness = freshnessForReadError(error)
        }
    }

    /// Heartbeats until cancelled; failures already degrade to the
    /// banner, so the loop itself never exits early.
    public func heartbeatLoop(every interval: Duration) async {
        while !Task.isCancelled {
            // Periodic, manual, foreground, and reachability refreshes share
            // one operation. Otherwise a periodic heartbeat can invalidate
            // the manual round's generation while that round still reports
            // completion to its caller.
            await periodicRefresh()
            try? await Task.sleep(for: interval)
        }
    }

    func periodicRefresh() async {
        await refresh()
    }

    /// One user-visible refresh operation. Concurrent callers join the same
    /// task, so pull-to-refresh, toolbar, keyboard, and activation triggers
    /// cannot multiply daemon traffic. Manual callers always start a round
    /// when none is in flight.
    public func refresh() async {
        if let refreshTask {
            await refreshTask.value
            return
        }

        let token = UUID()
        let task = Task { @MainActor [weak self] in
            guard let self else { return }
            await heartbeat()
            await refreshRuns()
            if store.freshness == .unvalidated,
                let cursors,
                cursors.highestObservedServerRevision > cursors.lastFullSnapshotRevision
            {
                // The run-list partial read can observe a mutation that
                // committed after the opening heartbeat. Close that gap
                // before a user-visible refresh reports completion.
                await heartbeat()
            }
        }
        refreshToken = token
        refreshTask = task
        await task.value
        if refreshToken == token {
            refreshTask = nil
            refreshToken = nil
        }
    }

    /// Foreground and restored-reachability events always enter the shared
    /// refresh gateway. Overlapping events join its in-flight task, but a
    /// recent success never suppresses the first read after an interruption.
    public func automaticRefresh() async {
        await refresh()
    }

    public func isStale(at now: Date = .now) -> Bool {
        guard let lastUpdatedAt else { return true }
        return now.timeIntervalSince(lastUpdatedAt) >= Self.stalenessThreshold
    }

    public func startReachabilityMonitoring() {
        guard reachabilityMonitor == nil else { return }
        let monitor = NWPathMonitor()
        reachabilityMonitor = monitor
        monitor.pathUpdateHandler = { [weak self] path in
            let isSatisfied = path.status == .satisfied
            Task { @MainActor [weak self] in
                self?.observeReachability(isSatisfied)
            }
        }
        monitor.start(queue: DispatchQueue(label: "ai.freeside.reachability"))
    }

    public func stopReachabilityMonitoring() {
        reachabilityMonitor?.cancel()
        reachabilityMonitor = nil
        lastReachabilitySatisfied = nil
    }

    func observeReachability(_ isSatisfied: Bool) {
        defer { lastReachabilitySatisfied = isSatisfied }
        guard lastReachabilitySatisfied == false, isSatisfied else { return }
        Task { await automaticRefresh() }
    }

    /// A canonical partial read advances only the observed cursor,
    /// never the full-snapshot cursor (test 11). Reads that arrive
    /// before any bootstrap scoped an epoch carry no usable cursor.
    public func observe(revision: Int64) {
        guard var cursors, revision > cursors.highestObservedServerRevision else { return }
        cursors.highestObservedServerRevision = revision
        self.cursors = cursors
        if revision > cursors.lastFullSnapshotRevision {
            store.freshness = .unvalidated
        }
        persist()
    }

    /// Refreshes the list-level run projections without claiming the whole
    /// cache current. The response's highest revision advances only the
    /// observed cursor (plan §5.14 sync test 11).
    public func refreshRuns() async {
        runListGeneration += 1
        let requestGeneration = runListGeneration
        let requestCacheGeneration = cacheGeneration
        do {
            let output = try await store.client.listRuns()
            guard requestGeneration == runListGeneration,
                requestCacheGeneration == cacheGeneration
            else {
                return
            }
            switch output {
            case .ok(let ok):
                let snapshots = try ok.body.json
                runs = snapshots
                if let revision = snapshots.map(\.as_of_revision).max() {
                    observe(revision: revision)
                } else {
                    // An empty collection has no row carrying the read's
                    // revision. Confirm it through the revision endpoint so
                    // the client never keeps a stale full cache fresh merely
                    // because no run exists to advance the partial cursor.
                    await performHeartbeat()
                }
                persist()
            case .undocumented(let statusCode, _):
                mark(failureStatus: statusCode)
            }
        } catch {
            guard requestGeneration == runListGeneration,
                requestCacheGeneration == cacheGeneration
            else { return }
            if error is CancellationError || Task.isCancelled { return }
            store.freshness = freshnessForReadError(error)
        }
    }

    /// Fetches one computed timeline on navigation. A cached same-epoch value
    /// remains available while unreachable; a successful partial read replaces
    /// it and advances only the observed cursor.
    public func refreshTimeline(for runID: String) async {
        let requestGeneration = (timelineGenerations[runID] ?? 0) + 1
        timelineGenerations[runID] = requestGeneration
        let requestCacheGeneration = cacheGeneration
        timelineLoadStates[runID] = .loading
        do {
            let output = try await store.client.getRunTimeline(
                path: .init(run_id: runID))
            guard timelineGenerations[runID] == requestGeneration,
                requestCacheGeneration == cacheGeneration
            else { return }
            switch output {
            case .ok(let ok):
                let timeline = try ok.body.json
                guard timeline.run_id == runID else {
                    // The daemon answered, just with the wrong run's
                    // timeline: a reachable-but-failing read, not silence.
                    timelineLoadStates[runID] = .unavailable
                    store.freshness = .syncFailing
                    return
                }
                timelinesByRunID[runID] = timeline
                timelineLoadStates[runID] = .loaded
                observe(revision: timeline.as_of_revision)
                persist()
            case .notFound:
                timelineLoadStates[runID] = .unavailable
            case .undocumented(let statusCode, _):
                timelineLoadStates[runID] = .unavailable
                mark(failureStatus: statusCode)
            }
        } catch {
            guard timelineGenerations[runID] == requestGeneration,
                requestCacheGeneration == cacheGeneration
            else { return }
            if error is CancellationError || Task.isCancelled {
                timelineLoadStates[runID] = .idle
                return
            }
            timelineLoadStates[runID] = .unavailable
            store.freshness = freshnessForReadError(error)
        }
    }

    /// Returns false when a same-epoch canonical snapshot was overtaken by a
    /// partial read. The caller must refetch instead of letting older rows
    /// replace the newer observation and falsely claim the cache is current.
    private func adopt(_ snapshot: Components.Schemas.BootstrapSnapshot) -> Bool {
        if let cursors,
            cursors.syncEpoch == snapshot.sync_epoch,
            cursors.highestObservedServerRevision > snapshot.revision
        {
            store.freshness = .unvalidated
            return false
        }
        if let cursors, cursors.syncEpoch != snapshot.sync_epoch {
            // The old epoch's cache and cursors are dead (test 8), even
            // when its revisions ran ahead of the restored daemon's:
            // revisions never compare across epochs.
            discardCache()
        }
        cacheGeneration += 1
        store.replaceAll(with: snapshot.attention_items)
        runs = snapshot.runs
        schedules = snapshot.schedules
        timelinesByRunID = [:]
        timelineLoadStates = [:]
        cursors = SyncCursors(
            syncEpoch: snapshot.sync_epoch,
            lastFullSnapshotRevision: snapshot.revision,
            highestObservedServerRevision: max(
                cursors?.highestObservedServerRevision ?? 0, snapshot.revision)
        )
        store.freshness = .fresh
        lastUpdatedAt = .now
        persist()
        return true
    }

    /// Maps an answered non-401 status to its freshness state. The daemon
    /// responded, so this is never `.unreachable`: 401 is the credential
    /// state, any other status is a reachable daemon whose reads are
    /// failing.
    private func mark(failureStatus: Int) {
        store.freshness = failureStatus == 401 ? .unauthenticated : .syncFailing
    }

    /// Classifies a thrown sync-read error. OpenAPIRuntime raises a
    /// `ClientError` for both a transport failure and a response that
    /// arrived but could not be decoded (a 200 whose body is malformed or
    /// schema-incompatible, since the generated client decodes eagerly on
    /// the operation call). The decode failure carries the received
    /// response; the transport failure carries none. The former is a
    /// reachable daemon whose read failed (`.syncFailing`), the latter is
    /// silence (`.unreachable`). A non-`ClientError` cannot have reached a
    /// response, so it fails closed to `.unreachable`.
    private func freshnessForReadError(_ error: any Error) -> InboxStore.Freshness {
        (error as? ClientError)?.response != nil ? .syncFailing : .unreachable
    }

    private func discardCache() {
        // Evict before re-persisting the ledger: if the save below is
        // lost, an absent cache is honest, while a lingering file of
        // dead-epoch rows is not. The ledger survives the discard (#115):
        // commitment is epoch-independent, and only its verbatim resend
        // can settle an unresolved command against the restored daemon.
        cache.discard()
        cacheGeneration += 1
        store.discardSnapshots()
        runs = []
        schedules = []
        timelinesByRunID = [:]
        timelineLoadStates = [:]
        cursors = nil
        persist()
    }

    /// Writes the current cache and reports whether it reached disk. The
    /// read-cache/cursor callers discard the result (a lost save costs one
    /// bootstrap), while the ledger-registration observer gates the first
    /// send on it (#163).
    @discardableResult
    private func persist() -> Bool {
        let pending = store.pendingCommandsByItemID
        // Nothing worth a file: keeping one would undo an eviction. An
        // empty ledger is durably recorded by definition.
        guard cursors != nil || !pending.isEmpty else {
            cache.discard()
            return true
        }
        // Rows are meaningless without the cursors that scope them, so a
        // cursor-less save carries the ledger alone.
        do {
            try cache.save(
                CachedState(
                    cursors: cursors,
                    attentionItems: cursors == nil ? [] : store.orderedSnapshots,
                    runs: cursors == nil ? [] : runs,
                    schedules: cursors == nil ? [] : schedules,
                    runTimelines: cursors == nil
                        ? [] : timelinesByRunID.keys.sorted().compactMap { timelinesByRunID[$0] },
                    pendingCommands: pending
                ))
            return true
        } catch {
            return false
        }
    }
}
