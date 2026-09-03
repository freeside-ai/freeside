import Foundation
import FreesideAPI
import Observation
import OpenAPIRuntime

/// The single client-side source of truth for attention item snapshots:
/// the inbox list and every decision card read the same table, so a
/// replacement swap or a revalidation refetch can never leave the two
/// rendering different states. SyncCoordinator drives cache persistence
/// and the §5.14 cursor semantics over this table.
@MainActor
@Observable
public final class InboxStore {
    public enum Scope: String, CaseIterable, Identifiable {
        case open
        case resolved
        case all

        public var id: Self { self }

        var label: String {
            switch self {
            case .open: "Open"
            case .resolved: "Resolved"
            case .all: "All"
            }
        }
    }

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
        /// The daemon is unreachable: the request never got an answer
        /// (a transport-level failure). Cached read-only view.
        case unreachable
        /// The daemon answered a sync read but the answer failed: a
        /// non-401 error status (e.g. a 500), or a 200 whose body this
        /// client cannot decode (schema skew or a malformed body). It is
        /// reachable but its reads are failing, so the cache cannot be
        /// confirmed current. Distinct from `.unreachable` (the request
        /// got no answer at all) so the operator sees a live-but-erroring
        /// daemon for what it is; still a cached read-only view.
        case syncFailing
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
    /// Supplies the active sync epoch to returned-object correlation without
    /// making decision cards own the coordinator. A nil cursor means no
    /// bootstrap has established an epoch yet.
    var syncCursorsProvider: (() -> SyncCursors?)?
    /// Reports a canonical resource mutation separately from a bare
    /// revision observation. Several rows may share one transaction's
    /// revision, but each accepted row must reach the disk cache.
    var snapshotObserver: ((Int64) -> Void)?
    /// Reports every pending-command ledger mutation so the sync
    /// coordinator can persist the ledger as it changes (#115): the
    /// retry affordance survives a relaunch only if each claim, state
    /// move, and release reaches disk when it happens, not at the next
    /// sync round. Returns whether the write reached disk, so a claim can
    /// gate the first send on durability (#163); the post-send state
    /// moves and releases ignore it (their loss only offers a harmless,
    /// idempotent verbatim resend on relaunch).
    public var pendingCommandsObserver: (() -> Bool)?
    /// Reports every comprehension-telemetry mutation (an enqueue, or a drain
    /// that sent or dropped events) so the sync coordinator persists the queue
    /// and the registered-capability fingerprint as they change (plan §8). The
    /// queue is best-effort: a lost persist only re-sends idempotent events.
    public var comprehensionObserver: (() -> Void)?
    public private(set) var snapshotsByID: [String: Components.Schemas.AttentionItemSnapshot] = [:]
    public private(set) var conversationsByID: [String: Components.Schemas.ConversationSnapshot] = [:]
    public var scope: Scope = .open
    public var projectID: String?
    private var pendingLaunchProjectID: String?

    public var projects: [String] {
        Array(Set(snapshotsByID.values.map(\.item.project_id))).sorted()
    }
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
    public private(set) var pendingCommandsByItemID: [String: PendingCommandEntry] = [:]
    /// One queued comprehension event: the client-generated idempotency key and
    /// the event body. Codable because the queue persists in the disk cache — a
    /// telemetry event should survive a relaunch and drain on the next round,
    /// exactly like the pending-command ledger (plan §8, delivery-receipt
    /// discipline). It carries identifiers, digests, and instants, never prose.
    public nonisolated struct QueuedComprehensionEvent: Codable, Equatable, Sendable {
        public let eventID: String
        public let input: Components.Schemas.ComprehensionEventInput

        public init(eventID: String, input: Components.Schemas.ComprehensionEventInput) {
            self.eventID = eventID
            self.input = input
        }
    }

    /// The best-effort comprehension-telemetry queue, drained after each sync
    /// round and after a submit. Events are idempotent by their event id, so a
    /// retry is safe.
    public private(set) var comprehensionQueue: [QueuedComprehensionEvent] = []
    /// The per-device event sequence; monotonic across the session and
    /// persisted so it keeps climbing across a relaunch.
    public private(set) var comprehensionSequence: Int = 0
    /// The fingerprint of the capability contract last registered with the
    /// daemon, so session start re-registers only a changed action set.
    public var registeredCapabilityFingerprint: String?
    /// Process-local claims held only while an external PR URL is opening.
    /// They coordinate re-created cards without entering the replay ledger:
    /// a crash before navigation succeeds must never resurrect a command that
    /// records engagement the operator may not have completed.
    private var navigationReservations: Set<String> = []
    private var serverOrder: [String] = []
    /// Filtering uses the status captured by the last full list rebuild. A
    /// resolving command can therefore show its applied confirmation on the
    /// open card; the next refresh moves it into the resolved scope.
    private var statusAtOrderRebuild: [String: Components.Schemas.ItemStatus] = [:]
    /// Same-epoch visibility removals retain the last rendered entity version
    /// so an older in-flight list/bootstrap cannot resurrect a snoozed row.
    /// A strictly newer snapshot is the release transition and clears it.
    private var removalVersionFloors: [String: Int64] = [:]
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

