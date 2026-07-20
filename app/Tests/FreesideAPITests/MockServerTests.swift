import Foundation
import FreesideAPI
import OpenAPIRuntime
import Testing

@Suite struct MockServerTests {
    @Test func listReturnsTheSeededInbox() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let snapshots = try await client.listAttentionItems().ok.body.json
        // Ordered by item id, as the daemon's list query is.
        let expected = AttentionFixtures.phase1Types.map { "item-\($0.rawValue)" }.sorted()
        #expect(snapshots.map(\.item.id) == expected)
    }

    @Test func getReturnsCanonicalStateAndUnknownIsNotFound() async throws {
        let client = APIClientFactory.mock(server: MockServer())
        let snapshot =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(snapshot.item._type == .spec_approval)
        let missing = try await client.getAttentionItem(path: .init(item_id: "item-unknown"))
        _ = try missing.notFound
    }

    @Test func attachmentsServeSeededBytesAndUnknownDigestIsNotFound() async throws {
        // The digest-addressed read path (plan §4): stored bytes come
        // back verbatim through the generated binary pipeline, and a
        // digest the store does not hold is an authoritative 404 (the
        // client's placeholder case), never a transport failure.
        let client = APIClientFactory.mock(server: MockServer())

        let image =
            try await client
            .getAttachment(path: .init(digest: "sha256:img-spec_approval")).ok.body.binary
        let imageBytes = try await Data(collecting: image, upTo: 1 << 20)
        #expect(imageBytes == AttentionFixtures.fixtureImagePNG)
        // The fixture is a real PNG: magic bytes, so clients can decode.
        #expect(imageBytes.starts(with: [0x89, 0x50, 0x4E, 0x47]))

        let log =
            try await client
            .getAttachment(path: .init(digest: "sha256:log-spec_approval")).ok.body.binary
        let logBytes = try await Data(collecting: log, upTo: 1 << 20)
        #expect(String(data: logBytes, encoding: .utf8)?.contains("verify log") == true)

        let missing = try await client.getAttachment(path: .init(digest: "sha256:img-blocked"))
        _ = try missing.notFound
    }

    @Test func freshCommandAppliesAndRecordsTheDecision() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json

        let result =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-1", against: before)))
            .ok.body.json
        #expect(result.record.action == .approve)
        #expect(result.record.item_id == "item-spec_approval")

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(after.item.status == .resolved)
        #expect(after.item.item_version == before.item.item_version + 1)
        #expect(after.entity_version == before.entity_version + 1)
        // The concluding decision stamps the decision instant (#171).
        #expect(before.item.decided_at == nil)
        #expect(after.item.decided_at != nil)
    }

    @Test func staleCommandIsRejectedWithTheReplacementAndNoSideEffect() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        await server.advance(itemID: "item-spec_approval")

        let output =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-stale", against: before)))
        let rejection = try output.conflict.body.json
        #expect(rejection.replacement_item.entity_version == before.entity_version + 1)
        #expect(rejection.replacement_item.item.item_version == before.item.item_version + 1)

        // No side effect: the live item is still open at the advanced version.
        let current =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(current.item.status == .open)
        #expect(current == rejection.replacement_item)
    }

    @Test func retryByCommandIDReturnsTheRecordedResultWithoutReapplying() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-agent_question")).ok.body.json
        let command = Self.command(id: "cmd-retry", against: before, action: .stop)

        let first = try await client.submitCommand(body: .json(command)).ok.body.json
        // The retry races a now-stale prepared state (the apply bumped the
        // versions); idempotent replay must still win over staleness.
        let second = try await client.submitCommand(body: .json(command)).ok.body.json
        #expect(first == second)

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-agent_question")).ok.body.json
        #expect(after.item.item_version == before.item.item_version + 1)
        #expect(after.item.decided_at != nil)
    }

    @Test func reusedCommandIDWithADifferentBodyIsRejectedNotReplayed() async throws {
        // The daemon converges only on a byte-identical command under a
        // reused command_id (ErrImmutableConflict); the mock must not
        // hide that misuse behind a successful replay.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let first =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        _ =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-reused", against: first)))
            .ok.body.json

        let other =
            try await client
            .getAttentionItem(path: .init(item_id: "item-agent_question")).ok.body.json
        let output = try await client.submitCommand(
            body: .json(Self.command(id: "cmd-reused", against: other, action: .stop)))
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 422)
    }

    @Test func malformedCommandIsRejectedBeforeReplayOrStaleHandling() async throws {
        // Well-formedness precedes every lookup (domain.NewCommand runs
        // before the replay read): a recorded id resubmitted with a
        // non-positive expected version is malformed, never a replay.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        _ =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-valid", against: before)))
            .ok.body.json

        var invalid = Self.command(id: "cmd-valid", against: before)
        invalid.expected_entity_version = 0
        let replayed = try await client.submitCommand(body: .json(invalid))
        guard case .undocumented(let statusCode, _) = replayed else {
            Issue.record("expected a malformed rejection, got \(replayed)")
            return
        }
        #expect(statusCode == 422)

        // Malformed also outranks staleness: the empty digest entry both
        // malforms the command and mismatches the bindings, and the
        // rejection is 422, not a 409 replacement — with no effect.
        var emptyDigest = Self.command(id: "cmd-empty-digest", against: before)
        emptyDigest.payload.artifact_digests.append("")
        let rejected = try await client.submitCommand(body: .json(emptyDigest))
        guard case .undocumented(let emptyStatus, _) = rejected else {
            Issue.record("expected a malformed rejection, got \(rejected)")
            return
        }
        #expect(emptyStatus == 422)
        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(after.item.item_version == before.item.item_version + 1)
    }

    @Test func pendingActionAgainstAMissingItemIsNotFound() async throws {
        // The item lookup and its policy re-gate precede the pending
        // gate (signet.Submit): a pending action aimed at a missing item
        // reports not-found, never unsupported-action.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        var command = Self.command(id: "cmd-pending-missing", against: before, action: .discuss)
        command.payload.item_id = "item-none"

        let output = try await client.submitCommand(body: .json(command))
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 404)
    }

    @Test func retryWithRefreshedExpectationsStillReplays() async throws {
        // The daemon replays against the normalized persisted body:
        // expected_entity_version and digest order are acceptance-time
        // inputs, not part of the record, so a retry prepared with
        // refreshed expectations still converges on the recorded result.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        let first =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-refresh", against: before)))
            .ok.body.json

        var retry = Self.command(id: "cmd-refresh", against: before)
        retry.expected_entity_version = before.entity_version + 1
        retry.payload.artifact_digests = retry.payload.artifact_digests.reversed()
        let second = try await client.submitCommand(body: .json(retry)).ok.body.json
        #expect(first == second)
        // The record carries the canonical digest set (domain.NewCommand),
        // regardless of the order the payload submitted.
        #expect(second.record.artifact_digests == before.item.artifact_digests)

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(after.item.item_version == before.item.item_version + 1)
    }

    @Test func reusedIDWithAPendingActionIsAnImmutableConflictNotUnsupported() async throws {
        // Replay is determined first, as the daemon orders it: a reused
        // id with a changed body is an immutable conflict even when the
        // new body names a pending action.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        _ =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-order", against: before)))
            .ok.body.json

        let output = try await client.submitCommand(
            body: .json(Self.command(id: "cmd-order", against: before, action: .discuss)))
        guard case .undocumented(let statusCode, let payload) = output, let body = payload.body
        else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 422)
        let data = try await Data(collecting: body, upTo: 1 << 20)
        let message = try JSONDecoder().decode(Components.Schemas._Error.self, from: data).message
        #expect(message.contains("reused"))
    }

    @Test func invalidSeedFailsReadsLoudly() async throws {
        // The daemon's read paths re-validate reconstructed rows and
        // fail the whole read on the first bad one: a client sees a
        // failed refresh or detail load, never a partial inbox or a
        // not-found for a row that exists but is invalid.
        var forged = AttentionFixtures.fixture(type: .spec_approval)
        forged.item.artifact_digests.removeLast()
        let valid = AttentionFixtures.fixture(type: .agent_question)
        let server = MockServer(items: [forged, valid])
        let client = APIClientFactory.mock(server: server)

        let list = try await client.listAttentionItems()
        guard case .undocumented(let listStatus, _) = list else {
            Issue.record("expected a failed list read, got \(list)")
            return
        }
        #expect(listStatus == 500)

        let detail = try await client.getAttentionItem(path: .init(item_id: forged.item.id))
        guard case .undocumented(let getStatus, _) = detail else {
            Issue.record("expected a failed detail read, got \(detail)")
            return
        }
        #expect(getStatus == 500)
    }

    @Test func everyValidateInvariantFailsReadsLoudly() async throws {
        // A sample across domain.AttentionItem.Validate's representable
        // invariants: each malformed seed fails the list read, exactly
        // as the daemon's reconstruction would.
        var emptyClaimID = AttentionFixtures.fixture(type: .system_health)
        emptyClaimID.item.agent_claims[0].artifact_id = ""

        var headMismatch = AttentionFixtures.fixture(type: .spec_approval)
        headMismatch.item.pr_head_sha = "deadbeef"

        var duplicateEvidence = AttentionFixtures.fixture(type: .review_dispute)
        duplicateEvidence.item.evidence_snapshot.append(
            duplicateEvidence.item.evidence_snapshot[0])

        var claimReusesEvidenceID = AttentionFixtures.fixture(type: .agent_question)
        claimReusesEvidenceID.item.agent_claims[0].artifact_id =
            claimReusesEvidenceID.item.evidence_snapshot[0].id

        var negativeTiming = AttentionFixtures.fixture(type: .run_proposal)
        negativeTiming.item.timing.delivery_count = -1

        // The scanner also rejects non-positive snapshot metadata.
        var zeroMeta = AttentionFixtures.fixture(type: .publish_blocked)
        zeroMeta.entity_version = 0

        // TimingSummary.Validate: count and endpoints must agree, and the
        // span exists exactly when both endpoints do.
        var receiptsWithoutDeliveries = AttentionFixtures.fixture(type: .spec_approval)
        receiptsWithoutDeliveries.item.timing.first_submitted_at =
            Date(timeIntervalSince1970: 1_700_000_000)

        var acceptedBeforeSubmitted = AttentionFixtures.fixture(type: .review_dispute)
        acceptedBeforeSubmitted.item.timing.delivery_count = 1
        acceptedBeforeSubmitted.item.timing.first_submitted_at =
            Date(timeIntervalSince1970: 1_700_000_100)
        acceptedBeforeSubmitted.item.timing.first_accepted_at =
            Date(timeIntervalSince1970: 1_700_000_000)

        var endpointsWithoutSpan = AttentionFixtures.fixture(type: .ready_for_final_review)
        endpointsWithoutSpan.item.timing.delivery_count = 1
        endpointsWithoutSpan.item.timing.first_submitted_at =
            Date(timeIntervalSince1970: 1_700_000_000)
        endpointsWithoutSpan.item.timing.first_opened_at =
            Date(timeIntervalSince1970: 1_700_000_300)

        // The evidence gate re-runs against the approved-recipe set.
        var unapprovedRecipe = AttentionFixtures.fixture(type: .execution_failure)
        unapprovedRecipe.item.evidence_snapshot[0].provenance = .head_bound(
            .init(
                producer_class: .verifier,
                producer_invocation_id: "inv-unapproved",
                head_binding: .head_bound,
                source_head_sha: "cafebabe",
                verification_recipe_digest: "sha256:recipe-unapproved",
                sensitivity_class: .normal
            ))

        // publish_eligible is policy-computed; a stale false is rejected.
        var staleEligibleBit = AttentionFixtures.fixture(type: .review_diminishing_returns)
        staleEligibleBit.item.evidence_snapshot[0].publish_eligible = false

        // Claim provenance (#173): the schema pins the agent producer and a
        // null recipe digest, so the representable breach left is an empty
        // required field, checked like domain.Provenance.Validate.
        var claimEmptyInvocation = AttentionFixtures.fixture(type: .blocked)
        claimEmptyInvocation.item.agent_claims[0].provenance = .head_bound(
            .init(
                producer_class: .agent,
                producer_invocation_id: "",
                head_binding: .head_bound,
                source_head_sha: "cafebabe",
                verification_recipe_digest: nil,
                sensitivity_class: .normal
            ))

        // Agent output is never recipe-produced: a non-null recipe digest on
        // a claim is ErrProvenanceInconsistent on the daemon, and the
        // generated container type makes it representable here.
        var claimRecipeBound = AttentionFixtures.fixture(type: .agent_question)
        claimRecipeBound.item.agent_claims[0].provenance = .head_bound(
            .init(
                producer_class: .agent,
                producer_invocation_id: "inv-forged",
                head_binding: .head_bound,
                source_head_sha: "cafebabe",
                verification_recipe_digest: "sha256:recipe-forged",
                sensitivity_class: .normal
            ))

        for (label, seed) in [
            ("empty claim artifact_id", emptyClaimID),
            ("empty claim provenance invocation", claimEmptyInvocation),
            ("recipe-bound claim provenance", claimRecipeBound),
            ("head-bound evidence off the item head", headMismatch),
            ("duplicate evidence id", duplicateEvidence),
            ("claim reusing an evidence id", claimReusesEvidenceID),
            ("negative delivery_count", negativeTiming),
            ("non-positive entity_version", zeroMeta),
            ("receipts without deliveries", receiptsWithoutDeliveries),
            ("accepted before submitted", acceptedBeforeSubmitted),
            ("endpoints without a span", endpointsWithoutSpan),
            ("unapproved evidence recipe", unapprovedRecipe),
            ("stale publish_eligible bit", staleEligibleBit),
        ] {
            let server = MockServer(items: [seed])
            let client = APIClientFactory.mock(server: server)
            let list = try await client.listAttentionItems()
            guard case .undocumented(let status, _) = list else {
                Issue.record("\(label): expected a failed list read, got \(list)")
                continue
            }
            #expect(status == 500, "\(label)")
        }
    }

    @Test func seededItemWithForgedBindingsAcceptsNothing() async throws {
        // The row re-validation runs before any binding comparison: a
        // seed whose artifact_digests is not the canonical union of the
        // rendered digests fails closed even for a command that matches
        // the forged field (the stale-approval class, plan §3.1).
        var forged = AttentionFixtures.fixture(type: .spec_approval)
        forged.item.artifact_digests.removeLast()
        let server = MockServer(items: [forged])
        let client = APIClientFactory.mock(server: server)

        let output = try await client.submitCommand(
            body: .json(Self.command(id: "cmd-forged", against: forged)))
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 422)

        // No effect on the row; the read path (rightly) refuses to serve
        // it at all, so assert through the raw table.
        let after = await server.snapshot(itemID: forged.item.id)
        #expect(after == forged)

        // Snapshot metadata is part of the same submit-time re-gate: a
        // zero as_of_revision fails acceptance even with a valid item.
        var zeroMeta = AttentionFixtures.fixture(type: .agent_question)
        zeroMeta.as_of_revision = 0
        let metaServer = MockServer(items: [zeroMeta])
        let metaClient = APIClientFactory.mock(server: metaServer)
        let rejected = try await metaClient.submitCommand(
            body: .json(Self.command(id: "cmd-meta", against: zeroMeta, action: .stop)))
        guard case .undocumented(let metaStatus, _) = rejected else {
            Issue.record("expected an authoritative rejection, got \(rejected)")
            return
        }
        #expect(metaStatus == 422)
    }

    @Test func seededItemViolatingTypePolicyAcceptsNothing() async throws {
        // Signet re-runs the full per-type policy against the durable
        // row before any acceptance: a seeded spec_approval row that
        // also offers start fails the re-gate for every command, even
        // an otherwise-legal approve.
        var invalid = AttentionFixtures.fixture(type: .spec_approval)
        invalid.item.requested_decision.append(.start)
        let server = MockServer(items: [invalid])
        let client = APIClientFactory.mock(server: server)

        let output = try await client.submitCommand(
            body: .json(Self.command(id: "cmd-policy", against: invalid, action: .approve)))
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 422)

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: invalid.item.id)).ok.body.json
        #expect(after == invalid)
    }

    @Test func blockedItemAcceptsNoActionEvenWhenOffered() async throws {
        // Signet policy pins blocked read-only (#97): since #96 the
        // canonical fixture offers the empty set, so any command's
        // action is rejected as not offered, without effect.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-blocked")).ok.body.json
        #expect(before.item.requested_decision.isEmpty)

        let output = try await client.submitCommand(
            body: .json(Self.command(id: "cmd-blocked", against: before, action: .acknowledge)))
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 422)

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-blocked")).ok.body.json
        #expect(after == before)

        // "Even when offered": a seeded blocked row that forges an
        // offered action fails the per-type policy re-gate for that very
        // action — blocked's allowed set is empty, as on the daemon.
        var forged = AttentionFixtures.fixture(type: .blocked)
        forged.item.requested_decision = [.acknowledge]
        let forgedServer = MockServer(items: [forged])
        let forgedClient = APIClientFactory.mock(server: forgedServer)
        let rejected = try await forgedClient.submitCommand(
            body: .json(Self.command(id: "cmd-blocked-forged", against: forged)))
        guard case .undocumented(let forgedStatus, _) = rejected else {
            Issue.record("expected an authoritative rejection, got \(rejected)")
            return
        }
        #expect(forgedStatus == 422)
    }

    @Test func actionTheItemDidNotOfferIsRejectedWithoutSideEffect() async throws {
        // A valid Action outside the item's requested_decision set is
        // rejected (daemon ErrActionNotOffered), even when every binding
        // matches the live item.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(!before.item.requested_decision.contains(.start))

        let output = try await client.submitCommand(
            body: .json(Self.command(id: "cmd-unoffered", against: before, action: .start)))
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 422)

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(after == before)
    }

    @Test func commandAgainstAClosedItemReturnsTheClosedReplacement() async throws {
        // Openness is checked before binding equality, and closure shares
        // the API's 409 replacement-snapshot shape with staleness (the
        // recorded #65 decision): a concluded item reports its canonical
        // closed state at any version, never a rebind invitation.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        _ =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-close", against: before)))
            .ok.body.json
        let closed =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(closed.item.status == .resolved)

        // Prepared against the closed item's own current (matching)
        // bindings: still a closure rejection carrying the closed item.
        let output = try await client.submitCommand(
            body: .json(Self.command(id: "cmd-after-close", against: closed)))
        let rejection = try output.conflict.body.json
        #expect(rejection.replacement_item == closed)

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(after == closed)
    }

    @Test func unknownItemIsAnAuthoritativeNotFound() async throws {
        // A command against a nonexistent item is a pre-commit rejection
        // surfaced as an authoritative HTTP response, never a thrown
        // transport error a client could mistake for a lost response.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        var command = Self.command(id: "cmd-unknown-item", against: before)
        command.payload.item_id = "item-unknown"

        let output = try await client.submitCommand(body: .json(command))
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 404)
    }

    @Test func seededRevisionNeverRunsBackwards() async throws {
        // Seeding is the public scenario-control path: the server
        // revision starts at the maximum seeded as_of_revision, so the
        // heartbeat and the next CommandResult always advance.
        var seeded = AttentionFixtures.fixture(type: .spec_approval)
        seeded.as_of_revision = 7
        let server = MockServer(items: [seeded])
        let client = APIClientFactory.mock(server: server)

        let heartbeat = try await client.getSyncRevision().ok.body.json
        #expect(heartbeat.revision >= 7)

        let result =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-rev", against: seeded)))
            .ok.body.json
        #expect(result.revision > 7)
    }

    @Test func recordOnlyActionLeavesTheItemUntouched() async throws {
        // Mirrors signet actionOutcome: for mark_seen the command record
        // is the whole server-side effect; the item row is untouched, so
        // no version bumps and the item stays open at the same state.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-ready_for_final_review")).ok.body.json

        let command = Self.command(id: "cmd-seen", against: before, action: .mark_seen)
        _ = try await client.submitCommand(body: .json(command)).ok.body.json

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-ready_for_final_review")).ok.body.json
        #expect(after == before)
    }

    @Test func concludingRetryResolvesTheFailureItem() async throws {
        // Mirrors signet actionOutcome: plain retry concludes the failure
        // item as resolved; the superseding replacement is the Wave 2
        // engine's reaction, not the acceptance's.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-execution_failure")).ok.body.json
        let command = Self.command(id: "cmd-retry-resolve", against: before, action: .retry)
        _ = try await client.submitCommand(body: .json(command)).ok.body.json
        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-execution_failure")).ok.body.json
        #expect(after.item.status == .resolved)
    }

    @Test func pendingActionIsRejectedBeforeAnyEffect() async throws {
        // Mirrors signet actionOutcome: a pending action's transaction
        // belongs to a later unit, so acceptance rejects it rather than
        // record a command whose effect would be silently dropped.
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(before.item.requested_decision.contains(.discuss))

        let output = try await client.submitCommand(
            body: .json(Self.command(id: "cmd-pending", against: before, action: .discuss)))
        guard case .undocumented(let statusCode, _) = output else {
            Issue.record("expected an authoritative rejection, got \(output)")
            return
        }
        #expect(statusCode == 422)

        // No effect: the same prepared state still applies cleanly.
        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        #expect(after == before)
        _ =
            try await client
            .submitCommand(body: .json(Self.command(id: "cmd-still-valid", against: before)))
            .ok.body.json
    }

    @Test func dismissDismissesTheItem() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let before =
            try await client
            .getAttentionItem(path: .init(item_id: "item-ready_for_final_review")).ok.body.json

        let command = Self.command(id: "cmd-dismiss", against: before, action: .dismiss)
        _ = try await client.submitCommand(body: .json(command)).ok.body.json

        let after =
            try await client
            .getAttentionItem(path: .init(item_id: "item-ready_for_final_review")).ok.body.json
        #expect(after.item.status == .dismissed)
        // Dismiss is a concluding decision like resolve: it stamps (#171).
        #expect(after.item.decided_at != nil)
    }

    static func command(
        id: String,
        against snapshot: Components.Schemas.AttentionItemSnapshot,
        action: Components.Schemas.Action? = nil
    ) -> Components.Schemas.ClientCommand {
        .init(
            command_id: id,
            device_id: "device-mock",
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .init(
                item_id: snapshot.item.id,
                action: action ?? snapshot.item.requested_decision[0],
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests
            )
        )
    }
}
