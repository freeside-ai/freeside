import Foundation
import FreesideAPI
import OpenAPIRuntime
import Testing

/// A token the tests can mint mid-session, as pairing does.
private actor TokenBox {
    private var token: String?

    func set(_ value: String?) {
        token = value
    }

    func get() -> String? {
        token
    }
}

@Suite struct BearerAuthMiddlewareTests {
    @Test func oneClientSpansPairingByConsultingTheProviderPerRequest() async throws {
        // Against an enforcing server: no header while the provider is
        // empty (401), the pairing exchange itself needs none (201), and
        // the minted credential then authenticates the same client.
        let server = MockServer(authMode: .enforcing)
        await server.seedPairingCode("483911")
        let box = TokenBox()
        let client = APIClientFactory.mock(server: server) { await box.get() }

        let before = try await client.getSyncRevision()
        guard case .undocumented(401, _) = before else {
            Issue.record("expected 401 without a credential, got \(before)")
            return
        }

        let grant = try await client.pairDevice(
            body: .json(.init(pairing_code: "483911", display_name: "Ben's iPhone"))
        ).created.body.json
        await box.set(grant.device_token)

        _ = try await client.getSyncRevision().ok.body.json
        _ = try await client.getSyncBootstrap().ok.body.json
    }

    @Test func aWrongTokenIsSentAndRejectedNotSilentlyDropped() async throws {
        // The middleware injects whatever the provider holds; a stale or
        // corrupt credential must surface as the server's 401, never as
        // an anonymous retry.
        let server = MockServer(authMode: .enforcing)
        let client = APIClientFactory.mock(server: server) { "fsd1.bogus.token" }
        let output = try await client.getSyncRevision()
        guard case .undocumented(401, _) = output else {
            Issue.record("expected 401 for a bogus credential, got \(output)")
            return
        }
    }
}