    /// The inbox rows in the selected scope: open items first when scopes are
    /// combined, urgent-to-low within a status, server order as the stable
    /// tiebreak.
    public var rows: [Components.Schemas.AttentionItemSnapshot] {
        filteredRows(in: scope, projectID: projectID).sorted { lhs, rhs in
            let (lhsKey, rhsKey) = (
                sortKey(
                    lhs.1, index: lhs.0,
                    status: statusAtOrderRebuild[lhs.1.item.id] ?? lhs.1.item.status),
                sortKey(
                    rhs.1, index: rhs.0,
                    status: statusAtOrderRebuild[rhs.1.item.id] ?? rhs.1.item.status)
            )
            return lhsKey < rhsKey
        }.map(\.1)
    }

    /// All rows whose status was open at the last full list rebuild. This
    /// projection deliberately ignores the interactive project filter so
    /// top-level app summaries cannot contradict the retained Open state.
    public var openSnapshots: [Components.Schemas.AttentionItemSnapshot] {
        filteredRows(in: .open, projectID: nil).map(\.1)
    }

    /// One global urgent-open projection for every top-level navigation
    /// surface: iOS badge, macOS sidebar, and menu-bar extra.
    public var urgentOpenCount: Int {
        filteredRows(in: .open, projectID: nil).count { $0.1.item.priority == .urgent }
    }

    public func nextOpenItemID(excluding itemID: String) -> String? {
        filteredRows(in: .open, projectID: projectID)
            .filter { $0.1.item.id != itemID && $0.1.item.status == .open }
            .sorted {
                sortKey($0.1, index: $0.0, status: .open)
                    < sortKey($1.1, index: $1.0, status: .open)
            }
            .first?.1.item.id
    }

    public func count(in scope: Scope) -> Int {
        filteredRows(in: scope, projectID: projectID).count
    }

    public func urgentCount(in scope: Scope) -> Int {
        filteredRows(in: scope, projectID: projectID).count { $0.1.item.priority == .urgent }
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
            captureStatusesForCurrentOrder()
            loadState = .loaded
            finishLaunchProjectRepair()
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
        if let floor = removalVersionFloors[snapshot.item.id] {
            guard snapshot.entity_version > floor else { return false }
            removalVersionFloors.removeValue(forKey: snapshot.item.id)
        }
        if let existing = snapshotsByID[snapshot.item.id],
            existing.entity_version > snapshot.entity_version
        {
            return false
        }
        snapshotsByID[snapshot.item.id] = snapshot
        if !serverOrder.contains(snapshot.item.id) {
            serverOrder.append(snapshot.item.id)
        }
        snapshotObserver?(snapshot.as_of_revision)
        return true
    }

    /// Upserts one whole conversation snapshot without letting an older
    /// partial read replace a newer thread or status.
    @discardableResult
    public func apply(_ snapshot: Components.Schemas.ConversationSnapshot) -> Bool {
        let id = snapshot.conversation.id
        if let existing = conversationsByID[id],
            existing.entity_version > snapshot.entity_version
        {
            return false
        }
        conversationsByID[id] = snapshot
        snapshotObserver?(snapshot.as_of_revision)
        return true
    }

