import Foundation
import HTTPTypes
import OpenAPIRuntime

/// An in-process mock of the daemon API that implements the contract's
/// command semantics over an in-memory item table, rather than replaying
/// canned bodies: submission is idempotent by command_id, and a stale
/// submission is rejected with the current snapshot as the replacement
/// (plan §5.14 sync tests 2 and 4). #72 extends this same server for the
/// sync surface.
public actor MockServer {
    /// Test hook run before every response; suspend it to hold a response
    /// open, throw to fail the request.
    public typealias BeforeRespond = @Sendable (_ operationID: String) async throws -> Void

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

        init(_ command: Components.Schemas.ClientCommand) {
            commandID = command.command_id
            deviceID = command.device_id
            itemID = command.payload.item_id
            action = command.payload.action
            itemVersion = command.payload.item_version
            prHeadSHA = command.payload.pr_head_sha
            artifactDigests = Array(Set(command.payload.artifact_digests)).sorted()
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

    public init(
        items: [Components.Schemas.AttentionItemSnapshot] = AttentionFixtures.defaultInbox(),
        approvedRecipes: Set<String> = [AttentionFixtures.approvedRecipeDigest]
    ) {
        for snapshot in items {
            itemsByID[snapshot.item.id] = snapshot
        }
        // The server revision starts at or beyond every seeded snapshot's
        // as_of_revision, so the heartbeat and the next CommandResult can
        // never run backwards relative to what this mock lists.
        revision = max(1, items.map(\.as_of_revision).max() ?? 1)
        self.approvedRecipes = approvedRecipes
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
    /// be torn. Deliveries, runs, and conversations stay empty until
    /// their units seed them; the envelope still carries all four
    /// collections, so a client decodes the real shape today.
    func bootstrapSnapshot() throws -> Components.Schemas.BootstrapSnapshot {
        .init(
            sync_epoch: syncEpoch,
            revision: revision,
            attention_items: try listAttentionItems(),
            attention_deliveries: [],
            runs: [],
            conversations: []
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
        if snapshot.entity_version < 1 { return "non-positive entity_version" }
        if snapshot.as_of_revision < 1 { return "non-positive as_of_revision" }
        if let breach = Self.itemValidityBreach(snapshot.item) { return breach }
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
            if let at = endpoint, at.timeIntervalSince1970 < -62_000_000_000 {
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
            let nanos = Int64((opened.timeIntervalSince(submitted) * 1_000_000_000).rounded())
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
                action: payload.action
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
        case "submitCommand":
            guard let body else {
                return (HTTPResponse(status: .badRequest), nil)
            }
            let data = try await Data(collecting: body, upTo: 1 << 20)
            let command = try Self.decoder.decode(
                Components.Schemas.ClientCommand.self, from: data)
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
}
