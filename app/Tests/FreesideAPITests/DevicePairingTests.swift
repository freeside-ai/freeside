import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

/// Wraps the mock transport with a fixed bearer credential, standing in
/// for the client middleware while the mock's auth surface is under test.
private struct AuthorizedTransport: ClientTransport {
    let server: MockServer
    let token: String?

    func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        var request = request
        if let token {
            request.headerFields[.authorization] = "Bearer \(token)"
        }
        return try await MockServerTransport(server: server)
            .send(request, body: body, baseURL: baseURL, operationID: operationID)
    }
}

private func client(server: MockServer, token: String? = nil) -> Client {
    Client(
        serverURL: URL(string: "https://freeside.invalid")!,
        transport: AuthorizedTransport(server: server, token: token)
    )
}

/// The mock's device surface (plan §5.14 sync tests 13-16, server
/// halves): pairing-code lifecycle, single-winner consumption, terminal
/// idempotent revocation, and fail-closed bearer authentication.
@Suite struct DevicePairingTests {
    @Test func pairingExchangesAValidCodeExactlyOnce() async throws {
        let server = MockServer()
        await server.seedPairingCode("483911")
        let api = client(server: server)

        let grant = try await api.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        #expect(grant.device_token.hasPrefix("fsd1."))
        #expect(grant.ntfy_subscription.server_url == "https://ntfy.example")
        #expect(grant.ntfy_subscription.topic == "fs-00000000000000000000000000000001")
        guard case .active(let active) = grant.device.device else {
            Issue.record("expected an active device, got \(grant.device.device)")
            return
        }
        #expect(active.display_name == "Ben's iPhone")

        // The code is consumed (test 13's consumed half): a second
        // exchange creates nothing.
        let replay = try await api.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "second")))
        _ = try replay.forbidden
        #expect(await server.device(id: "device-2") == nil)
    }

    @Test func rejectionNeverDistinguishesUnknownExpiredOrConsumed() async throws {
        // One undifferentiated 403: an unauthenticated caller cannot
        // probe code validity (test 13).
        let server = MockServer()
        await server.seedPairingCode("expired", state: .expired)
        await server.seedPairingCode("consumed", state: .consumed)
        let api = client(server: server)

        var messages: Set<String> = []
        for code in ["expired", "consumed", "never-minted"] {
            let output = try await api.pairDevice(
                body: .json(.init(pairing_code: code, display_name: "probe")))
            messages.insert(try output.forbidden.body.json.message)
        }
        #expect(messages.count == 1)
    }

    @Test func simultaneousPairingWithOneCodeYieldsOneDevice() async throws {
        // Test 14, server half: the winner is single however simultaneous
        // the attempts are at their callers.
        let server = MockServer()
        await server.seedPairingCode("only-code")
        let api = client(server: server)

        let outcomes = await withTaskGroup(of: Bool.self) { group in
            for attempt in 0..<8 {
                group.addTask {
                    let output = try? await api.pairDevice(
                        body: .json(
                            .init(pairing_code: "only-code", display_name: "racer-\(attempt)")))
                    if case .created = output { return true }
                    return false
                }
            }
            return await group.reduce(into: [Bool]()) { $0.append($1) }
        }
        #expect(outcomes.filter { $0 }.count == 1)
        #expect(await server.device(id: "device-1") != nil)
        #expect(await server.device(id: "device-2") == nil)
    }

    @Test func revocationIsTerminalIdempotentAndClientVisible() async throws {
        let server = MockServer()
        await server.seedPairingCode("483911")
        let api = client(server: server)
        let grant = try await api.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        guard case .active(let active) = grant.device.device else {
            Issue.record("expected an active device")
            return
        }

        let revoked = try await api.revokeDevice(path: .init(device_id: active.id))
            .ok.body.json
        guard case .revoked(let record) = revoked.device else {
            Issue.record("expected a revoked device, got \(revoked.device)")
            return
        }
        #expect(record.id == active.id)
        #expect(revoked.entity_version == grant.device.entity_version + 1)
        // Pairing and revocation are both client-visible writes: each
        // moved the snapshot's revision forward.
        #expect(revoked.as_of_revision > grant.device.as_of_revision)

        // The replay returns the same recorded snapshot without a write:
        // no version moves, and the heartbeat holds still.
        let before = try await api.getSyncRevision().ok.body.json
        let replay = try await api.revokeDevice(path: .init(device_id: active.id))
            .ok.body.json
        #expect(replay == revoked)
        let after = try await api.getSyncRevision().ok.body.json
        #expect(after == before)
    }

    @Test func revokingAnUnknownDeviceIsNotFound() async throws {
        let api = client(server: MockServer())
        let output = try await api.revokeDevice(path: .init(device_id: "device-ghost"))
        _ = try output.notFound
    }

    @Test func enforcingModeFailsClosedExceptForPairing() async throws {
        let server = MockServer(authMode: .enforcing)
        await server.seedPairingCode("483911")

        // No credential and a garbage credential are both 401 on every
        // authenticated operation.
        for api in [client(server: server), client(server: server, token: "fsd1.bogus.token")] {
            let heartbeat = try await api.getSyncRevision()
            guard case .undocumented(let status, _) = heartbeat else {
                Issue.record("expected 401, got \(heartbeat)")
                continue
            }
            #expect(status == 401)
        }

        // Pairing is the one unauthenticated operation; its grant then
        // authenticates the sync surface.
        let grant = try await client(server: server).pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        let paired = client(server: server, token: grant.device_token)
        _ = try await paired.getSyncRevision().ok.body.json
        _ = try await paired.getSyncBootstrap().ok.body.json
    }

    @Test func credentialCannotNameAnotherDeviceInACommand() async throws {
        // Mirrors the daemon boundary (#105): the body's device_id must
        // equal the authenticated identity, ahead of contract semantics.
        let server = MockServer(authMode: .enforcing)
        await server.seedPairingCode("483911")
        let grant = try await client(server: server).pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        guard case .active(let active) = grant.device.device else {
            Issue.record("expected an active device")
            return
        }
        let paired = client(server: server, token: grant.device_token)
        let item =
            try await paired
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json

        var command = MockServerTests.command(id: "cmd-imposter", against: item)
        command.device_id = "device-someone-else"
        let output = try await paired.submitCommand(body: .json(command))
        guard case .undocumented(let status, _) = output else {
            Issue.record("expected 403, got \(output)")
            return
        }
        #expect(status == 403)

        // The matching identity is accepted.
        var honest = MockServerTests.command(id: "cmd-honest", against: item)
        honest.device_id = active.id
        _ = try await paired.submitCommand(body: .json(honest)).ok.body.json
    }

    @Test func revokedDeviceCannotSubmitAPreparedCommand() async throws {
        // Test 15, server half: revocation lands between preparing the
        // command and submitting it, and the submission commits nothing.
        let server = MockServer(authMode: .enforcing)
        await server.seedPairingCode("483911")
        let grant = try await client(server: server).pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        guard case .active(let active) = grant.device.device else {
            Issue.record("expected an active device")
            return
        }
        let paired = client(server: server, token: grant.device_token)
        let before =
            try await paired
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        var prepared = MockServerTests.command(id: "cmd-prepared", against: before)
        prepared.device_id = active.id

        // Revocation itself is an authenticated call; the still-active
        // credential performs it, and only then stops authenticating.
        _ = try await paired.revokeDevice(path: .init(device_id: active.id)).ok

        let output = try await paired.submitCommand(body: .json(prepared))
        guard case .undocumented(let status, _) = output else {
            Issue.record("expected 401, got \(output)")
            return
        }
        #expect(status == 401)
        // No side effect: the item is untouched at its original state.
        #expect(await server.snapshot(itemID: before.item.id) == before)
    }

    @Test func revokedRetryOfACommittedCommandReplaysWithoutSideEffect() async throws {
        // Test 16, server half, on the contract's may-branch: a verbatim
        // retry of the device's own committed command returns the
        // recorded result; anything else from the revoked device is 401,
        // and nothing new commits either way.
        let server = MockServer(authMode: .enforcing)
        await server.seedPairingCode("483911")
        let grant = try await client(server: server).pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        guard case .active(let active) = grant.device.device else {
            Issue.record("expected an active device")
            return
        }
        let paired = client(server: server, token: grant.device_token)
        let before =
            try await paired
            .getAttentionItem(path: .init(item_id: "item-spec_approval")).ok.body.json
        var command = MockServerTests.command(id: "cmd-committed", against: before)
        command.device_id = active.id
        let committed = try await paired.submitCommand(body: .json(command)).ok.body.json

        _ = try await paired.revokeDevice(path: .init(device_id: active.id)).ok
        let heartbeat = try await client(server: server, token: grant.device_token)
            .getSyncRevision()
        guard case .undocumented(401, _) = heartbeat else {
            Issue.record("expected the revoked credential to fail reads")
            return
        }

        let replayed = try await paired.submitCommand(body: .json(command)).ok.body.json
        #expect(replayed == committed)

        // A fresh command from the revoked device is rejected outright.
        let itemNow = await server.snapshot(itemID: before.item.id)
        var fresh = MockServerTests.command(
            id: "cmd-after-revocation", against: before, action: .stop)
        fresh.device_id = active.id
        let rejected = try await paired.submitCommand(body: .json(fresh))
        guard case .undocumented(let status, _) = rejected else {
            Issue.record("expected 401, got \(rejected)")
            return
        }
        #expect(status == 401)
        #expect(await server.snapshot(itemID: before.item.id) == itemNow)
    }
}
