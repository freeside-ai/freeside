import Foundation
import FreesideAPI
import Testing

/// Parity for the comprehension-telemetry surfaces (plan §8): the mock
/// implements capability registration, action-surface derivation, event
/// ingestion, and the submit-time surface revalidation exactly as the daemon
/// does, exercised end to end through the generated client.
@Suite struct MockServerComprehensionTests {
    private struct Paired {
        let client: any APIProtocol
        let server: MockServer
        let itemID: String
    }

    private func paired() async throws -> Paired {
        var seed = AttentionFixtures.fixture(type: .ready_for_final_review)
        seed.item.id = "item-1"
        let server = MockServer(
            items: [seed], authMode: .enforcing, pairingCodes: ["483911": .valid])
        let anon = APIClientFactory.mock(server: server)
        let grant = try await anon.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "d"))
        ).created.body.json
        let token = grant.device_token
        return Paired(
            client: APIClientFactory.mock(server: server, token: { token }),
            server: server, itemID: "item-1")
    }

    private func register(_ p: Paired) async throws -> Components.Schemas.ClientCapabilityContract {
        let actions = Components.Schemas.Action.allCases.filter { ActionOutcome.of($0) != .pending }
        return try await p.client.registerCapabilityContract(
            path: .init(device_id: "device-1"), body: .json(.init(actions: actions))
        ).ok.body.json
    }

    private func item(_ p: Paired) async throws -> Components.Schemas.AttentionItemSnapshot {
        try await p.client.getAttentionItem(path: .init(item_id: p.itemID)).ok.body.json
    }

    private func command(
        _ snapshot: Components.Schemas.AttentionItemSnapshot,
        id: String, action: Components.Schemas.Action, surfaceDigest: String?
    ) -> Components.Schemas.ClientCommand {
        .init(
            command_id: id, device_id: "device-1",
            expected_entity_version: snapshot.entity_version,
            expected_bindings: .init(additionalProperties: [:]),
            payload: .init(
                item_id: snapshot.item.id, action: action,
                item_version: snapshot.item.item_version,
                pr_head_sha: snapshot.item.pr_head_sha,
                artifact_digests: snapshot.item.artifact_digests,
                decision_action_surface_digest: surfaceDigest))
    }

    @Test func registerIsIdempotentByContent() async throws {
        let p = try await paired()
        let first = try await register(p)
        let before = try await p.client.getSyncRevision().ok.body.json
        let second = try await register(p)
        #expect(first.digest == second.digest)
        let after = try await p.client.getSyncRevision().ok.body.json
        #expect(after.revision == before.revision)
    }

    @Test func registerRejectsEmptyActions() async throws {
        let p = try await paired()
        let output = try await p.client.registerCapabilityContract(
            path: .init(device_id: "device-1"), body: .json(.init(actions: [])))
        if case .badRequest = output {
        } else {
            Issue.record("empty capability registration was accepted")
        }
    }

    @Test func actionSurfaceRequiresContract() async throws {
        let p = try await paired()
        let output = try await p.client.getActionSurface(path: .init(item_id: p.itemID))
        if case .conflict = output {
        } else {
            Issue.record("action surface derived without a registered contract")
        }
    }

    @Test func actionSurfaceIntersectsContract() async throws {
        let p = try await paired()
        _ = try await register(p)
        let surface = try await p.client.getActionSurface(
            path: .init(item_id: p.itemID)
        ).ok.body.json
        let requested = Set(try await item(p).item.requested_decision)
        #expect(Set(surface.actions) == requested)
        #expect(surface.device_id == "device-1")
    }

    @Test func submitStampsAndRevalidatesTheSurface() async throws {
        let p = try await paired()
        _ = try await register(p)
        let surface = try await p.client.getActionSurface(
            path: .init(item_id: p.itemID)
        ).ok.body.json
        let snapshot = try await item(p)
        let action = surface.actions.first { $0 == .open_pr } ?? surface.actions[0]

        // Accepted with a valid surface digest stamps the evidence.
        let ok = try await p.client.submitCommand(
            body: .json(command(snapshot, id: "cmd-1", action: action, surfaceDigest: surface.digest))
        ).ok.body.json
        #expect(ok.record.decision_evidence?.value1.action_surface_digest == surface.digest)

        // An unknown surface digest is rejected (400), never widening.
        let bogus = "sha256:" + String(repeating: "a", count: 64)
        let rejected = try await p.client.submitCommand(
            body: .json(command(snapshot, id: "cmd-2", action: action, surfaceDigest: bogus)))
        if case .undocumented(let code, _) = rejected {
            #expect(code == 400)
        } else {
            Issue.record("an unknown surface digest was accepted")
        }
    }

    @Test func recordEventIsIdempotentAndDoesNotMoveRevision() async throws {
        let p = try await paired()
        _ = try await register(p)
        let surface = try await p.client.getActionSurface(
            path: .init(item_id: p.itemID)
        ).ok.body.json
        let before = try await p.client.getSyncRevision().ok.body.json
        let input = Components.Schemas.ComprehensionEventInput(
            item_id: p.itemID, kind: .card_opened,
            item_decision_surface_digest: surface.item_decision_surface_digest,
            occurred_at: Date(), sequence: 1)
        let first = try await p.client.recordComprehensionEvent(
            path: .init(event_id: "event-1"), body: .json(input)
        ).ok.body.json
        #expect(first.received_at != Date(timeIntervalSince1970: 0))
        // A replay with a different sequence returns the recorded row.
        var replay = input
        replay.sequence = 99
        let replayed = try await p.client.recordComprehensionEvent(
            path: .init(event_id: "event-1"), body: .json(replay)
        ).ok.body.json
        #expect(replayed.sequence == first.sequence)
        let after = try await p.client.getSyncRevision().ok.body.json
        #expect(after.revision == before.revision)
    }

    @Test func actionTakenRequiresAMatchingCommand() async throws {
        let p = try await paired()
        _ = try await register(p)
        let surface = try await p.client.getActionSurface(
            path: .init(item_id: p.itemID)
        ).ok.body.json
        let snapshot = try await item(p)
        let action = surface.actions.first { $0 == .open_pr } ?? surface.actions[0]
        _ = try await p.client.submitCommand(
            body: .json(command(snapshot, id: "cmd-1", action: action, surfaceDigest: surface.digest))
        ).ok

        func actionEvent(eventID: String, commandID: String) -> Components.Schemas.ComprehensionEventInput {
            .init(
                item_id: p.itemID, kind: .action_taken,
                item_decision_surface_digest: surface.item_decision_surface_digest,
                decision_action_surface_digest: surface.digest, command_id: commandID,
                occurred_at: Date(), sequence: 1)
        }
        // Backed by the accepted command: recorded.
        _ = try await p.client.recordComprehensionEvent(
            path: .init(event_id: "e-ok"), body: .json(actionEvent(eventID: "e-ok", commandID: "cmd-1"))
        ).ok
        // Referencing an unknown command: rejected.
        let bad = try await p.client.recordComprehensionEvent(
            path: .init(event_id: "e-bad"), body: .json(actionEvent(eventID: "e-bad", commandID: "cmd-missing")))
        if case .badRequest = bad {
        } else {
            Issue.record("an unbacked action_taken event was accepted")
        }
    }
}
