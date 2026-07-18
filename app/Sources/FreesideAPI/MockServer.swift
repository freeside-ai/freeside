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
    /// Test hook run before every response; suspend it to hold a response
    /// open, throw to fail the request.
    public typealias BeforeRespond = @Sendable (_ operationID: String) async throws -> Void

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

        init(_ command: Components.Schemas.ClientCommand) {
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
            message = command.payload.message ?? ""
            attachments = command.payload.attachments ?? []
        }
    }

    private var itemsByID: [String: Components.Schemas.AttentionItemSnapshot] = [:]
    private var commandsByID: [String: NormalizedCommand] = [:]
    private var resultsByCommandID: [String: Components.Schemas.CommandResult] = [:]
    private var revision: Int64 = 1
    private var syncEpoch = "mock-epoch"
    private var epochGeneration = 1
    private var beforeRespond: BeforeRespond?
    private var afterRespond: BeforeRespond?
    /// The trusted approved-recipe set the evidence gate re-runs
    /// against; policy state owned by the server, never by the rows.
    private let approvedRecipes: Set<String>
    private let authMode: AuthMode
    private var pairingCodes: [String: PairingCodeState] = [:]
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
        deliveries: [Components.Schemas.AttentionDeliverySnapshot] = [],
        approvedRecipes: Set<String> = [AttentionFixtures.approvedRecipeDigest],
        authMode: AuthMode = .permissive,
        pairingCodes: [String: PairingCodeState] = [:],
        pairingNtfyServerURL: String = "https://ntfy.example",
        pairingNtfyTopic: String? = nil,
        pairingDeviceToken: String? = nil,
        attachments: [String: Data] = AttentionFixtures.defaultAttachments()
    ) {
        for snapshot in items {
            itemsByID[snapshot.item.id] = snapshot
        }
        for snapshot in deliveries {
            deliveriesByKey[DeliveryKey(snapshot.delivery)] = snapshot
        }
        // The server revision starts at or beyond every seeded snapshot's
        // as_of_revision, so the heartbeat and the next CommandResult can
        // never run backwards relative to what this mock lists.
        revision = max(1, items.map(\.as_of_revision).max() ?? 1,
            deliveries.map(\.as_of_revision).max() ?? 1)
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
            if Self.snapshotBreach(snapshot, approvedRecipes: approvedRecipes) != nil { continue }
            let rows = deliveriesByKey
                .filter { $0.key.itemID == itemID }
                .sorted { $0.key < $1.key }
                .map(\.value)
                .filter {
                    Self.deliveryBreach(
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
        self.pairingNtfyServerURL = pairingNtfyServerURL
        self.pairingNtfyTopic = pairingNtfyTopic
        self.pairingDeviceToken = pairingDeviceToken
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

    /// The server's current canonical snapshot, for test assertions.
    public func snapshot(itemID: String) -> Components.Schemas.AttentionItemSnapshot? {
        itemsByID[itemID]
    }

    /// Simulates a daemon restore (plan §5.14 sync test 8): a new sync
    /// epoch, optionally rewinding the revision to the restored state's.
    /// Rows are left alone; to a client the epoch change alone is what
    /// makes its cached cursors meaningless, whatever the new revision.
    public func rotateEpoch(revision restored: Int64? = nil) {
        epochGeneration += 1
        syncEpoch = "mock-epoch-\(epochGeneration)"
        if let restored {
            revision = max(1, restored)
        }
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
            ntfy_subscription: subscription
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
            original == NormalizedCommand(command),
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

    /// Field-for-field mirror of domain.AttentionItem.Validate over the
    /// generated shapes. Checks the schema already makes unrepresentable
    /// are omitted: invalid enum members, an agent producer class in
    /// evidence, mixed provenance branches, a run_id on an unscoped
    /// subject, and caller-set publish_eligible. Recipe approval is
    /// runtime policy: snapshotBreach re-runs that gate against the
    /// server's approved set, since Validate holds no policy.
    static func itemValidityBreach(
        _ item: Components.Schemas.AttentionItem
    ) -> String? {
        if item.id.isEmpty { return "empty id" }
        if item.project_id.isEmpty { return "empty project_id" }
        switch item.subject {
        case .run(let scoped), .proposal_batch(let scoped):
            if scoped.subject_id.isEmpty { return "empty subject_id" }
            if let runID = scoped.run_id, runID.isEmpty { return "empty run_id" }
        case .project(let unscoped), .system(let unscoped):
            if unscoped.subject_id.isEmpty { return "empty subject_id" }
        }
        if let conversation = item.conversation_id, conversation.isEmpty {
            return "empty conversation_id"
        }
        if let expires = item.expires_when, expires.timeIntervalSince1970 < -62_000_000_000 {
            return "zero expires_when"
        }
        if item.item_version < 1 { return "non-positive item_version" }
        // An empty requested_decision is structurally valid (#96): which
        // types must offer an action is signet policy (itemPolicyBreach).
        if let breach = timingBreach(item.timing) { return breach }
        var evidenceIDs = Set<String>()
        for artifact in item.evidence_snapshot {
            if artifact.id.isEmpty { return "empty evidence artifact id" }
            if artifact._type.isEmpty { return "empty evidence artifact type" }
            if artifact.digest.isEmpty { return "empty evidence digest" }
            switch artifact.provenance {
            case .head_bound(let bound):
                if bound.producer_invocation_id.isEmpty {
                    return "empty producer_invocation_id"
                }
                if bound.source_head_sha.isEmpty { return "empty source_head_sha" }
                if bound.verification_recipe_digest.isEmpty { return "empty recipe digest" }
                // Head-bound evidence must match the head the item names;
                // head-independent evidence is exempt by design (§5.15).
                if !item.pr_head_sha.isEmpty, bound.source_head_sha != item.pr_head_sha {
                    return "head-bound evidence names a different head than the item"
                }
            case .head_independent(let free):
                if free.producer_invocation_id.isEmpty {
                    return "empty producer_invocation_id"
                }
                if free.verification_recipe_digest.isEmpty { return "empty recipe digest" }
            }
            if !evidenceIDs.insert(artifact.id).inserted {
                return "duplicate evidence artifact id"
            }
        }
        // An artifact id is a content address: it maps to one digest and
        // never spans the two trust channels.
        var claimDigests: [String: String] = [:]
        for claim in item.agent_claims {
            if claim.label.isEmpty { return "empty claim label" }
            if claim.artifact_id.isEmpty { return "empty claim artifact_id" }
            if claim.digest.isEmpty { return "empty claim digest" }
            // Claim provenance is agent-pinned by the schema's producer enum,
            // but the generated recipe-digest container accepts any JSON
            // value, so the representable invariants to check are the
            // non-empty fields domain.Provenance.Validate requires plus the
            // agent-never-recipe-bound rule (agent + non-null digest is
            // ErrProvenanceInconsistent on the daemon). Claims are not
            // head-matched against the item; only evidence is (§5.15).
            switch claim.provenance {
            case .head_bound(let bound):
                if bound.producer_invocation_id.isEmpty {
                    return "empty producer_invocation_id"
                }
                if bound.source_head_sha.isEmpty { return "empty source_head_sha" }
                if bound.verification_recipe_digest?.value != nil {
                    return "claim recipe digest must be null"
                }
            case .head_independent(let free):
                if free.producer_invocation_id.isEmpty {
                    return "empty producer_invocation_id"
                }
                if free.verification_recipe_digest?.value != nil {
                    return "claim recipe digest must be null"
                }
            }
            if evidenceIDs.contains(claim.artifact_id) {
                return "claim reuses an evidence artifact id"
            }
            if let existing = claimDigests[claim.artifact_id], existing != claim.digest {
                return "one claim artifact id maps to two digests"
            }
            claimDigests[claim.artifact_id] = claim.digest
        }
        let union = Array(
            Set(item.evidence_snapshot.map(\.digest) + item.agent_claims.map(\.digest))
        ).sorted()
        if item.artifact_digests != union {
            return "artifact_digests is not the canonical union of rendered digests"
        }
        return nil
    }

    /// Mirrors signet's validateRequestedActions over the authoritative
    /// per-type table (phase1ActionSets matches the merged policy):
    /// blocked is read-only and must offer the empty set (#96); every
    /// other type must offer at least one action from its allowed set.
    static func itemPolicyBreach(
        _ item: Components.Schemas.AttentionItem
    ) -> String? {
        guard let allowed = AttentionFixtures.phase1ActionSets[item._type] else {
            return "unknown attention type \(item._type.rawValue)"
        }
        if item.requested_decision.isEmpty {
            if item._type == .blocked { return nil }
            return "no offered actions"
        }
        // blocked's allowed set is empty, so any offered action on a
        // blocked item fails here, exactly as signet rejects it.
        if let stray = item.requested_decision.first(where: { !allowed.contains($0) }) {
            return "action \(stray.rawValue) is not allowed for \(item._type.rawValue)"
        }
        return nil
    }

    static func validate(_ command: Components.Schemas.ClientCommand) throws {
        func malformed(_ reason: String) -> MalformedCommandError {
            MalformedCommandError(commandID: command.command_id, reason: reason)
        }
        guard !command.command_id.isEmpty else { throw malformed("empty command_id") }
        guard !command.device_id.isEmpty else { throw malformed("empty device_id") }
        guard !command.payload.item_id.isEmpty else { throw malformed("empty item_id") }
        guard command.payload.item_version >= 1 else {
            throw malformed("non-positive item_version")
        }
        guard command.expected_entity_version >= 1 else {
            throw malformed("non-positive expected_entity_version")
        }
        guard !command.payload.artifact_digests.contains("") else {
            throw malformed("empty artifact digest")
        }
        // Attachments mirror domain.NewCommand: entries are content
        // addresses (empty is malformed) and a repeat is rejected rather
        // than deduplicated, since order is authored content the daemon
        // never canonicalizes.
        if let attachments = command.payload.attachments {
            guard !attachments.contains("") else {
                throw malformed("empty attachment digest")
            }
            guard Set(attachments).count == attachments.count else {
                throw malformed("duplicate attachment digest")
            }
        }
    }

    func runBeforeRespond(_ operationID: String) async throws {
        try await beforeRespond?(operationID)
    }

    func runAfterRespond(_ operationID: String) async throws {
        try await afterRespond?(operationID)
    }

    func serverRevision() -> Components.Schemas.ServerRevision {
        .init(sync_epoch: syncEpoch, revision: revision)
    }

    /// One canonical snapshot of every synchronized resource from a
    /// single actor-isolated read, as the daemon's bootstrap is one
    /// Store.Read (plan §5.14): the cursor pair and the rows can never
    /// be torn. Runs and conversations stay empty until their units seed
    /// them; the envelope still carries all four collections, so a
    /// client decodes the real shape today.
    func bootstrapSnapshot() throws -> Components.Schemas.BootstrapSnapshot {
        .init(
            sync_epoch: syncEpoch,
            revision: revision,
            attention_items: try listAttentionItems(),
            attention_deliveries: try listAttentionDeliveries(),
            runs: [],
            conversations: []
        )
    }

    struct InvalidDeliveryError: Error {
        let itemID: String
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
        if let breach = Self.deliveryBreach(
            snapshot, serverRevision: revision, hasParentItem: itemsByID[key.itemID] != nil)
        {
            throw InvalidDeliveryError(itemID: key.itemID, reason: breach)
        }
        return snapshot
    }

    /// Go's `time.Time{}` zero instant (serialized "0001-01-01T00:00:00Z"),
    /// the exact value `AttentionDelivery.Validate` rejects as an unset
    /// submitted_at.
    private static let daemonZeroInstant: Date = {
        var components = DateComponents()
        components.year = 1
        components.month = 1
        components.day = 1
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        return calendar.date(from: components)!
    }()

    private static func deliveryBreach(
        _ snapshot: Components.Schemas.AttentionDeliverySnapshot,
        serverRevision: Int64,
        hasParentItem: Bool
    ) -> String? {
        let key = DeliveryKey(snapshot.delivery)
        if snapshot.entity_version < 1 { return "non-positive entity_version" }
        if snapshot.as_of_revision < 1 || snapshot.as_of_revision > serverRevision {
            return "as_of_revision outside the server revision"
        }
        if key.itemID.isEmpty || key.deviceID.isEmpty || key.channel.isEmpty {
            return "empty identity field"
        }
        if key.attempt < 1 { return "non-positive attempt" }
        // submitted_at is required and never the type's zero value:
        // AttentionDelivery.Validate rejects SubmittedAt.IsZero(), so a
        // seed at or before the daemon zero instant is unproducible state.
        let submittedAt: Date
        switch snapshot.delivery {
        case .submitted(let row): submittedAt = row.submitted_at
        case .channel_accepted(let row): submittedAt = row.submitted_at
        case .opened(let row): submittedAt = row.submitted_at
        }
        if submittedAt == daemonZeroInstant { return "submitted_at is unset" }
        // A delivery row exists only because the pipeline recorded it for
        // an existing item; an orphan row is unrepresentable daemon state.
        if !hasParentItem { return "no parent item" }
        switch snapshot.delivery {
        case .submitted:
            break
        case .channel_accepted(let row):
            if row.channel_accepted_at < row.submitted_at {
                return "channel_accepted_at precedes submitted_at"
            }
        case .opened(let row):
            if row.opened_at < row.submitted_at {
                return "opened_at precedes submitted_at"
            }
            if let accepted = row.channel_accepted_at,
                accepted < row.submitted_at || row.opened_at < accepted
            {
                return "receipt ordering violated"
            }
        }
        return nil
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
                submitted.append(wireDate(row.submitted_at))
            case .channel_accepted(let row):
                submitted.append(wireDate(row.submitted_at))
                accepted.append(wireDate(row.channel_accepted_at))
            case .opened(let row):
                submitted.append(wireDate(row.submitted_at))
                if let acceptedAt = row.channel_accepted_at {
                    accepted.append(wireDate(acceptedAt))
                }
                opened.append(wireDate(row.opened_at))
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
                firstOpened.map { durationNanoseconds(from: start, to: $0) }
            }
        )
    }

    /// The generated runtime's RFC 3339 decoder accepts whole seconds, which
    /// is also what the mock's `.iso8601` encoder emits. Derive
    /// timing from those same instants so the duration always agrees with
    /// the timestamps the generated client actually decodes, including when
    /// a fixture supplies finer-grained `Date` values.
    private static func wireDate(_ date: Date) -> Date {
        Date(timeIntervalSince1970: date.timeIntervalSince1970.rounded(.down))
    }

    /// Mirrors `time.Time.Sub`: nanosecond spans outside `time.Duration`'s
    /// int64 range saturate instead of trapping. Long but valid RFC 3339
    /// fixture dates therefore remain servable just as they are in Go.
    private static func durationNanoseconds(from start: Date, to end: Date) -> Int64 {
        let nanoseconds = (end.timeIntervalSince(start) * 1_000_000_000).rounded()
        if nanoseconds >= Double(Int64.max) { return Int64.max }
        if nanoseconds <= Double(Int64.min) { return Int64.min }
        return Int64(nanoseconds)
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
        // The daemon's list query orders by item id (store list.go), not
        // by insertion, so seeded scenarios see the same stable order a
        // real inbox would.
        let snapshots = itemsByID.keys.sorted().compactMap { itemsByID[$0] }
        for snapshot in snapshots {
            if let breach = snapshotBreach(snapshot) {
                throw InvalidItemError(itemID: snapshot.item.id, reason: breach)
            }
        }
        return snapshots
    }

    /// nil means truly absent (404); an invalid row is a thrown
    /// reconstruction failure, never a not-found.
    func servedSnapshot(
        itemID: String
    ) throws -> Components.Schemas.AttentionItemSnapshot? {
        guard let snapshot = itemsByID[itemID] else { return nil }
        if let breach = snapshotBreach(snapshot) {
            throw InvalidItemError(itemID: itemID, reason: breach)
        }
        return snapshot
    }

    /// The scanner also rejects non-positive snapshot metadata during
    /// reconstruction, ahead of the item's own validation, and the read
    /// path then re-runs the evidence gate against the current
    /// approved-recipe set — trusted policy state, never the row's word
    /// (EligibleForEvidenceSnapshot; the store trust-boundary re-gate).
    func snapshotBreach(
        _ snapshot: Components.Schemas.AttentionItemSnapshot
    ) -> String? {
        Self.snapshotBreach(snapshot, approvedRecipes: approvedRecipes)
    }

    /// The policy set is passed rather than read from `self` so seed-time
    /// derivation (which runs in init before `approvedRecipes` is stored)
    /// can gate a parent through the same check the serve paths run.
    static func snapshotBreach(
        _ snapshot: Components.Schemas.AttentionItemSnapshot,
        approvedRecipes: Set<String>
    ) -> String? {
        if snapshot.entity_version < 1 { return "non-positive entity_version" }
        if snapshot.as_of_revision < 1 { return "non-positive as_of_revision" }
        if let breach = itemValidityBreach(snapshot.item) { return breach }
        for artifact in snapshot.item.evidence_snapshot {
            let recipe: String
            switch artifact.provenance {
            case .head_bound(let bound): recipe = bound.verification_recipe_digest
            case .head_independent(let free): recipe = free.verification_recipe_digest
            }
            if !approvedRecipes.contains(recipe) {
                return "evidence artifact \(artifact.id) recipe is not approved"
            }
            // The trusted bit is policy-computed, never the row's word:
            // under an approved recipe the computation yields true, so a
            // stale false is corrupt reconstructed data
            // (EligibleForEvidenceSnapshot re-verifies it).
            if !artifact.publish_eligible {
                return "evidence artifact \(artifact.id) carries a stale publish_eligible bit"
            }
        }
        return nil
    }

    /// Field-for-field mirror of domain.TimingSummary.Validate: count
    /// and endpoints must agree, receipts imply submission, receipt
    /// minima fall on or after it (they carry no order between each
    /// other), and the submit-to-open span exists exactly when both of
    /// its endpoints do and equals their difference.
    static func timingBreach(_ timing: Components.Schemas.TimingSummary) -> String? {
        if timing.delivery_count < 0 { return "negative delivery_count" }
        for (name, endpoint) in [
            ("first_submitted_at", timing.first_submitted_at),
            ("first_accepted_at", timing.first_accepted_at),
            ("first_opened_at", timing.first_opened_at),
        ] {
            if let at = endpoint, at == daemonZeroInstant {
                return "zero \(name)"
            }
        }
        let hasReceipt = timing.first_accepted_at != nil || timing.first_opened_at != nil
        if timing.delivery_count == 0,
            timing.first_submitted_at != nil || hasReceipt
        {
            return "timing without deliveries carries endpoints"
        }
        if timing.delivery_count > 0, timing.first_submitted_at == nil {
            return "deliveries without first_submitted_at"
        }
        if hasReceipt, timing.first_submitted_at == nil {
            return "receipt without submission"
        }
        if let submitted = timing.first_submitted_at {
            if let accepted = timing.first_accepted_at, accepted < submitted {
                return "first_accepted_at before first_submitted_at"
            }
            if let opened = timing.first_opened_at, opened < submitted {
                return "first_opened_at before first_submitted_at"
            }
        }
        let bothEndpoints = timing.first_submitted_at != nil && timing.first_opened_at != nil
        if timing.submit_to_first_open == nil, bothEndpoints {
            return "submit_to_first_open missing"
        }
        if let span = timing.submit_to_first_open {
            guard bothEndpoints,
                let submitted = timing.first_submitted_at,
                let opened = timing.first_opened_at
            else { return "submit_to_first_open without both endpoints" }
            let nanos = durationNanoseconds(
                from: wireDate(submitted), to: wireDate(opened))
            if nanos != span { return "submit_to_first_open disagrees with its endpoints" }
        }
        return nil
    }

    func submitCommand(_ command: Components.Schemas.ClientCommand) throws -> SubmitOutcome {
        // Well-formedness precedes every lookup, as the signet boundary
        // orders it (domain.NewCommand, then expected_entity_version):
        // ids identify, versions are positive, digests content-address.
        // Action validity is the generated enum's decode, upstream.
        try Self.validate(command)
        if let original = commandsByID[command.command_id] {
            // Replay is determined first, as the daemon orders it: a
            // reused id converges only on an identical normalized body,
            // and a different one is an immutable conflict even when its
            // new action would be rejected on other grounds.
            guard original == NormalizedCommand(command),
                let recorded = resultsByCommandID[command.command_id]
            else {
                throw ImmutableConflictError(commandID: command.command_id)
            }
            return .ok(recorded)
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
        if let breach = Self.itemPolicyBreach(current.item) {
            throw ItemPolicyError(itemID: payload.item_id, reason: breach)
        }
        // The pending gate runs only for a genuinely new command against
        // a policy-valid item (ErrUnsupportedAction).
        if case .pending = ActionOutcome.of(payload.action) {
            throw UnsupportedActionError(
                commandID: command.command_id, action: payload.action)
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
                    replacement_item: current
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
                    replacement_item: current
                ))
        }
        // The command binds the live item; the action must also be one it
        // offered. Checked after staleness, as the daemon orders it: a
        // stale client re-decides against the replacement's offered set.
        guard current.item.requested_decision.contains(payload.action) else {
            throw ActionNotOfferedError(
                commandID: command.command_id, action: payload.action, itemID: payload.item_id)
        }
        revision += 1
        switch ActionOutcome.of(payload.action) {
        case .concludes(let status):
            var applied = current
            applied.entity_version += 1
            applied.as_of_revision = revision
            applied.item.item_version += 1
            applied.item.status = status
            itemsByID[payload.item_id] = applied
        case .records:
            // The command record is the whole server-side effect; the
            // item row is left untouched (signet outcomeRecords).
            break
        case .pending:
            // Unreachable: the pending gate above already rejected it.
            throw UnsupportedActionError(
                commandID: command.command_id, action: payload.action)
        }
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
                message: payload.message ?? "",
                attachments: payload.attachments ?? []
            ),
            revision: revision
        )
        commandsByID[command.command_id] = NormalizedCommand(command)
        resultsByCommandID[command.command_id] = result
        return .ok(result)
    }

}

