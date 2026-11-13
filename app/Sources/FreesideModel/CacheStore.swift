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
    public var pendingTaskStops: [String: PendingTaskStop]?
    public var stopDaemonID: String?
    /// The unresolved routing policy change, and the daemon it was sent to,
    /// kept like a pending Stop so its retry survives a relaunch.
    public var pendingRoutingPolicyChange: PendingRoutingPolicyChange?
    public var routingDaemonID: String?
    /// The unresolved workflow policy change, and the daemon it was sent
    /// to, kept the same way. Additive: a cache without it loads none.
    public var pendingWorkflowPolicyChange: PendingWorkflowPolicyChange?
    public var workflowDaemonID: String?
    /// The unresolved usage command, and the daemon it was sent to, kept
    /// the same way. Additive: a cache without it loads none.
    public var pendingUsageCommand: PendingUsageCommand?
    public var usageDaemonID: String?
    /// The unresolved move of a parked task, and the daemon it was sent to,
    /// kept the same way. Additive: a cache without it loads none.
    public var pendingParkedTaskMove: PendingParkedTaskMove?
    public var moveDaemonID: String?
    /// The unresolved change of a task's line, and the daemon it was sent
    /// to, kept the same way. Additive: a cache without it loads none.
    public var pendingTaskLineChange: PendingTaskLineChange?
    public var lineChangeDaemonID: String?
    public var pendingTaskSubmissions: [String: Components.Schemas.ClientCommand]?
    public var submissionDaemonID: String?
    public var cursors: SyncCursors?
    public var attentionItems: [Components.Schemas.AttentionItemSnapshot]
    public var conversations: [Components.Schemas.ConversationSnapshot]
    public var runs: [Components.Schemas.RunSnapshot]
    public var schedules: [Components.Schemas.ScheduleSnapshot]
    /// The daemon's unattended-admission state as of `cursors`; nil when no
    /// bootstrap has supplied one, which renders no indicator rather than
    /// guessing "open".
    public var unattendedOperation: Components.Schemas.UnattendedOperationSnapshot?
    /// The committed agent tree's catalog as of `cursors`; nil when no
    /// bootstrap has supplied one or the daemon holds no tree. It is additive:
    /// a cache without it loads nil, and the next heartbeat's lineup revision
    /// triggers the bootstrap that fills it, so no format bump.
    public var agentCatalog: Components.Schemas.AgentCatalog?
    /// The routing policy as of `cursors`; nil when none was ever set.
    public var routingPolicy: Components.Schemas.RoutingPolicySnapshot?
    /// The workflow policy layers as of `cursors`; nil in a cache written
    /// before the Workflow pane, which the next bootstrap fills.
    public var workflowPolicy: Components.Schemas.WorkflowPolicySnapshot?
    /// Every usage pool's record as of `cursors`.
    public var usagePools: [Components.Schemas.UsagePoolSnapshot]
    public var runTimelines: [Components.Schemas.RunTimeline]
    public var tasks: [Components.Schemas.TaskSnapshot]
    public var taskTimelines: [Components.Schemas.TaskTimeline]
    /// The persisted ledger by item id; nil when absent or unreadable.
    /// The ledger is client mutation state, not readable cache, so it
    /// degrades independently: a corrupt section costs the retry
    /// affordance, never the rows and cursors around it.
    public var pendingCommands: [String: InboxStore.PendingCommandEntry]?
    /// The best-effort comprehension-telemetry queue and its state (plan §8),
    /// persisted alongside the ledger and degrading independently: like the
    /// ledger they are additive optionals, so an older client ignores them and
    /// a newer client tolerates their absence.
    public var comprehensionQueue: [InboxStore.QueuedComprehensionEvent]?
    public var comprehensionSequence: Int?
    public var registeredCapabilityFingerprint: String?
    /// The device that owns the comprehension queue and fingerprint above. A
    /// comprehension event carries no device id, so a cache reused across a
    /// re-pair is told apart only by this owner: a mismatch on restore drops
    /// both device-scoped sections rather than resending or suppressing under
    /// the new credential (plan §8).
    public var comprehensionDeviceID: String?

    public init(
        cursors: SyncCursors?,
        attentionItems: [Components.Schemas.AttentionItemSnapshot],
        conversations: [Components.Schemas.ConversationSnapshot] = [],
        runs: [Components.Schemas.RunSnapshot] = [],
        schedules: [Components.Schemas.ScheduleSnapshot] = [],
        unattendedOperation: Components.Schemas.UnattendedOperationSnapshot? = nil,
        agentCatalog: Components.Schemas.AgentCatalog? = nil,
        routingPolicy: Components.Schemas.RoutingPolicySnapshot? = nil,
        workflowPolicy: Components.Schemas.WorkflowPolicySnapshot? = nil,
        usagePools: [Components.Schemas.UsagePoolSnapshot] = [],
        runTimelines: [Components.Schemas.RunTimeline] = [],
        tasks: [Components.Schemas.TaskSnapshot] = [],
        taskTimelines: [Components.Schemas.TaskTimeline] = [],
        pendingCommands: [String: InboxStore.PendingCommandEntry]? = nil,
        pendingTaskSubmissions: [String: Components.Schemas.ClientCommand]? = nil,
        submissionDaemonID: String? = nil,
        pendingTaskStops: [String: PendingTaskStop]? = nil,
        stopDaemonID: String? = nil,
        pendingRoutingPolicyChange: PendingRoutingPolicyChange? = nil,
        routingDaemonID: String? = nil,
        pendingWorkflowPolicyChange: PendingWorkflowPolicyChange? = nil,
        workflowDaemonID: String? = nil,
        pendingUsageCommand: PendingUsageCommand? = nil,
        usageDaemonID: String? = nil,
        pendingParkedTaskMove: PendingParkedTaskMove? = nil,
        moveDaemonID: String? = nil,
        pendingTaskLineChange: PendingTaskLineChange? = nil,
        lineChangeDaemonID: String? = nil,
        comprehensionQueue: [InboxStore.QueuedComprehensionEvent]? = nil,
        comprehensionSequence: Int? = nil,
        registeredCapabilityFingerprint: String? = nil,
        comprehensionDeviceID: String? = nil
    ) {
        self.cursors = cursors
        self.attentionItems = attentionItems
        self.conversations = conversations
        self.runs = runs
        self.schedules = schedules
        self.unattendedOperation = unattendedOperation
        self.agentCatalog = agentCatalog
        self.routingPolicy = routingPolicy
        self.workflowPolicy = workflowPolicy
        self.usagePools = usagePools
        self.runTimelines = runTimelines
        self.tasks = tasks
        self.taskTimelines = taskTimelines
        self.pendingCommands = pendingCommands
        self.pendingTaskSubmissions = pendingTaskSubmissions
        self.submissionDaemonID = submissionDaemonID
        self.pendingTaskStops = pendingTaskStops
        self.stopDaemonID = stopDaemonID
        self.pendingRoutingPolicyChange = pendingRoutingPolicyChange
        self.routingDaemonID = routingDaemonID
        self.pendingWorkflowPolicyChange = pendingWorkflowPolicyChange
        self.workflowDaemonID = workflowDaemonID
        self.pendingUsageCommand = pendingUsageCommand
        self.usageDaemonID = usageDaemonID
        self.pendingParkedTaskMove = pendingParkedTaskMove
        self.moveDaemonID = moveDaemonID
        self.pendingTaskLineChange = pendingTaskLineChange
        self.lineChangeDaemonID = lineChangeDaemonID
        self.comprehensionQueue = comprehensionQueue
        self.comprehensionSequence = comprehensionSequence
        self.registeredCapabilityFingerprint = registeredCapabilityFingerprint
        self.comprehensionDeviceID = comprehensionDeviceID
    }

    /// Set on the decoder for a file the load keeps only the ledger of.
    static let ledgerOnly: CodingUserInfoKey = {
        guard let key = CodingUserInfoKey(rawValue: "freeside.cache.ledgerOnly") else {
            preconditionFailure("a non-empty raw value always makes a key")
        }
        return key
    }()

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        // A file the load keeps only the ledger of skips its snapshots, which
        // another contract may have written in a shape this build no longer
        // decodes, such as one lacking a field this build requires.
        let ledgerOnly = decoder.userInfo[Self.ledgerOnly] as? Bool == true
        cursors = ledgerOnly ? nil : try container.decodeIfPresent(SyncCursors.self, forKey: .cursors)
        attentionItems =
            ledgerOnly
            ? []
            : try container.decode(
                [Components.Schemas.AttentionItemSnapshot].self, forKey: .attentionItems)
        func snapshots<T: Decodable>(_ type: [T].Type, _ key: CodingKeys) throws -> [T] {
            ledgerOnly ? [] : try container.decodeIfPresent(type, forKey: key) ?? []
        }
        func snapshot<T: Decodable>(_ type: T.Type, _ key: CodingKeys) throws -> T? {
            ledgerOnly ? nil : try container.decodeIfPresent(type, forKey: key)
        }
        conversations = try snapshots([Components.Schemas.ConversationSnapshot].self, .conversations)
        runs = try snapshots([Components.Schemas.RunSnapshot].self, .runs)
        schedules = try snapshots([Components.Schemas.ScheduleSnapshot].self, .schedules)
        unattendedOperation = try snapshot(
            Components.Schemas.UnattendedOperationSnapshot.self, .unattendedOperation)
        agentCatalog = try snapshot(Components.Schemas.AgentCatalog.self, .agentCatalog)
        routingPolicy = try snapshot(Components.Schemas.RoutingPolicySnapshot.self, .routingPolicy)
        workflowPolicy = try snapshot(Components.Schemas.WorkflowPolicySnapshot.self, .workflowPolicy)
        usagePools = try snapshots([Components.Schemas.UsagePoolSnapshot].self, .usagePools)
        runTimelines = try snapshots([Components.Schemas.RunTimeline].self, .runTimelines)
        tasks = try snapshots([Components.Schemas.TaskSnapshot].self, .tasks)
        taskTimelines = try snapshots([Components.Schemas.TaskTimeline].self, .taskTimelines)
        // The ledger section is forgiving on its own: an undecodable
        // section loads as absent without failing the whole file.
        pendingCommands = try? container.decodeIfPresent(
            [String: InboxStore.PendingCommandEntry].self, forKey: .pendingCommands)
        pendingTaskSubmissions = try? container.decodeIfPresent(
            [String: Components.Schemas.ClientCommand].self, forKey: .pendingTaskSubmissions)
        submissionDaemonID = try? container.decodeIfPresent(String.self, forKey: .submissionDaemonID)
        pendingTaskStops = try? container.decodeIfPresent([String: PendingTaskStop].self, forKey: .pendingTaskStops)
        stopDaemonID = try? container.decodeIfPresent(String.self, forKey: .stopDaemonID)
        pendingRoutingPolicyChange = try? container.decodeIfPresent(
            PendingRoutingPolicyChange.self, forKey: .pendingRoutingPolicyChange)
        routingDaemonID = try? container.decodeIfPresent(String.self, forKey: .routingDaemonID)
        pendingWorkflowPolicyChange = try? container.decodeIfPresent(
            PendingWorkflowPolicyChange.self, forKey: .pendingWorkflowPolicyChange)
        workflowDaemonID = try? container.decodeIfPresent(String.self, forKey: .workflowDaemonID)
        pendingUsageCommand = try? container.decodeIfPresent(
            PendingUsageCommand.self, forKey: .pendingUsageCommand)
        usageDaemonID = try? container.decodeIfPresent(String.self, forKey: .usageDaemonID)
        pendingParkedTaskMove = try? container.decodeIfPresent(
            PendingParkedTaskMove.self, forKey: .pendingParkedTaskMove)
        moveDaemonID = try? container.decodeIfPresent(String.self, forKey: .moveDaemonID)
        pendingTaskLineChange = try? container.decodeIfPresent(
            PendingTaskLineChange.self, forKey: .pendingTaskLineChange)
        lineChangeDaemonID = try? container.decodeIfPresent(String.self, forKey: .lineChangeDaemonID)
        // The telemetry queue and its state degrade independently too: an
        // undecodable section loads absent, costing at most queued events.
        comprehensionQueue = try? container.decodeIfPresent(
            [InboxStore.QueuedComprehensionEvent].self, forKey: .comprehensionQueue)
        comprehensionSequence = try? container.decodeIfPresent(
            Int.self, forKey: .comprehensionSequence)
        registeredCapabilityFingerprint = try? container.decodeIfPresent(
            String.self, forKey: .registeredCapabilityFingerprint)
        comprehensionDeviceID = try? container.decodeIfPresent(
            String.self, forKey: .comprehensionDeviceID)
    }
}

