import Foundation
import HTTPTypes
import OpenAPIRuntime

/// An in-process mock of the daemon API that implements the contract's
/// semantics over in-memory tables, rather than replaying canned bodies:
/// command submission is idempotent by command_id and a stale submission
/// is rejected with the current snapshot as the replacement (plan §5.14
/// sync tests 2 and 4); the sync envelope carries the epoch/revision
/// cursor (tests 8 and 11); and the device surface pairs, revokes, and
/// authenticates bearer tokens (tests 13-16).
public actor MockServer {
    public struct HealthUnavailableError: Error {}

    /// Test hook run before every response; suspend it to hold a response
    /// open, throw to fail the request.
    public typealias BeforeRespond = @Sendable (_ operationID: String) async throws -> Void
    public typealias CommandResultTransform =
        @Sendable (Components.Schemas.CommandResult) -> Components.Schemas.CommandResult
    public typealias ConversationTransform =
        @Sendable (Components.Schemas.ConversationSnapshot) -> Components.Schemas.ConversationSnapshot
    public typealias BootstrapTransform =
        @Sendable (Components.Schemas.BootstrapSnapshot) -> Components.Schemas.BootstrapSnapshot
    public typealias EffectProposalFactsTransform =
        @Sendable (Components.Schemas.EffectProposalFactsSnapshot) -> Components.Schemas.EffectProposalFactsSnapshot

    /// Thrown from a `beforeRespond` hook to make the mock answer with a
    /// specific HTTP status and a generic error-shaped body, modelling a
    /// reachable daemon whose read failed. Distinct from an arbitrary
    /// thrown error, which the transport lets propagate as a
    /// transport-level outage; this one comes back as an answered
    /// response. A non-2xx code (e.g. 500) surfaces to the generated
    /// client as `.undocumented`; a 2xx code (e.g. 200) pairs a success
    /// status with that error-shaped body, so the client's schema decode
    /// throws — a schema-skew stand-in for an incompatible 200 body.
    public struct ForcedStatus: Error, Sendable {
        public let code: Int
        public init(_ code: Int) { self.code = code }
    }

    /// How the mock authenticates requests: `permissive` trusts every
    /// caller (the pre-device inbox tests), `enforcing` requires an
    /// active paired device's bearer token on everything except pairing,
    /// as the daemon's fail-closed injected authorizer does (#105).
    public enum AuthMode: Sendable {
        case permissive
        case enforcing
    }

    /// A seedable pairing code's lifecycle (plan §5.14 sync test 13):
    /// only a valid code pairs, and consumption is single-winner
    /// (test 14).
    public enum PairingCodeState: Sendable {
        case valid
        case expired
        case consumed
    }

    /// The normalized body the daemon persists and replays against
    /// (signet ClientCommand → domain.NewCommand): the payload and
    /// device fields only, with the digest set canonicalized.
    /// expected_entity_version and the provisional expected_bindings
    /// map are acceptance-time inputs, never part of the recorded body,
    /// so a retry with refreshed expectations still converges.
    struct NormalizedCommand: Equatable {
        let commandID: String
        let deviceID: String
        let itemID: String
        let action: Components.Schemas.Action
        let itemVersion: Int
        let prHeadSHA: String
        let artifactDigests: [String]
        let message: String
        let attachments: [String]

        init(
            _ command: Components.Schemas.ClientCommand,
            _ payload: Components.Schemas.DecisionPayload,
            message: String
        ) {
            commandID = command.command_id
            deviceID = command.device_id
            itemID = payload.item_id
            action = payload.action
            itemVersion = payload.item_version
            prHeadSHA = payload.pr_head_sha
            artifactDigests = Array(Set(payload.artifact_digests)).sorted()
            // Content fields normalize absent to empty (the daemon's record
            // shape); attachment order is authored, so it is compared as
            // sent, never canonicalized.
            self.message = message
            attachments = payload.attachments ?? []
        }
    }

    private var itemsByID: [String: Components.Schemas.AttentionItemSnapshot] = [:]
    private var taskHistoryFixtures: [String: Components.Schemas.TaskTimeline] = [:]
    private var conversationsByID: [String: Components.Schemas.ConversationSnapshot] = [:]
    private var commandsByID: [String: NormalizedCommand] = [:]
    private var resultsByCommandID: [String: Components.Schemas.CommandResult] = [:]
    private var submissionRequestsByID: [String: Components.Schemas.ClientCommand] = [:]
    private var pendingSpecificationReplacements: [String: Components.Schemas.AttentionItemSnapshot] = [:]
    private var pendingSpecificationComments: [String: String] = [:]
    private var proposalFactsByItemID: [String: Components.Schemas.TaskProposalFactsSnapshot] = [:]
    private var effectProposalFactsByItemID: [String: Components.Schemas.EffectProposalFactsSnapshot] = [:]
    private var proposalSnoozesByItemID: [String: Date] = [:]
    /// The operator stop in force: the accepted stop_unattended command and
    /// its accepting instant, cleared only by resume_unattended. It stands in
    /// for the daemon's durable transition log, whose latest row wins.
    private var operatorStop: (commandID: String, since: Date)?
    /// The update drain in force: when it began and whether it was forced.
    /// It stands in for the daemon's update drain log, which never shares a
    /// row with the operator stop, so neither lifts the other.
    private var updateDrain: (since: Date, forced: Bool)?
    private var currentTime = Date(timeIntervalSince1970: 1_786_502_645)
    private var revision: Int64 = 1 {
        didSet { if revision != oldValue { releaseRevisionWaiters() } }
    }
    private var syncEpoch = "mock-epoch" {
        didSet { if syncEpoch != oldValue { releaseRevisionWaiters() } }
    }
    /// Open `getSyncRevision` long polls, each released by a revision or
    /// epoch move, its bound, or its task's cancellation, as the daemon's
    /// `RevisionAfter` is.
    private var revisionWaiters: [UUID: CheckedContinuation<Void, Never>] = [:]
    private var revisionWaiterArrivals: [(count: Int, waiter: CheckedContinuation<Void, Never>)] = []
    private var revisionWaitBound: Duration = .seconds(25)
    /// Every `after` a `getSyncRevision` request carried, nil for the
    /// immediate form, in arrival order.
    public private(set) var receivedRevisionAfters: [Int64?] = []
    private var epochGeneration = 1
    private var healthVersion = "mock"
    private var healthStartedAt = Date(timeIntervalSince1970: 1_725_184_800)
    /// The level and floor `/health` reports, each omitted when nil as a
    /// daemon built before #3109 omits them.
    private var healthCompatibilityLevel: Int? = APIContract.compatibilityLevel
    private var healthCompatibilityFloor: Int? = APIContract.compatibilityFloor
    private var healthContractDigest = APIContract.digest
    private var healthAvailable = true
    /// The features `/health` lists, or nil to omit the field as a daemon
    /// built before the list does. A command whose feature isn't listed is
    /// refused as a kind this daemon doesn't serve.
    private var healthFeatures: [Components.Schemas.DaemonFeature]? = MockServer.servedFeatures
    private var backupHealthy = true
    private var beforeRespond: BeforeRespond?
    private var afterRespond: BeforeRespond?
    private var commandResultTransform: CommandResultTransform?
    private var conversationTransform: ConversationTransform?
    private var bootstrapTransform: BootstrapTransform?
    private var effectProposalFactsTransform: EffectProposalFactsTransform?
    private let automaticallyCompletesAgentWork: Bool
    /// The trusted approved-recipe set the evidence gate re-runs
    /// against; policy state owned by the server, never by the rows.
    private let approvedRecipes: Set<String>
    private let authMode: AuthMode
    private var pairingCodes: [String: PairingCodeState] = [:]
    private let configuredPairingFacts: Components.Schemas.PairingFacts
    private var devicesByID: [String: Components.Schemas.DeviceSnapshot] = [:]
    /// Whole-token lookup: the mock never parses the token's segments,
    /// exactly as the daemon treats it as one opaque credential whose
    /// digest keys the stored record.
    private var deviceIDsByToken: [String: String] = [:]
    private var pairedDeviceCount = 0
    /// When each device last made an authenticated request. Daemon parity:
    /// activity is server bookkeeping outside the synchronized device row,
    /// so recording it moves no entity version and no server revision.
    private var lastSeenByDeviceID: [String: Date] = [:]
    /// Each device's notifications switch. Daemon parity: the switch lives
    /// beside the device row, so setting it moves the server revision but
    /// not the device's entity version, and a device with none is off.
    private var notificationsEnabledByDeviceID: [String: Bool] = [:]
    /// Pairing-grant test hooks. Defaults satisfy the contract; callers can
    /// inject malformed values to exercise client-side returned-object gates.
    private let pairingNtfyServerURL: String
    private let pairingNtfyTopic: String?
    private let pairingDeviceToken: String?
    /// The digest-addressed artifact bytes `getAttachment` serves
    /// (plan §4: cards render image attachments directly from the
    /// artifact store by digest). Content is immutable per digest: an
    /// upload adds a digest that is absent and converges on one that is
    /// present, and nothing rewrites an entry.
    private var attachmentsByDigest: [String: Data]
    /// Delivery rows the receipt surface serves and advances, keyed by
    /// the attempt's full identity as the daemon's composite key is.
    /// Rows only seed through init; the one mutation is the opened
    /// receipt, which advances an existing attempt and never creates one.
    private var deliveriesByKey: [DeliveryKey: Components.Schemas.AttentionDeliverySnapshot] = [:]
    private var runsByID: [String: Components.Schemas.RunSnapshot] = [:]
    private var tasksByID: [String: Components.Schemas.TaskSnapshot] = [:]
    private var schedulesByID: [String: Components.Schemas.ScheduleSnapshot] = [:]
    /// Usage records by pool id. A pool with no record yet keeps
    /// entity_version 0 and is stamped with the serving revision, as the
    /// daemon projects it.
    private var usagePoolsByID: [String: Components.Schemas.UsagePoolSnapshot] = [:]
    /// The current routing policy revision; the daemon stamps it with the
    /// serving revision because the policy row stores none of its own.
    private var routingPolicy: Components.Schemas.RoutingPolicy?
    /// The workflow policy layers, and the projects the daemon serves, which
    /// the projection lists and a project-scope change must name.
    private var workflowPolicy: WorkflowPolicyLayers
    private let workflowProjects: [String]
    /// The agent catalog: the enrollments a line's selection is checked
    /// against, and the committed tree's half. A catalog with a null
    /// lineup_revision models a daemon that holds no tree (the fake driver),
    /// which refuses a policy.
    private var agentCatalog: Components.Schemas.AgentCatalog
    /// Each task role's newest line version, cleared ones included, keyed by
    /// taskLineKey: the wire TaskLine never carries a cleared version, so a
    /// clear lives only here and as the role's absence from the task's
    /// task_lines.
    private var taskLineHeads: [String: (version: Int, id: String, line: Components.Schemas.TaskLine?)] = [:]
    /// The task_line_set events for the line versions this mock wrote, keyed
    /// by task ID: taskLineHeads keeps only the newest version, and the wire
    /// Task carries no history. Seeded lines add none, since the mock can't
    /// name a seeded line's predecessor.
    private var taskLineEvents: [String: [Components.Schemas.TaskEvent]] = [:]
    /// Task roles with a running attempt (setTaskLineRoleRunning).
    private var runningTaskLineRoles: Set<String> = []
    private var timelinesByRunID: [String: Components.Schemas.RunTimeline] = [:]
    // Comprehension telemetry (plan §8): the device capability contracts, the
    // derived action surfaces keyed by their digest, and the recorded events
    // keyed by (device, event_id). Written through their own operations; events
    // never move the revision, mirroring the daemon's internal write path.
    private var capabilityContractsByDevice: [String: Components.Schemas.ClientCapabilityContract] = [:]
    private var actionSurfacesByDigest: [String: Components.Schemas.DecisionActionSurface] = [:]
    private var comprehensionEventsByKey: [String: Components.Schemas.ComprehensionEvent] = [:]

    struct DeliveryKey: Hashable, Comparable {
        let itemID: String
        let deviceID: String
        let channel: String
        let attempt: Int

        init(_ delivery: Components.Schemas.AttentionDelivery) {
            switch delivery {
            case .submitted(let row):
                (itemID, deviceID, channel, attempt) = (row.item_id, row.device_id, row.channel, row.attempt)
            case .channel_accepted(let row):
                (itemID, deviceID, channel, attempt) = (row.item_id, row.device_id, row.channel, row.attempt)
            case .opened(let row):
                (itemID, deviceID, channel, attempt) = (row.item_id, row.device_id, row.channel, row.attempt)
            }
        }

        static func < (lhs: Self, rhs: Self) -> Bool {
            (lhs.itemID, lhs.deviceID, lhs.channel, lhs.attempt)
                < (rhs.itemID, rhs.deviceID, rhs.channel, rhs.attempt)
        }
    }

    public init(
        items: [Components.Schemas.AttentionItemSnapshot] = AttentionFixtures.defaultInbox(),
        conversations: [Components.Schemas.ConversationSnapshot] = AttentionFixtures.defaultConversations(),
        deliveries: [Components.Schemas.AttentionDeliverySnapshot] = [],
        runs: [Components.Schemas.RunSnapshot] = RunFixtures.defaultRuns(),
        tasks: [Components.Schemas.TaskSnapshot] = TaskFixtures.defaultTasks(),
        schedules: [Components.Schemas.ScheduleSnapshot] = RunFixtures.defaultSchedules(),
        timelines: [Components.Schemas.RunTimeline] = RunFixtures.defaultTimelines(),
        taskTimelines: [Components.Schemas.TaskTimeline] = [],
        usagePools: [Components.Schemas.UsagePoolSnapshot] = UsageFixtures.defaultUsagePools(),
        routingPolicy: Components.Schemas.RoutingPolicy? = UsageFixtures.defaultRoutingPolicy(),
        workflowPolicy: WorkflowPolicyLayers = WorkflowPolicyFixtures.defaultLayers(),
        workflowProjects: [String] = [WorkflowPolicyFixtures.projectID],
        agentCatalog: Components.Schemas.AgentCatalog = UsageFixtures.defaultAgentCatalog(),
        approvedRecipes: Set<String> = [AttentionFixtures.approvedRecipeDigest],
        authMode: AuthMode = .permissive,
        pairingCodes: [String: PairingCodeState] = [:],
        pairingFacts: Components.Schemas.PairingFacts = MockServer.pairingFacts,
        pairingNtfyServerURL: String = "https://ntfy.example",
        pairingNtfyTopic: String? = nil,
        pairingDeviceToken: String? = nil,
        attachments: [String: Data] = AttentionFixtures.defaultAttachments(),
        automaticallyCompletesAgentWork: Bool = false
    ) {
        for timeline in taskTimelines {
            taskHistoryFixtures[timeline.task_id] = timeline
        }
        for snapshot in items {
            itemsByID[snapshot.item.id] = snapshot
        }
        for snapshot in conversations {
            conversationsByID[snapshot.conversation.id] = snapshot
        }
        for snapshot in deliveries {
            deliveriesByKey[DeliveryKey(snapshot.delivery)] = snapshot
        }
        for snapshot in schedules {
            schedulesByID[snapshot.schedule.id] = snapshot
        }
        for timeline in timelines {
            timelinesByRunID[timeline.run_id] = timeline
        }
        for snapshot in runs {
            runsByID[snapshot.run.id] = RunFixtures.projectingObservationTimes(
                snapshot, from: timelinesByRunID[snapshot.run.id])
        }
        for snapshot in tasks {
            tasksByID[snapshot.task.id] = snapshot
            // A seeded line is its role's newest version.
            for line in snapshot.task.task_lines {
                taskLineHeads[Self.taskLineKey(line.task_id, line.role)] = (line.version, line.id, line)
            }
        }
        for snapshot in usagePools {
            usagePoolsByID[snapshot.pool.id] = snapshot
        }
        self.routingPolicy = routingPolicy
        self.workflowPolicy = workflowPolicy
        self.workflowProjects = workflowProjects
        self.agentCatalog = agentCatalog
        // The server revision starts at or beyond every seeded snapshot's
        // as_of_revision, so the heartbeat and the next CommandResult can
        // never run backwards relative to what this mock lists.
        revision = max(
            1, items.map(\.as_of_revision).max() ?? 1,
            conversations.map(\.as_of_revision).max() ?? 1,
            deliveries.map(\.as_of_revision).max() ?? 1,
            runs.map(\.as_of_revision).max() ?? 1,
            tasks.map(\.as_of_revision).max() ?? 1,
            schedules.map(\.as_of_revision).max() ?? 1,
            timelines.map(\.as_of_revision).max() ?? 1,
            usagePools.map(\.as_of_revision).max() ?? 1)
        // Seeded delivery rows exist only because the daemon's pipeline
        // would have recorded them, and that pipeline re-derives the
        // item's timing and raises its entity_version in the same write
        // (SubmitDelivery → recomputeItemTiming), so a fixture item's
        // authored timing is never trusted next to seeded rows: seeding
        // applies the same derivation, entity_version bump, and
        // unchanged-summary skip a live write would. item_version stays
        // where the fixture put it, as it does in the daemon: a timing
        // change is not a change to what the operator decides on.
        // Only rows the daemon's PutAttentionDelivery would have accepted
        // fold into the derivation: an invalid seed never reaches
        // recomputeItemTiming in the daemon, and here it stays out of the
        // served aggregates while every delivery-serving read still fails
        // closed on it. The daemon records one SubmitDelivery transaction
        // per row and raises entity_version on each summary-changing
        // recompute, so seeding replays the rows one at a time in
        // composite-key order rather than folding the set in one pretended
        // write.
        let seedRevision = revision
        for itemID in Set(deliveriesByKey.keys.map(\.itemID)) {
            guard var snapshot = itemsByID[itemID] else { continue }
            // Validate the parent snapshot before deriving. The daemon's
            // recomputeItemTiming reconstructs the item through
            // GetAttentionItemSnapshot and fails closed, so seed derivation
            // must not rewrite an invalid parent (bad metadata, inconsistent
            // timing, or an unapproved-recipe evidence gate) into a servable
            // row. An invalid parent is left exactly as seeded; the serve
            // paths' snapshotBreach then fails it closed, as the daemon does.
            if MockContractValidation.snapshotBreach(snapshot, approvedRecipes: approvedRecipes) != nil { continue }
            let rows =
                deliveriesByKey
                .filter { $0.key.itemID == itemID }
                .sorted { $0.key < $1.key }
                .map(\.value)
                .filter {
                    MockContractValidation.deliveryBreach(
                        $0, serverRevision: seedRevision, hasParentItem: true) == nil
                }
                .map(\.delivery)
            for prefixEnd in rows.indices {
                if let next = Self.withDerivedTiming(
                    snapshot, rows: Array(rows.prefix(prefixEnd + 1)), asOf: seedRevision)
                {
                    snapshot = next
                }
            }
            itemsByID[itemID] = snapshot
        }
        // The permissive mock's caller is the fixed mock identity, which
        // never pairs here, so the device list is seeded with it and with
        // the other states the Devices screen renders. An enforcing mock
        // lists only what pairs with it, as the daemon does.
        if case .permissive = authMode {
            for entry in DeviceFixtures.defaultDevices() {
                let id = Self.id(of: entry.device)
                devicesByID[id] = .init(
                    as_of_revision: entry.as_of_revision,
                    entity_version: entry.entity_version,
                    device: entry.device)
                lastSeenByDeviceID[id] = entry.last_seen_at
                notificationsEnabledByDeviceID[id] = entry.notifications_enabled
            }
        }
        self.approvedRecipes = approvedRecipes
        self.authMode = authMode
        self.pairingCodes = pairingCodes
        self.configuredPairingFacts = pairingFacts
        self.pairingNtfyServerURL = pairingNtfyServerURL
        self.pairingNtfyTopic = pairingNtfyTopic
        self.pairingDeviceToken = pairingDeviceToken
        self.automaticallyCompletesAgentWork = automaticallyCompletesAgentWork
        attachmentsByDigest = attachments
    }

    func attachmentBytes(digest: String) -> Data? {
        attachmentsByDigest[digest]
    }

    /// The outcome of a digest-addressed upload (`uploadAttachment`).
    enum AttachmentUpload: Equatable {
        case created, converged, digestMismatch
    }

    /// Stores uploaded bytes under their digest, as the daemon's blob store
    /// does: the body must hash to the path digest, and a re-upload of a
    /// stored digest converges without rewriting it.
    func storeAttachment(digest: String, bytes: Data) -> AttachmentUpload {
        guard MockContractValidation.sha256Digest(of: bytes) == digest else { return .digestMismatch }
        guard attachmentsByDigest[digest] == nil else { return .converged }
        attachmentsByDigest[digest] = bytes
        return .created
    }

    public func setBeforeRespond(_ hook: BeforeRespond?) {
        beforeRespond = hook
    }

    /// Test hook run after the handler applied but before the response
    /// returns; throw to simulate a committed command whose HTTP
    /// response was lost (plan §5.14 sync test 4).
    public func setAfterRespond(_ hook: BeforeRespond?) {
        afterRespond = hook
    }

    /// Mutates only the returned command result, never the server's stored
    /// record, so clients can exercise returned-object trust boundaries.
    public func setCommandResultTransform(_ transform: CommandResultTransform?) {
        commandResultTransform = transform
    }

    /// Mutates only the returned conversation, never the server's stored
    /// row, so clients can exercise returned-object identity gates.
    public func setConversationTransform(_ transform: ConversationTransform?) {
        conversationTransform = transform
    }

    /// Mutates only the returned bootstrap, never the server's stored rows,
    /// so clients can exercise canonical-frontier trust boundaries.
    public func setBootstrapTransform(_ transform: BootstrapTransform?) {
        bootstrapTransform = transform
    }

    /// Mutates only the returned effect-proposal facts, never the server's
    /// stored rows, so a client can exercise the facts-versus-item version
    /// match gate that keeps a stale facts snapshot off the card.
    public func setEffectProposalFactsTransform(_ transform: EffectProposalFactsTransform?) {
        effectProposalFactsTransform = transform
    }

    /// Controls the mock process boundary independently of synchronized
    /// state, so liveness clients can exercise running, outage, and restart.
    public func setHealthAvailable(_ available: Bool) {
        healthAvailable = available
    }

    /// Models live backup health, which can change admission without a
    /// synchronized write or a new revision.
    public func setBackupHealthy(_ healthy: Bool) {
        backupHealthy = healthy
    }

    /// Puts an update drain in force under the next revision, as a
    /// client-visible write would. The daemon has no client command for it:
    /// an update requests a drain over the control socket.
    public func requestUpdateDrain(forced: Bool = false) {
        revision += 1
        updateDrain = (since: currentTime, forced: forced)
    }

    /// Lifts the update drain under the next revision, as the daemon does
    /// when it restarts on a new build.
    public func liftUpdateDrain() {
        revision += 1
        updateDrain = nil
    }

    /// Replaces the history `getTaskTimeline` serves for its task, as the
    /// daemon's projection moves when an event is recorded.
    public func setTaskTimeline(_ timeline: Components.Schemas.TaskTimeline) {
        taskHistoryFixtures[timeline.task_id] = timeline
    }

    /// Replaces the agent catalog without a synchronized write, as a daemon
    /// restarted on a changed agent tree serves a new catalog and lineup
    /// revision at the same sync revision.
    public func setAgentCatalog(_ catalog: Components.Schemas.AgentCatalog) {
        agentCatalog = catalog
    }

    /// Shortens how long a `getSyncRevision` long poll waits with no change
    /// (the daemon's is 25 seconds), so a test can reach the bound.
    public func setRevisionWaitBound(_ bound: Duration) {
        revisionWaitBound = bound
    }

    /// Returns once at least `count` `getSyncRevision` long polls are held,
    /// so a test can act during a wait without sleeping.
    public func waitForHeldRevisionRequests(_ count: Int) async {
        if revisionWaiters.count >= count { return }
        await withCheckedContinuation { revisionWaiterArrivals.append((count, $0)) }
    }

    /// Serves a compatibility level and floor other than this client's, or
    /// omits either when nil, so a test can exercise the app's refusal of an
    /// incompatible daemon (#3109). The mock, like the daemon, still serves
    /// every request; only the app decides.
    public func setHealthCompatibility(level: Int?, floor: Int?) {
        healthCompatibilityLevel = level
        healthCompatibilityFloor = floor
    }

    /// Serves a contract digest other than this client's, so a test can show
    /// a digest difference alone never stops an app (#3109).
    public func setHealthContractDigest(_ digest: String) {
        healthContractDigest = digest
    }

    /// Every feature the mock serves unless a test withholds one.
    public static let servedFeatures = Components.Schemas.DaemonFeature.allCases.filter { $0 != .unknown }

    /// Serves `features` from `/health`, or omits the field when nil, as an
    /// older daemon does. A command of a feature not listed then answers the
    /// 400 a daemon gives a command kind it predates: the generated client
    /// can't encode a kind it doesn't know, so withholding a feature is how a
    /// test reaches that path.
    public func setHealthFeatures(_ features: [Components.Schemas.DaemonFeature]?) {
        healthFeatures = features
    }

    public func restart(version: String? = nil, startedAt: Date) {
        if let version {
            healthVersion = version
        }
        healthStartedAt = startedAt
        healthAvailable = true
    }

    /// Bumps the live item's versions as if a concurrent write applied,
    /// so a submission prepared against the old snapshot is stale.
    public func advance(itemID: String) {
        guard var snapshot = itemsByID[itemID] else { return }
        revision += 1
        snapshot.entity_version += 1
        snapshot.as_of_revision = revision
        snapshot.item.item_version += 1
        itemsByID[itemID] = snapshot
    }

    /// Advances one run projection and its computed timeline under the next
    /// revision, modeling a daemon observation write for partial-sync tests.
    public func advanceRun(id: String) {
        guard var snapshot = runsByID[id] else { return }
        revision += 1
        snapshot.as_of_revision = revision
        runsByID[id] = snapshot
        if var timeline = timelinesByRunID[id] {
            timeline.as_of_revision = revision
            timelinesByRunID[id] = timeline
        }
    }

    /// Appends one milestone to a run's computed timeline and advances both
    /// the run projection and the timeline under the next revision, modeling a
    /// daemon observation write that records a new event. `advanceRun` only
    /// bumps the revision cursors, which cannot model a live run emitting
    /// timeline events; this can.
    public func recordMilestone(runID: String, kind: Components.Schemas.RunMilestoneKind) {
        guard var timeline = timelinesByRunID[runID], var snapshot = runsByID[runID] else { return }
        revision += 1
        timeline.milestones.append(
            .init(
                run_id: runID,
                kind: kind,
                invocation_id: "inv-\(runID)-recorded-\(revision)",
                recorded_at: currentTime))
        timeline.as_of_revision = revision
        timelinesByRunID[runID] = timeline
        snapshot.as_of_revision = revision
        runsByID[runID] = snapshot
    }

    /// Advances one pool's usage record under the next revision, modeling a
    /// daemon collection write: the write stamps the revision and raises the
    /// record's version, so a pool with no record gains its first.
    public func advanceUsagePool(id: String) {
        guard var snapshot = usagePoolsByID[id] else { return }
        revision += 1
        snapshot.as_of_revision = revision
        snapshot.entity_version += 1
        usagePoolsByID[id] = snapshot
    }

    public func advanceTime(to instant: Date) {
        currentTime = instant
        convergeProposalSnoozes()
    }

    /// The server's current canonical snapshot, for test assertions.
    public func snapshot(itemID: String) -> Components.Schemas.AttentionItemSnapshot? {
        itemsByID[itemID]
    }

    /// Simulates a daemon restore (plan §5.14 sync test 8): a new sync
    /// epoch, optionally rewinding the revision to the restored state's.
    /// When the revision rewinds, the mock restamps computed run resources
    /// into that restored epoch as the real restored store would.
    public func rotateEpoch(revision restored: Int64? = nil) {
        epochGeneration += 1
        syncEpoch = "mock-epoch-\(epochGeneration)"
        if let restored {
            revision = max(1, restored)
            for id in runsByID.keys {
                runsByID[id]?.as_of_revision = revision
            }
            for id in tasksByID.keys {
                tasksByID[id]?.as_of_revision = revision
            }
            for id in schedulesByID.keys {
                schedulesByID[id]?.as_of_revision = revision
            }
            for id in usagePoolsByID.keys {
                usagePoolsByID[id]?.as_of_revision = revision
            }
            for id in timelinesByRunID.keys {
                timelinesByRunID[id]?.as_of_revision = revision
            }
        }
    }

    /// Replaces the durable attention and conversation frontier and drops
    /// write history, modelling a restore whose snapshot predates some rows
    /// and commands.
    public func restoreAttentionState(
        items: [Components.Schemas.AttentionItemSnapshot],
        conversations: [Components.Schemas.ConversationSnapshot] = [],
        revision restored: Int64
    ) {
        rotateEpoch(revision: restored)
        itemsByID = Dictionary(uniqueKeysWithValues: items.map { ($0.item.id, $0) })
        conversationsByID = Dictionary(
            uniqueKeysWithValues: conversations.map { ($0.conversation.id, $0) })
        commandsByID.removeAll()
        resultsByCommandID.removeAll()
        // The restored store carries its own transition and drain logs;
        // this restore models one with no stop or drain in force.
        operatorStop = nil
        updateDrain = nil
        pendingSpecificationReplacements.removeAll()
        pendingSpecificationComments.removeAll()
        proposalFactsByItemID.removeAll()
        effectProposalFactsByItemID.removeAll()
        proposalSnoozesByItemID.removeAll()
    }

    // MARK: - Devices and pairing

    /// Seeds a pairing code in the given lifecycle state; only `valid`
    /// can ever be consumed. The mock keys codes by plaintext where the
    /// daemon stores a keyed digest; the lifecycle semantics are what
    /// the client tests exercise.
    public func seedPairingCode(_ code: String, state: PairingCodeState = .valid) {
        pairingCodes[code] = state
    }

    /// The device's current snapshot, for test assertions.
    public func device(id: String) -> Components.Schemas.DeviceSnapshot? {
        devicesByID[id]
    }

    /// Every pairing failure is one undifferentiated rejection, so an
    /// unauthenticated caller cannot probe code validity.
    struct PairingRejectedError: Error {}

    /// Pairing and revocation instants are fixed, not wall-clock, so
    /// device snapshots stay deterministic under test equality.
    private static let pairedInstant = Date(timeIntervalSince1970: 1_767_323_045)
    private static let revokedInstant = Date(timeIntervalSince1970: 1_767_326_645)
    /// The instant an authenticated request is recorded at: after pairing
    /// and before revocation, and fixed like both. The daemon refreshes a
    /// recorded instant only once it is several minutes old; a fixed
    /// instant satisfies that trivially.
    private static let seenInstant = Date(timeIntervalSince1970: 1_767_323_105)
    /// The decision instant a concluding command stamps (daemon parity,
    /// #171): fixed like the pairing instants so item snapshots stay
    /// deterministic under test equality.
    private static let decidedInstant = Date(timeIntervalSince1970: 1_767_330_245)

    /// The process-fixed pairing facts the mock daemon reports (plan
    /// §5.14): a stable host name, the code's expiry ten minutes past the
    /// pairing instant, loopback, and the single operator scope. Fixed
    /// like the pairing instants so previews and grants stay deterministic
    /// under test equality.
    public static let pairingFacts = Components.Schemas.PairingFacts(
        host_display_name: "mock-daemon.local",
        code_expires_at: pairedInstant.addingTimeInterval(600),
        connection_mode: .loopback,
        granted_scope: ._operator
    )

    /// The default mock with a quota failure and a quota-parked task that
    /// each offer `UsageFixtures.alternateAgentOffers()`, on pools that read
    /// current, stale, and unsupported at `now` (`-FreesideAlternates`,
    /// #1722). The parked task is the retry campaign's: its active run
    /// holds on the weekly window the provider refused its lineup
    /// implementer on `UsageFixtures.readingPoolID`, the pool's recorded
    /// refusal, instead of verification findings. Its task history is
    /// seeded, so a move from its menu is recorded as the daemon records
    /// one.
    public static func alternates(now: Date) -> MockServer {
        let runID = RunFixtures.activeRunID
        var timelines = RunFixtures.defaultTimelines()
        let pools = UsageFixtures.alternatesUsagePools(now: now)
        guard let timelineIndex = timelines.firstIndex(where: { $0.run_id == runID }),
            var hold = timelines[timelineIndex].hold?.value1,
            let refusal = pools.first(where: { $0.pool.id == UsageFixtures.readingPoolID })?.pool
                .recorded_exhausted_marks.first(where: { $0.source == .provider_refusal }),
            let resetsAt = refusal.resets_at
        else { preconditionFailure("The retry run must carry a hold and its pool a recorded refusal with a reset") }
        hold.reason = .quota_window_exhausted
        hold.quota_window = .init(
            value1: .init(
                pool: UsageFixtures.readingPoolID, window: refusal.window, source: .provider_refusal,
                invocation_id: hold.invocation_id, resets_at: resetsAt, set_at: refusal.set_at,
                alternate_agents: UsageFixtures.alternateAgentOffers()))
        timelines[timelineIndex].hold = .init(value1: hold)
        let runs = RunFixtures.defaultRuns().map { snapshot in
            var snapshot = snapshot
            if snapshot.run.id == runID { snapshot.run.hold_reason = .init(value1: .quota_window_exhausted) }
            return snapshot
        }
        let tasks = TaskFixtures.defaultTasks().map { snapshot in
            var snapshot = snapshot
            if snapshot.task.current_position?.value1.run_id == runID {
                snapshot.task.current_position?.value1.hold_reason = .init(value1: .quota_window_exhausted)
            }
            return snapshot
        }
        guard let parked = tasks.first(where: { $0.task.current_position?.value1.run_id == runID }) else {
            preconditionFailure("The retry campaign's task must sit on its active run")
        }
        let history = TaskFixtures.timeline(
            for: parked.task, runs: runs.map(\.run),
            timelines: Dictionary(uniqueKeysWithValues: timelines.map { ($0.run_id, $0) }),
            revision: parked.as_of_revision, asOf: RunFixtures.screenshotInstant)
        return MockServer(
            items: AttentionFixtures.alternatesInbox(), runs: runs, tasks: tasks, timelines: timelines,
            taskTimelines: [history], usagePools: pools, agentCatalog: UsageFixtures.alternatesAgentCatalog(),
            automaticallyCompletesAgentWork: true)
    }

    /// Reports the facts a live code would grant without consuming it: the
    /// code's state never changes here, so a previewed code still pairs
    /// exactly once, and a dead code is rejected exactly as pairing
    /// rejects it (test 13's undifferentiated 403).
    func previewPairing(
        _ request: Components.Schemas.PairingPreviewRequest
    ) throws -> Components.Schemas.PairingFacts {
        guard pairingCodes[request.pairing_code] == .valid else {
            throw PairingRejectedError()
        }
        return configuredPairingFacts
    }

    func pairDevice(
        _ request: Components.Schemas.PairingRequest
    ) throws -> Components.Schemas.PairingGrant {
        guard pairingCodes[request.pairing_code] == .valid else {
            throw PairingRejectedError()
        }
        // Single-winner consumption (test 14): the actor serializes
        // requests, so the first flips the code and every other attempt,
        // however simultaneous at its caller, finds it consumed.
        pairingCodes[request.pairing_code] = .consumed
        pairedDeviceCount += 1
        let deviceID = "device-\(pairedDeviceCount)"
        // The contract's token shape: version prefix, unpadded-base64url
        // device id, secret. Deterministic here; entropy is the daemon's
        // concern, and nothing in the mock parses the token back apart.
        let idSegment = Data(deviceID.utf8).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
        let secretSegment = Data(
            repeating: UInt8(truncatingIfNeeded: pairedDeviceCount), count: 32
        ).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
        let token = pairingDeviceToken ?? "fsd1.\(idSegment).\(secretSegment)"
        // Devices are synchronized entities (#64): pairing is a
        // client-visible write and increments the server revision.
        revision += 1
        let snapshot = Components.Schemas.DeviceSnapshot(
            as_of_revision: revision,
            entity_version: 1,
            device: .active(
                .init(
                    id: deviceID,
                    display_name: request.display_name,
                    status: .active,
                    paired_at: Self.pairedInstant,
                    // The contract requires the key with an explicit
                    // null; an empty container encodes as JSON null.
                    revoked_at: try .init(unvalidatedValue: nil)
                ))
        )
        devicesByID[deviceID] = snapshot
        deviceIDsByToken[token] = deviceID
        let subscription = Components.Schemas.NtfySubscription(
            server_url: pairingNtfyServerURL,
            topic: pairingNtfyTopic ?? String(format: "fs-%032x", pairedDeviceCount)
        )
        return .init(
            device_token: token,
            device: snapshot,
            ntfy_subscription: subscription,
            facts: configuredPairingFacts
        )
    }

    enum RevokeOutcome {
        case revoked(Components.Schemas.DeviceSnapshot)
        case unknown
    }

    func revokeDevice(id: String) -> RevokeOutcome {
        guard let current = devicesByID[id] else { return .unknown }
        switch current.device {
        case .revoked:
            // Terminal and idempotent (#64): an identical replay passes
            // without a write, so no version or revision moves.
            return .revoked(current)
        case .active(let active):
            revision += 1
            let snapshot = Components.Schemas.DeviceSnapshot(
                as_of_revision: revision,
                entity_version: current.entity_version + 1,
                device: .revoked(
                    .init(
                        id: active.id,
                        display_name: active.display_name,
                        status: .revoked,
                        paired_at: active.paired_at,
                        revoked_at: Self.revokedInstant
                    ))
            )
            devicesByID[id] = snapshot
            return .revoked(snapshot)
        }
    }

    /// Every paired device, active or revoked, in id order with its
    /// last-seen instant and notifications switch (GET /devices). The entry
    /// is built from the device snapshot alone, so no token can reach it.
    func listDevices() -> [Components.Schemas.DeviceListEntry] {
        devicesByID.keys.sorted().compactMap(listEntry(id:))
    }

    /// A revoked device lists off whatever its stored switch says, as the
    /// daemon's list does.
    private func listEntry(id: String) -> Components.Schemas.DeviceListEntry? {
        devicesByID[id].map {
            let active = if case .active = $0.device { true } else { false }
            return .init(
                as_of_revision: $0.as_of_revision,
                entity_version: $0.entity_version,
                device: $0.device,
                last_seen_at: lastSeenByDeviceID[id],
                notifications_enabled: active && (notificationsEnabledByDeviceID[id] ?? false))
        }
    }

    enum NotificationsOutcome {
        case set(Components.Schemas.DeviceListEntry)
        /// No such device, or a revoked one: a revoked device has no switch.
        case unknown
    }

    /// Sets a device's notifications switch (PUT
    /// /devices/{device_id}/notifications). Daemon parity: a request for
    /// the stored value writes nothing and moves no revision.
    func setDeviceNotifications(id: String, enabled: Bool) -> NotificationsOutcome {
        guard let current = devicesByID[id], case .active = current.device else { return .unknown }
        if (notificationsEnabledByDeviceID[id] ?? false) != enabled {
            revision += 1
            notificationsEnabledByDeviceID[id] = enabled
        }
        return listEntry(id: id).map { .set($0) } ?? .unknown
    }

    private static func id(of device: Components.Schemas.Device) -> String {
        switch device {
        case .active(let active): active.id
        case .revoked(let revoked): revoked.id
        }
    }

    enum AuthOutcome {
        /// Permissive mode: the caller is whoever it claims to be.
        case anonymous
        case device(id: String)
        case revokedDevice(id: String)
        case unauthorized
    }

    func authenticate(authorization: String?) -> AuthOutcome {
        if case .permissive = authMode { return .anonymous }
        guard let authorization, authorization.hasPrefix("Bearer "),
            let deviceID = deviceIDsByToken[String(authorization.dropFirst("Bearer ".count))],
            let snapshot = devicesByID[deviceID]
        else { return .unauthorized }
        switch snapshot.device {
        case .active:
            // Recorded before the handler runs, so a device listing the
            // devices sees its own request (daemon parity).
            lastSeenByDeviceID[deviceID] = Self.seenInstant
            return .device(id: deviceID)
        case .revoked: return .revokedDevice(id: deviceID)
        }
    }

    /// Test 16's may-branch: a revoked device's verbatim retry of its own
    /// committed command may return the recorded result (the contract
    /// permits recorded-result or rejection; the daemon decides in #67).
    /// The mock takes the permissive branch so the client's rendering
    /// path is exercisable; every other request from a revoked device
    /// stays rejected, and this lookup writes nothing.
    func recordedResultForRevokedRetry(
        _ command: Components.Schemas.ClientCommand, deviceID: String
    ) -> Components.Schemas.CommandResult? {
        switch command.payload {
        case .decision(let payload):
            guard let original = commandsByID[command.command_id],
                original == Self.normalizedReplayCommand(command, payload),
                original.deviceID == deviceID
            else { return nil }
            return resultsByCommandID[command.command_id]
        case .stop_task, .refresh_usage, .reassign_collector, .change_task_line, .set_routing_policy,
            .move_parked_task, .set_workflow_policy:
            // Active-device authority is required even for Stop and operator
            // command replay; the daemon gates the device before its replay read.
            return nil
        case .submit_task:
            // A submit_task record carries no normalized body; its recorded
            // result replays for the same device.
            guard let recorded = resultsByCommandID[command.command_id],
                case .submit_task(let record) = recorded.record,
                record.device_id == deviceID
            else { return nil }
            return recorded
        }
    }

    // MARK: - Contract semantics

    enum SubmitOutcome {
        case ok(Components.Schemas.CommandResult)
        case stale(Components.Schemas.StaleVersionRejection)
        case staleTask(Components.Schemas.StaleTaskRejection)
        case taskLineRejected(Components.Schemas.TaskLineChangeRejection)
        case routingPolicyRejected(Components.Schemas.RoutingPolicyRejection)
        case workflowPolicyRejected(Components.Schemas.WorkflowPolicyRejection)
        /// The command's feature is withheld: the daemon's build predates it.
        case unservedKind
    }

    struct UnknownItemError: Error {
        let itemID: String
    }

    /// A reused command_id with a different body is misuse, never a
    /// replay; the daemon converges only on a byte-identical command
    /// (store.PutCommand, ErrImmutableConflict).
    public struct ImmutableConflictError: Error {
        public let commandID: String
    }

    /// A command naming a valid action outside the item's
    /// requested_decision set is rejected; the daemon enforces the
    /// offered set (store.PutCommand, ErrActionNotOffered).
    public struct ActionNotOfferedError: Error {
        public let commandID: String
        public let action: Components.Schemas.Action
        public let itemID: String
    }

    /// A pending action's accepted effect belongs to a later unit; the
    /// signet boundary rejects a genuinely new pending command after the
    /// replay lookup and the item-policy re-gate, rather than record a
    /// command whose effect would be silently dropped
    /// (ErrUnsupportedAction).
    public struct UnsupportedActionError: Error {
        public let commandID: String
        public let action: Components.Schemas.Action
    }

    /// A malformed command is rejected before any lookup, as the signet
    /// boundary does (domain.NewCommand validation plus the
    /// expected-version check, both ahead of the replay read).
    public struct MalformedCommandError: Error {
        public let commandID: String
        public let reason: String
    }

    public struct ProposalSnoozedError: Error {
        public let itemID: String
    }

    public struct InvalidProposalDecisionError: Error {
        public let reason: String
    }

    public struct InvalidFindingAdjudicationDecisionError: Error {
        public let reason: String
    }

    /// The durable row fails current signet policy
    /// (validateRequestedActions): it offers an action outside its
    /// type's allowed set, offers nothing, or is the read-only blocked
    /// type. Such a row is no authority for accepting any command.
    public struct ItemPolicyError: Error {
        public let itemID: String
        public let reason: String
    }

    /// The durable row itself is malformed (the daemon's
    /// GetAttentionItemSnapshot re-runs domain.AttentionItem.Validate
    /// before signet policy): most importantly, a binding set that is
    /// not the canonical union of the rendered digests would let an
    /// approval display one set while binding another (the
    /// stale-approval class, plan §3.1).
    public struct InvalidItemError: Error {
        public let itemID: String
        public let reason: String
    }

    func runBeforeRespond(_ operationID: String) async throws {
        try await beforeRespond?(operationID)
    }

    func runAfterRespond(_ operationID: String) async throws {
        try await afterRespond?(operationID)
    }

    /// The daemon's restart readiness read (store.RestartReadiness). The mock
    /// runs no work, so nothing blocks and it is ready exactly while a drain
    /// is in force.
    func restartReadiness() -> Components.Schemas.RestartReadiness {
        .init(
            ready: updateDrain != nil,
            drain: updateDrain.map { .init(value1: .init(since: $0.since, forced: $0.forced)) },
            blockers: [], as_of: currentTime)
    }

    func serverRevision() -> Components.Schemas.ServerRevision {
        return .init(
            sync_epoch: syncEpoch, revision: revision,
            unattended_operation: unattendedOperationSnapshot(),
            lineup_revision: agentCatalog.lineup_revision.map { .init(value1: $0.value1) })
    }

    /// The heartbeat with the daemon's long-poll rule: a request whose
    /// `after` equals the revision is held until the revision or epoch
    /// moves, the wait bound elapses, or the request is cancelled; any other
    /// request answers at once. Every answer is read when it is sent.
    func serverRevision(after: Int64?) async -> Components.Schemas.ServerRevision {
        receivedRevisionAfters.append(after)
        guard let after, after == revision else { return serverRevision() }
        let id = UUID()
        let bound = revisionWaitBound
        let timeout = Task { [weak self] in
            try? await Task.sleep(for: bound)
            await self?.releaseRevisionWaiter(id)
        }
        await withTaskCancellationHandler {
            await withCheckedContinuation {
                revisionWaiters[id] = $0
                let held = revisionWaiters.count
                let arrived = revisionWaiterArrivals.filter { $0.count <= held }
                revisionWaiterArrivals.removeAll { $0.count <= held }
                for arrival in arrived { arrival.waiter.resume() }
            }
        } onCancel: {
            Task { await self.releaseRevisionWaiter(id) }
        }
        timeout.cancel()
        return serverRevision()
    }

    private func releaseRevisionWaiter(_ id: UUID) {
        revisionWaiters.removeValue(forKey: id)?.resume()
    }

    private func releaseRevisionWaiters() {
        let waiters = revisionWaiters.values
        revisionWaiters.removeAll()
        for waiter in waiters { waiter.resume() }
    }

    func healthStatus() throws -> Components.Schemas.HealthStatus {
        guard healthAvailable else { throw HealthUnavailableError() }
        return .init(
            status: .ok, contract_digest: healthContractDigest,
            compatibility_level: healthCompatibilityLevel, compatibility_floor: healthCompatibilityFloor,
            features: healthFeatures, version: healthVersion, started_at: healthStartedAt)
    }

    /// One canonical snapshot of every synchronized resource from a
    /// single actor-isolated read, as the daemon's bootstrap is one
    /// Store.Read (plan §5.14): the cursor pair and the rows can never
    /// be torn. The run fixtures are deterministic daemon observations used by both
    /// app platforms and their screenshot workflow.
    func bootstrapSnapshot() throws -> Components.Schemas.BootstrapSnapshot {
        let snapshot = Components.Schemas.BootstrapSnapshot(
            sync_epoch: syncEpoch,
            revision: revision,
            attention_items: try listAttentionItems(),
            attention_deliveries: try listAttentionDeliveries(),
            runs: try listRuns(),
            tasks: try listTasks(),
            conversations: conversationsByID.keys.sorted().compactMap { conversationsByID[$0] },
            schedules: listSchedules(),
            unattended_operation: unattendedOperationSnapshot(),
            usage_pools: listUsagePools(),
            routing_policy: routingPolicySnapshot().map { .init(value1: $0) },
            workflow_policy: workflowPolicy.snapshot(projects: workflowProjects, asOfRevision: revision),
            agent_catalog: agentCatalog
        )
        return bootstrapTransform?(snapshot) ?? snapshot
    }

    /// Every pool's usage record in pool id order, a pool with no record
    /// stamped with the serving revision (signet usagePoolSnapshots).
    func listUsagePools() -> [Components.Schemas.UsagePoolSnapshot] {
        usagePoolsByID.keys.sorted().compactMap { id in
            guard var snapshot = usagePoolsByID[id] else { return nil }
            if snapshot.entity_version == 0 { snapshot.as_of_revision = revision }
            return snapshot
        }
    }

    /// The current routing policy at the serving revision, its version the
    /// policy revision (signet currentRoutingPolicySnapshot).
    func routingPolicySnapshot() -> Components.Schemas.RoutingPolicySnapshot? {
        routingPolicy.map {
            .init(as_of_revision: revision, entity_version: Int64($0.revision), policy: $0)
        }
    }

    /// The finding code on the notice an operator stop raises, as the
    /// daemon's signet stamps it.
    static let stoppedNoticeDiagnostic = "unattended_operation_stopped"

    /// The daemon's unattended-admission projection (signet
    /// unattendedOperationSnapshot over store.UnattendedOperationGate): the
    /// operator stop first, bound to its open resume notice, then every other
    /// open blocking system_health item in id order, then an update drain. The
    /// daemon starts sending the drain in #3104. The notice is itself a
    /// blocking item, so it is reported once, as the operator stop. The mock
    /// evaluates backup supersession against its live backup-health switch,
    /// which defaults to healthy and moves no revision.
    func unattendedOperationSnapshot() -> Components.Schemas.UnattendedOperationSnapshot {
        let items: [Components.Schemas.AttentionItem] = itemsByID.keys.sorted().compactMap {
            itemsByID[$0]?.item
        }
        let blocking = items.filter { item in
            guard item.status == .open, item._type == .system_health else { return false }
            return item.posture?.value1 == .blocking
                && (item.blocking_supersession == nil || !backupHealthy)
        }
        var stops: [Components.Schemas.UnattendedStop] = []
        var noticeID: String?
        if let operatorStop {
            noticeID =
                blocking.first { item in
                    item.health_diagnostic?.value1.code == Self.stoppedNoticeDiagnostic
                        && item.requested_decision.contains(.resume_unattended)
                }?.id
            stops.append(
                .init(
                    kind: .operator_stop, item_id: noticeID,
                    command_id: operatorStop.commandID, since: operatorStop.since))
        }
        for item in blocking where item.id != noticeID {
            stops.append(
                .init(
                    kind: .blocking_system_health, item_id: item.id,
                    command_id: nil, since: item.created_at))
        }
        if let updateDrain {
            stops.append(.init(kind: .update_drain, item_id: nil, command_id: nil, since: updateDrain.since))
        }
        return .init(admission: stops.isEmpty ? .open : .stopped, stops: stops)
    }

    func conversation(id: String) -> Components.Schemas.ConversationSnapshot? {
        conversationsByID[id].map { conversationTransform?($0) ?? $0 }
    }

    func listRuns() throws -> [Components.Schemas.RunSnapshot] {
        try runsByID.keys.sorted().compactMap { id in
            guard let snapshot = runsByID[id] else { return nil }
            if let reason = MockContractValidation.runSnapshotBreach(snapshot, serverRevision: revision) {
                throw InvalidRunError(runID: id, reason: reason)
            }
            return snapshot
        }
    }

    func run(id: String) throws -> Components.Schemas.RunSnapshot? {
        guard let snapshot = runsByID[id] else { return nil }
        if let reason = MockContractValidation.runSnapshotBreach(snapshot, serverRevision: revision) {
            throw InvalidRunError(runID: id, reason: reason)
        }
        return snapshot
    }

    func listTasks() throws -> [Components.Schemas.TaskSnapshot] {
        try tasksByID.keys.sorted().compactMap { try task(id: $0) }
    }

    func task(id: String) throws -> Components.Schemas.TaskSnapshot? {
        guard var snapshot = tasksByID[id] else { return nil }
        if let reason = MockContractValidation.taskSnapshotBreach(
            snapshot, serverRevision: revision, servedDigests: Set(attachmentsByDigest.keys))
        {
            throw InvalidTaskError(taskID: id, reason: reason)
        }
        snapshot.entity_version = revision
        snapshot.as_of_revision = revision
        return snapshot
    }

    struct InvalidTaskError: Error {
        let taskID: String
        let reason: String
    }

    func runTimeline(id: String) -> Components.Schemas.RunTimeline? {
        timelinesByRunID[id]
    }

    func taskTimeline(id: String) throws -> Components.Schemas.TaskTimeline? {
        guard let snapshot = try task(id: id) else { return nil }
        if let history = taskHistoryFixtures[id] { return history }
        let snapshots: [Components.Schemas.RunSnapshot] = try runsByID.keys.sorted().compactMap { runID in
            guard runsByID[runID]?.run.task_id == id else { return nil }
            return try run(id: runID)
        }
        guard Set(snapshots.map(\.run.id)) == Set(snapshot.task.run_ids),
            snapshots.allSatisfy({ $0.run.project_id == snapshot.task.project_id })
        else {
            throw InvalidTaskError(taskID: id, reason: "Task history disagrees with its run membership")
        }
        let snapshotsByID = Dictionary(uniqueKeysWithValues: snapshots.map { ($0.run.id, $0) })
        var campaigns: [String] = []
        for runID in snapshot.task.run_ids {
            if let campaign = snapshotsByID[runID]?.run.campaign_id, !campaigns.contains(campaign) {
                campaigns.append(campaign)
            }
        }
        guard campaigns == snapshot.task.campaign_ids else {
            throw InvalidTaskError(taskID: id, reason: "Task campaign order disagrees with its run history")
        }
        for snapshot in snapshots {
            let timeline = timelinesByRunID[snapshot.run.id]
            if let timeline,
                let breach = MockContractValidation.taskTimelineObservationsBreach(timeline)
            {
                throw InvalidRunError(runID: snapshot.run.id, reason: breach)
            }
            if snapshot.run.campaign_id != nil,
                timeline?.milestones.contains(where: { $0.kind == .run_submitted }) != true
            {
                throw InvalidRunError(runID: snapshot.run.id, reason: "Campaign run has no submission evidence")
            }
        }
        let runs = snapshots.map {
            RunFixtures.projectingObservationTimes($0, from: timelinesByRunID[$0.run.id]).run
        }
        return TaskFixtures.timeline(
            for: snapshot.task, runs: runs, timelines: timelinesByRunID,
            revision: revision, asOf: currentTime, lineEvents: taskLineEvents[id] ?? [])
    }

    func listSchedules() -> [Components.Schemas.ScheduleSnapshot] {
        schedulesByID.keys.sorted().compactMap { schedulesByID[$0] }
    }

    struct InvalidDeliveryError: Error {
        let itemID: String
        let reason: String
    }

    struct InvalidRunError: Error {
        let runID: String
        let reason: String
    }

    /// Re-validates one delivery snapshot before it is served, as the
    /// daemon's read paths run validateSnapshot plus the domain validator
    /// on every row (signet sync.go, store reconstruction): a seed the
    /// daemon would fail closed on fails the mock's read loudly instead
    /// of letting a client test pass against unservable cache state. The
    /// generated variant structs already make status/receipt
    /// correspondence unrepresentable; what stays checkable here is the
    /// snapshot metadata, the identity fields, and receipt ordering.
    private func validated(
        _ snapshot: Components.Schemas.AttentionDeliverySnapshot
    ) throws -> Components.Schemas.AttentionDeliverySnapshot {
        let key = DeliveryKey(snapshot.delivery)
        if let breach = MockContractValidation.deliveryBreach(
            snapshot, serverRevision: revision, hasParentItem: itemsByID[key.itemID] != nil)
        {
            throw InvalidDeliveryError(itemID: key.itemID, reason: breach)
        }
        return snapshot
    }

    /// Deliveries in the store's deterministic composite-key order
    /// (item, device, channel, attempt), as the daemon lists them; every
    /// served row re-validates first (see `validated`).
    func listAttentionDeliveries() throws -> [Components.Schemas.AttentionDeliverySnapshot] {
        try deliveriesByKey.sorted { $0.key < $1.key }.map { try validated($0.value) }
    }

    /// One item's delivery rows in composite-key order (the daemon's
    /// ListAttentionItemDeliveries): a missing parent item is a loud
    /// not-found rather than an indistinguishable empty history, the
    /// parent reconstructs through the item gate (the daemon validates
    /// the item snapshot in the same read), and the whole delivery table
    /// validates (the daemon's ListAttentionDeliveries gates every row)
    /// before the item filter.
    func listAttentionItemDeliveries(
        itemID: String
    ) throws -> [Components.Schemas.AttentionDeliverySnapshot] {
        guard try servedSnapshot(itemID: itemID) != nil else {
            throw UnknownItemError(itemID: itemID)
        }
        // Validate the entire table before filtering: the daemon lists all
        // rows through the shared decode gate (which cannot skip a gate the
        // Get runs) ahead of the item filter, so one corrupt row for any
        // item fails the listing closed rather than serving this item.
        for delivery in deliveriesByKey.values {
            _ = try validated(delivery)
        }
        return deliveriesByKey.filter { $0.key.itemID == itemID }
            .sorted { $0.key < $1.key }
            .map { $0.value }
    }

    enum ReceiptOutcome {
        case ok(Components.Schemas.AttentionDeliverySnapshot)
        case unknown
    }

    /// The one client write on the deliveries surface (#130): advances an
    /// existing attempt to opened with a daemon-stamped receipt, replays
    /// idempotently without consuming revision, and never creates a row.
    /// The device is the caller's credential identity; permissive mode has
    /// none, so there the row matches on the path identity alone.
    func reportDeliveryOpened(
        itemID: String, channel: String, attempt: Int, deviceID: String?
    ) throws -> ReceiptOutcome {
        guard
            let key = deliveriesByKey.keys.sorted().first(where: {
                $0.itemID == itemID && $0.channel == channel && $0.attempt == attempt
                    && (deviceID == nil || $0.deviceID == deviceID)
            }), var snapshot = deliveriesByKey[key]
        else { return .unknown }
        // Validate the whole delivery table before the replay check and
        // before any mutation, as the daemon reconstructs every row (store
        // decode gate) via recomputeItemTiming's ListAttentionDeliveries,
        // which cannot skip a gate the Get runs, ahead of
        // RecordDeliveryOpened's write. The list reconstructs the entire
        // table before the service filters to this item, so one corrupt
        // row for any item fails the receipt closed with no effect rather
        // than being healed into a servable 200. The target row validates
        // as part of the table; the served snapshot is its stored value.
        for delivery in deliveriesByKey.values {
            _ = try validated(delivery)
        }
        // The daemon's recompute reads the parent item through the
        // reconstruction gate (GetAttentionItemSnapshot): an absent
        // parent surfaces as not-found, a corrupt one fails closed.
        guard try servedSnapshot(itemID: key.itemID) != nil else { return .unknown }
        let opened: Components.Schemas.AttentionDeliveryOpened
        switch snapshot.delivery {
        case .opened:
            // Idempotent replay: the recorded row, no revision movement.
            return .ok(snapshot)
        case .submitted(let row):
            opened = .init(
                item_id: row.item_id, device_id: row.device_id,
                channel: row.channel, attempt: row.attempt,
                submitted_at: row.submitted_at,
                opened_at: row.submitted_at.addingTimeInterval(60),
                delivery_status: .opened
            )
        case .channel_accepted(let row):
            opened = .init(
                item_id: row.item_id, device_id: row.device_id,
                channel: row.channel, attempt: row.attempt,
                submitted_at: row.submitted_at,
                channel_accepted_at: row.channel_accepted_at,
                opened_at: row.channel_accepted_at.addingTimeInterval(60),
                delivery_status: .opened
            )
        }
        revision += 1
        snapshot.delivery = .opened(opened)
        snapshot.entity_version += 1
        snapshot.as_of_revision = revision
        deliveriesByKey[key] = snapshot
        recomputeItemTiming(itemID: key.itemID)
        return .ok(snapshot)
    }

    // MARK: - Comprehension telemetry (plan §8)

    struct CapabilityInvalidError: Error { let reason: String }
    struct ActionSurfaceMismatchError: Error { let commandID: String }
    struct InvalidComprehensionEventError: Error {
        let eventID: String
        let reason: String
    }

    enum ActionSurfaceOutcome {
        case ok(Components.Schemas.DecisionActionSurface)
        case noContract
        case unknownItem
    }

    enum ComprehensionEventOutcome {
        case ok(Components.Schemas.ComprehensionEvent)
        case unknownItem
    }

    /// Registers the device's capability contract (plan §5.14, §8). Idempotent
    /// by content: the same canonical action set yields the same digest, and
    /// registering it again never moves the revision.
    func registerCapabilityContract(
        deviceID: String, actions: [Components.Schemas.Action]
    ) throws -> Components.Schemas.ClientCapabilityContract {
        let canonical = Array(Set(actions)).sorted { $0.rawValue < $1.rawValue }
        guard !canonical.isEmpty else {
            throw CapabilityInvalidError(reason: "actions must be non-empty")
        }
        let digest = MockContractValidation.sha256Digest(
            of: "capability:" + canonical.map(\.rawValue).joined(separator: ","))
        let contract = Components.Schemas.ClientCapabilityContract(
            device_id: deviceID, actions: canonical, digest: digest)
        capabilityContractsByDevice[deviceID] = contract
        return contract
    }

    /// Derives, and records on first sight, the device's action surface for the
    /// item's current decision surface: the intersection of the item's requested
    /// decisions with the device's capability contract. Telemetry evidence only;
    /// never widens the offered actions.
    func getActionSurface(deviceID: String, itemID: String) throws -> ActionSurfaceOutcome {
        guard let current = try servedSnapshot(itemID: itemID) else { return .unknownItem }
        guard let contract = capabilityContractsByDevice[deviceID] else { return .noContract }
        let contractSet = Set(contract.actions)
        let offered = current.item.requested_decision.filter { contractSet.contains($0) }
        let canonical = Array(Set(offered)).sorted { $0.rawValue < $1.rawValue }
        let itemSurfaceDigest = current.item.decision_surface.digest
        let digest = MockContractValidation.sha256Digest(
            of: "surface:" + deviceID + "|" + itemSurfaceDigest + "|" + contract.digest
                + "|" + canonical.map(\.rawValue).joined(separator: ","))
        let surface = Components.Schemas.DecisionActionSurface(
            device_id: deviceID, item_id: itemID,
            item_decision_surface_digest: itemSurfaceDigest,
            client_capability_digest: contract.digest,
            actions: canonical, digest: digest)
        actionSurfacesByDigest[digest] = surface
        return .ok(surface)
    }

    /// Ingests one comprehension event, idempotent by (device, event_id).
    /// Action-bearing kinds must reference a matching accepted command. As the
    /// daemon does, the first `card_opened` bound to the served item's current
    /// decision-surface digest stamps `surface_first_opened_at` and moves the
    /// revision once, leaving `entity_version` alone; every other event, a
    /// repeat open, and an open of an older surface move nothing.
    func recordComprehensionEvent(
        deviceID: String, eventID: String, input: Components.Schemas.ComprehensionEventInput
    ) throws -> ComprehensionEventOutcome {
        let key = deviceID + "|" + eventID
        if let existing = comprehensionEventsByKey[key] {
            return .ok(existing)
        }
        guard try servedSnapshot(itemID: input.item_id) != nil else { return .unknownItem }
        let actionBearing = input.kind == .action_taken || input.kind == .recommendation_override
        if actionBearing {
            guard let commandID = input.command_id, !commandID.isEmpty,
                let surfaceDigest = input.decision_action_surface_digest,
                let recorded = resultsByCommandID[commandID],
                // Comprehension events reference an accepted decision command; a
                // task submission carries no item or decision evidence.
                case .decision(let record) = recorded.record,
                record.device_id == deviceID,
                record.item_id == input.item_id,
                let evidence = record.decision_evidence,
                evidence.value1.action_surface_digest == surfaceDigest
            else {
                throw InvalidComprehensionEventError(
                    eventID: eventID, reason: "no matching accepted command")
            }
        } else if input.decision_action_surface_digest != nil
            || (input.command_id?.isEmpty == false)
        {
            throw InvalidComprehensionEventError(
                eventID: eventID, reason: "kind carries a command or surface reference")
        }
        let event = Components.Schemas.ComprehensionEvent(
            device_id: deviceID, event_id: eventID, item_id: input.item_id,
            kind: input.kind, item_decision_surface_digest: input.item_decision_surface_digest,
            decision_action_surface_digest: input.decision_action_surface_digest,
            command_id: input.command_id ?? "", occurred_at: input.occurred_at,
            sequence: input.sequence, received_at: currentTime)
        comprehensionEventsByKey[key] = event
        if input.kind == .card_opened, var snapshot = itemsByID[input.item_id],
            snapshot.item.decision_surface.digest == input.item_decision_surface_digest,
            snapshot.item.surface_first_opened_at == nil
        {
            revision += 1
            snapshot.item.surface_first_opened_at = currentTime
            snapshot.as_of_revision = revision
            itemsByID[input.item_id] = snapshot
        }
        return .ok(event)
    }

    /// Revalidates a referenced action surface and derives the daemon-stamped
    /// decision evidence for a command. The client's surface is never trusted: a
    /// foreign, stale, or unknown surface, or a selected action the surface does
    /// not offer, throws ActionSurfaceMismatchError. When neither a surface nor a
    /// recommendation is present, the command carries no evidence.
    private func stampedDecisionEvidence(
        for command: Components.Schemas.ClientCommand,
        item current: Components.Schemas.AttentionItemSnapshot
    ) throws -> Components.Schemas.CommandDecisionEvidence? {
        guard case .decision(let payload) = command.payload else { return nil }
        var recommendedAction: Components.Schemas.Action?
        var recommendationSource: Components.Schemas.RecommendationSource?
        if let rec = current.item.recommendation {
            recommendedAction = rec.value1.action
            recommendationSource = rec.value1.source
        }
        var surfaceDigest = ""
        if let digest = payload.decision_action_surface_digest {
            guard let surface = actionSurfacesByDigest[digest],
                surface.device_id == command.device_id,
                surface.item_id == payload.item_id,
                surface.item_decision_surface_digest == current.item.decision_surface.digest,
                let contract = capabilityContractsByDevice[command.device_id],
                surface.client_capability_digest == contract.digest,
                surface.actions.contains(payload.action)
            else {
                throw ActionSurfaceMismatchError(commandID: command.command_id)
            }
            surfaceDigest = digest
        }
        if surfaceDigest.isEmpty && recommendedAction == nil && recommendationSource == nil {
            return nil
        }
        return .init(
            action_surface_digest: surfaceDigest,
            recommended_action: recommendedAction.map { .init(value1: $0) },
            recommendation_source: recommendationSource.map { .init(value1: $0) })
    }

    /// Mirrors the daemon's recomputeItemTiming (signet delivery.go): the
    /// receipt's write re-derives the item's timing aggregates from the
    /// full delivery set in the same "transaction" (same revision), and
    /// raises entity_version only when the summary actually changed: an
    /// aggregate-neutral receipt writes nothing. item_version never moves
    /// on a timing change.
    private func recomputeItemTiming(itemID: String) {
        guard let snapshot = itemsByID[itemID],
            let next = Self.withDerivedTiming(
                snapshot,
                rows: deliveriesByKey.filter { $0.key.itemID == itemID }.map(\.value.delivery),
                asOf: revision)
        else { return }
        itemsByID[itemID] = next
    }

    /// The item snapshot after the daemon's timing write: derived
    /// aggregates, entity_version raised, item_version unchanged, snapshot
    /// stamped at the given revision; nil when the summary is unchanged
    /// (no write for an aggregate-neutral event).
    private static func withDerivedTiming(
        _ snapshot: Components.Schemas.AttentionItemSnapshot,
        rows: [Components.Schemas.AttentionDelivery],
        asOf revision: Int64
    ) -> Components.Schemas.AttentionItemSnapshot? {
        let derived = derivedTiming(from: rows)
        guard snapshot.item.timing != derived else { return nil }
        var next = snapshot
        next.item.timing = derived
        next.entity_version += 1
        next.as_of_revision = revision
        return next
    }

    /// The item's timing aggregates as the daemon derives them from the
    /// full delivery set (domain WithTiming).
    private static func derivedTiming(
        from rows: [Components.Schemas.AttentionDelivery]
    ) -> Components.Schemas.TimingSummary {
        var submitted: [Date] = []
        var accepted: [Date] = []
        var opened: [Date] = []
        for row in rows {
            switch row {
            case .submitted(let row):
                submitted.append(MockContractValidation.wireDate(row.submitted_at))
            case .channel_accepted(let row):
                submitted.append(MockContractValidation.wireDate(row.submitted_at))
                accepted.append(MockContractValidation.wireDate(row.channel_accepted_at))
            case .opened(let row):
                submitted.append(MockContractValidation.wireDate(row.submitted_at))
                if let acceptedAt = row.channel_accepted_at {
                    accepted.append(MockContractValidation.wireDate(acceptedAt))
                }
                opened.append(MockContractValidation.wireDate(row.opened_at))
            }
        }
        let firstSubmitted = submitted.min()
        let firstOpened = opened.min()
        return .init(
            delivery_count: submitted.count,
            first_submitted_at: firstSubmitted,
            first_accepted_at: accepted.min(),
            first_opened_at: firstOpened,
            submit_to_first_open: firstSubmitted.flatMap { start in
                firstOpened.map { MockContractValidation.durationNanoseconds(from: start, to: $0) }
            }
        )
    }

    /// Read paths re-validate every row they would serve, as the
    /// daemon's reconstruction does (GetAttentionItemSnapshot and
    /// store ListAttentionItems re-run decode().Validate() and the
    /// evidence gate, failing the whole read on the first bad row): a
    /// seed the daemon could never serve fails the read loudly instead
    /// of being hidden, so a partially reconstructed inbox is
    /// unrepresentable. Evidence eligibility beyond the binding
    /// invariant is already unrepresentable in the generated shapes.
    func listAttentionItems() throws -> [Components.Schemas.AttentionItemSnapshot] {
        convergeProposalSnoozes()
        // The daemon's list query orders by item id (store list.go), not
        // by insertion, so seeded scenarios see the same stable order a
        // real inbox would.
        let snapshots = itemsByID.keys.sorted().compactMap { id in
            proposalSnoozesByItemID[id] == nil ? itemsByID[id] : nil
        }
        for snapshot in snapshots {
            if let breach = snapshotBreach(snapshot) {
                throw InvalidItemError(itemID: snapshot.item.id, reason: breach)
            }
        }
        return snapshots.map(projectingEvidenceAvailability)
    }

    /// nil means truly absent (404); an invalid row is a thrown
    /// reconstruction failure, never a not-found.
    func servedSnapshot(
        itemID: String
    ) throws -> Components.Schemas.AttentionItemSnapshot? {
        guard proposalSnoozesByItemID[itemID] == nil else { return nil }
        guard let snapshot = itemsByID[itemID] else { return nil }
        if let breach = snapshotBreach(snapshot) {
            throw InvalidItemError(itemID: itemID, reason: breach)
        }
        return projectingEvidenceAvailability(snapshot)
    }

    func taskProposalFacts(
        itemID: String
    ) throws -> Components.Schemas.TaskProposalFactsSnapshot? {
        guard let snapshot = try servedSnapshot(itemID: itemID), snapshot.item._type == .task_proposal,
            let digest = snapshot.item.evidence_snapshot.first?.digest
        else { return nil }
        if var facts = proposalFactsByItemID[itemID] {
            // As the daemon does, stamp the served item's revision, which a
            // first card open raises (#2930); the card requires them equal.
            facts.as_of_revision = snapshot.as_of_revision
            return facts
        }
        return .init(
            as_of_revision: snapshot.as_of_revision,
            entity_version: snapshot.entity_version,
            item_version: snapshot.item.item_version,
            proposal_digest: digest,
            supersedes: nil,
            intent: .implement_subject,
            expected_cost_units: 12,
            scope: .init(
                component_count: 1, declared_path_count: 3,
                touches_control_plane: false))
    }

    /// Serves the authenticated source-issue-closure facts for one
    /// effect-proposal item, mirroring `taskProposalFacts`: a snoozed or
    /// non-effect_proposal item is invisible (nil, which the transport renders
    /// as 404). An override in `effectProposalFactsByItemID` (set by an
    /// approve_with_changes replacement) wins over the default fixture facts,
    /// which are built from the served snapshot's version tuple.
    func effectProposalFacts(
        itemID: String
    ) throws -> Components.Schemas.EffectProposalFactsSnapshot? {
        guard let snapshot = try servedSnapshot(itemID: itemID), snapshot.item._type == .effect_proposal
        else { return nil }
        if var facts = effectProposalFactsByItemID[itemID] {
            // Stamped with the served item's revision, as task facts are.
            facts.as_of_revision = snapshot.as_of_revision
            return facts
        }
        return AttentionFixtures.effectProposalFacts(for: snapshot)
    }

    /// The facts endpoint's response-boundary read: the authoritative facts
    /// with the test transform applied. The transform mutates only what the
    /// client reads, never the authority the command paths (revision
    /// validation and application) validate and apply against, so those paths
    /// call `effectProposalFacts` directly.
    func effectProposalFactsResponse(
        itemID: String
    ) throws -> Components.Schemas.EffectProposalFactsSnapshot? {
        try effectProposalFacts(itemID: itemID).map { effectProposalFactsTransform?($0) ?? $0 }
    }

    /// The actor's convenience wrapper over
    /// `MockContractValidation.snapshotBreach`, supplying its own trusted
    /// approved-recipe set — never the row's word — so the read paths
    /// re-run the evidence gate against current policy during
    /// reconstruction (EligibleForEvidenceSnapshot; the store
    /// trust-boundary re-gate).
    func snapshotBreach(
        _ snapshot: Components.Schemas.AttentionItemSnapshot
    ) -> String? {
        MockContractValidation.snapshotBreach(snapshot, approvedRecipes: approvedRecipes)
    }

    /// Recomputes each evidence and claim reference's availability from the
    /// byte store on the way out, mirroring signet.projectEvidenceAvailability
    /// (plan §5.15): run evidence and referenced claims are available only
    /// when the digest's bytes are held, and an inline text claim renders
    /// in-band without a fetch, so it is always available. The persisted
    /// availability is never trusted on the wire; it is recomputed per read
    /// exactly as the daemon does immediately before serialization, so it can
    /// differ from the stored value and between two reads of the same item.
    func projectingEvidenceAvailability(
        _ snapshot: Components.Schemas.AttentionItemSnapshot
    ) -> Components.Schemas.AttentionItemSnapshot {
        var snapshot = snapshot
        func availability(of digest: String) -> Components.Schemas.EvidenceAvailability {
            attachmentBytes(digest: digest) != nil ? .available : .bytes_absent
        }
        for index in snapshot.item.evidence_snapshot.indices {
            snapshot.item.evidence_snapshot[index].metadata.availability =
                availability(of: snapshot.item.evidence_snapshot[index].digest)
        }
        for index in snapshot.item.agent_claims.indices {
            let claim = snapshot.item.agent_claims[index]
            snapshot.item.agent_claims[index].metadata.availability =
                claim.text != nil ? .available : availability(of: claim.digest)
        }
        return snapshot
    }

    /// The named feature a command belongs to, or nil for a command every
    /// daemon serves.
    static func feature(
        of payload: Components.Schemas.ClientCommand.payloadPayload
    )
        -> Components.Schemas.DaemonFeature?
    {
        switch payload {
        case .move_parked_task: .parked_task_move
        case .stop_task, .submit_task, .decision, .refresh_usage, .reassign_collector, .change_task_line,
            .set_routing_policy, .set_workflow_policy:
            nil
        }
    }

    func submitCommand(_ command: Components.Schemas.ClientCommand) throws -> SubmitOutcome {
        // The daemon refuses a kind it doesn't serve on the kind alone,
        // before it decodes the payload (signet submitCommand's default arm).
        if let feature = Self.feature(of: command.payload), !(healthFeatures ?? []).contains(feature) {
            return .unservedKind
        }
        convergeProposalSnoozes()
        // Structural well-formedness precedes every lookup, as the signet
        // boundary orders it (domain.NewCommand, then
        // expected_entity_version): ids identify, versions are positive,
        // and digests content-address. Parameterized action content is
        // interpreted only for a genuinely new command below.
        try MockContractValidation.validateStructure(command)
        switch command.payload {
        case .stop_task(let payload):
            return try submitStopTaskCommand(command, payload)
        case .submit_task(let payload):
            return try submitTaskCommand(command, payload)
        case .decision(let payload):
            return try submitDecisionCommand(command, payload)
        case .refresh_usage(let payload):
            return try submitRefreshUsageCommand(command, payload)
        case .reassign_collector(let payload):
            return try submitReassignCollectorCommand(command, payload)
        case .change_task_line(let payload):
            return try submitChangeTaskLineCommand(command, payload)
        case .set_routing_policy(let payload):
            return try submitSetRoutingPolicyCommand(command, payload)
        case .set_workflow_policy(let payload):
            return try submitSetWorkflowPolicyCommand(command, payload)
        case .move_parked_task(let payload):
            return try submitMoveParkedTaskCommand(command, payload)
        }
    }

    private func submitDecisionCommand(
        _ command: Components.Schemas.ClientCommand,
        _ payload: Components.Schemas.DecisionPayload
    ) throws -> SubmitOutcome {
        if let original = commandsByID[command.command_id] {
            // Replay is determined first, as the daemon orders it: a
            // reused id converges only on an identical normalized body,
            // and a different one is an immutable conflict even when its
            // new action would be rejected on other grounds.
            guard original == Self.normalizedReplayCommand(command, payload),
                let recorded = resultsByCommandID[command.command_id]
            else {
                throw ImmutableConflictError(commandID: command.command_id)
            }
            return .ok(commandResultTransform?(recorded) ?? recorded)
        }
        if resultsByCommandID[command.command_id] != nil {
            throw ImmutableConflictError(commandID: command.command_id)
        }
        guard let current = itemsByID[payload.item_id] else {
            throw UnknownItemError(itemID: payload.item_id)
        }
        // Row re-validation precedes signet policy, as the daemon orders
        // it (Submit fetches through GetAttentionItemSnapshot, whose
        // scanner rejects bad metadata and re-runs AttentionItem
        // Validate): a forged seed must fail closed before any binding
        // comparison that would trust the same forged field.
        if let breach = snapshotBreach(current) {
            throw InvalidItemError(itemID: payload.item_id, reason: breach)
        }
        // Durable-item policy re-gates before the pending-action gate,
        // as signet.Submit orders it (validateRequestedActions): a row
        // offering actions outside its type's allowed set is no
        // authority for accepting anything. blocked is read-only (#97):
        // its empty offered set means any command against it falls to
        // the action-not-offered gate below, as on the daemon.
        if let breach = MockContractValidation.itemPolicyBreach(current.item) {
            throw ItemPolicyError(itemID: payload.item_id, reason: breach)
        }
        if proposalSnoozesByItemID[payload.item_id] != nil {
            throw ProposalSnoozedError(itemID: payload.item_id)
        }
        // The pending gate runs only for a genuinely new command against
        // a policy-valid item (ErrUnsupportedAction).
        if case .pending = ActionOutcome.of(payload.action) {
            throw UnsupportedActionError(
                commandID: command.command_id, action: payload.action)
        }
        // Match Signet's command-id-first contract: malformed typed action
        // content cannot displace a recorded result when its normalized
        // durable command body is unchanged. New commands still fail closed
        // before lifecycle and binding decisions.
        try MockContractValidation.validateActionInput(command)
        if payload.action == .discuss,
            let missingDigest = payload.attachments?.first(where: {
                attachmentsByDigest[$0] == nil
            })
        {
            throw MalformedCommandError(
                commandID: command.command_id,
                reason: "attachment \(missingDigest) is not stored")
        }
        // Openness before binding equality, as the daemon orders it. Per
        // the recorded #65 decision (devlog 2026-07-15-1655), a closed
        // item shares the API's 409 replacement-snapshot shape with
        // staleness: closure at any version reports the canonical closed
        // item, never a rebind invitation.
        guard current.item.status == .open else {
            return .stale(
                .init(
                    message: "the item's lifecycle has concluded",
                    replacement_item: projectingEvidenceAvailability(current)
                ))
        }
        let stale =
            command.expected_entity_version != current.entity_version
            || payload.item_version != current.item.item_version
            || payload.pr_head_sha != current.item.pr_head_sha
            // The item's set is canonical; the payload's is canonicalized
            // before comparison (domain.NewCommand), so order and
            // duplicates do not affect binding equality.
            || Array(Set(payload.artifact_digests)).sorted() != current.item.artifact_digests
        if stale {
            return .stale(
                .init(
                    message: "the item changed after the decision was rendered",
                    replacement_item: projectingEvidenceAvailability(current)
                ))
        }
        // The command binds the live item; the action must also be one it
        // offered. Checked after staleness, as the daemon orders it: a
        // stale client re-decides against the replacement's offered set.
        guard current.item.requested_decision.contains(payload.action) else {
            throw ActionNotOfferedError(
                commandID: command.command_id, action: payload.action, itemID: payload.item_id)
        }
        // Revalidate a referenced action surface and derive the daemon-stamped
        // decision evidence before any state mutation (plan §8). A foreign,
        // stale, or unknown surface rejects here, never widening the offered set.
        let stampedEvidence = try stampedDecisionEvidence(for: command, item: current)
        if payload.action == .retry_with_capabilities {
            guard let digest = payload.capability_manifest_digest?.value1,
                current.item.execution_failure?.value1.offered_manifests?.contains(where: {
                    $0.digest == digest
                }) == true
            else {
                throw MalformedCommandError(
                    commandID: command.command_id,
                    reason: "capability manifest was not offered")
            }
        }
        // signet validateAlternateAgentRetryDecision: the choice names an
        // enrollment the item offered, and the engine's chosenSelection
        // holds its model and effort to the enrollment's catalog. Whether the
        // offer still qualifies against current state has no mock analogue,
        // so an offered enrollment is accepted.
        if payload.action == .retry_with_alternate_agent {
            guard let choice = payload.alternate_agent_choice?.value1,
                current.item.alternate_agents?.contains(where: { $0.enrollment == choice.enrollment_id }) == true
            else {
                throw MalformedCommandError(
                    commandID: command.command_id,
                    reason: "alternate agent was not offered")
            }
            _ = try chosenSelection(choice, commandID: command.command_id)
        }
        // The daemon's answer_route policy (signet validateAnswerRoute): an
        // implementation-stage question must name where the answer goes, and
        // both retry_implementation and revise_specification are accepted
        // (#1083); no other command may carry a route.
        let routedAnswer =
            payload.action == .answer_and_retry && current.item._type == .agent_question
            && current.item.agent_question?.value1.stage == .implementation
        if routedAnswer {
            if payload.answer_route?.value1 == nil {
                throw MalformedCommandError(
                    commandID: command.command_id,
                    reason: "answer_and_retry on an implementation-stage question requires answer_route")
            }
        } else if payload.answer_route != nil {
            throw MalformedCommandError(
                commandID: command.command_id, reason: "answer_route is not allowed for this command")
        }
        switch ActionOutcome.of(payload.action) {
        case .discusses:
            if let conversationID = current.item.conversation_id,
                conversationsByID[conversationID]?.conversation.status == .awaiting_agent
            {
                return .stale(
                    .init(
                        message: "the agent's reply is still pending",
                        replacement_item: projectingEvidenceAvailability(current)))
            }
        case .revisesProposal where current.item._type == .effect_proposal:
            guard let closure = payload.effect_proposal_revision?.value1.source_issue_closure else {
                throw MalformedCommandError(
                    commandID: command.command_id, reason: "missing effect_proposal_revision")
            }
            guard let facts = try effectProposalFacts(itemID: payload.item_id),
                facts.source_issue_closure?.value1.resolves != closure.resolves
            else {
                throw InvalidProposalDecisionError(reason: "effect_proposal_revision is unchanged")
            }
        case .revisesProposal:
            guard let revised = payload.task_proposal_revision?.value1 else {
                throw MalformedCommandError(
                    commandID: command.command_id, reason: "missing task_proposal_revision")
            }
            guard let facts = try taskProposalFacts(itemID: payload.item_id),
                !Self.isSameProposal(revised, facts)
            else {
                throw InvalidProposalDecisionError(reason: "task_proposal_revision is unchanged")
            }
        case .snoozesProposal:
            guard let until = payload.snooze_until, until > currentTime else {
                throw MalformedCommandError(
                    commandID: command.command_id, reason: "snooze_until is not in the future")
            }
        case .concludes(_) where payload.action == .adjudicate_findings:
            guard let findings = current.item.review_dispute?.value1.findings,
                let choices = payload.finding_routes, choices.count == findings.count,
                choices.allSatisfy({ choice in
                    findings.contains { $0.finding_id == choice.finding_id && $0.offered_routes.contains(choice.route) }
                })
            else {
                throw InvalidFindingAdjudicationDecisionError(reason: "finding routes are incomplete or not offered")
            }
            // A decline or defer on an external finding posts a reply, so it
            // needs the operator's reason; every other choice refuses one.
            for choice in choices {
                let external = findings.first { $0.finding_id == choice.finding_id }?.external != nil
                let repliesExternally = external && (choice.route == .decline || choice.route == ._defer)
                guard repliesExternally == !(choice.reason ?? "").isEmpty else {
                    throw InvalidFindingAdjudicationDecisionError(
                        reason: "finding route reason is missing, not allowed, or invalid")
                }
            }
        case .concludes(_) where payload.action == .choose_alternative_route:
            guard let binding = current.item.finding_adjudication?.value1,
                let choices = payload.alternative_choices
            else {
                throw InvalidFindingAdjudicationDecisionError(
                    reason: "finding adjudication binding or choices are unavailable")
            }
            for choice in choices {
                guard
                    let proposal = binding.proposals.first(where: {
                        $0.finding_id == choice.finding_id
                    }), proposal.offered_alternatives.contains(where: { $0.route == choice.route })
                else {
                    throw InvalidFindingAdjudicationDecisionError(
                        reason: "choice was not offered for finding \(choice.finding_id)")
                }
            }
        default:
            break
        }
        revision += 1
        switch ActionOutcome.of(payload.action) {
        case .concludes(let status):
            itemsByID[payload.item_id] = concluded(current, as: status, by: command, payload)
            if payload.action == .request_changes {
                pendingSpecificationReplacements[command.command_id] = current
                pendingSpecificationComments[command.command_id] = payload.message
            }
            if payload.action == .answer_and_retry
                && payload.answer_route?.value1 == .revise_specification,
                let provenance = current.item.agent_claims.first?.provenance
            {
                // Mirror the daemon route (#1083 D8): the answer files as
                // specification feedback and a revised spec_approval supersedes
                // the conceptual baseline. The fixture synthesizes that baseline
                // (a prior approved specification) shaped like a spec_approval and
                // keyed on the question, so the existing replacement path mints
                // the revised card whose spec_revision names the question and
                // whose prior_comments carries the answer. The mock does not mint
                // a new campaign or run; #1209 owns those fixtures.
                let baselineBody = "# Specification\n\nThe blocked implementation's approved specification."
                var baseline = current
                baseline.item._type = .spec_approval
                baseline.item.agent_question = nil
                baseline.item.spec_revision = nil
                baseline.item.agent_claims = [
                    AttentionFixtures.agentClaim(
                        label: "Specification",
                        artifactID: "spec-revision-baseline-\(command.command_id)",
                        digest: MockContractValidation.sha256Digest(of: baselineBody),
                        provenance: provenance,
                        text: .init(media_type: .text_sol_markdown, content: baselineBody))
                ]
                pendingSpecificationReplacements[command.command_id] = baseline
                pendingSpecificationComments[command.command_id] = payload.message
            }
        case .discusses:
            let conversationID = current.item.conversation_id ?? "conv-\(payload.item_id)"
            var conversation =
                conversationsByID[conversationID]
                ?? .init(
                    as_of_revision: revision,
                    entity_version: 0,
                    conversation: .init(id: conversationID, status: .idle, messages: []))
            conversation.as_of_revision = revision
            conversation.entity_version += 1
            conversation.conversation.status = .awaiting_agent
            conversation.conversation.messages.append(
                .init(
                    id: "msg-user-\(command.command_id)",
                    conversation_id: conversationID,
                    sequence: conversation.conversation.messages.count + 1,
                    author: .user,
                    body: payload.message ?? "",
                    attachments: payload.attachments ?? [],
                    created_at: currentTime))
            conversationsByID[conversationID] = conversation
            var discussing = current
            discussing.as_of_revision = revision
            discussing.entity_version += 1
            discussing.item.item_version += 1
            discussing.item.conversation_id = conversationID
            itemsByID[payload.item_id] = discussing
        case .stopsUnattended:
            // The daemon's stop transaction (signet applyStopUnattended,
            // #319): conclude the decided item and ensure exactly one open
            // notice offers resume_unattended — a duplicate open notice
            // would still block after the other one resumed, so a second
            // stop converges on the existing one. The durable transition
            // is recorded for every accepted stop, converged or not, and
            // rides the bootstrap's unattended_operation.
            itemsByID[payload.item_id] = concluded(current, as: .resolved, by: command, payload)
            operatorStop = (commandID: command.command_id, since: currentTime)
            let alreadyOffered = itemsByID.values.contains {
                $0.item.status == .open && $0.item._type == .system_health
                    && $0.item.requested_decision.contains(.resume_unattended)
            }
            if !alreadyOffered {
                let noticeID = "system-health-unattended-stopped-\(command.command_id)"
                itemsByID[noticeID] = .init(
                    as_of_revision: revision, entity_version: 1,
                    item: .init(
                        id: noticeID,
                        project_id: current.item.project_id,
                        subject: .system(
                            .init(subject_type: .system, subject_id: "daemon", run_id: nil)),
                        _type: .system_health,
                        priority: .high,
                        reason: "Unattended operation is stopped by operator decision "
                            + "(item \(payload.item_id)). No new unattended work is admitted "
                            + "until resume_unattended is accepted.",
                        requested_decision: [.resume_unattended, .acknowledge],
                        recommendation: nil,
                        decision_surface: .init(
                            epoch: 1,
                            digest: MockContractValidation.sha256Digest(
                                of: "decision-surface-\(noticeID)-1")),
                        evidence_snapshot: [],
                        agent_claims: [],
                        artifact_digests: [],
                        pr_head_sha: "",
                        commit_plan_notice: nil,
                        review_recovery_binding: nil,
                        codex_reenrollment_recovery_binding: nil,
                        review_configuration_recovery: nil,
                        health_diagnostic: .init(
                            value1: .init(
                                code: Self.stoppedNoticeDiagnostic,
                                impairs: .unattended_admission)),
                        item_version: 1,
                        interruption_class: .exceptional,
                        conversation_id: nil,
                        timing: .init(
                            delivery_count: 0,
                            first_submitted_at: nil,
                            first_accepted_at: nil,
                            first_opened_at: nil,
                            submit_to_first_open: nil
                        ),
                        created_at: currentTime,
                        expires_when: nil,
                        decided_at: nil,
                        posture: .init(value1: .blocking),
                        blocking_supersession: nil,
                        status: .open
                    ))
            }
        case .resumesUnattended:
            // The daemon's resume transaction concludes the stopped notice
            // and appends the resumed transition, lifting the operator stop.
            itemsByID[payload.item_id] = concluded(current, as: .resolved, by: command, payload)
            operatorStop = nil
        case .recoversReview:
            // The daemon also appends the exact-row transition; it has no API
            // surface, so resolving the carrier is the mock's observable parity.
            itemsByID[payload.item_id] = concluded(current, as: .resolved, by: command, payload)
        case .adoptsReviewConfiguration:
            // The daemon also appends the profile-supersession-bound
            // transition; it has no API surface, so resolving the carrier is
            // the mock's observable parity.
            itemsByID[payload.item_id] = concluded(current, as: .resolved, by: command, payload)
        case .resolvesReenrollment:
            // The daemon additionally records and re-gates the verified
            // operation transition; the carrier resolution is the sync-visible
            // portion the mock can mirror.
            itemsByID[payload.item_id] = concluded(current, as: .resolved, by: command, payload)
        case .revisesProposal where current.item._type == .effect_proposal:
            guard let closure = payload.effect_proposal_revision?.value1.source_issue_closure,
                payload.task_proposal_revision == nil, payload.snooze_until == nil,
                (payload.message ?? "").isEmpty, (payload.attachments ?? []).isEmpty
            else {
                throw MalformedCommandError(
                    commandID: command.command_id,
                    reason: "approve_with_changes requires only effect_proposal_revision")
            }
            guard let priorFacts = try effectProposalFacts(itemID: payload.item_id),
                let priorClosure = priorFacts.source_issue_closure?.value1,
                var artifact = current.item.evidence_snapshot.first
            else {
                throw MalformedCommandError(
                    commandID: command.command_id, reason: "effect proposal facts are unavailable")
            }
            let revisedDigest = Self.closureProposalDigest(
                prior: priorFacts.proposal_digest, resolves: closure.resolves)
            var superseded = current
            superseded.entity_version += 1
            superseded.as_of_revision = revision
            superseded.item.item_version += 1
            superseded.item.status = .superseded
            superseded.item.conclusion = Self.daemonConclusion(
                .superseded, at: currentTime,
                reason: "Superseded by the revised proposal this decision made.",
                successor: payload.item_id + "/revision/" + command.command_id)
            itemsByID[payload.item_id] = superseded

            artifact.id += "-revision-\(command.command_id)"
            artifact.digest = revisedDigest
            let replacementID = payload.item_id + "/revision/" + command.command_id
            var replacement = current
            replacement.as_of_revision = revision
            replacement.entity_version = 1
            replacement.item.id = replacementID
            // A new item on a new surface: nobody has opened it (#2930).
            replacement.item.surface_first_opened_at = nil
            replacement.item.reason = "Approve the revised source-issue-closure proposal"
            replacement.item.evidence_snapshot = [artifact]
            replacement.item.agent_claims = []
            replacement.item.artifact_digests = [revisedDigest]
            replacement.item.decision_surface = .init(
                epoch: 1,
                digest: MockContractValidation.sha256Digest(
                    of: "decision-surface-\(replacementID)-1"))
            replacement.item.item_version += 1
            replacement.item.status = .resolved
            replacement.item.created_at = currentTime
            replacement.item.decided_at = currentTime
            replacement.item.conclusion = decisionConclusion(command, payload, at: currentTime)
            itemsByID[replacementID] = replacement
            effectProposalFactsByItemID[replacementID] = .init(
                as_of_revision: revision, entity_version: 1,
                item_version: replacement.item.item_version,
                proposal_digest: revisedDigest,
                effect_kind: .source_issue_closure,
                supersedes: .init(
                    value1: .init(
                        proposal_digest: priorFacts.proposal_digest,
                        source_issue_closure: .init(resolves: priorClosure.resolves))),
                source_issue_closure: .init(
                    value1: .init(
                        target: priorClosure.target,
                        resolves: closure.resolves,
                        provenance: priorClosure.provenance,
                        origin: priorClosure.origin,
                        merge: priorClosure.merge)))
        case .revisesProposal:
            guard let revised = payload.task_proposal_revision?.value1, payload.snooze_until == nil,
                (payload.message ?? "").isEmpty, (payload.attachments ?? []).isEmpty
            else {
                throw MalformedCommandError(
                    commandID: command.command_id,
                    reason: "start_with_changes requires only task_proposal_revision")
            }
            guard let priorFacts = try taskProposalFacts(itemID: payload.item_id),
                var artifact = current.item.evidence_snapshot.first
            else {
                throw MalformedCommandError(
                    commandID: command.command_id, reason: "proposal facts are unavailable")
            }
            let revisedDigest = Self.proposalDigest(revised)
            var superseded = current
            superseded.entity_version += 1
            superseded.as_of_revision = revision
            superseded.item.item_version += 1
            superseded.item.status = .superseded
            superseded.item.conclusion = Self.daemonConclusion(
                .superseded, at: currentTime,
                reason: "Superseded by the revised proposal this decision made.",
                successor: payload.item_id + "/revision/" + command.command_id)
            itemsByID[payload.item_id] = superseded

            artifact.id += "-revision-\(command.command_id)"
            artifact.digest = revisedDigest
            let replacementID = payload.item_id + "/revision/" + command.command_id
            var replacement = current
            replacement.as_of_revision = revision
            replacement.entity_version = 1
            replacement.item.id = replacementID
            // A new item on a new surface: nobody has opened it (#2930).
            replacement.item.surface_first_opened_at = nil
            replacement.item.reason = "Start the revised daemon-enumerated work subject"
            replacement.item.evidence_snapshot = [artifact]
            replacement.item.agent_claims = []
            replacement.item.artifact_digests = [revisedDigest]
            replacement.item.decision_surface = .init(
                epoch: 1,
                digest: MockContractValidation.sha256Digest(
                    of: "decision-surface-\(replacementID)-1"))
            replacement.item.item_version += 1
            replacement.item.status = .resolved
            replacement.item.created_at = currentTime
            replacement.item.decided_at = currentTime
            replacement.item.conclusion = decisionConclusion(command, payload, at: currentTime)
            itemsByID[replacementID] = replacement
            proposalFactsByItemID[replacementID] = .init(
                as_of_revision: revision, entity_version: 1,
                item_version: replacement.item.item_version,
                proposal_digest: revisedDigest,
                supersedes: .init(
                    value1: .init(
                        proposal_digest: priorFacts.proposal_digest,
                        intent: priorFacts.intent,
                        expected_cost_units: priorFacts.expected_cost_units,
                        scope: priorFacts.scope)),
                intent: revised.intent,
                expected_cost_units: revised.expected_cost_units,
                scope: revised.scope)
        case .snoozesProposal:
            guard let until = payload.snooze_until,
                payload.task_proposal_revision == nil, (payload.message ?? "").isEmpty,
                (payload.attachments ?? []).isEmpty
            else {
                throw MalformedCommandError(
                    commandID: command.command_id,
                    reason: "snooze requires only a future snooze_until")
            }
            var snoozed = current
            snoozed.entity_version += 1
            snoozed.as_of_revision = revision
            snoozed.item.item_version += 1
            itemsByID[payload.item_id] = snoozed
            proposalSnoozesByItemID[payload.item_id] = until
        case .records:
            // The command record is the whole server-side effect; the
            // item row is left untouched (signet outcomeRecords).
            break
        case .pending:
            // Unreachable: the pending gate above already rejected it.
            throw UnsupportedActionError(
                commandID: command.command_id, action: payload.action)
        }
        let recordedMessage = try CommandResultTrust.recordedMessage(payload)
        let result = Components.Schemas.CommandResult(
            record: .decision(
                .init(
                    kind: .decision,
                    command_id: command.command_id,
                    device_id: command.device_id,
                    item_id: payload.item_id,
                    item_version: payload.item_version,
                    pr_head_sha: payload.pr_head_sha,
                    // The record persists the canonical set (domain.NewCommand),
                    // whatever order or duplication the payload carried.
                    artifact_digests: Array(Set(payload.artifact_digests)).sorted(),
                    action: payload.action,
                    // Conversation content renders in the record even when empty
                    // (one byte-form per write-once record, domain.NewCommand);
                    // attachment order is authored, never canonicalized.
                    message: recordedMessage,
                    attachments: payload.attachments ?? [],
                    answer_route: payload.answer_route.map { .init(value1: $0.value1) },
                    decision_evidence: stampedEvidence.map { .init(value1: $0) }
                )),
            revision: revision
        )
        commandsByID[command.command_id] = NormalizedCommand(
            command, payload, message: recordedMessage)
        resultsByCommandID[command.command_id] = result
        scheduleAutomaticAgentCompletionIfNeeded(for: payload.action)
        return .ok(commandResultTransform?(result) ?? result)
    }

    struct InactiveStopDeviceError: Error {}

    private func submitStopTaskCommand(
        _ command: Components.Schemas.ClientCommand,
        _ payload: Components.Schemas.StopTaskPayload
    ) throws -> SubmitOutcome {
        if case .enforcing = authMode {
            guard let device = devicesByID[command.device_id], case .active = device.device else {
                throw InactiveStopDeviceError()
            }
        }
        if let recorded = resultsByCommandID[command.command_id] {
            guard CommandResultTrust.accepts(recorded, for: command) else {
                throw ImmutableConflictError(commandID: command.command_id)
            }
            return .ok(commandResultTransform?(recorded) ?? recorded)
        }
        guard var snapshot = try task(id: payload.task_id), snapshot.task.project_id == payload.project_id else {
            throw UnknownItemError(itemID: payload.task_id)
        }
        // Accept any revision the client could have observed (1..current):
        // unrelated writes advance the revision on a live task. Only a stale
        // epoch or a version above the current revision is rejected, mirroring
        // the daemon's StopTask rule.
        guard let expectedVersion = command.expected_entity_version, payload.expected_sync_epoch == syncEpoch,
            expectedVersion >= 1, expectedVersion <= revision
        else {
            return .staleTask(
                .init(message: "task cancellation binding changed", replacement_task: snapshot, sync_epoch: syncEpoch))
        }
        let target = Components.Schemas.TaskCancellationTarget(
            task_id: payload.task_id, project_id: payload.project_id,
            episode_ordinal: snapshot.task.lifecycle_facts.lastIndex(where: { $0.kind == .started }).map { $0 + 1 }
                ?? 0,
            runs: try snapshot.task.run_ids.map { id in
                guard let run = runsByID[id]?.run, run.task_id == payload.task_id, run.project_id == payload.project_id
                else {
                    throw InvalidTaskError(taskID: payload.task_id, reason: "foreign cancellation run")
                }
                return .init(run_id: id, campaign_id: run.campaign_id)
            })
        let digest = MockContractValidation.cancellationTargetDigest(target, epoch: syncEpoch)
        revision += 1
        let cancellation: Components.Schemas.TaskCancellation
        if let old = snapshot.task.cancellation?.value1, old.target_digest == digest {
            cancellation = old
        } else {
            cancellation = .init(
                request_id: "cancel-\(UUID().uuidString.lowercased())", target: target,
                target_digest: digest, sync_epoch: syncEpoch, fence_revision: revision,
                requested_at: currentTime, state: .requested, acknowledgement: nil)
        }
        snapshot.task.cancellation = .init(value1: cancellation)
        snapshot.entity_version = revision
        snapshot.as_of_revision = revision
        tasksByID[payload.task_id] = snapshot
        let record = Components.Schemas.StopTaskRecord(
            kind: .stop_task, command_id: command.command_id,
            device_id: command.device_id, task_id: payload.task_id, project_id: payload.project_id,
            expected_sync_epoch: payload.expected_sync_epoch, expected_entity_version: expectedVersion,
            cancellation: cancellation)
        let result = Components.Schemas.CommandResult(record: .stop_task(record), revision: revision)
        resultsByCommandID[command.command_id] = result
        return .ok(commandResultTransform?(result) ?? result)
    }

    // MARK: - Operator commands

    /// An operator command the daemon refuses as invalid client input (400):
    /// an enrollment not on the pool, an enrollment the daemon doesn't hold,
    /// or a malformed order.
    public struct OperatorCommandRejectedError: Error {
        public let commandID: String
        public let reason: String
    }

    /// A set_routing_policy against a daemon that holds no committed agent
    /// tree: the daemon's hook fails closed with
    /// signet.ErrOperatorSettingUnavailable, which it answers as a 500.
    public struct OperatorSettingUnavailableError: Error {}

    /// Marks whether an attempt of a task's role is running; while it runs,
    /// change_task_line for that role is refused (domain.ErrTaskLineRoleRunning).
    public func setTaskLineRoleRunning(taskID: String, role: Components.Schemas.TaskLineRole, running: Bool) {
        if running {
            runningTaskLineRoles.insert(Self.taskLineKey(taskID, role))
        } else {
            runningTaskLineRoles.remove(Self.taskLineKey(taskID, role))
        }
    }

    private static func taskLineKey(_ taskID: String, _ role: Components.Schemas.TaskLineRole) -> String {
        "\(taskID)/\(role.rawValue)"
    }

    /// Replays a recorded operator command, as the daemon's receipt replay
    /// does: the recorded result for the same request, an immutable conflict
    /// for any other body or kind under the command_id, nil when none is
    /// recorded.
    private func replayOperatorCommand(_ command: Components.Schemas.ClientCommand) throws -> SubmitOutcome? {
        guard let recorded = resultsByCommandID[command.command_id] else { return nil }
        guard CommandResultTrust.accepts(recorded, for: command) else {
            throw ImmutableConflictError(commandID: command.command_id)
        }
        return .ok(commandResultTransform?(recorded) ?? recorded)
    }

    private func recordOperatorResult(
        _ command: Components.Schemas.ClientCommand, _ record: Components.Schemas.CommandResult.recordPayload
    ) -> SubmitOutcome {
        let result = Components.Schemas.CommandResult(record: record, revision: revision)
        resultsByCommandID[command.command_id] = result
        return .ok(commandResultTransform?(result) ?? result)
    }

    /// Records a refresh request. Readings arrive through the pool's usage
    /// record (advanceUsagePool), never through the receipt.
    private func submitRefreshUsageCommand(
        _ command: Components.Schemas.ClientCommand, _ payload: Components.Schemas.RefreshUsagePayload
    ) throws -> SubmitOutcome {
        if let replay = try replayOperatorCommand(command) { return replay }
        guard usagePoolsByID[payload.pool] != nil else { throw UnknownItemError(itemID: payload.pool) }
        revision += 1
        return recordOperatorResult(
            command,
            .refresh_usage(
                .init(
                    kind: .refresh_usage, command_id: command.command_id, device_id: command.device_id,
                    pool: payload.pool, requested_at: currentTime)))
    }

    /// Records a collector reassignment to an enrollment on the pool and, in
    /// the same revision, the pool record it leaves, as the daemon's
    /// `usagecollect.ReassignTx` does: the new collector, with the old
    /// collector's readings, plan, model map, and refresh status cleared.
    /// Availability answers and refusal marks stay, because they describe the
    /// pool; a reading's exhausted mark goes with its reading. Naming the
    /// current collector records only the receipt.
    private func submitReassignCollectorCommand(
        _ command: Components.Schemas.ClientCommand, _ payload: Components.Schemas.ReassignCollectorPayload
    ) throws -> SubmitOutcome {
        if let replay = try replayOperatorCommand(command) { return replay }
        guard var snapshot = usagePoolsByID[payload.pool] else { throw UnknownItemError(itemID: payload.pool) }
        guard snapshot.pool.enrollments.contains(where: { $0.enrollment_id == payload.enrollment_id }) else {
            throw OperatorCommandRejectedError(
                commandID: command.command_id, reason: "enrollment is not on pool \(payload.pool)")
        }
        revision += 1
        if snapshot.pool.collector != payload.enrollment_id {
            snapshot.pool.collector = payload.enrollment_id
            snapshot.pool.plan = nil
            snapshot.pool.last_attempt = nil
            snapshot.pool.model_windows = []
            snapshot.pool.windows = []
            snapshot.pool.recorded_exhausted_marks.removeAll { $0.source == .usage_reading }
            snapshot.as_of_revision = revision
            snapshot.entity_version += 1
            usagePoolsByID[payload.pool] = snapshot
        }
        return recordOperatorResult(
            command,
            .reassign_collector(
                .init(
                    kind: .reassign_collector, command_id: command.command_id, device_id: command.device_id,
                    pool: payload.pool, collector: payload.enrollment_id, recorded_at: currentTime)))
    }

    /// Changes or clears one role's line on a task (engine.ChangeTaskLineTx):
    /// a role that already has the line records nothing new, and a role whose
    /// attempt runs refuses with the current task.
    private func submitChangeTaskLineCommand(
        _ command: Components.Schemas.ClientCommand, _ payload: Components.Schemas.ChangeTaskLinePayload
    ) throws -> SubmitOutcome {
        if let replay = try replayOperatorCommand(command) { return replay }
        guard var snapshot = try task(id: payload.task_id) else { throw UnknownItemError(itemID: payload.task_id) }
        let selection = payload.selection?.value1
        let key = Self.taskLineKey(payload.task_id, payload.role)
        let head = taskLineHeads[key]
        let same = selection == head?.line?.selection?.value1 && head?.line?.agent == nil
        // The daemon checks the selection first, then answers a request for
        // the line the role already has before it asks whether the role runs.
        if let selection {
            try validateLineSelection(selection, field: "selection", commandID: command.command_id)
        }
        guard same || !runningTaskLineRoles.contains(key) else {
            return .taskLineRejected(
                .init(
                    message: "an attempt of the role is running", reason: .role_running,
                    replacement_task: snapshot, sync_epoch: syncEpoch))
        }
        revision += 1
        if !same {
            let version = (head?.version ?? 0) + 1
            let setBy = MockContractValidation.sha256Digest(of: "client_line_change/\(command.command_id)")
            let id = MockContractValidation.sha256Digest(
                of: [
                    payload.task_id, payload.role.rawValue, selection.map(Self.selectionText) ?? "", String(version),
                    head?.id ?? "", setBy,
                ]
                .joined(separator: "/"))
            let line = selection.map { selection in
                Components.Schemas.TaskLine(
                    id: id, task_id: payload.task_id, role: payload.role, selection: .init(value1: selection),
                    version: version, predecessor_id: head.map { .init(value1: $0.id) }, source: .client_line_change,
                    set_by: setBy, set_at: currentTime, provenance: nil)
            }
            recordTaskLineEvent(
                taskID: payload.task_id, lineID: id, role: payload.role, selection: selection, version: version,
                source: .client_line_change, superseded: head?.line)
            taskLineHeads[key] = (version, id, line)
            snapshot.task.task_lines = Components.Schemas.TaskLineRole.allCases.compactMap {
                taskLineHeads[Self.taskLineKey(payload.task_id, $0)]?.line
            }
            snapshot.entity_version = revision
            snapshot.as_of_revision = revision
            tasksByID[payload.task_id] = snapshot
        }
        return recordOperatorResult(
            command,
            .change_task_line(
                .init(
                    kind: .change_task_line, command_id: command.command_id, device_id: command.device_id,
                    task_id: payload.task_id, role: payload.role, agent: nil,
                    selection: selection.map { .init(value1: $0) }, recorded: !same,
                    line: taskLineHeads[key]?.line.map { .init(value1: $0) })))
    }

    /// Moves a task parked on its implementer's exhausted quota window to an
    /// enrollment its hold offers (engine.MoveParkedTaskTx). The mock pins
    /// only the implementer, under the offer's pool, and releases the hold as
    /// the parked attempt's start would; a task with no such hold, an
    /// enrollment the hold does not offer, or a model or effort it does not
    /// serve, is refused and records nothing.
    private func submitMoveParkedTaskCommand(
        _ command: Components.Schemas.ClientCommand, _ payload: Components.Schemas.MoveParkedTaskPayload
    ) throws -> SubmitOutcome {
        if let replay = try replayOperatorCommand(command) { return replay }
        guard var snapshot = try task(id: payload.task_id) else { throw UnknownItemError(itemID: payload.task_id) }
        guard let runID = snapshot.task.current_position?.value1.run_id,
            var history = taskHistoryFixtures[payload.task_id],
            let hold = history.sections.flatMap(\.runs).first(where: { $0.run_id == runID })?.hold?.value1,
            let window = hold.quota_window?.value1, let invocationID = window.invocation_id
        else {
            throw OperatorCommandRejectedError(commandID: command.command_id, reason: "task is not parked")
        }
        guard let offer = window.alternate_agents?.first(where: { $0.enrollment == payload.choice.enrollment_id })
        else {
            throw OperatorCommandRejectedError(
                commandID: command.command_id, reason: "alternate agent no longer qualifies")
        }
        let selection = try chosenSelection(payload.choice, commandID: command.command_id)
        revision += 1
        let retry = Components.Schemas.AlternateAgentRetry(
            id: MockContractValidation.sha256Digest(
                of: [
                    "alternate_agent_retry", payload.task_id, invocationID, command.command_id,
                    Self.selectionText(selection),
                ]
                .joined(separator: "/")),
            task_id: payload.task_id, role: .implementer, failed_invocation_id: invocationID, item_id: "",
            command_id: command.command_id, selection: selection, pool: offer.pool,
            qualification: offer.qualification, recorded_at: currentTime)
        let key = Self.taskLineKey(payload.task_id, .implementer)
        let head = taskLineHeads[key]
        let version = (head?.version ?? 0) + 1
        let id = MockContractValidation.sha256Digest(
            of: [
                payload.task_id, "implementer", Self.selectionText(selection), String(version), head?.id ?? "",
                retry.id,
            ]
            .joined(separator: "/"))
        // The provenance names the enrollment the move left, with the hold's
        // mark, when the policy orders it (engine.movedPastMark): a v2 line's
        // selection's enrollment. A v1 line names no enrollment, and an
        // agents-form role orders nothing (engine.roleOrder, #2599).
        let leftEnrollment = head?.line.flatMap { $0.selection?.value1.enrollment_id }
        let left = leftEnrollment.flatMap { enrollment -> String? in
            let ordered = routingPolicy?.roles.contains { ($0.enrollments ?? []).contains(enrollment) }
            return ordered == true ? enrollment : nil
        }
        let line = Components.Schemas.TaskLine(
            id: id, task_id: payload.task_id, role: .implementer, selection: .init(value1: selection), version: version,
            predecessor_id: head.map { .init(value1: $0.id) }, source: .alternate_agent_move, set_by: retry.id,
            set_at: currentTime,
            provenance: .init(
                value1: .init(
                    policy_revision: routingPolicy?.revision ?? 1, tier: .policy_order, pool: offer.pool,
                    ranked_by: [],
                    passed_over: left.map {
                        .init(
                            value1: .init(
                                agent: nil, enrollment: $0, pool: window.pool, window: window.window,
                                source: window.source,
                                marked_at: window.set_at, resets_at: window.resets_at))
                    })))
        recordTaskLineEvent(
            taskID: payload.task_id, lineID: id, role: .implementer, selection: selection, version: version,
            source: .alternate_agent_move, superseded: head?.line)
        taskLineHeads[key] = (version, id, line)
        snapshot.task.task_lines = Components.Schemas.TaskLineRole.allCases.compactMap {
            taskLineHeads[Self.taskLineKey(payload.task_id, $0)]?.line
        }
        snapshot.entity_version = revision
        snapshot.as_of_revision = revision
        tasksByID[payload.task_id] = snapshot
        for section in history.sections.indices {
            for run in history.sections[section].runs.indices where history.sections[section].runs[run].run_id == runID
            {
                history.sections[section].runs[run].hold = nil
            }
        }
        taskHistoryFixtures[payload.task_id] = history
        return recordOperatorResult(
            command,
            .move_parked_task(
                .init(
                    kind: .move_parked_task, command_id: command.command_id, device_id: command.device_id,
                    task_id: payload.task_id, choice: payload.choice, retry: retry, lines: [line])))
    }

    /// The selection a choice from an offer runs on the implementer
    /// (engine chosenSelection): the choice's enrollment with its model and
    /// effort, or the implementer lineup line's where it names none. The
    /// enrollment's discovered catalog must serve the pair; one with no
    /// catalog takes only the lineup's. A refusal names the field.
    private func chosenSelection(
        _ choice: Components.Schemas.AlternateAgentChoice, commandID: String
    ) throws -> Components.Schemas.TaskLineSelection {
        guard let line = agentCatalog.lineup.first(where: { $0.role == .implementer })?.selection else {
            throw OperatorCommandRejectedError(commandID: commandID, reason: "alternate agent no longer qualifies")
        }
        let selection = Components.Schemas.TaskLineSelection(
            enrollment_id: choice.enrollment_id, model_id: choice.model_id ?? line.model_id,
            effort: choice.effort ?? line.effort)
        try validateLineSelection(selection, field: "choice", commandID: commandID)
        let catalogued =
            agentCatalog.enrollments.first(where: { $0.enrollment_id == choice.enrollment_id })?.discovery != nil
        if !catalogued, selection.model_id != line.model_id || selection.effort != line.effort {
            let field = selection.model_id != line.model_id ? "model_id" : "effort"
            throw MalformedCommandError(
                commandID: commandID, reason: "choice.\(field): not the lineup's on an enrollment with no catalog")
        }
        return selection
    }

    /// Records the task_line_set event for a line version this mock wrote,
    /// as the daemon's taskHistoryEvents builds it: the mock writes only v2
    /// versions, so the selection is nil only on a cleared version, and the
    /// superseded pair is the previous version in its own encoding, nil for
    /// version 1 or after a cleared version.
    private func recordTaskLineEvent(
        taskID: String, lineID: String, role: Components.Schemas.TaskLineRole,
        selection: Components.Schemas.TaskLineSelection?, version: Int,
        source: Components.Schemas.TaskLineSource, superseded: Components.Schemas.TaskLine?
    ) {
        taskLineEvents[taskID, default: []].append(
            .init(
                line: .init(
                    value1: .init(
                        line_id: lineID, role: role, selection: selection.map { .init(value1: $0) },
                        version: version, source: source, superseded_agent: superseded?.agent,
                        superseded_selection: superseded?.selection.map { .init(value1: $0.value1) },
                        provenance: nil)),
                kind: .task_line_set, recorded_at: currentTime))
    }

    /// A selection as one string, for the mock's line identities.
    private static func selectionText(_ selection: Components.Schemas.TaskLineSelection) -> String {
        [selection.enrollment_id, selection.model_id, selection.effort].joined(separator: "/")
    }

    /// The daemon's store check of a line's selection (signet
    /// validateLineSelection, domain DiscoveredModelCatalog.Serves): the
    /// enrollment is one the catalog lists, and when it has a discovery
    /// record, the model is one the record lists, by id or else by resolved
    /// model, at an effort one of those entries lists, or harness_default.
    /// Claude Code's native-default model id names no catalog model. The
    /// refusal names field and its leg, never the selection's text.
    private func validateLineSelection(
        _ selection: Components.Schemas.TaskLineSelection, field: String, commandID: String
    ) throws {
        func unserved(_ leg: String, _ reason: String) -> MalformedCommandError {
            MalformedCommandError(commandID: commandID, reason: "\(field).\(leg): \(reason)")
        }
        guard let enrollment = agentCatalog.enrollments.first(where: { $0.enrollment_id == selection.enrollment_id })
        else {
            throw unserved("enrollment_id", "no enrollment the daemon holds")
        }
        guard selection.model_id != "claude-code-native-default", let discovery = enrollment.discovery?.value1 else {
            return
        }
        let byID = discovery.models.filter { $0.id == selection.model_id }
        let entries = byID.isEmpty ? discovery.models.filter { $0.resolved_model == selection.model_id } : byID
        guard !entries.isEmpty else {
            throw unserved("model_id", "not a model the enrollment's discovered catalog lists")
        }
        guard
            selection.effort == "harness_default"
                || entries.contains(where: { $0.effort_levels.contains(selection.effort) })
        else {
            throw unserved("effort", "not an effort the model's discovered catalog entry lists")
        }
    }

    /// Replaces the routing policy (engine.SetRoutingPolicyTx) when the
    /// client's expected_revision names the current one; the orders the
    /// current revision already holds record no new revision.
    private func submitSetRoutingPolicyCommand(
        _ command: Components.Schemas.ClientCommand, _ payload: Components.Schemas.SetRoutingPolicyPayload
    ) throws -> SubmitOutcome {
        func rejected(_ reason: String) -> OperatorCommandRejectedError {
            OperatorCommandRejectedError(commandID: command.command_id, reason: reason)
        }
        let roles: [Components.Schemas.RoutingPolicyRole]
        do {
            roles = try MockContractValidation.canonicalRoutingPolicyRoles(payload.roles)
        } catch let shape as MockContractValidation.RoutingPolicyShapeError {
            throw rejected(shape.reason)
        }
        if let replay = try replayOperatorCommand(command) { return replay }
        guard payload.expected_revision == routingPolicy?.revision else {
            return .routingPolicyRejected(
                .init(message: "the routing policy changed", current_policy: routingPolicy.map { .init(value1: $0) }))
        }
        guard let lineupRevision = agentCatalog.lineup_revision?.value1 else { throw OperatorSettingUnavailableError() }
        // The daemon refuses an enrollment it doesn't hold by the role and
        // position (engine.checkRoutingPolicyEnrollments), never the id.
        let held = Set(agentCatalog.enrollments.map(\.enrollment_id))
        for role in roles {
            if let position = role.enrollments?.firstIndex(where: { !held.contains($0) }) {
                throw rejected(
                    "routing policy role \(role.role.rawValue) enrollment \(position + 1): "
                        + "routing policy enrollment is not one the store holds")
            }
        }
        // The drain-first account must be the pool of an ordered enrollment.
        if let account = payload.drain_first_account {
            let ordered = Set(roles.flatMap { $0.enrollments ?? [] })
            guard
                let pool = usagePoolsByID[account],
                pool.pool.enrollments.contains(where: { ordered.contains($0.enrollment_id) })
            else {
                throw rejected("routing policy drain-first account is no ordered enrollment's usage pool")
            }
        }
        let recorded =
            routingPolicy?.roles != roles || routingPolicy?.drain_first_account != payload.drain_first_account
        revision += 1
        if recorded {
            routingPolicy = .init(
                revision: (routingPolicy?.revision ?? 0) + 1, roles: roles, source: .client_routing_set,
                set_by: "device:\(command.device_id)", set_at: currentTime,
                lineup_revision: lineupRevision, drain_first_account: payload.drain_first_account)
        }
        guard let policy = routingPolicy else { throw rejected("no routing policy") }
        return recordOperatorResult(
            command,
            .set_routing_policy(
                .init(
                    kind: .set_routing_policy, command_id: command.command_id, device_id: command.device_id,
                    expected_revision: payload.expected_revision, recorded: recorded, policy: policy)))
    }

    /// Sets or clears one workflow policy layer (engine.SetWorkflowPolicyTx)
    /// when the client's expected_revision names the current one; the value
    /// the layer already holds records no new revision. The shape was checked
    /// in validateStructure.
    private func submitSetWorkflowPolicyCommand(
        _ command: Components.Schemas.ClientCommand, _ payload: Components.Schemas.SetWorkflowPolicyPayload
    ) throws -> SubmitOutcome {
        if let replay = try replayOperatorCommand(command) { return replay }
        if let project = payload.project_id, !workflowProjects.contains(project) {
            throw OperatorCommandRejectedError(
                commandID: command.command_id, reason: "workflow policy project is not one the daemon serves")
        }
        guard (payload.expected_revision ?? 0) == workflowPolicy.revision else {
            return .workflowPolicyRejected(
                .init(
                    message: "the workflow policy changed",
                    current_workflow_policy: workflowPolicy.snapshot(projects: workflowProjects, asOfRevision: revision)
                ))
        }
        let layer = WorkflowPolicyLayers.Layer(projectID: payload.project_id, key: payload.key)
        let recorded = workflowPolicy.entries[layer]?.value != payload.value
        revision += 1
        if recorded {
            workflowPolicy.revision += 1
            workflowPolicy.entries[layer] = payload.value.map {
                .init(value: $0, setBy: "device:\(command.device_id)", setAt: currentTime)
            }
        }
        return recordOperatorResult(
            command,
            .set_workflow_policy(
                .init(
                    kind: .set_workflow_policy, command_id: command.command_id, device_id: command.device_id,
                    scope: payload.scope, project_id: payload.project_id, key: payload.key, value: payload.value,
                    expected_revision: payload.expected_revision, recorded: recorded,
                    revision: workflowPolicy.revision)))
    }

    /// Creates new work for each distinct command ID and replays the original
    /// result for an exact retry. The mock accepts any project; transport owns
    /// revoked-device refusal, as for a decision command.
    private func submitTaskCommand(
        _ command: Components.Schemas.ClientCommand,
        _ payload: Components.Schemas.SubmitTaskPayload
    ) throws -> SubmitOutcome {
        // The lines are checked and put in role order before the replay
        // check, as the daemon does (signet canonicalTaskLines), so a retry
        // listing the same lines in another order is the same request.
        var payload = payload
        let requestedLines = payload.task_lines
        payload.task_lines = try Self.canonicalTaskLines(payload.task_lines, commandID: command.command_id)
        // Attachments are resolved before the replay check too, as the daemon
        // does (signet ResolveTaskAttachments); their order is part of the
        // request, so the stored request below compares it.
        let attachments = try resolveTaskAttachments(payload.attachments, commandID: command.command_id)
        var request = command
        request.payload = .submit_task(payload)
        let digest = MockContractValidation.sha256Digest(of: payload.source)
        if let recorded = resultsByCommandID[command.command_id] {
            // A reused command_id replays only a submit_task result for the same
            // device, project, source, name, and set of lines; anything else is
            // an immutable conflict, matching the daemon's replay guard.
            guard case .submit_task(let record) = recorded.record,
                record.device_id == command.device_id,
                record.project_id == payload.project_id,
                record.source_digest.value1 == digest,
                submissionRequestsByID[command.command_id] == request,
                Self.attachmentNameBytes(submissionRequestsByID[command.command_id])
                    == Self.attachmentNameBytes(request)
            else {
                throw ImmutableConflictError(commandID: command.command_id)
            }
            return .ok(commandResultTransform?(recorded) ?? recorded)
        }
        // Checked after the replay, in the order the client sent them, as the
        // daemon checks them against its store (signet validateLineSelections).
        for (index, line) in (requestedLines ?? []).enumerated() {
            try validateLineSelection(
                line.selection, field: "task_lines[\(index)].selection", commandID: command.command_id)
        }
        let key = command.command_id
        let suffix = String(
            MockContractValidation.sha256Digest(of: key).dropFirst("sha256:".count).suffix(12))
        let taskID = "task-submitted-\(suffix)"
        let runID = "run-submitted-\(suffix)"
        let name = Self.submittedTaskName(operatorName: payload.name, source: payload.source, fallback: taskID)
        // The daemon puts the source in the blob store before it records the
        // task (signet task_submission), and the task names it as its
        // work-item artifact, so the read endpoints can serve the text by
        // the digest the task carries. Stored only for a fresh submission.
        attachmentsByDigest[digest] = Data(payload.source.utf8)
        revision += 1
        materializeSubmittedTask(
            taskID: taskID, runID: runID, campaignID: "campaign-submitted-\(suffix)",
            artifactID: "artifact-submitted-\(suffix)",
            projectID: payload.project_id, name: name, sourceDigest: digest,
            lines: payload.task_lines ?? [], attachments: attachments, commandID: command.command_id)
        revision += 1
        let record = Components.Schemas.TaskSubmissionRecord(
            kind: .submit_task, command_id: command.command_id, origin: .device,
            device_id: command.device_id,
            project_id: payload.project_id, source_digest: .init(value1: digest),
            task_id: taskID, specification_run_id: runID, name: .init(value1: name),
            task_lines: payload.task_lines?.map { .init(role: $0.role, selection: .init(value1: $0.selection)) })
        let result = Components.Schemas.CommandResult(record: .submit_task(record), revision: revision)
        resultsByCommandID[command.command_id] = result
        submissionRequestsByID[command.command_id] = request
        return .ok(commandResultTransform?(result) ?? result)
    }

    /// The daemon's task-line check for a submission (domain
    /// CanonicalTaskLineChoices): absent stays absent; a present list names
    /// at least one role, each once, with a well-formed selection, and comes
    /// back in TaskLineRole order. The store check is validateLineSelection.
    private static func canonicalTaskLines(
        _ lines: [Components.Schemas.TaskLineInput]?, commandID: String
    ) throws -> [Components.Schemas.TaskLineInput]? {
        guard let lines else { return nil }
        func malformed(_ reason: String) -> MalformedCommandError {
            MalformedCommandError(commandID: commandID, reason: reason)
        }
        guard !lines.isEmpty else { throw malformed("task_lines must name at least one role when present") }
        guard Set(lines.map(\.role)).count == lines.count else { throw malformed("task_lines names a role twice") }
        guard lines.allSatisfy({ MockContractValidation.validLineSelection($0.selection) }) else {
            throw malformed("task_lines carries a malformed selection")
        }
        return Components.Schemas.TaskLineRole.allCases.compactMap { role in lines.first { $0.role == role } }
    }

    /// The daemon's attachment check for a submission (signet
    /// ResolveTaskAttachments and domain ValidateTaskAttachments): absent is
    /// none; a present list is non-empty, every name and claim is well formed,
    /// every digest is stored, and the sizes, count, and names fit the task's
    /// limits. Each attachment takes its media type from its bytes' leading
    /// signature and its size from the stored bytes, in submitted order.
    private func resolveTaskAttachments(
        _ inputs: [Components.Schemas.TaskAttachmentInput]?, commandID: String
    ) throws -> [Components.Schemas.TaskAttachment] {
        guard let inputs else { return [] }
        func malformed(_ reason: String) -> MalformedCommandError {
            MalformedCommandError(commandID: commandID, reason: reason)
        }
        guard !inputs.isEmpty else { throw malformed("attachments must name at least one file when present") }
        guard inputs.count <= TaskAttachmentLimits.maxCount else { throw malformed("too many attachments") }
        for (index, input) in inputs.enumerated() {
            guard TaskAttachmentLimits.validDigest(input.digest) else {
                throw malformed("attachment \(index) has a malformed digest")
            }
            guard TaskAttachmentLimits.validName(input.name) else {
                throw malformed("attachment \(index) has an invalid name")
            }
            guard TaskAttachmentLimits.validClaim(input.media_type) else {
                throw malformed("attachment \(index) has a malformed claimed media type")
            }
        }
        var total = 0
        let resolved = try inputs.enumerated().map { index, input in
            guard let bytes = attachmentsByDigest[input.digest] else {
                throw malformed("attachment \(index) is not stored")
            }
            let mediaType = TaskAttachmentLimits.sniff(bytes)
            let limit =
                mediaType == .application_sol_octet_hyphen_stream
                ? TaskAttachmentLimits.maxFileBytes : TaskAttachmentLimits.maxImageBytes
            guard bytes.count <= limit else { throw malformed("attachment \(index) exceeds its size limit") }
            total += bytes.count
            return Components.Schemas.TaskAttachment(
                digest: input.digest, name: input.name, media_type: mediaType,
                claimed_media_type: input.media_type, size_bytes: Int64(bytes.count))
        }
        guard total <= TaskAttachmentLimits.maxTotalBytes else { throw malformed("attachments exceed the task budget") }
        guard !TaskAttachmentLimits.namesConflict(resolved.map(\.name)) else {
            throw malformed("attachment names repeat, or one is a directory of another")
        }
        return resolved
    }

    /// The attachment names of a stored submit_task request as UTF-8 bytes.
    /// The request comparison above uses String equality, which is canonical
    /// equivalence; the daemon's request digest tells an NFC name from its
    /// NFD form, so a replay renamed that way conflicts there and here.
    private static func attachmentNameBytes(_ command: Components.Schemas.ClientCommand?) -> [[UInt8]]? {
        guard case .submit_task(let payload)? = command?.payload else { return nil }
        return payload.attachments?.map { Array($0.name.utf8) }
    }

    /// submittedTaskName mirrors the daemon: a trimmed operator-supplied name
    /// wins (the daemon stores the whitespace-trimmed canonical form); a source
    /// whose first non-empty line is a Markdown heading names the task from that
    /// heading; anything else keeps the identifier fallback. The name has already
    /// passed validateCommand, so a non-empty trim is guaranteed here.
    private static func submittedTaskName(
        operatorName: String?, source: String, fallback: String
    ) -> Components.Schemas.DisplayName {
        if let operatorName {
            let trimmed = operatorName.trimmingCharacters(in: .whitespacesAndNewlines)
            if !trimmed.isEmpty {
                return .init(text: trimmed, source: ._operator)
            }
        }
        for rawLine in source.split(separator: "\n", omittingEmptySubsequences: false) {
            let line = rawLine.trimmingCharacters(in: .whitespaces)
            if line.isEmpty { continue }
            guard line.hasPrefix("#") else { break }
            let title = line.drop(while: { $0 == "#" }).trimmingCharacters(in: .whitespaces)
            if !title.isEmpty {
                return .init(text: title, source: ._operator)
            }
            break
        }
        return .init(text: fallback, source: .identifier)
    }

    /// materializeSubmittedTask makes the created task and its specification run
    /// visible to the read endpoints so a submission is followed by read-your-
    /// write. The run is templated from a fixture to keep a valid shape.
    private func materializeSubmittedTask(
        taskID: String, runID: String, campaignID: String, artifactID: String,
        projectID: String, name: Components.Schemas.DisplayName, sourceDigest: String,
        lines: [Components.Schemas.TaskLineInput], attachments: [Components.Schemas.TaskAttachment],
        commandID: String
    ) {
        let displayNames = Components.Schemas.DisplayNames(
            project: .init(text: projectID, source: .identifier), task: name)
        // Template the run from a fixture for a valid shape, then clear the
        // stages so their bindings do not name the fixture's run (a contract
        // breach the read endpoints reject).
        if var template = RunFixtures.defaultRuns().first?.run {
            template.id = runID
            template.project_id = projectID
            template.task_id = taskID
            template.campaign_id = campaignID
            template.attempt_number = 1
            template.attempt_reason = nil
            template.parent_run_id = nil
            template.spec_digest = sourceDigest
            template.display_names = .init(value1: displayNames)
            template.created_at = currentTime
            template.last_activity_at = currentTime
            template.stages = []
            runsByID[runID] = .init(as_of_revision: revision, entity_version: 1, run: template)
        }
        // The specification run's timeline carries its submission milestone. A
        // campaign-backed run must show run-submitted evidence, so without this
        // taskTimeline throws when the newly submitted task's timeline is opened.
        timelinesByRunID[runID] = .init(
            as_of_revision: revision, as_of: currentTime, run_id: runID,
            milestones: [
                .init(
                    run_id: runID, kind: .run_submitted,
                    invocation_id: "inv-\(runID)-1", recorded_at: currentTime)
            ],
            invocations: [], admissions: [])
        // The task projects its one specification run: an active, work-in-progress
        // task positioned at that run, with the run and its campaign in the task's
        // membership so taskTimeline agrees with the run store (mirrors the daemon
        // and TaskFixtures.defaultTasks). Without the membership and position the
        // task read is self-inconsistent and read-your-write on the timeline fails.
        // Each operator line is version 1 of its role, set by the command when
        // the task is created (engine SubmitTask), so a later change_task_line
        // supersedes it.
        let taskLines = lines.map { line in
            let id = MockContractValidation.sha256Digest(
                of: [taskID, line.role.rawValue, Self.selectionText(line.selection), "1", "", commandID]
                    .joined(separator: "/"))
            let recorded = Components.Schemas.TaskLine(
                id: id, task_id: taskID, role: line.role, selection: .init(value1: line.selection), version: 1,
                predecessor_id: nil, source: .submit_task, set_by: commandID, set_at: currentTime, provenance: nil)
            taskLineHeads[Self.taskLineKey(taskID, line.role)] = (1, id, recorded)
            recordTaskLineEvent(
                taskID: taskID, lineID: id, role: line.role, selection: line.selection, version: 1,
                source: .submit_task, superseded: nil)
            return recorded
        }
        let task = Components.Schemas.Task(
            id: taskID, project_id: projectID, display_names: displayNames,
            source: .init(
                value1: .work_item_artifact(
                    .init(
                        kind: .work_item_artifact, work_item_artifact_id: artifactID,
                        work_item_artifact_digest: .init(value1: sourceDigest)))),
            created_at: currentTime, last_activity_at: currentTime,
            lifecycle: .active,
            current_position: .init(value1: .init(run_id: runID)),
            campaign_ids: [campaignID], run_ids: [runID], wip: true,
            lifecycle_facts: [.init(kind: .started, run_id: runID, recorded_at: currentTime)],
            task_lines: taskLines, attachments: attachments)
        tasksByID[taskID] = .init(as_of_revision: revision, entity_version: 1, task: task)
    }

    /// Completes asynchronous mock work explicitly. Read endpoints never call
    /// this hook, matching the daemon's side-effect-free reads.
    public func completePendingAgentWork() {
        for commandID in pendingSpecificationReplacements.keys.sorted() {
            guard var replacement = pendingSpecificationReplacements.removeValue(forKey: commandID)
            else { continue }
            let priorItemID = replacement.item.id
            let comment = pendingSpecificationComments.removeValue(forKey: commandID) ?? ""
            let priorRevision = replacement.item.spec_revision?.value1
            let iteration = (priorRevision?.iteration ?? 1) + 1
            let commentID = commandID
            let response = "Updated the specification to address this comment."
            let addressals = [
                Components.Schemas.SpecAddressalClaim(
                    comment_id: commentID, response: response)
            ]
            let addressalsDigest = MockContractValidation.addressalsDigest(addressals)
            guard
                let specificationIndex = replacement.item.agent_claims.firstIndex(where: {
                    $0.label == "Specification"
                }),
                let priorSpecification = replacement.item.agent_claims[specificationIndex].text?
                    .content
            else { continue }
            let priorSpecClaim = replacement.item.agent_claims[specificationIndex]
            let revisionLine = "Revision response: \(response)"
            let revisedSpecification = priorSpecification + "\n" + revisionLine
            let priorLineCount = priorSpecification.split(
                separator: "\n", omittingEmptySubsequences: false
            ).count
            let unifiedDiff =
                "@@ -1,\(priorLineCount) +1,\(priorLineCount + 1) @@\n"
                + priorSpecification.split(separator: "\n", omittingEmptySubsequences: false)
                .map { " \($0)" }.joined(separator: "\n")
                + "\n+\(revisionLine)"
            var comments = priorRevision?.prior_comments ?? []
            comments.append(
                .init(
                    comment_id: commentID,
                    artifact_id: "spec-feedback-\(commentID)",
                    digest: MockContractValidation.sha256Digest(of: comment),
                    raised_on_item_id: priorItemID,
                    iteration: iteration - 1,
                    body: comment))
            revision += 1
            replacement.as_of_revision = revision
            replacement.entity_version = 1
            replacement.item.id = Self.specificationReplacementID(
                for: replacement, commandID: commandID)
            replacement.item.item_version = 1
            replacement.item.reason = "The revised specification is ready for approval"
            replacement.item.status = .open
            replacement.item.decided_at = nil
            replacement.item.conclusion = nil
            replacement.item.created_at = currentTime
            replacement.item.conversation_id = nil
            replacement.item.spec_revision = .init(
                value1: .init(
                    iteration: iteration,
                    prior_item_id: priorItemID,
                    prior_spec_artifact_id: priorSpecClaim.artifact_id,
                    prior_spec_digest: priorSpecClaim.digest,
                    diff: .init(
                        lines_added: 1,
                        lines_removed: 0,
                        unified: unifiedDiff,
                        truncated: false),
                    prior_comments: comments,
                    claimed_addressals: addressals,
                    addressals_digest: addressalsDigest))
            let specificationClaim = AttentionFixtures.agentClaim(
                label: "Specification",
                artifactID: "spec-mock-\(iteration)",
                digest: MockContractValidation.sha256Digest(of: revisedSpecification),
                provenance: priorSpecClaim.provenance,
                text: .init(media_type: .text_sol_markdown, content: revisedSpecification))
            let summary = replacement.item.reason
            let summaryClaim = AttentionFixtures.agentClaim(
                label: "freeside.summary",
                artifactID: "spec-summary-mock-\(iteration)",
                digest: MockContractValidation.sha256Digest(of: summary),
                provenance: priorSpecClaim.provenance,
                text: .init(media_type: .text_sol_markdown, content: summary))
            let addressalsClaim = AttentionFixtures.agentClaim(
                label: "Addressals",
                artifactID: "spec-addressals-mock-\(iteration)",
                digest: addressalsDigest,
                provenance: priorSpecClaim.provenance)
            replacement.item.agent_claims = [
                specificationClaim,
                summaryClaim,
                addressalsClaim,
            ]
            replacement.item.artifact_digests = Array(
                Set(
                    replacement.item.evidence_snapshot.map(\.digest)
                        + replacement.item.agent_claims.map(\.digest))
            ).sorted()
            itemsByID[replacement.item.id] = replacement
        }
        for id in conversationsByID.keys.sorted() {
            guard var snapshot = conversationsByID[id],
                snapshot.conversation.status == .awaiting_agent,
                let userMessage = snapshot.conversation.messages.last,
                userMessage.author == .user
            else { continue }
            revision += 1
            snapshot.as_of_revision = revision
            snapshot.entity_version += 1
            snapshot.conversation.status = .idle
            let commandID =
                userMessage.id.hasPrefix("msg-user-")
                ? String(userMessage.id.dropFirst("msg-user-".count)) : userMessage.id
            snapshot.conversation.messages.append(
                .init(
                    id: "msg-agent-\(commandID)",
                    conversation_id: id,
                    sequence: snapshot.conversation.messages.count + 1,
                    author: .agent,
                    body: "I reviewed your message and updated the work.",
                    attachments: [],
                    created_at: currentTime.addingTimeInterval(30)))
            conversationsByID[id] = snapshot
            if let itemID = itemsByID.keys.sorted().first(where: {
                itemsByID[$0]?.item.conversation_id == id
            }), var item = itemsByID[itemID], item.item.status == .open {
                item.as_of_revision = revision
                item.entity_version += 1
                item.item.item_version += 1
                itemsByID[itemID] = item
            }
        }
    }

    private func scheduleAutomaticAgentCompletionIfNeeded(
        for action: Components.Schemas.Action
    ) {
        guard automaticallyCompletesAgentWork,
            action == .discuss || action == .request_changes
        else { return }
        Task {
            try? await Task.sleep(for: .seconds(1))
            completePendingAgentWork()
        }
    }

    /// Reconstruct the body Signet compares on command-id replay. A valid
    /// typed payload becomes its canonical durable message; malformed typed
    /// input falls back to the raw structural message, so the already-written
    /// command remains the sole replay authority.
    private static func normalizedReplayCommand(
        _ command: Components.Schemas.ClientCommand,
        _ payload: Components.Schemas.DecisionPayload
    ) -> NormalizedCommand {
        let message: String
        do {
            try MockContractValidation.validateActionInput(command)
            message = try CommandResultTrust.recordedMessage(payload)
        } catch {
            message = payload.message ?? ""
        }
        return NormalizedCommand(command, payload, message: message)
    }

    private func convergeProposalSnoozes() {
        let expired = proposalSnoozesByItemID.filter { $0.value <= currentTime }.map(\.key)
        for itemID in expired {
            proposalSnoozesByItemID.removeValue(forKey: itemID)
            guard var snapshot = itemsByID[itemID] else { continue }
            revision += 1
            snapshot.as_of_revision = revision
            snapshot.entity_version += 1
            snapshot.item.item_version += 1
            itemsByID[itemID] = snapshot
        }
    }

    private static func proposalDigest(
        _ revision: Components.Schemas.TaskProposalRevisionInput
    ) -> String {
        MockContractValidation.sha256Digest(
            of: "\(revision.intent.rawValue)|\(revision.expected_cost_units)|"
                + "\(revision.scope.component_count)|\(revision.scope.declared_path_count)|"
                + "\(revision.scope.touches_control_plane)")
    }

    /// A distinct digest for a revised closure proposal: an approve_with_changes
    /// flips only resolves, so the digest is derived from the prior digest and
    /// the new flag. It must differ from the prior digest, which the caller
    /// already ensured by rejecting an unchanged resolves.
    private static func closureProposalDigest(prior: String, resolves: Bool) -> String {
        MockContractValidation.sha256Digest(of: "\(prior)|resolves:\(resolves)")
    }

    private static func specificationReplacementID(
        for snapshot: Components.Schemas.AttentionItemSnapshot,
        commandID: String
    ) -> String {
        let runID: String?
        switch snapshot.item.subject {
        case .run(let run), .proposal_batch(let run): runID = run.run_id
        case .task, .project, .system: runID = nil
        }
        if let runID {
            let prefix = "spec-approval-\(runID)-"
            if snapshot.item.id.hasPrefix(prefix),
                let iteration = Int(snapshot.item.id.dropFirst(prefix.count))
            {
                return "\(prefix)\(iteration + 1)"
            }
            return "\(prefix)2"
        }
        return snapshot.item.id + "/revision/" + commandID
    }

    private static func isSameProposal(
        _ revision: Components.Schemas.TaskProposalRevisionInput,
        _ facts: Components.Schemas.TaskProposalFactsSnapshot
    ) -> Bool {
        revision.intent == facts.intent
            && revision.expected_cost_units == facts.expected_cost_units
            && revision.scope.component_count == facts.scope.component_count
            && revision.scope.declared_path_count == facts.scope.declared_path_count
            && revision.scope.touches_control_plane == facts.scope.touches_control_plane
    }

    /// The concluding decision's item side: version bump, terminal status,
    /// and the decision instant stamped in the same mutation as the flip
    /// (signet concludeItem, #171), with the conclusion naming the command
    /// (#2097); a retry replays the recorded result and never re-stamps.
    private func concluded(
        _ snapshot: Components.Schemas.AttentionItemSnapshot,
        as status: Components.Schemas.ItemStatus,
        by command: Components.Schemas.ClientCommand,
        _ payload: Components.Schemas.DecisionPayload
    ) -> Components.Schemas.AttentionItemSnapshot {
        var applied = snapshot
        applied.entity_version += 1
        applied.as_of_revision = revision
        applied.item.item_version += 1
        applied.item.status = status
        applied.item.decided_at = Self.decidedInstant
        applied.item.conclusion = decisionConclusion(command, payload, at: Self.decidedInstant)
        return applied
    }

    /// The decision conclusion signet's commandDecision records: the
    /// command, its action and device with the device's name at decision
    /// time, and the route list a route-carrying action sent, sorted by
    /// finding. The daemon refuses a command from a device it has no row
    /// for; the permissive mock accepts unpaired devices, so it names one
    /// by its id.
    private func decisionConclusion(
        _ command: Components.Schemas.ClientCommand,
        _ payload: Components.Schemas.DecisionPayload,
        at instant: Date
    ) -> Components.Schemas.AttentionItem.conclusionPayload {
        let routes: [Components.Schemas.AlternativeChoice]? =
            switch payload.action {
            case .adjudicate_findings:
                // The conclusion records each route, never the reply reason.
                payload.finding_routes?.map { .init(finding_id: $0.finding_id, route: $0.route) }
            case .choose_alternative_route: payload.alternative_choices
            default: nil
            }
        let deviceName: String =
            switch devicesByID[command.device_id]?.device {
            case .active(let active): active.display_name
            case .revoked(let revoked): revoked.display_name
            case nil: command.device_id
            }
        return .init(
            value1: .init(
                cause: .decision, concluded_at: instant, reason: "", successor_item_id: nil,
                decision: .init(
                    value1: .init(
                        command_id: command.command_id, action: payload.action,
                        device_id: command.device_id, device_display_name: deviceName,
                        finding_routes: routes?.sorted { $0.finding_id < $1.finding_id },
                        answer_route: payload.answer_route.map { .init(value1: $0.value1) }))))
    }

    /// A conclusion the daemon writes on its own close: cause, instant, the
    /// one-sentence reason, and the replacement it minted, if any.
    static func daemonConclusion(
        _ cause: Components.Schemas.ConclusionCause, at instant: Date, reason: String,
        successor: String? = nil
    ) -> Components.Schemas.AttentionItem.conclusionPayload {
        .init(
            value1: .init(
                cause: cause, concluded_at: instant, reason: reason, successor_item_id: successor,
                decision: nil))
    }

}
