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

        init(_ command: Components.Schemas.ClientCommand, message: String) {
            commandID = command.command_id
            deviceID = command.device_id
            itemID = command.payload.item_id
            action = command.payload.action
            itemVersion = command.payload.item_version
            prHeadSHA = command.payload.pr_head_sha
            artifactDigests = Array(Set(command.payload.artifact_digests)).sorted()
            // Content fields normalize absent to empty (the daemon's record
            // shape); attachment order is authored, so it is compared as
            // sent, never canonicalized.
            self.message = message
            attachments = command.payload.attachments ?? []
        }
    }

    private var itemsByID: [String: Components.Schemas.AttentionItemSnapshot] = [:]
    private var conversationsByID: [String: Components.Schemas.ConversationSnapshot] = [:]
    private var commandsByID: [String: NormalizedCommand] = [:]
    private var resultsByCommandID: [String: Components.Schemas.CommandResult] = [:]
    private var pendingSpecificationReplacements: [String: Components.Schemas.AttentionItemSnapshot] = [:]
    private var pendingSpecificationComments: [String: String] = [:]
    private var proposalFactsByItemID: [String: Components.Schemas.RunProposalFactsSnapshot] = [:]
    private var proposalSnoozesByItemID: [String: Date] = [:]
    private var currentTime = Date(timeIntervalSince1970: 1_786_502_645)
    private var revision: Int64 = 1
    private var syncEpoch = "mock-epoch"
    private var epochGeneration = 1
    private var healthVersion = "mock"
    private var healthStartedAt = Date(timeIntervalSince1970: 1_725_184_800)
    private var healthAvailable = true
    private var beforeRespond: BeforeRespond?
    private var afterRespond: BeforeRespond?
    private var commandResultTransform: CommandResultTransform?
    private var conversationTransform: ConversationTransform?
    private var bootstrapTransform: BootstrapTransform?
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
    /// Pairing-grant test hooks. Defaults satisfy the contract; callers can
    /// inject malformed values to exercise client-side returned-object gates.
    private let pairingNtfyServerURL: String
    private let pairingNtfyTopic: String?
    private let pairingDeviceToken: String?
    /// The digest-addressed artifact bytes `getAttachment` serves
    /// (plan §4: cards render image attachments directly from the
    /// artifact store by digest). Content is immutable per digest, so
    /// the table only seeds; nothing rewrites an entry.
    private let attachmentsByDigest: [String: Data]
    /// Delivery rows the receipt surface serves and advances, keyed by
    /// the attempt's full identity as the daemon's composite key is.
    /// Rows only seed through init; the one mutation is the opened
    /// receipt, which advances an existing attempt and never creates one.
    private var deliveriesByKey: [DeliveryKey: Components.Schemas.AttentionDeliverySnapshot] = [:]
    private var runsByID: [String: Components.Schemas.RunSnapshot] = [:]
    private var schedulesByID: [String: Components.Schemas.ScheduleSnapshot] = [:]
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
        schedules: [Components.Schemas.ScheduleSnapshot] = RunFixtures.defaultSchedules(),
        timelines: [Components.Schemas.RunTimeline] = RunFixtures.defaultTimelines(),
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
        // The server revision starts at or beyond every seeded snapshot's
        // as_of_revision, so the heartbeat and the next CommandResult can
        // never run backwards relative to what this mock lists.
        revision = max(
            1, items.map(\.as_of_revision).max() ?? 1,
            conversations.map(\.as_of_revision).max() ?? 1,
            deliveries.map(\.as_of_revision).max() ?? 1,
            runs.map(\.as_of_revision).max() ?? 1,
            schedules.map(\.as_of_revision).max() ?? 1,
            timelines.map(\.as_of_revision).max() ?? 1)
        // Seeded delivery rows exist only because the daemon's pipeline
        // would have recorded them, and that pipeline re-derives the
        // item's timing and bumps the item's versions in the same write
        // (SubmitDelivery → recomputeItemTiming), so a fixture item's
        // authored timing is never trusted next to seeded rows: seeding
        // applies the same derivation, version bump, and
        // unchanged-summary skip a live write would.
        // Only rows the daemon's PutAttentionDelivery would have accepted
        // fold into the derivation: an invalid seed never reaches
        // recomputeItemTiming in the daemon, and here it stays out of the
        // served aggregates while every delivery-serving read still fails
        // closed on it. The daemon records one SubmitDelivery transaction
        // per row and bumps the item on each summary-changing recompute,
        // so seeding replays the rows one at a time in composite-key
        // order rather than folding the set in one pretended write.
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

    /// Controls the mock process boundary independently of synchronized
    /// state, so liveness clients can exercise running, outage, and restart.
    public func setHealthAvailable(_ available: Bool) {
        healthAvailable = available
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
            for id in schedulesByID.keys {
                schedulesByID[id]?.as_of_revision = revision
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
        pendingSpecificationReplacements.removeAll()
        pendingSpecificationComments.removeAll()
        proposalFactsByItemID.removeAll()
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
        case .active: return .device(id: deviceID)
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
        guard let original = commandsByID[command.command_id],
            original == Self.normalizedReplayCommand(command),
            original.deviceID == deviceID
        else { return nil }
        return resultsByCommandID[command.command_id]
    }

    // MARK: - Contract semantics

    enum SubmitOutcome {
        case ok(Components.Schemas.CommandResult)
        case stale(Components.Schemas.StaleVersionRejection)
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

    func serverRevision() -> Components.Schemas.ServerRevision {
        return .init(sync_epoch: syncEpoch, revision: revision)
    }

    func healthStatus() throws -> Components.Schemas.HealthStatus {
        guard healthAvailable else { throw HealthUnavailableError() }
        return .init(status: .ok, version: healthVersion, started_at: healthStartedAt)
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
            conversations: conversationsByID.keys.sorted().compactMap { conversationsByID[$0] },
            schedules: listSchedules()
        )
        return bootstrapTransform?(snapshot) ?? snapshot
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

    func runTimeline(id: String) -> Components.Schemas.RunTimeline? {
        timelinesByRunID[id]
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

    /// Ingests one comprehension event, idempotent by (device, event_id) and
    /// without moving the revision. Action-bearing kinds must reference a
    /// matching accepted command.
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
                recorded.record.device_id == deviceID,
                recorded.record.item_id == input.item_id,
                let evidence = recorded.record.decision_evidence,
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
        var recommendedAction: Components.Schemas.Action?
        var recommendationSource: Components.Schemas.RecommendationSource?
        if let rec = current.item.recommendation {
            recommendedAction = rec.value1.action
            recommendationSource = rec.value1.source
        }
        var surfaceDigest = ""
        if let digest = command.payload.decision_action_surface_digest {
            guard let surface = actionSurfacesByDigest[digest],
                surface.device_id == command.device_id,
                surface.item_id == command.payload.item_id,
                surface.item_decision_surface_digest == current.item.decision_surface.digest,
                let contract = capabilityContractsByDevice[command.device_id],
                surface.client_capability_digest == contract.digest,
                surface.actions.contains(command.payload.action)
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
    /// bumps the item's versions only when the summary actually changed —
    /// an aggregate-neutral receipt must not churn item versions.
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
    /// aggregates, item/entity versions bumped, snapshot stamped at the
    /// given revision; nil when the summary is unchanged (no version
    /// churn for an aggregate-neutral event).
    private static func withDerivedTiming(
        _ snapshot: Components.Schemas.AttentionItemSnapshot,
        rows: [Components.Schemas.AttentionDelivery],
        asOf revision: Int64
    ) -> Components.Schemas.AttentionItemSnapshot? {
        let derived = derivedTiming(from: rows)
        guard snapshot.item.timing != derived else { return nil }
        var next = snapshot
        next.item.timing = derived
        next.item.item_version += 1
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

    func runProposalFacts(
        itemID: String
    ) throws -> Components.Schemas.RunProposalFactsSnapshot? {
        guard let snapshot = try servedSnapshot(itemID: itemID), snapshot.item._type == .run_proposal,
            let digest = snapshot.item.evidence_snapshot.first?.digest
        else { return nil }
        if let facts = proposalFactsByItemID[itemID] {
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

    func submitCommand(_ command: Components.Schemas.ClientCommand) throws -> SubmitOutcome {
        convergeProposalSnoozes()
        // Structural well-formedness precedes every lookup, as the signet
        // boundary orders it (domain.NewCommand, then
        // expected_entity_version): ids identify, versions are positive,
        // and digests content-address. Parameterized action content is
        // interpreted only for a genuinely new command below.
        try MockContractValidation.validateStructure(command)
        if let original = commandsByID[command.command_id] {
            // Replay is determined first, as the daemon orders it: a
            // reused id converges only on an identical normalized body,
            // and a different one is an immutable conflict even when its
            // new action would be rejected on other grounds.
            guard original == Self.normalizedReplayCommand(command),
                let recorded = resultsByCommandID[command.command_id]
            else {
                throw ImmutableConflictError(commandID: command.command_id)
            }
            return .ok(commandResultTransform?(recorded) ?? recorded)
        }
        let payload = command.payload
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
        // The daemon's answer_route policy (signet validateAnswerRoute): an
        // implementation-stage question must name where the answer goes;
        // revise_specification is refused as not yet available; no other
        // command may carry a route.
        let routedAnswer =
            payload.action == .answer_and_retry && current.item._type == .agent_question
            && current.item.agent_question?.value1.stage == .implementation
        if routedAnswer {
            guard let route = payload.answer_route?.value1 else {
                throw MalformedCommandError(
                    commandID: command.command_id,
                    reason: "answer_and_retry on an implementation-stage question requires answer_route")
            }
            if route == .revise_specification {
                throw UnsupportedActionError(commandID: command.command_id, action: payload.action)
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
        case .revisesProposal:
            guard let revised = payload.run_proposal_revision?.value1 else {
                throw MalformedCommandError(
                    commandID: command.command_id, reason: "missing run_proposal_revision")
            }
            guard let facts = try runProposalFacts(itemID: payload.item_id),
                !Self.isSameProposal(revised, facts)
            else {
                throw InvalidProposalDecisionError(reason: "run_proposal_revision is unchanged")
            }
        case .snoozesProposal:
            guard let until = payload.snooze_until, until > currentTime else {
                throw MalformedCommandError(
                    commandID: command.command_id, reason: "snooze_until is not in the future")
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
            itemsByID[payload.item_id] = concluded(current, as: status)
            if payload.action == .request_changes {
                pendingSpecificationReplacements[command.command_id] = current
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
            // log itself has no API surface, so the item effects are the
            // whole observable parity.
            itemsByID[payload.item_id] = concluded(current, as: .resolved)
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
            // The daemon's resume transaction concludes the stopped notice;
            // the operating-state effect has no API surface.
            itemsByID[payload.item_id] = concluded(current, as: .resolved)
        case .recoversReview:
            // The daemon also appends the exact-row transition; it has no API
            // surface, so resolving the carrier is the mock's observable parity.
            itemsByID[payload.item_id] = concluded(current, as: .resolved)
        case .adoptsReviewConfiguration:
            // The daemon also appends the profile-supersession-bound
            // transition; it has no API surface, so resolving the carrier is
            // the mock's observable parity.
            itemsByID[payload.item_id] = concluded(current, as: .resolved)
        case .resolvesReenrollment:
            // The daemon additionally records and re-gates the verified
            // operation transition; the carrier resolution is the sync-visible
            // portion the mock can mirror.
            itemsByID[payload.item_id] = concluded(current, as: .resolved)
        case .revisesProposal:
            guard let revised = payload.run_proposal_revision?.value1, payload.snooze_until == nil,
                (payload.message ?? "").isEmpty, (payload.attachments ?? []).isEmpty
            else {
                throw MalformedCommandError(
                    commandID: command.command_id,
                    reason: "start_with_changes requires only run_proposal_revision")
            }
            guard let priorFacts = try runProposalFacts(itemID: payload.item_id),
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
            itemsByID[payload.item_id] = superseded

            artifact.id += "-revision-\(command.command_id)"
            artifact.digest = revisedDigest
            let replacementID = payload.item_id + "/revision/" + command.command_id
            var replacement = current
            replacement.as_of_revision = revision
            replacement.entity_version = 1
            replacement.item.id = replacementID
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
                payload.run_proposal_revision == nil, (payload.message ?? "").isEmpty,
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
            record: .init(
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
            ),
            revision: revision
        )
        commandsByID[command.command_id] = NormalizedCommand(
            command, message: recordedMessage)
        resultsByCommandID[command.command_id] = result
        scheduleAutomaticAgentCompletionIfNeeded(for: payload.action)
        return .ok(commandResultTransform?(result) ?? result)
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
        _ command: Components.Schemas.ClientCommand
    ) -> NormalizedCommand {
        let message: String
        do {
            try MockContractValidation.validateActionInput(command)
            message = try CommandResultTrust.recordedMessage(command.payload)
        } catch {
            message = command.payload.message ?? ""
        }
        return NormalizedCommand(command, message: message)
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
        _ revision: Components.Schemas.RunProposalRevisionInput
    ) -> String {
        MockContractValidation.sha256Digest(
            of: "\(revision.intent.rawValue)|\(revision.expected_cost_units)|"
                + "\(revision.scope.component_count)|\(revision.scope.declared_path_count)|"
                + "\(revision.scope.touches_control_plane)")
    }

    private static func specificationReplacementID(
        for snapshot: Components.Schemas.AttentionItemSnapshot,
        commandID: String
    ) -> String {
        let runID: String?
        switch snapshot.item.subject {
        case .run(let run), .proposal_batch(let run): runID = run.run_id
        case .project, .system: runID = nil
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
        _ revision: Components.Schemas.RunProposalRevisionInput,
        _ facts: Components.Schemas.RunProposalFactsSnapshot
    ) -> Bool {
        revision.intent == facts.intent
            && revision.expected_cost_units == facts.expected_cost_units
            && revision.scope.component_count == facts.scope.component_count
            && revision.scope.declared_path_count == facts.scope.declared_path_count
            && revision.scope.touches_control_plane == facts.scope.touches_control_plane
    }

    /// The concluding decision's item side: version bump, terminal status,
    /// and the decision instant stamped in the same mutation as the flip
    /// (signet concludeItem, #171); a retry replays the recorded result and
    /// never re-stamps.
    private func concluded(
        _ snapshot: Components.Schemas.AttentionItemSnapshot,
        as status: Components.Schemas.ItemStatus
    ) -> Components.Schemas.AttentionItemSnapshot {
        var applied = snapshot
        applied.entity_version += 1
        applied.as_of_revision = revision
        applied.item.item_version += 1
        applied.item.status = status
        applied.item.decided_at = Self.decidedInstant
        return applied
    }

}