/// Persistence for the disposable read cache (plan §5.14: the daemon is
/// sole authority; client databases are caches a client can always
/// rebuild by bootstrapping). Loading is forgiving — anything unreadable
/// is absent. Saving now *reports* failure by throwing; whether that is
/// fatal is the caller's call: the read-cache and cursor paths stay
/// best-effort (a lost save costs one bootstrap, never correctness), but
/// registering a pending command treats a failed save as a hard
/// precondition, because an unpersisted command_id cannot be replayed
/// after a relaunch (plan §5.14 sync test 4, issue #163).
public protocol CacheStore: Sendable {
    func load() -> CachedState?
    func save(_ state: CachedState) throws
    func discard()
}

/// One atomic JSON file in the app's protected container. Rejected
/// heavier stores (SwiftData, SQLite): the whole cache is a small
/// Codable payload rebuilt wholesale on every bootstrap, and epoch
/// discard is `rm`; a database earns nothing here.
public struct DiskCacheStore: CacheStore {
    /// Bumped when persisted snapshots change incompatibly. A pre-ledger
    /// file loads absent; formats 2 to 6 preserve their independent ledger
    /// and telemetry sections while invalidating snapshots and forcing a
    /// bootstrap. 2: cursors became optional and the pending-command ledger
    /// joined (#115). 3: run and schedule snapshots joined. 4: conversation
    /// snapshots joined. 5: task snapshots and task timelines joined. 6: the
    /// unattended-admission state joined; keeping a format 5 cursor would let
    /// an unchanged revision skip the bootstrap that supplies it, so a
    /// stopped daemon would show no indicator. 7: the routing policy joined,
    /// for the same reason: a format 6 cursor would skip the bootstrap that
    /// supplies it and show no policy where one is set. 8: the usage pools
    /// joined, for the same reason: a format 7 cursor would show no pools
    /// until a write moved the revision. 9: an artifact-sourced task's
    /// source joined its required work-item digest; a format 8 file's
    /// artifact-sourced tasks lack it, and its cursor would skip the
    /// bootstrap that supplies them. 10: the agent catalog gained the
    /// enrollments and each agent's selection (#2598); a format 9 cursor
    /// would skip the bootstrap that supplies them. 11: alternate-agent offers
    /// name enrollments (#2808); a format 10 file's offers named agents, and
    /// its cursor would skip the bootstrap that restores them. 12: the
    /// routing policy's roles and passed-over marks gained their enrollment
    /// form (#2807); an older snapshot holds a policy the interim wire
    /// rendered as agent names, and its cursor would skip the bootstrap that
    /// replaces it. An unsettled routing change from before format 12 orders
    /// names the daemon now refuses, so it fails its own section's decode
    /// and loads absent. 13: the file records the contract digest it was
    /// written under (#1266), and a file from another contract keeps only its
    /// epoch-independent state, as an older format does. Responses tolerate
    /// additive change, so a snapshot from an older contract still decodes,
    /// but it lacks what the newer contract added and may hold `unknown`
    /// where this build now knows the value; its cursor would skip the
    /// bootstrap that supplies both. The digest moves with every contract
    /// change, so such a change needs no format bump of its own. A
    /// pre-current cache cannot claim freshness while durable rows from a
    /// newer client surface are absent.
    static let format = 13

