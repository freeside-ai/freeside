import Foundation

/// Pure, state-free mirror of the daemon's contract validation over the
/// generated wire shapes, extracted from `MockServer` (#205) so the actor
/// changes only for state and transitions. Each function is an input →
/// verdict predicate the actor calls during reconstruction and submission
/// (domain.AttentionItem.Validate, TimingSummary.Validate,
/// AttentionDelivery.Validate, signet's per-type action policy, and the
/// command well-formedness the signet boundary enforces ahead of any
/// lookup). Nothing here reads actor state: `snapshotBreach` takes the
/// approved-recipe set as a parameter so the actor's bridge can pass its
/// own policy set. `MockContractValidationTests` pins each in isolation.
enum MockContractValidation {
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
        if let decided = item.decided_at, decided.timeIntervalSince1970 < -62_000_000_000 {
            return "zero decided_at"
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
        func malformed(_ reason: String) -> MockServer.MalformedCommandError {
            MockServer.MalformedCommandError(commandID: command.command_id, reason: reason)
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

    /// The policy set is passed rather than read from an actor so seed-time
    /// derivation (which runs in init before `approvedRecipes` is stored)
    /// can gate a parent through the same check the serve paths run. The
    /// scanner also rejects non-positive snapshot metadata during
    /// reconstruction, ahead of the item's own validation, and the read
    /// path then re-runs the evidence gate against the current
    /// approved-recipe set — trusted policy state, never the row's word
    /// (EligibleForEvidenceSnapshot; the store trust-boundary re-gate).
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

    /// Re-validates one delivery snapshot before it is served, as the
    /// daemon's read paths run validateSnapshot plus the domain validator
    /// on every row (signet sync.go, store reconstruction): a seed the
    /// daemon would fail closed on fails the mock's read loudly instead
    /// of letting a client test pass against unservable cache state. The
    /// generated variant structs already make status/receipt
    /// correspondence unrepresentable; what stays checkable here is the
    /// snapshot metadata, the identity fields, and receipt ordering.
    static func deliveryBreach(
        _ snapshot: Components.Schemas.AttentionDeliverySnapshot,
        serverRevision: Int64,
        hasParentItem: Bool
    ) -> String? {
        let key = MockServer.DeliveryKey(snapshot.delivery)
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

    /// Go's `time.Time{}` zero instant (serialized "0001-01-01T00:00:00Z"),
    /// the exact value `AttentionDelivery.Validate` rejects as an unset
    /// submitted_at.
    // swift-format-ignore: NeverForceUnwrap
    private static let daemonZeroInstant: Date = {
        var components = DateComponents()
        components.year = 1
        components.month = 1
        components.day = 1
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        return calendar.date(from: components)!
    }()

    /// The generated runtime's RFC 3339 decoder accepts whole seconds, which
    /// is also what the mock's `.iso8601` encoder emits. Derive
    /// timing from those same instants so the duration always agrees with
    /// the timestamps the generated client actually decodes, including when
    /// a fixture supplies finer-grained `Date` values.
    static func wireDate(_ date: Date) -> Date {
        Date(timeIntervalSince1970: date.timeIntervalSince1970.rounded(.down))
    }

    /// Mirrors `time.Time.Sub`: nanosecond spans outside `time.Duration`'s
    /// int64 range saturate instead of trapping. Long but valid RFC 3339
    /// fixture dates therefore remain servable just as they are in Go.
    static func durationNanoseconds(from start: Date, to end: Date) -> Int64 {
        let nanoseconds = (end.timeIntervalSince(start) * 1_000_000_000).rounded()
        if nanoseconds >= Double(Int64.max) { return Int64.max }
        if nanoseconds <= Double(Int64.min) { return Int64.min }
        return Int64(nanoseconds)
    }
}