    /// Removes a row after an authoritative read proves it is intentionally
    /// absent from the visible inbox, as an active proposal snooze does. This
    /// is a same-epoch visibility transition, not a cache-epoch reset.
    public func removeSnapshot(itemID: String, atLeastEntityVersion floor: Int64) {
        let renderedVersion = snapshotsByID[itemID]?.entity_version ?? 0
        removalVersionFloors[itemID] = max(
            removalVersionFloors[itemID] ?? 0, renderedVersion, floor)
        snapshotsByID.removeValue(forKey: itemID)
        serverOrder.removeAll { $0 == itemID }
        statusAtOrderRebuild.removeValue(forKey: itemID)
        repairProjectFilter()
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
            if let floor = removalVersionFloors[snapshot.item.id] {
                guard snapshot.entity_version > floor else { continue }
                removalVersionFloors.removeValue(forKey: snapshot.item.id)
            }
            if let existing = snapshotsByID[snapshot.item.id],
                existing.entity_version > snapshot.entity_version
            {
                replaced[snapshot.item.id] = existing
            } else {
                replaced[snapshot.item.id] = snapshot
            }
        }
        snapshotsByID = replaced
        serverOrder = snapshots.map(\.item.id).filter { replaced[$0] != nil }
        captureStatusesForCurrentOrder()
        repairProjectFilter()
        loadState = .loaded
    }

    public func replaceAllConversations(
        with snapshots: [Components.Schemas.ConversationSnapshot]
    ) {
        var replaced: [String: Components.Schemas.ConversationSnapshot] = [:]
        for snapshot in snapshots {
            let id = snapshot.conversation.id
            if let existing = conversationsByID[id],
                existing.entity_version > snapshot.entity_version
            {
                replaced[id] = existing
            } else {
                replaced[id] = snapshot
            }
        }
        conversationsByID = replaced
    }

    public func conversation(
        for item: Components.Schemas.AttentionItem
    ) -> Components.Schemas.ConversationSnapshot? {
        guard let id = item.conversation_id else { return nil }
        return conversationsByID[id]
    }

    /// Drops every cached row (an epoch change made them meaningless,
    /// plan §5.14 sync test 8). The pending-command ledger survives:
    /// commitment is epoch-independent, and only a verbatim resend can
    /// settle an ambiguous command against the restored daemon.
    public func discardSnapshots() {
        snapshotsByID = [:]
        conversationsByID = [:]
        serverOrder = []
        statusAtOrderRebuild = [:]
        projectID = nil
        removalVersionFloors = [:]
        loadState = .idle
        // A new epoch: every prior per-item validation is now stale, even
        // for rows a subsequent bootstrap repopulates (issue #162).
        cacheGeneration += 1
        // The epoch can be a daemon restore that rolled the capability row
        // back, so the last-registered fingerprint no longer proves a live
        // server-side contract. Clearing it forces one idempotent
        // re-registration next round instead of skipping it and 409-ing every
        // action-surface request (plan §8).
        registeredCapabilityFingerprint = nil
    }

    private func captureStatusesForCurrentOrder() {
        statusAtOrderRebuild = [:]
        for id in serverOrder {
            statusAtOrderRebuild[id] = snapshotsByID[id]?.item.status
        }
    }

    public func repairProjectFilter() {
        if let pendingLaunchProjectID {
            if projects.contains(pendingLaunchProjectID) {
                projectID = pendingLaunchProjectID
            } else if projects.isEmpty, loadState != .loaded {
                projectID = pendingLaunchProjectID
            } else {
                projectID = nil
            }
            return
        }
        if let projectID, !projects.contains(projectID) {
            self.projectID = nil
        }
    }

    func applyLaunchProjectFilter(_ projectID: String) {
        pendingLaunchProjectID = projectID
        self.projectID = projectID
        repairProjectFilter()
        if freshness == .fresh {
            finishLaunchProjectRepair()
        }
    }

    func selectProjectFilter(_ projectID: String?) {
        pendingLaunchProjectID = nil
        self.projectID = projectID
    }

    func finishLaunchProjectRepair() {
        guard let pendingLaunchProjectID else {
            repairProjectFilter()
            return
        }
        if projects.contains(pendingLaunchProjectID) {
            projectID = pendingLaunchProjectID
        } else {
            projectID = nil
            self.pendingLaunchProjectID = nil
        }
    }

    /// Rows in server order, for cache persistence.
    public var orderedSnapshots: [Components.Schemas.AttentionItemSnapshot] {
        serverOrder.compactMap { snapshotsByID[$0] }
    }

    public var orderedConversations: [Components.Schemas.ConversationSnapshot] {
        conversationsByID.keys.sorted().compactMap { conversationsByID[$0] }
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

    /// Claims an item's process-local navigation slot. The reservation is
    /// deliberately non-durable and contains no command: only a successful
    /// opener may proceed to the replay-safe pending-command registration.
    func reserveNavigation(itemID: String) -> Bool {
        guard pendingCommandsByItemID[itemID] == nil else { return false }
        return navigationReservations.insert(itemID).inserted
    }

    func releaseNavigation(itemID: String) {
        navigationReservations.remove(itemID)
    }

    func isNavigationReserved(itemID: String) -> Bool {
        navigationReservations.contains(itemID)
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
        guard pendingCommandsByItemID[itemID] == nil,
            !navigationReservations.contains(itemID)
        else { return .slotOccupied }
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

    /// Appends one comprehension event to the best-effort queue, stamping a
    /// fresh client event id and the next per-device sequence (plan §8). The
    /// caller supplies the surface and command references the event kind
    /// requires; the daemon validates the by-kind contract.
    public func enqueueComprehensionEvent(
        kind: Components.Schemas.ComprehensionEventKind,
        itemID: String,
        itemDecisionSurfaceDigest: String,
        decisionActionSurfaceDigest: String?,
        commandID: String?
    ) {
        comprehensionSequence += 1
        let input = Components.Schemas.ComprehensionEventInput(
            item_id: itemID,
            kind: kind,
            item_decision_surface_digest: itemDecisionSurfaceDigest,
            decision_action_surface_digest: decisionActionSurfaceDigest,
            command_id: commandID,
            occurred_at: Date(),
            sequence: comprehensionSequence)
        comprehensionQueue.append(
            QueuedComprehensionEvent(eventID: UUID().uuidString, input: input))
        comprehensionObserver?()
    }

    /// Drains the queue best-effort: each event is sent through the typed
    /// client and removed on success. A definitive client rejection (a 4xx: a
    /// malformed or unbacked event, a missing item/device) drops the poison
    /// entry so the queue stops looping on it; a transient 5xx and a transport
    /// outage keep it for the next round.
    public func drainComprehensionEvents() async {
        guard !comprehensionQueue.isEmpty else { return }
        // Remove only the events this drain definitively settled, never a whole
        // snapshot of the queue. Actor isolation yields at each network await,
        // so a concurrent drain or enqueue may add or settle events in between;
        // replacing the live queue with a stale `remaining` snapshot could drop
        // an event another drain kept after a transport failure. A settled id is
        // one the daemon accepted (idempotent by event id) or definitively
        // rejected as poison.
        var settled: Set<String> = []
        for queued in comprehensionQueue {
            do {
                let output = try await client.recordComprehensionEvent(
                    path: .init(event_id: queued.eventID),
                    body: .json(queued.input)
                )
                switch output {
                case .ok:
                    settled.insert(queued.eventID)
                case .badRequest, .forbidden, .notFound:
                    // A definitive client rejection: the same event will never
                    // be accepted, so drop it rather than loop forever.
                    settled.insert(queued.eventID)
                case .undocumented(let statusCode, _):
                    // Drop only definitive client errors (4xx). A transient 5xx
                    // is retryable, so leave it queued for the next drain.
                    if (400..<500).contains(statusCode) {
                        settled.insert(queued.eventID)
                    }
                }
            } catch {
                // A transport outage (no HTTP response) leaves the event queued
                // for the next round.
            }
        }
        if !settled.isEmpty {
            comprehensionQueue.removeAll { settled.contains($0.eventID) }
            comprehensionObserver?()
        }
    }

    /// Restores the persisted telemetry queue, sequence, and registered
    /// fingerprint at relaunch, but only when the cache belongs to this device.
    /// A deployment cache reused across a re-pair carries the prior device's
    /// state, and a comprehension event records no device id of its own, so a
    /// foreign queue would resend under the new bearer credential and a foreign
    /// fingerprint would suppress this device's own registration. Both are
    /// dropped on a device mismatch, mirroring the pending-command ledger's
    /// per-entry device guard (plan §8).
    public func restoreComprehension(
        queue: [QueuedComprehensionEvent], sequence: Int, fingerprint: String?,
        owningDeviceID: String?
    ) {
        guard owningDeviceID == device.deviceID else { return }
        comprehensionQueue = queue
        comprehensionSequence = max(comprehensionSequence, sequence)
        registeredCapabilityFingerprint = fingerprint
    }

    private func sortKey(
        _ snapshot: Components.Schemas.AttentionItemSnapshot, index: Int,
        status: Components.Schemas.ItemStatus
    ) -> (Int, Int, Int) {
        let statusRank = status == .open ? 0 : 1
        let priorityRank: Int
        switch snapshot.item.priority {
        case .urgent: priorityRank = 0
        case .high: priorityRank = 1
        case .normal: priorityRank = 2
        case .low: priorityRank = 3
        }
        return (statusRank, priorityRank, index)
    }

    private func filteredRows(
        in scope: Scope,
        projectID: String?
    ) -> [(Int, Components.Schemas.AttentionItemSnapshot)] {
        serverOrder.enumerated().compactMap { index, id in
            snapshotsByID[id].map { (index, $0) }
        }.filter { _, snapshot in
            let status = statusAtOrderRebuild[snapshot.item.id] ?? snapshot.item.status
            let isInScope =
                switch scope {
                case .open: status == .open
                case .resolved: status != .open
                case .all: true
                }
            return isInScope && (projectID == nil || snapshot.item.project_id == projectID)
        }
    }
}