    /// The format and contract a file was written under, read before the
    /// rest so a file the load keeps only the ledger of never has its
    /// snapshots decoded.
    private struct CacheHeader: Decodable {
        var format: Int
        var contract: String?
    }

    private struct CacheFile: Codable {
        var format: Int
        /// The contract digest the file was written under; absent before
        /// format 13.
        var contract: String?
        var state: CachedState
    }

    private let fileURL: URL

    public init(directory: URL) {
        fileURL = directory.appendingPathComponent("cache.json")
    }

    public func load() -> CachedState? {
        guard let raw = try? Data(contentsOf: fileURL) else { return nil }
        let data = Self.migratingLegacyEncodings(raw)
        guard let header = try? Self.decoder.decode(CacheHeader.self, from: data) else { return nil }
        let current = header.format == Self.format && header.contract == APIContract.digest
        guard let file = try? (current ? Self.decoder : Self.ledgerDecoder).decode(CacheFile.self, from: data)
        else { return nil }
        switch file.format {
        case Self.format where file.contract == APIContract.digest:
            return file.state
        case 2...Self.format:
            // Formats 2 to 12, and a current-format file written under another
            // contract, already carried retryable commands, but predate later
            // durable read surfaces. Keep only the epoch-independent
            // client state: the ledger, because losing a command ID could
            // duplicate an operator decision after relaunch, and the
            // telemetry queue and its registration (absent before format 4),
            // because a queued event should drain rather than vanish. A
            // format 5 file may also hold unsettled task submissions and
            // stops, a format 7 file an unsettled routing change, and later
            // files an unsettled usage command, parked move, or line change,
            // which are the same kind of state. Their cursors cannot scope an
            // incomplete cache.
            return CachedState(
                cursors: nil,
                attentionItems: [],
                pendingCommands: file.state.pendingCommands,
                pendingTaskSubmissions: file.state.pendingTaskSubmissions,
                submissionDaemonID: file.state.submissionDaemonID,
                pendingTaskStops: file.state.pendingTaskStops,
                stopDaemonID: file.state.stopDaemonID,
                pendingRoutingPolicyChange: file.state.pendingRoutingPolicyChange,
                routingDaemonID: file.state.routingDaemonID,
                pendingWorkflowPolicyChange: file.state.pendingWorkflowPolicyChange,
                workflowDaemonID: file.state.workflowDaemonID,
                pendingUsageCommand: file.state.pendingUsageCommand,
                usageDaemonID: file.state.usageDaemonID,
                pendingParkedTaskMove: file.state.pendingParkedTaskMove,
                moveDaemonID: file.state.moveDaemonID,
                pendingTaskLineChange: file.state.pendingTaskLineChange,
                lineChangeDaemonID: file.state.lineChangeDaemonID,
                comprehensionQueue: file.state.comprehensionQueue,
                comprehensionSequence: file.state.comprehensionSequence,
                registeredCapabilityFingerprint: file.state.registeredCapabilityFingerprint,
                comprehensionDeviceID: file.state.comprehensionDeviceID
            )
        default:
            return nil
        }
    }