/// Routes generated-client requests to a MockServer over real JSON, so
/// every call exercises the full generated encode/decode pipeline.
public struct MockServerTransport: ClientTransport {
    public let server: MockServer

    public init(server: MockServer) {
        self.server = server
    }

    public func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        try await server.runBeforeRespond(operationID)
        let response = try await route(request, body: body, operationID: operationID)
        try await server.runAfterRespond(operationID)
        return response
    }

    private func route(
        _ request: HTTPRequest,
        body: HTTPBody?,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        // The daemon authorizes before any handler runs and fails closed
        // (#105); pairing is the one unauthenticated operation.
        var authenticatedDevice: String?
        if operationID != "pairDevice" {
            switch await server.authenticate(
                authorization: request.headerFields[.authorization])
            {
            case .anonymous:
                break
            case .device(let id):
                authenticatedDevice = id
            case .revokedDevice(let id):
                // A revoked credential authenticates nothing except test
                // 16's recorded-replay branch on command submission.
                if operationID == "submitCommand", let body {
                    let data = try await Data(collecting: body, upTo: 1 << 20)
                    if let command = try? Self.decoder.decode(
                        Components.Schemas.ClientCommand.self, from: data),
                        let recorded = await server.recordedResultForRevokedRetry(
                            command, deviceID: id)
                    {
                        return try Self.json(status: .ok, body: recorded)
                    }
                }
                return try Self.json(
                    status: .unauthorized,
                    body: Components.Schemas._Error(message: "unauthorized"))
            case .unauthorized:
                return try Self.json(
                    status: .unauthorized,
                    body: Components.Schemas._Error(message: "unauthorized"))
            }
        }
        switch operationID {
        case "getSyncRevision":
            return try Self.json(status: .ok, body: await server.serverRevision())
        case "getSyncBootstrap":
            do {
                return try Self.json(status: .ok, body: await server.bootstrapSnapshot())
            } catch let invalid as MockServer.InvalidItemError {
                // One invalid row fails the whole bootstrap closed, as the
                // daemon's single-read upper-bound gate does (#105).
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "bootstrap reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            } catch let invalid as MockServer.InvalidDeliveryError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "bootstrap reconstruction failed: delivery for item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "listAttentionItems":
            do {
                return try Self.json(status: .ok, body: await server.listAttentionItems())
            } catch let invalid as MockServer.InvalidItemError {
                // The daemon fails the whole read on the first invalid
                // row; a client sees a failed refresh, never a partial
                // inbox.
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message: "list reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "getAttentionItem":
            do {
                guard
                    let itemID = Self.lastPathComponent(request.path),
                    let snapshot = try await server.servedSnapshot(itemID: itemID)
                else {
                    return try Self.json(
                        status: .notFound,
                        body: Components.Schemas._Error(
                            message: "no entity exists under the identifier")
                    )
                }
                return try Self.json(status: .ok, body: snapshot)
            } catch let invalid as MockServer.InvalidItemError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message: "reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "pairDevice":
            guard let body else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            let data = try await Data(collecting: body, upTo: 1 << 20)
            let pairing = try Self.decoder.decode(
                Components.Schemas.PairingRequest.self, from: data)
            do {
                return try Self.json(status: .created, body: try await server.pairDevice(pairing))
            } catch is MockServer.PairingRejectedError {
                return try Self.json(
                    status: .forbidden,
                    body: Components.Schemas._Error(
                        message: "the pairing code is unknown, expired, or already consumed")
                )
            }
        case "revokeDevice":
            guard let deviceID = Self.deviceID(inRevokePath: request.path) else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            switch await server.revokeDevice(id: deviceID) {
            case .revoked(let snapshot):
                return try Self.json(status: .ok, body: snapshot)
            case .unknown:
                return try Self.json(
                    status: .notFound,
                    body: Components.Schemas._Error(
                        message: "no entity exists under the identifier")
                )
            }
        case "submitCommand":
            guard let body else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            let data = try await Data(collecting: body, upTo: 1 << 20)
            let command = try Self.decoder.decode(
                Components.Schemas.ClientCommand.self, from: data)
            // One valid device credential can never name another device
            // in a command body (#105), ahead of the contract semantics.
            if let authenticatedDevice, command.device_id != authenticatedDevice {
                return try Self.json(
                    status: .forbidden,
                    body: Components.Schemas._Error(
                        message: "device_id does not match the authenticated device")
                )
            }
            do {
                switch try await server.submitCommand(command) {
                case .ok(let result):
                    return try Self.json(status: .ok, body: result)
                case .stale(let rejection):
                    return try Self.json(status: .conflict, body: rejection)
                }
            } catch let missing as MockServer.UnknownItemError {
                // Daemon rejections are authoritative HTTP responses, not
                // transport failures: the generated client surfaces the
                // undocumented ones as their status, distinguishable from
                // a lost response.
                return try Self.json(
                    status: .notFound,
                    body: Components.Schemas._Error(
                        message: "no item exists under \(missing.itemID)")
                )
            } catch let rejection as MockServer.ImmutableConflictError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message: "command \(rejection.commandID) reused with a different body")
                )
            } catch let rejection as MockServer.ActionNotOfferedError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message:
                            "action \(rejection.action.rawValue) not offered by item \(rejection.itemID)"
                    )
                )
            } catch let rejection as MockServer.UnsupportedActionError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message:
                            "action \(rejection.action.rawValue) is not acceptable yet; its transaction belongs to a later unit"
                    )
                )
            } catch let rejection as MockServer.MalformedCommandError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message: "malformed command: \(rejection.reason)")
                )
            } catch let rejection as MockServer.ItemPolicyError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message: "item \(rejection.itemID) fails signet policy: \(rejection.reason)"
                    )
                )
            } catch let rejection as MockServer.InvalidItemError {
                return try Self.json(
                    status: .unprocessableContent,
                    body: Components.Schemas._Error(
                        message: "item \(rejection.itemID) fails validation: \(rejection.reason)"
                    )
                )
            }
        case "listAttentionItemDeliveries":
            guard let itemID = Self.itemID(inDeliveriesPath: request.path) else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            do {
                return try Self.json(
                    status: .ok,
                    body: try await server.listAttentionItemDeliveries(itemID: itemID))
            } catch let missing as MockServer.UnknownItemError {
                return try Self.json(
                    status: .notFound,
                    body: Components.Schemas._Error(
                        message: "no entity exists under \(missing.itemID)")
                )
            } catch let invalid as MockServer.InvalidDeliveryError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "list reconstruction failed: delivery for item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            } catch let invalid as MockServer.InvalidItemError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message: "list reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "reportDeliveryOpened":
            guard let identity = Self.deliveryIdentity(inOpenedPath: request.path) else {
                return try Self.json(
                    status: .badRequest,
                    body: Components.Schemas._Error(
                        message: "attempt must be a positive integer")
                )
            }
            do {
                switch try await server.reportDeliveryOpened(
                    itemID: identity.itemID, channel: identity.channel,
                    attempt: identity.attempt, deviceID: authenticatedDevice)
                {
                case .ok(let snapshot):
                    return try Self.json(status: .ok, body: snapshot)
                case .unknown:
                    return try Self.json(
                        status: .notFound,
                        body: Components.Schemas._Error(
                            message: "no entity exists under the identifier")
                    )
                }
            } catch let invalid as MockServer.InvalidDeliveryError {
                // The daemon's wire method re-validates the snapshot it
                // returns and fails closed (writeReadError → 500).
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "delivery reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            } catch let invalid as MockServer.InvalidItemError {
                return try Self.json(
                    status: .internalServerError,
                    body: Components.Schemas._Error(
                        message:
                            "delivery reconstruction failed: item \(invalid.itemID): \(invalid.reason)"
                    )
                )
            }
        case "getAttachment":
            // The digest-addressed read path: stored bytes verbatim, or
            // an authoritative 404 the client renders as a placeholder.
            // Bytes are opaque to the server (plan §5.15: nothing
            // server-side decodes an image; rendering is the client's).
            guard let digest = Self.lastPathComponent(request.path),
                let bytes = await server.attachmentBytes(digest: digest)
            else {
                return try Self.json(
                    status: .notFound,
                    body: Components.Schemas._Error(
                        message: "no attachment exists under the digest")
                )
            }
            let response = HTTPResponse(
                status: .ok,
                headerFields: [.contentType: "application/octet-stream"]
            )
            return (response, HTTPBody(bytes))
        default:
            return (HTTPResponse(status: .notImplemented), nil)
        }
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

    private static func json(
        status: HTTPResponse.Status,
        body: some Encodable
    ) throws -> (HTTPResponse, HTTPBody?) {
        let response = HTTPResponse(
            status: status,
            headerFields: [.contentType: "application/json"]
        )
        return (response, HTTPBody(try encoder.encode(body)))
    }

    private static func lastPathComponent(_ path: String?) -> String? {
        path?.split(separator: "/").last.flatMap { String($0).removingPercentEncoding }
    }

    /// `/devices/{device_id}/revoke`: the id is the segment ahead of the
    /// trailing verb.
    private static func deviceID(inRevokePath path: String?) -> String? {
        let parts = path?.split(separator: "/") ?? []
        guard parts.count >= 2, parts.last == "revoke" else { return nil }
        return String(parts[parts.count - 2]).removingPercentEncoding
    }

    /// `/attention/items/{item_id}/deliveries`: the id is the segment
    /// ahead of the trailing collection name.
    private static func itemID(inDeliveriesPath path: String?) -> String? {
        let parts = (path?.split(separator: "/") ?? [])
            .map { String($0).removingPercentEncoding ?? String($0) }
        guard parts.count == 4, parts[0] == "attention", parts[1] == "items",
            parts[3] == "deliveries"
        else { return nil }
        return parts[2]
    }

    /// `/attention/items/{item_id}/deliveries/{channel}/{attempt}/opened`:
    /// nil when the shape is wrong or the attempt segment is not a
    /// positive integer (the daemon answers 400 before its service runs).
    private static func deliveryIdentity(
        inOpenedPath path: String?
    ) -> (itemID: String, channel: String, attempt: Int)? {
        let parts = (path?.split(separator: "/") ?? [])
            .map { String($0).removingPercentEncoding ?? String($0) }
        guard parts.count == 7, parts[0] == "attention", parts[1] == "items",
            parts[3] == "deliveries", parts[6] == "opened",
            let attempt = Int(parts[5]), attempt >= 1
        else { return nil }
        return (parts[2], parts[4], attempt)
    }
}
