import Foundation
import OpenAPIRuntime
import Testing

@testable import FreesideAPI

/// Direct unit tests for the pure contract validators extracted from the
/// mock actor (#205). The transport-level suites reach these only as a
/// coarse HTTP 500/422, so any breach satisfies them; here each predicate
/// is pinned in isolation to the exact breach string it returns (and `nil`
/// for a valid input), so a regression that trips the wrong invariant, or
/// silently accepts a bad one, is caught at its source.
@Suite struct MockContractValidationTests {
    // MARK: - itemValidityBreach

    @Test func validItemHasNoBreach() {
        let item = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(MockContractValidation.itemValidityBreach(item) == nil)
    }

    @Test func itemValidityBreachNamesTheFailedInvariant() {
        var empty = AttentionFixtures.fixture(type: .spec_approval).item
        empty.id = ""
        #expect(MockContractValidation.itemValidityBreach(empty) == "empty id")

        var noProject = AttentionFixtures.fixture(type: .spec_approval).item
        noProject.project_id = ""
        #expect(MockContractValidation.itemValidityBreach(noProject) == "empty project_id")

        var zeroVersion = AttentionFixtures.fixture(type: .spec_approval).item
        zeroVersion.item_version = 0
        #expect(MockContractValidation.itemValidityBreach(zeroVersion) == "non-positive item_version")

        // A head-bound evidence artifact must name the same head as the
        // item; only the item's head moves here, so the binding diverges.
        var headMismatch = AttentionFixtures.fixture(type: .spec_approval).item
        headMismatch.pr_head_sha = "deadbeef"
        #expect(
            MockContractValidation.itemValidityBreach(headMismatch)
                == "head-bound evidence names a different head than the item")

