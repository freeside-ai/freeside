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
    /// The foreground refresh cadence and the age at which the last
    /// successful refresh is called stale, declared together because the
    /// cadence must stay well under the threshold: both apps' heartbeat
    /// loops run every `heartbeatInterval`, while the banner waits
    /// `stalenessThreshold`, so a Stale banner means several consecutive
    /// rounds all ended without reaching a stamp (`performHeartbeat` or the
    /// bootstrap `adopt`), not a single missed beat. `SyncCoordinatorTests`
    /// pins the ordering between the two.
    public static let stalenessThreshold: TimeInterval = 60
    public static let heartbeatInterval: Duration = .seconds(15)

    public enum TimelineLoadState: Equatable, Sendable {
        case idle
        case loading
        case loaded
        case unavailable
    }

    public let store: InboxStore
    public private(set) var pendingTaskSubmissions: [String: Components.Schemas.ClientCommand] = [:]
    public private(set) var pendingTaskStops: [String: PendingTaskStop] = [:]
    @ObservationIgnored lazy var taskStop = TaskStopModel(coordinator: self)
    private let submissionDaemonID: String
    public private(set) var cursors: SyncCursors?
    public private(set) var runs: [Components.Schemas.RunSnapshot] = []
    public private(set) var schedules: [Components.Schemas.ScheduleSnapshot] = []
    public private(set) var timelinesByRunID: [String: Components.Schemas.RunTimeline] = [:]
    public private(set) var timelineLoadStates: [String: TimelineLoadState] = [:]
    public private(set) var tasks: [Components.Schemas.TaskSnapshot] = []
    public private(set) var taskTimelinesByTaskID: [String: Components.Schemas.TaskTimeline] = [:]
    public private(set) var taskTimelineLoadStates: [String: TimelineLoadState] = [:]
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
    private var taskTimelineGenerations: [String: Int] = [:]
    private struct TaskHistoryKey: Equatable {
        let taskID: String
        let revision: Int64?
        let epoch: String?
        let fullRevision: Int64?
        let generation: Int
    }
    private struct TaskHistoryRequest {
        let key: TaskHistoryKey
        let token: UUID
        let operation: Task<Void, Never>
    }
    private var taskHistoryRequests: [String: TaskHistoryRequest] = [:]

    private func taskHistoryKey(_ taskID: String) -> TaskHistoryKey {
        .init(
            taskID: taskID, revision: tasks.first { $0.task.id == taskID }?.as_of_revision,
            epoch: cursors?.syncEpoch, fullRevision: cursors?.lastFullSnapshotRevision,
            generation: cacheGeneration)
    }
    private struct TaskReviewRequest {
        let key: TaskReviewRequestKey
        let token: UUID
        let operation: Task<Void, Never>
    }
    private var taskReviewRequests: [String: TaskReviewRequest] = [:]

    struct TaskReviewRequestKey: Hashable {
        let taskID: String
        let taskRevision: Int64
        let historyRevision: Int64
        let epoch: String
        let fullSnapshotRevision: Int64?
        let cacheGeneration: Int
        let runIDs: [String]
    }

    func taskReviewRequestKey(for taskID: String, revision: Int64) -> TaskReviewRequestKey? {
        guard let history = taskTimelinesByTaskID[taskID], let cursors else { return nil }
        var seen = Set<String>()
        let runIDs = history.sections.flatMap(\.runs).filter {
            ($0.role == nil || $0.role?.value1 == .implementation) && seen.insert($0.run_id).inserted
        }.map(\.run_id)
        return .init(
            taskID: taskID, taskRevision: revision, historyRevision: history.as_of_revision,
            epoch: cursors.syncEpoch, fullSnapshotRevision: cursors.lastFullSnapshotRevision,
            cacheGeneration: cacheGeneration, runIDs: runIDs)
    }

    /// Only fetched task membership authorizes these reads. Mark before awaiting
    /// so overlapping views share work; failed/cancelled reads can be retried.
    func refreshTaskReviews(for taskID: String, revision: Int64) async {
        guard let key = taskReviewRequestKey(for: taskID, revision: revision) else { return }
        runLoop: for runID in key.runIDs {
            guard !Task.isCancelled,
                key == taskReviewRequestKey(for: taskID, revision: revision)
            else { return }
            while let existing = taskReviewRequests[runID], existing.key == key {
                await existing.operation.value
                guard !Task.isCancelled,
                    key == taskReviewRequestKey(for: taskID, revision: revision)
                else { return }
                if timelineLoadStates[runID] == .loaded { continue runLoop }
                if taskReviewRequests[runID]?.token == existing.token {
                    taskReviewRequests[runID] = nil
                }
            }
            let operation = Task { await refreshTimeline(for: runID) }
            let token = UUID()
            taskReviewRequests[runID] = .init(key: key, token: token, operation: operation)
            await withTaskCancellationHandler {
                await operation.value
            } onCancel: {
                operation.cancel()
            }
            if taskReviewRequests[runID]?.token == token,
                timelineLoadStates[runID] != .loaded || key.cacheGeneration != cacheGeneration
            {
                taskReviewRequests[runID] = nil
            }
        }
    }
    private var heartbeatTask: Task<Void, Never>?
    private var heartbeatToken: UUID?
    private var refreshTask: Task<Bool, Never>?
    private var refreshToken: UUID?
    private var reachabilityMonitor: NWPathMonitor?
    private var lastReachabilitySatisfied: Bool?

    public init(
        client: any APIProtocol,
        device: DeviceIdentity = .mock,
        cache: CacheStore,
        submissionDaemonID: String = "mock",
        inboxOrderNow: @escaping () -> Date = Date.init
    ) {
        store = InboxStore(client: client, device: device, now: inboxOrderNow)
        self.cache = cache
        self.submissionDaemonID = submissionDaemonID
        if let cached = cache.load() {
            if cached.stopDaemonID == submissionDaemonID {
                pendingTaskStops = (cached.pendingTaskStops ?? [:]).filter { id, entry in
                    entry.isValid(commandID: id, deviceID: device.deviceID)
                }
            }
            if cached.submissionDaemonID == submissionDaemonID {
                pendingTaskSubmissions = (cached.pendingTaskSubmissions ?? [:]).filter { id, command in
                    guard case .submit_task(let payload) = command.payload else { return false }
                    return id == command.command_id && !id.isEmpty && command.device_id == device.deviceID
                        && !payload.project_id.isEmpty && !payload.source.isEmpty
                        && command.expected_entity_version == nil && command.expected_bindings == nil
                }
            }
            if let cursors = cached.cursors,
                (try? ConversationContractValidation.validate(
                    cached.conversations,
                    maximumRevision: cursors.highestObservedServerRevision)) != nil
            {
                self.cursors = cursors
                store.replaceAll(with: cached.attentionItems)
                store.replaceAllConversations(with: cached.conversations)
                runs = cached.runs
                schedules = cached.schedules
                timelinesByRunID = cached.runTimelines.reduce(into: [:]) { timelines, timeline in
                    timelines[timeline.run_id] = timeline
                }
                tasks = cached.tasks
                taskTimelinesByTaskID = cached.taskTimelines.reduce(into: [:]) { timelines, timeline in
                    timelines[timeline.task_id] = timeline
                }
            }
            // The ledger restores even without cursors: an epoch discard
            // preserves it precisely so an unresolved command's retry
            // survives the relaunch that follows (#115).
            if let pending = cached.pendingCommands {
                store.restorePendingCommands(pending)
            }
            // The telemetry queue restores alongside the ledger, and for the
            // same reason: a queued event survives the relaunch that follows
            // and drains on the next round (plan §8).
            store.restoreComprehension(
                queue: cached.comprehensionQueue ?? [],
                sequence: cached.comprehensionSequence ?? 0,
                fingerprint: cached.registeredCapabilityFingerprint,
                owningDeviceID: cached.comprehensionDeviceID)
        }
        // Freshness stays .unvalidated until a round-trip settles it:
        // the cached view renders immediately, but nothing claims it is
        // current before the first heartbeat or bootstrap says so.
        store.revisionObserver = { [weak self] revision in
            self?.observe(revision: revision)
        }
        store.syncCursorsProvider = { [weak self] in self?.cursors }
        store.snapshotObserver = { [weak self] revision in
            self?.observe(snapshotRevision: revision)
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
        // Persist the telemetry queue on every mutation, like the ledger, so a
        // queued event survives a relaunch (plan §8).
        store.comprehensionObserver = { [weak self] in _ = self?.persist() }
    }

    /// Registers the capability contract when the local action set differs from
    /// the last one registered, then drains the telemetry queue. Both are
    /// best-effort: a failure retries on the next round. Runs after a
    /// successful bootstrap so it has a settled session.
    private func postRoundHousekeeping() async {
        let actions = ClientCapability.presentableActions
        let fingerprint = ClientCapability.fingerprint(of: actions)
        if store.registeredCapabilityFingerprint != fingerprint {
            do {
                _ = try await store.client.registerCapabilityContract(
                    path: .init(device_id: store.device.deviceID),
                    body: .json(.init(actions: actions))
                ).ok
                store.registeredCapabilityFingerprint = fingerprint
                persist()
            } catch {
                // Best-effort: a failed registration retries next round.
            }
        }
        await store.drainComprehensionEvents()
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
                let snapshot = try ok.body.json
                try ConversationContractValidation.validate(
                    snapshot.conversations, maximumRevision: snapshot.revision)
                if adopt(snapshot) {
                    await postRoundHousekeeping()
                } else {
                    // A same-epoch partial read completed while this
                    // bootstrap response was in flight. Fetch one new
                    // canonical snapshot rather than replacing that newer
                    // observation with an older full image.
                    await bootstrap()
                }
            case .undocumented(let statusCode, _):
                let diagnosed = await failureFreshness(status: statusCode)
                guard generation == syncGeneration else { return }
                store.freshness = diagnosed
            }
        } catch {
            guard generation == syncGeneration else { return }
            let diagnosed: InboxStore.Freshness
            if error is ConversationContractValidation.InvalidConversation {
                // A decoded-but-invalid conversation is a reachable,
                // failing read: probe for a contract skew like any other.
                diagnosed = await diagnoseSyncFailure()
            } else {
                diagnosed = await diagnosedReadFreshness(error)
            }
            guard generation == syncGeneration else { return }
            store.freshness = diagnosed
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
                        store.rebuildTimeBasedOrder()
                        store.freshness = .fresh
                        lastUpdatedAt = .now
                        settleTaskStops()
                    }
                }
            case .undocumented(let statusCode, _):
                let diagnosed = await failureFreshness(status: statusCode)
                guard generation == syncGeneration else { return }
                store.freshness = diagnosed
            }
        } catch {
            guard generation == syncGeneration else { return }
            let diagnosed = await diagnosedReadFreshness(error)
            guard generation == syncGeneration else { return }
            store.freshness = diagnosed
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
            _ = await refreshTask.value
            return
        }
        await startRefreshRound()
    }

    /// Read-your-write refresh: always runs a sync round whose first daemon
    /// read is issued after this call, so a caller that just committed a
    /// command can await it and then trust the cache to reflect the commit
    /// (plan §5.14). A round already in flight may have read its bootstrap
    /// before the commit, so unlike `refresh()` this call never returns on the
    /// strength of it: it waits for that round to finish, then starts a fresh
    /// round, or joins one that another caller started after this call.
    ///
    /// It waits rather than cancels because cancelling the in-flight round
    /// would bump `syncGeneration` under that round's caller and return it
    /// without a stamp, which the `heartbeatLoop` comment forbids. The cost is
    /// one extra round of latency when a refresh is mid-flight at commit time.
    ///
    /// Returns whether that round ended `.fresh`. The result is captured
    /// when the round finishes, so a partial read that lands after the round
    /// and demotes the published freshness cannot fail it; one that lands
    /// inside the round's final step still can, which fails closed.
    @discardableResult
    public func refreshAfterCommit() async -> Bool {
        // A round tagged with the token observed at entry began before this
        // call. Awaiting it drains that pre-commit round; only a round whose
        // token differs started after the call and may be joined.
        let entryToken = refreshToken
        if let refreshTask {
            _ = await refreshTask.value
        }
        if let refreshTask, refreshToken != entryToken {
            return await refreshTask.value
        }
        return await startRefreshRound()
    }

    /// Starts and awaits one daemon round, publishing its task and token so
    /// concurrent `refresh()` callers coalesce onto it. Every round awaits its
    /// own `heartbeat()` before finishing, and nothing outside a round calls
    /// `heartbeat()`, so a later `refreshAfterCommit()` that awaits this round
    /// observes reads it issued, never a stale heartbeat from an earlier round.
    @discardableResult
    private func startRefreshRound() async -> Bool {
        let token = UUID()
        let task = Task { @MainActor [weak self] in
            guard let self else { return false }
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
            return store.freshness == .fresh
        }
        refreshToken = token
        refreshTask = task
        let endedFresh = await task.value
        if refreshToken == token {
            refreshTask = nil
            refreshToken = nil
        }
        return endedFresh
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
        guard advanceObservedRevision(to: revision) else { return }
        persist()
    }

    /// An accepted resource snapshot must persist even when another row
    /// from the same transaction already advanced the observed cursor.
    private func observe(snapshotRevision revision: Int64) {
        guard cursors != nil else { return }
        _ = advanceObservedRevision(to: revision)
        persist()
    }

    @discardableResult
    private func advanceObservedRevision(to revision: Int64) -> Bool {
        guard var cursors, revision > cursors.highestObservedServerRevision else { return false }
        cursors.highestObservedServerRevision = revision
        self.cursors = cursors
        if revision > cursors.lastFullSnapshotRevision {
            store.freshness = .unvalidated
        }
        return true
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
                let diagnosed = await failureFreshness(status: statusCode)
                guard requestGeneration == runListGeneration,
                    requestCacheGeneration == cacheGeneration
                else { return }
                store.freshness = diagnosed
            }
        } catch {
            guard requestGeneration == runListGeneration,
                requestCacheGeneration == cacheGeneration
            else { return }
            if error is CancellationError || Task.isCancelled { return }
            let diagnosed = await diagnosedReadFreshness(error)
            guard requestGeneration == runListGeneration,
                requestCacheGeneration == cacheGeneration
            else { return }
            store.freshness = diagnosed
        }
    }

    enum ReviewEvidenceReadError: Error { case unavailable, bindingMismatch }

    public func reviewEvidence(
        for runID: String, round: Components.Schemas.RunReviewRound
    ) async throws -> Components.Schemas.ReviewEvidence {
        let generation = cacheGeneration
        let output = try await store.client.getReviewEvidence(path: .init(run_id: runID, round: round.round))
        try Task.checkCancellation()
        guard generation == cacheGeneration else { throw CancellationError() }
        switch output {
        case .ok(let ok):
            let evidence = try ok.body.json
            guard evidence.run_id == runID, evidence.round == round.round,
                evidence.invocation_id == round.invocation_id,
                evidence.source_head_sha == round.head_sha,
                evidence.source == round.source, !evidence.publish_eligible
            else { throw ReviewEvidenceReadError.bindingMismatch }
            return evidence
        case .notFound, .undocumented:
            throw ReviewEvidenceReadError.unavailable
        }
    }

    /// Fetches one computed run timeline on navigation. A cached same-epoch
    /// value remains available while unreachable; a successful partial read
    /// replaces it and advances only the observed cursor.
    public func refreshTimeline(for runID: String) async {
        await refreshComputedTimeline(
            id: runID,
            generations: \.timelineGenerations,
            loadStates: \.timelineLoadStates,
            read: {
                switch try await store.client.getRunTimeline(path: .init(run_id: runID)) {
                case .ok(let ok): .ok(try ok.body.json)
                case .notFound: .notFound
                case .undocumented(let statusCode, _): .undocumented(status: statusCode)
                }
            },
            binds: { $0.run_id == runID },
            adopt: { timelinesByRunID[runID] = $0 },
            revision: \.as_of_revision)
    }

    /// The task counterpart of `refreshTimeline(for:)`, keyed by task id and
    /// held under the same cache-generation rules.
    public func refreshTaskTimeline(for taskID: String) async {
        let key = taskHistoryKey(taskID)
        while let existing = taskHistoryRequests[taskID], existing.key == key {
            await existing.operation.value
            guard !Task.isCancelled, key == taskHistoryKey(taskID) else { return }
            if taskTimelineLoadStates[taskID] == .loaded { return }
            if taskHistoryRequests[taskID]?.token == existing.token { taskHistoryRequests[taskID] = nil }
        }
        guard !Task.isCancelled else { return }
        let token = UUID()
        let operation = Task { await loadTaskTimeline(for: taskID) }
        taskHistoryRequests[taskID] = .init(key: key, token: token, operation: operation)
        await withTaskCancellationHandler {
            await operation.value
        } onCancel: {
            operation.cancel()
        }
        if taskHistoryRequests[taskID]?.token == token,
            taskTimelineLoadStates[taskID] != .loaded || key != taskHistoryKey(taskID)
        {
            taskHistoryRequests[taskID] = nil
        }
    }

    private func loadTaskTimeline(for taskID: String) async {
        await refreshComputedTimeline(
            id: taskID,
            generations: \.taskTimelineGenerations,
            loadStates: \.taskTimelineLoadStates,
            read: {
                switch try await store.client.getTaskTimeline(path: .init(task_id: taskID)) {
                case .ok(let ok): .ok(try ok.body.json)
                case .notFound: .notFound
                case .undocumented(let statusCode, _): .undocumented(status: statusCode)
                }
            },
            binds: { history in
                history.task_id == taskID
                    && tasks.first(where: { $0.task.id == taskID }).map {
                        $0.task.project_id == history.project_id
                    } != false
            },
            adopt: { taskTimelinesByTaskID[taskID] = $0 },
            revision: \.as_of_revision)
    }

    /// One computed-timeline read's outcome, the shape both operations
    /// reduce their generated output to.
    private enum TimelineRead<Timeline> {
        case ok(Timeline)
        case notFound
        case undocumented(status: Int)
    }

    /// The generation and load-state discipline the run and task timelines
    /// share. The newest request per id wins; a result issued against a
    /// cache that a bootstrap replaced mid-flight is dropped; and whenever
    /// this newest request produces nothing, the load state returns to
    /// `.idle` rather than staying `.loading` with no request in flight,
    /// which is what let the spinner stick after a bootstrap. The view's
    /// refetch, keyed on the new full-snapshot revision, issues the fresh
    /// request. `binds` rejects an answer for the wrong entity: the daemon
    /// responded, so that is a reachable-but-failing read, not silence.
    private func refreshComputedTimeline<Timeline>(
        id: String,
        generations: ReferenceWritableKeyPath<SyncCoordinator, [String: Int]>,
        loadStates: ReferenceWritableKeyPath<SyncCoordinator, [String: TimelineLoadState]>,
        read: () async throws -> TimelineRead<Timeline>,
        binds: (Timeline) -> Bool,
        adopt: (Timeline) -> Void,
        revision: KeyPath<Timeline, Int64>
    ) async {
        let requestGeneration = (self[keyPath: generations][id] ?? 0) + 1
        self[keyPath: generations][id] = requestGeneration
        let requestCacheGeneration = cacheGeneration
        self[keyPath: loadStates][id] = .loading
        do {
            let output = try await read()
            try Task.checkCancellation()
            guard self[keyPath: generations][id] == requestGeneration else { return }
            guard requestCacheGeneration == cacheGeneration else {
                self[keyPath: loadStates][id] = .idle
                return
            }
            switch output {
            case .ok(let timeline):
                guard binds(timeline) else {
                    self[keyPath: loadStates][id] = .unavailable
                    let diagnosed = await diagnoseSyncFailure()
                    guard self[keyPath: generations][id] == requestGeneration,
                        requestCacheGeneration == cacheGeneration
                    else { return }
                    store.freshness = diagnosed
                    return
                }
                adopt(timeline)
                self[keyPath: loadStates][id] = .loaded
                observe(revision: timeline[keyPath: revision])
                persist()
            case .notFound:
                self[keyPath: loadStates][id] = .unavailable
            case .undocumented(let statusCode):
                self[keyPath: loadStates][id] = .unavailable
                let diagnosed = await failureFreshness(status: statusCode)
                guard self[keyPath: generations][id] == requestGeneration,
                    requestCacheGeneration == cacheGeneration
                else { return }
                store.freshness = diagnosed
            }
        } catch {
            guard self[keyPath: generations][id] == requestGeneration else { return }
            guard requestCacheGeneration == cacheGeneration else {
                self[keyPath: loadStates][id] = .idle
                return
            }
            if error is CancellationError || Task.isCancelled {
                self[keyPath: loadStates][id] = .idle
                return
            }
            self[keyPath: loadStates][id] = .unavailable
            let diagnosed = await diagnosedReadFreshness(error)
            guard self[keyPath: generations][id] == requestGeneration,
                requestCacheGeneration == cacheGeneration
            else { return }
            store.freshness = diagnosed
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
        taskReviewRequests = [:]
        store.replaceAll(with: snapshot.attention_items)
        store.replaceAllConversations(with: snapshot.conversations)
        runs = snapshot.runs
        schedules = snapshot.schedules
        tasks = snapshot.tasks
        // Timelines are not in the bootstrap payload, so a same-epoch
        // bootstrap can neither replace nor invalidate a cached one: keep
        // every timeline whose run the snapshot still lists and drop the
        // rest. The view refetches after each replacement (its request key
        // folds in `lastFullSnapshotRevision`), so holding the prior
        // projection until then is the same trust the relaunch path already
        // extends to cached timelines. A live run advances the observed
        // revision every round and so bootstraps every round; discarding the
        // timeline here is what made the detail pane flap to a spinner. The
        // epoch-change path above already cleared both through
        // `discardCache()`, and revisions never compare across epochs, so a
        // cross-epoch timeline is never retained.
        let listedRunIDs = Set(snapshot.runs.map(\.run.id))
        timelinesByRunID = timelinesByRunID.filter { listedRunIDs.contains($0.key) }
        timelineLoadStates = timelineLoadStates.filter { listedRunIDs.contains($0.key) }
        let listedTaskIDs = Set(snapshot.tasks.map(\.task.id))
        taskTimelinesByTaskID = taskTimelinesByTaskID.filter { listedTaskIDs.contains($0.key) }
        taskTimelineLoadStates = taskTimelineLoadStates.filter { listedTaskIDs.contains($0.key) }
        taskHistoryRequests = taskHistoryRequests.filter { listedTaskIDs.contains($0.key) }
        cursors = SyncCursors(
            syncEpoch: snapshot.sync_epoch,
            lastFullSnapshotRevision: snapshot.revision,
            highestObservedServerRevision: max(
                cursors?.highestObservedServerRevision ?? 0, snapshot.revision)
        )
        store.freshness = .fresh
        lastUpdatedAt = .now
        settleTaskStops()
        persist()
        return true
    }

    /// Maps an answered non-401 status to its freshness state. The daemon
    /// responded, so this is never `.unreachable`: 401 is the credential
    /// state, any other status is a reachable daemon whose reads are
    /// failing, refined by a health probe into `.contractMismatch` on a
    /// contract-digest skew. Returns the state; the caller re-checks its
    /// round generation before writing it, so the awaited probe cannot let
    /// a superseded round overwrite a newer one.
    private func failureFreshness(status: Int) async -> InboxStore.Freshness {
        status == 401 ? .unauthenticated : await diagnoseSyncFailure()
    }

    /// The diagnosed freshness for a thrown read error: `.unreachable`
    /// stays (no response arrived), while a reachable-but-failing read
    /// (`.syncFailing`) is refined by a health probe into
    /// `.contractMismatch` on a contract-digest skew. Caller re-guards the
    /// write as above.
    private func diagnosedReadFreshness(_ error: any Error) async -> InboxStore.Freshness {
        freshnessForReadError(error) == .syncFailing ? await diagnoseSyncFailure() : .unreachable
    }

    /// Probes `/health` to tell a contract skew from a generic sync
    /// failure. When a sync read has already failed as reachable-but-
    /// failing, a `/health` answer whose `contract_digest` differs from
    /// this client's compiled-in `APIContract.digest` means the two were
    /// built from different specs, so the client must not expect to sync;
    /// report `.contractMismatch`. A matching digest, a transport error,
    /// or an undecodable health body (including a pre-#1265 daemon with no
    /// digest field) stays `.syncFailing`.
    ///
    /// The probe races a short timeout, as the daemon-menu health checker
    /// does: a daemon that stops between the failed read and this request,
    /// or a network that starts black-holing traffic, would otherwise leave
    /// this await pending until the transport timeout, stalling the shared
    /// refresh task so later refreshes only join it. Timeout falls back to
    /// `.syncFailing`, the same as any other unreachable-probe outcome.
    private func diagnoseSyncFailure() async -> InboxStore.Freshness {
        let client = store.client
        return await withTaskGroup(of: InboxStore.Freshness.self) { group in
            group.addTask {
                guard let output = try? await client.getHealth(),
                    case .ok(let ok) = output,
                    let body = try? ok.body.json
                else {
                    return .syncFailing
                }
                if body.contract_digest != APIContract.digest {
                    return .contractMismatch(daemonContract: body.contract_digest)
                }
                return .syncFailing
            }
            group.addTask {
                try? await Task.sleep(for: .seconds(3))
                return .syncFailing
            }
            defer { group.cancelAll() }
            return await group.next() ?? .syncFailing
        }
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
        // Replace the cache atomically after clearing its snapshots. Never
        // delete the last durable command copy before its replacement saves.
        // If saving fails, restored snapshots remain unvalidated until sync;
        // the epoch-independent commands still survive for manual retry.
        cacheGeneration += 1
        store.discardSnapshots()
        runs = []
        schedules = []
        timelinesByRunID = [:]
        timelineLoadStates = [:]
        tasks = []
        taskTimelinesByTaskID = [:]
        taskTimelineLoadStates = [:]
        taskHistoryRequests = [:]
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
        let comprehensionQueue = store.comprehensionQueue
        let fingerprint = store.registeredCapabilityFingerprint
        // Nothing worth a file: keeping one would undo an eviction. An
        // empty ledger is durably recorded by definition. The telemetry queue
        // and fingerprint keep a file alive on their own, so a queued event or
        // a registered contract survives a relaunch with no cursors.
        guard
            cursors != nil || !pending.isEmpty || !pendingTaskSubmissions.isEmpty || !pendingTaskStops.isEmpty
                || !comprehensionQueue.isEmpty
                || fingerprint != nil
        else {
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
                    conversations: cursors == nil ? [] : store.orderedConversations,
                    runs: cursors == nil ? [] : runs,
                    schedules: cursors == nil ? [] : schedules,
                    runTimelines: cursors == nil
                        ? [] : timelinesByRunID.keys.sorted().compactMap { timelinesByRunID[$0] },
                    tasks: cursors == nil ? [] : tasks,
                    taskTimelines: cursors == nil
                        ? [] : taskTimelinesByTaskID.keys.sorted().compactMap { taskTimelinesByTaskID[$0] },
                    pendingCommands: pending,
                    pendingTaskSubmissions: pendingTaskSubmissions,
                    submissionDaemonID: submissionDaemonID,
                    pendingTaskStops: pendingTaskStops,
                    stopDaemonID: submissionDaemonID,
                    comprehensionQueue: comprehensionQueue,
                    comprehensionSequence: store.comprehensionSequence,
                    registeredCapabilityFingerprint: fingerprint,
                    comprehensionDeviceID: comprehensionQueue.isEmpty && fingerprint == nil
                        ? nil : store.device.deviceID
                ))
            return true
        } catch {
            return false
        }
    }

    /// Durability is a precondition for sending, including a manual retry.
    func retainTaskSubmission(_ command: Components.Schemas.ClientCommand) -> Bool {
        guard command.device_id == store.device.deviceID,
            case .submit_task = command.payload
        else { return false }
        let previous = pendingTaskSubmissions[command.command_id]
        guard previous == nil || previous == command else { return false }
        pendingTaskSubmissions[command.command_id] = command
        guard persist() else {
            pendingTaskSubmissions[command.command_id] = previous
            return false
        }
        return true
    }

    func finishTaskSubmission(_ commandID: String) {
        pendingTaskSubmissions.removeValue(forKey: commandID)
        persist()
    }

    /// Persist exact retry identity before sending; failed writes roll back.
    func retainTaskStop(_ entry: PendingTaskStop) -> Bool {
        let id = entry.command.command_id
        guard entry.isValid(commandID: id, deviceID: store.device.deviceID),
            !pendingTaskStops.values.contains(where: { $0.taskID == entry.taskID && $0.command.command_id != id }),
            pendingTaskStops[id] == nil || pendingTaskStops[id]?.command == entry.command
        else { return false }
        let previous = pendingTaskStops[id]
        pendingTaskStops[id] = entry
        guard persist() else {
            pendingTaskStops[id] = previous
            return false
        }
        return true
    }

    func finishTaskStop(_ commandID: String) {
        let previous = pendingTaskStops.removeValue(forKey: commandID)
        if !persist() { pendingTaskStops[commandID] = previous }
    }

    func settleTaskStops() {
        guard store.freshness == .fresh, let cursors else { return }
        for entry in pendingTaskStops.values {
            guard let receipt = entry.receipt, case .stop_task(let payload) = entry.command.payload,
                let task = tasks.first(where: { $0.task.id == entry.taskID })
            else { continue }
            if cursors.syncEpoch != payload.expected_sync_epoch
                || (task.as_of_revision >= receipt.revision && task.task.cancellation != nil)
            {
                finishTaskStop(entry.command.command_id)
            }
        }
    }
}