    /// Rewrites persisted tokens that a later schema change would otherwise
    /// reject or silently drop, before the strict generated decoder runs, so an
    /// in-place upgrade keeps the retryable command ledger and the cached
    /// attention snapshots intact (plan §5.14 sync test 4, #115). Each
    /// translation is anchored to a structural location, never a blind string
    /// swap, and the bytes are returned unchanged when nothing matched or the
    /// shape is unexpected (a corrupt file still fails the normal decode).
    ///
    /// 1. Legacy ledger entries predate the client-command payload's
    ///    `kind`-discriminated union (submit_task). A bare, `kind`-less payload
    ///    was a decision, so the discriminator is injected as `decision`;
    ///    without it `PendingCommandEntry` decoding fails and the whole ledger
    ///    with its retryable command IDs is dropped.
    /// 2. The `run_proposal` -> `task_proposal` attention-vocabulary rename
    ///    (#1210) changed a persisted enum value. `AttentionType` decodes
    ///    strictly, so a cached `run_proposal` snapshot would throw and discard
    ///    the entire cache, ledger included; the item `type` value is
    ///    translated in place.
    /// 3. The same rename changed the `run_proposal_revision` decision-payload
    ///    key to `task_proposal_revision`. The decoder ignores the unknown old
    ///    key, so a committed start_with_changes command would silently reload
    ///    without its revision and fail its verbatim retry; the key is renamed
    ///    in place.
    /// 4. Plan revision 97 removed the `agent_attended_mark` admission check
    ///    (#2265). `AdmissionCheck` decodes strictly, so a cached run hold
    ///    refused under it would throw and discard the cache, ledger
    ///    included. The hold's detail is dropped, the shape the daemon now
    ///    serves for such a hold, in run timelines and in task timelines'
    ///    run sections.
    /// 5. Run timelines gained the required `admissions` list (#979). A cached
    ///    timeline without it would throw and discard the cache, ledger
    ///    included. It is dropped rather than given an empty list, which
    ///    would claim no invocation was admitted; the run's timeline is
    ///    fetched again when it is opened.
    /// 6. Artifact-sourced task sources gained the required
    ///    `work_item_artifact_digest` (#2610). A format 8 cached task on that
    ///    arm would throw and discard the cache, ledger included. The task is
    ///    dropped; the format bump discards the rest of that file's snapshots
    ///    and its cursors, so the bootstrap restores it with its digest.
    /// 7. Plan revision 110 retired the `base_advance_watch` schedule kind
    ///    (#2645). `ScheduleKind` decodes strictly, so a cached schedule of
    ///    that kind would throw and discard the cache, ledger included. The
    ///    schedule is dropped, matching the daemon, which deletes those rows.
    /// 8. A client's task lines became selections (#2598). A cached agent
    ///    catalog without `enrollments` would throw and discard the cache,
    ///    ledger included; it is dropped, and the format bump makes the
    ///    bootstrap restore it. A saved command or submission whose lines,
    ///    or line change, name an agent would fail its section's decode and
    ///    drop every entry in it; only that entry is dropped, since the
    ///    daemon now refuses a name from a client, so its verbatim retry
    ///    could only be refused.
    /// 9. Alternate-agent offers and choices name enrollments (#2808). A
    ///    cached offer naming an agent would throw and discard the cache,
    ///    ledger included; every offer list holding one is nulled, and the
    ///    format bump makes the bootstrap restore it. A saved
    ///    retry_with_alternate_agent decision or parked move naming an agent
    ///    would fail its section's decode; only that entry is dropped, since
    ///    the daemon refuses a name now, so its verbatim retry could only be
    ///    refused.
    /// 10. The agent catalog lost its `agents` (#2599). A cached catalog
    ///    still carrying them would throw and discard the cache, ledger
    ///    included; it is dropped, and the next heartbeat's lineup revision
    ///    differs from the absent catalog's, so the bootstrap restores it.
    private static func migratingLegacyEncodings(_ data: Data) -> Data {
        guard var root = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any],
            var state = root["state"] as? [String: Any]
        else { return data }
        var changed = false