        // A second evidence entry reusing the first's id trips the
        // duplicate check inside the evidence loop.
        var duplicateEvidence = AttentionFixtures.fixture(type: .spec_approval).item
        duplicateEvidence.evidence_snapshot.append(duplicateEvidence.evidence_snapshot[0])
        #expect(
            MockContractValidation.itemValidityBreach(duplicateEvidence)
                == "duplicate evidence artifact id")

        // artifact_digests must be the sorted, deduplicated union of every
        // rendered digest; dropping one breaks the canonical-union invariant.
        var wrongUnion = AttentionFixtures.fixture(type: .spec_approval).item
        wrongUnion.artifact_digests = []
        #expect(
            MockContractValidation.itemValidityBreach(wrongUnion)
                == "artifact_digests is not the canonical union of rendered digests")
    }

    // MARK: - itemPolicyBreach

    @Test func validPolicyHasNoBreach() {
        let item = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(MockContractValidation.itemPolicyBreach(item) == nil)
    }

    @Test func blockedOffersNothingAndAnyActionIsRejected() {
        let blocked = AttentionFixtures.fixture(type: .blocked).item
        #expect(blocked.requested_decision.isEmpty)
        #expect(MockContractValidation.itemPolicyBreach(blocked) == nil)

        var blockedWithAction = blocked
        blockedWithAction.requested_decision = [.stop]
        #expect(
            MockContractValidation.itemPolicyBreach(blockedWithAction)
                == "action stop is not allowed for blocked")
    }

    @Test func nonBlockedMustOfferAnAllowedAction() {
        var empty = AttentionFixtures.fixture(type: .spec_approval).item
        empty.requested_decision = []
        #expect(MockContractValidation.itemPolicyBreach(empty) == "no offered actions")

        // `retry` is an execution_failure action, outside spec_approval's set.
        var stray = AttentionFixtures.fixture(type: .spec_approval).item
        stray.requested_decision = [.retry]
        #expect(
            MockContractValidation.itemPolicyBreach(stray)
                == "action retry is not allowed for spec_approval")
    }

    // MARK: - validate (command well-formedness)

    @Test func validCommandDoesNotThrow() throws {
        let snapshot = AttentionFixtures.fixture(type: .spec_approval)
        try MockContractValidation.validate(command(against: snapshot))
    }

    @Test func malformedCommandThrowsWithTheReason() {
        let snapshot = AttentionFixtures.fixture(type: .spec_approval)

        expectMalformed(reason: "empty command_id") {
            var c = command(against: snapshot)
            c.command_id = ""
            return c
        }
        expectMalformed(reason: "empty device_id") {
            var c = command(against: snapshot)
            c.device_id = ""
            return c
        }
        expectMalformed(reason: "empty item_id") {
            var c = command(against: snapshot)
            c.payload.item_id = ""
            return c
        }
        expectMalformed(reason: "non-positive item_version") {
            var c = command(against: snapshot)
            c.payload.item_version = 0
            return c
        }
        expectMalformed(reason: "non-positive expected_entity_version") {
            var c = command(against: snapshot)
            c.expected_entity_version = 0
            return c
        }
        expectMalformed(reason: "empty artifact digest") {
            var c = command(against: snapshot)
            c.payload.artifact_digests = [""]
            return c
        }
        expectMalformed(reason: "empty attachment digest") {
            var c = command(against: snapshot)
            c.payload.attachments = [""]
            return c
        }
        expectMalformed(reason: "duplicate attachment digest") {
            var c = command(against: snapshot)
            c.payload.attachments = ["sha256:a", "sha256:a"]
            return c
        }
    }

    // MARK: - snapshotBreach (metadata + evidence policy re-gate)

    @Test func validSnapshotUnderApprovedRecipeHasNoBreach() {
        let snapshot = AttentionFixtures.fixture(type: .spec_approval)
        #expect(
            MockContractValidation.snapshotBreach(
                snapshot, approvedRecipes: [AttentionFixtures.approvedRecipeDigest]) == nil)
    }

    @Test func snapshotBreachReGatesMetadataAndPolicy() {
        let approved: Set<String> = [AttentionFixtures.approvedRecipeDigest]

        var zeroEntity = AttentionFixtures.fixture(type: .spec_approval)
        zeroEntity.entity_version = 0
        #expect(
            MockContractValidation.snapshotBreach(zeroEntity, approvedRecipes: approved)
                == "non-positive entity_version")

        var zeroRevision = AttentionFixtures.fixture(type: .spec_approval)
        zeroRevision.as_of_revision = 0
        #expect(
            MockContractValidation.snapshotBreach(zeroRevision, approvedRecipes: approved)
                == "non-positive as_of_revision")

        // The evidence gate re-runs against the trusted approved set, never
        // the row's word: an empty set approves nothing.
        let unapproved = AttentionFixtures.fixture(type: .spec_approval)
        #expect(
            MockContractValidation.snapshotBreach(unapproved, approvedRecipes: [])
                == "evidence artifact art-log-spec_approval recipe is not approved")

        // publish_eligible is policy-computed; under an approved recipe a
        // stale false is corrupt reconstructed data.
        var staleBit = AttentionFixtures.fixture(type: .spec_approval)
        staleBit.item.evidence_snapshot[0].publish_eligible = false
        #expect(
            MockContractValidation.snapshotBreach(staleBit, approvedRecipes: approved)
                == "evidence artifact art-log-spec_approval carries a stale publish_eligible bit")
    }

    // MARK: - timingBreach

    @Test func zeroDeliveryTimingHasNoBreach() {
        let timing = Components.Schemas.TimingSummary(
            delivery_count: 0,
            first_submitted_at: nil,
            first_accepted_at: nil,
            first_opened_at: nil,
            submit_to_first_open: nil
        )
        #expect(MockContractValidation.timingBreach(timing) == nil)
    }

    @Test func fullyDerivedTimingWithAgreeingSpanHasNoBreach() {
        let submitted = Date(timeIntervalSince1970: 1_752_000_000)
        let opened = submitted.addingTimeInterval(60)
        let timing = Components.Schemas.TimingSummary(
            delivery_count: 1,
            first_submitted_at: submitted,
            first_accepted_at: nil,
            first_opened_at: opened,
            submit_to_first_open: 60 * 1_000_000_000
        )
        #expect(MockContractValidation.timingBreach(timing) == nil)
    }

    @Test func timingBreachNamesTheFailedInvariant() {
        let submitted = Date(timeIntervalSince1970: 1_752_000_000)
        let opened = submitted.addingTimeInterval(60)

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: -1, first_submitted_at: nil, first_accepted_at: nil,
                    first_opened_at: nil, submit_to_first_open: nil)) == "negative delivery_count")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 0, first_submitted_at: submitted, first_accepted_at: nil,
                    first_opened_at: nil, submit_to_first_open: nil))
                == "timing without deliveries carries endpoints")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 1, first_submitted_at: nil, first_accepted_at: nil,
                    first_opened_at: nil, submit_to_first_open: nil))
                == "deliveries without first_submitted_at")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 1, first_submitted_at: submitted,
                    first_accepted_at: submitted.addingTimeInterval(-1), first_opened_at: nil,
                    submit_to_first_open: nil)) == "first_accepted_at before first_submitted_at")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 1, first_submitted_at: submitted, first_accepted_at: nil,
                    first_opened_at: opened, submit_to_first_open: nil))
                == "submit_to_first_open missing")

        #expect(
            MockContractValidation.timingBreach(
                .init(
                    delivery_count: 1, first_submitted_at: submitted, first_accepted_at: nil,
                    first_opened_at: opened, submit_to_first_open: 1))
                == "submit_to_first_open disagrees with its endpoints")
    }

    // MARK: - deliveryBreach

    @Test func validDeliveryHasNoBreach() {
        let delivery = submittedDelivery()
        #expect(
            MockContractValidation.deliveryBreach(
                delivery, serverRevision: 1, hasParentItem: true) == nil)
    }

    @Test func deliveryBreachNamesTheFailedInvariant() {
        var zeroEntity = submittedDelivery()
        zeroEntity.entity_version = 0
        #expect(
            MockContractValidation.deliveryBreach(
                zeroEntity, serverRevision: 1, hasParentItem: true) == "non-positive entity_version")

        // The row's as_of_revision may not run ahead of the server.
        #expect(
            MockContractValidation.deliveryBreach(
                submittedDelivery(), serverRevision: 0, hasParentItem: true)
                == "as_of_revision outside the server revision")

        #expect(
            MockContractValidation.deliveryBreach(
                submittedDelivery(attempt: 0), serverRevision: 1, hasParentItem: true)
                == "non-positive attempt")

        // A delivery row exists only for an existing item; an orphan is
        // unrepresentable daemon state.
        #expect(
            MockContractValidation.deliveryBreach(
                submittedDelivery(), serverRevision: 1, hasParentItem: false) == "no parent item")

        // submitted_at is required and never the type's zero instant.
        #expect(
            MockContractValidation.deliveryBreach(
                submittedDelivery(submittedAt: daemonZeroInstant), serverRevision: 1,
                hasParentItem: true) == "submitted_at is unset")
    }

    // MARK: - Helpers

    /// A well-formed client command bound to `snapshot`, matching the
    /// transport suite's builder shape.
    private func command(
        against snapshot: Components.Schemas.AttentionItemSnapshot
    ) -> Components.Schemas.ClientCommand {
        .init(
            command_id: "cmd-1",
            device_id: "device-mock",
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .init(
                item_id: snapshot.item.id,
                action: snapshot.item.requested_decision[0],
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests
            )
        )
    }

    private func expectMalformed(
        reason: String,
        _ build: () -> Components.Schemas.ClientCommand,
        sourceLocation: SourceLocation = #_sourceLocation
    ) {
        #expect(sourceLocation: sourceLocation) {
            try MockContractValidation.validate(build())
        } throws: { error in
            guard let malformed = error as? MockServer.MalformedCommandError else { return false }
            return malformed.reason == reason
        }
    }

    private func submittedDelivery(
        attempt: Int = 1,
        submittedAt: Date = Date(timeIntervalSince1970: 1_752_000_000)
    ) -> Components.Schemas.AttentionDeliverySnapshot {
        .init(
            as_of_revision: 1,
            entity_version: 1,
            delivery: .submitted(
                .init(
                    item_id: "item-spec_approval",
                    device_id: "device-1",
                    channel: "ntfy",
                    attempt: attempt,
                    submitted_at: submittedAt,
                    delivery_status: .submitted
                ))
        )
    }

    /// Go's `time.Time{}` zero instant ("0001-01-01T00:00:00Z"), which
    /// `AttentionDelivery.Validate` rejects as an unset submitted_at.
    // swift-format-ignore: NeverForceUnwrap
    private var daemonZeroInstant: Date {
        var components = DateComponents()
        components.year = 1
        components.month = 1
        components.day = 1
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(identifier: "UTC")!
        return calendar.date(from: components)!
    }
}