        if var pending = state["pendingCommands"] as? [String: Any] {
            var pendingChanged = false
            for (itemID, entryValue) in pending {
                guard var entry = entryValue as? [String: Any],
                    var command = entry["command"] as? [String: Any],
                    var payload = command["payload"] as? [String: Any]
                else { continue }
                var payloadChanged = false
                if payload["kind"] == nil {
                    payload["kind"] = "decision"
                    payloadChanged = true
                }
                if let revision = payload.removeValue(forKey: "run_proposal_revision") {
                    payload["task_proposal_revision"] = revision
                    payloadChanged = true
                }
                guard payloadChanged else { continue }
                command["payload"] = payload
                entry["command"] = command
                pending[itemID] = entry
                pendingChanged = true
            }
            if pendingChanged {
                state["pendingCommands"] = pending
                changed = true
            }
        }

        if var snapshots = state["attentionItems"] as? [[String: Any]] {
            var snapshotsChanged = false
            for index in snapshots.indices {
                guard var item = snapshots[index]["item"] as? [String: Any],
                    item["type"] as? String == "run_proposal"
                else { continue }
                item["type"] = "task_proposal"
                snapshots[index]["item"] = item
                snapshotsChanged = true
            }
            if snapshotsChanged {
                state["attentionItems"] = snapshots
                changed = true
            }
        }

        if var timelines = state["runTimelines"] as? [[String: Any]] {
            var timelinesChanged = false
            let current = timelines.filter { $0["admissions"] != nil }
            if current.count != timelines.count {
                timelines = current
                timelinesChanged = true
            }
            for index in timelines.indices {
                guard var hold = timelines[index]["hold"] as? [String: Any],
                    droppingRetiredHoldDetail(&hold)
                else { continue }
                timelines[index]["hold"] = hold
                timelinesChanged = true
            }
            if timelinesChanged {
                state["runTimelines"] = timelines
                changed = true
            }
        }

        if var timelines = state["taskTimelines"] as? [[String: Any]] {
            var timelinesChanged = false
            for timelineIndex in timelines.indices {
                guard var sections = timelines[timelineIndex]["sections"] as? [[String: Any]]
                else { continue }
                var sectionsChanged = false
                for sectionIndex in sections.indices {
                    guard var runs = sections[sectionIndex]["runs"] as? [[String: Any]] else { continue }
                    var runsChanged = false
                    for runIndex in runs.indices {
                        guard var hold = runs[runIndex]["hold"] as? [String: Any],
                            droppingRetiredHoldDetail(&hold)
                        else { continue }
                        runs[runIndex]["hold"] = hold
                        runsChanged = true
                    }
                    guard runsChanged else { continue }
                    sections[sectionIndex]["runs"] = runs
                    sectionsChanged = true
                }
                guard sectionsChanged else { continue }
                timelines[timelineIndex]["sections"] = sections
                timelinesChanged = true
            }
            if timelinesChanged {
                state["taskTimelines"] = timelines
                changed = true
            }
        }

        if let tasks = state["tasks"] as? [[String: Any]] {
            let current = tasks.filter { snapshot in
                guard let task = snapshot["task"] as? [String: Any],
                    let source = task["source"] as? [String: Any],
                    source["kind"] as? String == "work_item_artifact"
                else { return true }
                return source["work_item_artifact_digest"] != nil
            }
            if current.count != tasks.count {
                state["tasks"] = current
                changed = true
            }
        }

        if let schedules = state["schedules"] as? [[String: Any]] {
            let current = schedules.filter { snapshot in
                (snapshot["schedule"] as? [String: Any])?["kind"] as? String != "base_advance_watch"
            }
            if current.count != schedules.count {
                state["schedules"] = current
                changed = true
            }
        }

        if let catalog = state["agentCatalog"] as? [String: Any],
            catalog["enrollments"] == nil || catalog["agents"] != nil
        {
            state.removeValue(forKey: "agentCatalog")
            changed = true
        }
        for section in ["pendingCommands", "pendingTaskSubmissions"] {
            guard let entries = state[section] as? [String: Any] else { continue }
            let current = entries.filter { !namesAnAgentLine($0.value) && !namesAnAlternateAgent($0.value) }
            if current.count != entries.count {
                state[section] = current
                changed = true
            }
        }
        if let move = state["pendingParkedTaskMove"], namesAnAlternateAgent(move) {
            state.removeValue(forKey: "pendingParkedTaskMove")
            changed = true
        }
        for section in ["attentionItems", "runs", "runTimelines", "tasks", "taskTimelines"] {
            guard var snapshots = state[section], nullingAgentOffers(&snapshots) else { continue }
            state[section] = snapshots
            changed = true
        }

        guard changed else { return data }
        root["state"] = state
        return (try? JSONSerialization.data(withJSONObject: root)) ?? data
    }

    /// Whether a saved ledger entry or submission carries a task line in the
    /// retired name encoding: `agent` and no `selection` (translation 8). A
    /// ledger entry wraps its command; a submission is the command. A line
    /// change names its agent on the payload itself; one that cleared the
    /// line omitted `agent` and still decodes as the same clear. A parked
    /// move's `agent` is translation 9's.
    private static func namesAnAgentLine(_ value: Any) -> Bool {
        guard let object = value as? [String: Any] else { return false }
        let command = object["command"] as? [String: Any] ?? object
        guard let payload = command["payload"] as? [String: Any] else { return false }
        if payload["kind"] as? String == "change_task_line" {
            return payload["agent"] is String && payload["selection"] == nil
        }
        guard let lines = payload["task_lines"] as? [[String: Any]] else { return false }
        return lines.contains { $0["agent"] != nil && $0["selection"] == nil }
    }

    /// Whether a saved command, or a ledger entry wrapping one, chooses an
    /// alternate by agent name (translation 9): a decision's
    /// `alternate_agent` or a parked move's `agent`.
    private static func namesAnAlternateAgent(_ value: Any) -> Bool {
        guard let object = value as? [String: Any] else { return false }
        let command = object["command"] as? [String: Any] ?? object
        guard let payload = command["payload"] as? [String: Any] else { return false }
        return payload["alternate_agent"] != nil
            || payload["kind"] as? String == "move_parked_task" && payload["agent"] != nil
    }

    /// Nulls every `alternate_agents` list under value that holds an offer
    /// naming an agent (translation 9), reporting whether it did. An offer
    /// list sits on attention items and on quota-window holds, which runs,
    /// timelines, and task positions carry at different depths.
    private static func nullingAgentOffers(_ value: inout Any) -> Bool {
        var changed = false
        if var object = value as? [String: Any] {
            for (key, child) in object {
                if key == "alternate_agents", let offers = child as? [[String: Any]],
                    offers.contains(where: { $0["agent"] != nil })
                {
                    object[key] = NSNull()
                    changed = true
                    continue
                }
                var child = child
                if nullingAgentOffers(&child) {
                    object[key] = child
                    changed = true
                }
            }
            if changed { value = object }
        } else if var array = value as? [Any] {
            for index in array.indices {
                if nullingAgentOffers(&array[index]) { changed = true }
            }
            if changed { value = array }
        }
        return changed
    }

    /// Drops a cached hold's detail when it names the retired
    /// `agent_attended_mark` check (translation 4 above), reporting whether
    /// it did.
    private static func droppingRetiredHoldDetail(_ hold: inout [String: Any]) -> Bool {
        guard let detail = hold["detail"] as? [String: Any],
            detail["check"] as? String == "agent_attended_mark"
        else { return false }
        hold.removeValue(forKey: "detail")
        return true
    }

    public func save(_ state: CachedState) throws {
        let data = try Self.encoder.encode(
            CacheFile(format: Self.format, contract: APIContract.digest, state: state))
        // createDirectory(withIntermediateDirectories:) is a no-op when the
        // directory already exists, so the steady state never throws here.
        try FileManager.default.createDirectory(
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
        try data.write(to: fileURL, options: options)
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

    /// Decodes only the epoch-independent sections, for a file the load
    /// keeps only the ledger of.
    private static let ledgerDecoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        decoder.userInfo[CachedState.ledgerOnly] = true
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

    public func save(_ state: CachedState) throws {
        lock.withLock { self.state = state }
    }

    public func discard() {
        lock.withLock { state = nil }
    }
}
