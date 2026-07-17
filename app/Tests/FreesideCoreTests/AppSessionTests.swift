import Foundation
import FreesideAPI
import FreesideCore
import Testing

private struct StoreRefused: Error {}

/// A credential store whose save always fails, for the grant-custody
/// failure path.
private struct FailingCredentialStore: DeviceCredentialStore {
    func load() throws -> DeviceCredential? { nil }
    func save(_ credential: DeviceCredential) throws { throw StoreRefused() }
    func delete() throws {}
}

@Suite @MainActor struct PairingModelTests {
    @Test func pairingStoresTheCredentialAndReturnsIt() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let credentials = InMemoryCredentialStore()
        let model = PairingModel(
            client: APIClientFactory.mock(server: server), credentials: credentials)
        #expect(!model.canSubmit)
        model.pairingCode = "483911"
        model.displayName = "Ben's iPhone"
        #expect(model.canSubmit)

        let credential = try #require(await model.pair())

        #expect(model.phase == .idle)
        #expect(credential.token.hasPrefix("fsd1."))
        #expect(credential.ntfySubscription.serverURL == "https://ntfy.example")
        #expect(credential.ntfySubscription.topic == "fs-00000000000000000000000000000001")
        // Custody moved inside the same operation: the stored credential
        // is the returned one.
        #expect(try credentials.load() == credential)
    }

    @Test func malformedSubscriptionNeverBecomesDurableAuthority() async throws {
        for (serverURL, topic) in [
            ("https://publisher-value@ntfy.example", "fs-00000000000000000000000000000001"),
            ("http://ntfy.example", "fs-00000000000000000000000000000001"),
            ("http://+127.0.0.1", "fs-00000000000000000000000000000001"),
            ("http://0127.0.0.1", "fs-00000000000000000000000000000001"),
            ("http://[::ffff:0127.0.0.1]", "fs-00000000000000000000000000000001"),
            ("http://[::1%25does-not-exist]", "fs-00000000000000000000000000000001"),
            ("https://ntfy.example:99999", "fs-00000000000000000000000000000001"),
            ("https://ntfy.example:0", "fs-00000000000000000000000000000001"),
            ("https://ntfy.example%3A99999", "fs-00000000000000000000000000000001"),
            ("https://ntfy.example%40evil.com", "fs-00000000000000000000000000000001"),
            ("https://ntfy.example%2Fevil", "fs-00000000000000000000000000000001"),
            ("https://[not-an-ip]", "fs-00000000000000000000000000000001"),
            ("https://[::gg]", "fs-00000000000000000000000000000001"),
            ("https://[%3A%3A1]", "fs-00000000000000000000000000000001"),
            ("https://[fe80::1%25en0%0Aevil]", "fs-00000000000000000000000000000001"),
            ("https://[fe80::1%25en0%0Aevil]:443", "fs-00000000000000000000000000000001"),
            ("https://[fe80::1%25en0%2Fevil]", "fs-00000000000000000000000000000001"),
            ("https://[fe80::1%25en0%ZZ]", "fs-00000000000000000000000000000001"),
            ("https://[not-an-ip]:443", "fs-00000000000000000000000000000001"),
            ("https://ntfy.example%40evil:443", "fs-00000000000000000000000000000001"),
            ("https://ntfy.example?shared=true", "fs-00000000000000000000000000000001"),
            ("https://ntfy.example", "not-a-private-topic"),
        ] {
            let server = MockServer(
                authMode: .enforcing,
                pairingCodes: ["483911": .valid],
                pairingNtfyServerURL: serverURL,
                pairingNtfyTopic: topic
            )
            let credentials = InMemoryCredentialStore()
            let model = PairingModel(
                client: APIClientFactory.mock(server: server), credentials: credentials)
            model.pairingCode = "483911"
            model.displayName = "Malformed grant"

            #expect(await model.pair() == nil)
            #expect(try credentials.load() == nil)
            guard case .failed(let message) = model.phase else {
                Issue.record("expected malformed subscription failure, got \(model.phase)")
                continue
            }
            #expect(message.contains("private grant"))
            #expect(message.contains("revoke"))
        }
    }

    @Test func daemonAcceptedSubscriptionURLFormsRemainUsable() async throws {
        for serverURL in [
            "http://[0:0:0:0:0:0:0:1]",
            "http://[::ffff:127.0.0.1]",
            "http://[0:0:0:0:0:ffff:7f00:1]",
            "https://m%C3%BCnich.example",
            "https://[fe80::1%25en0]",
            "https://[fe80::1%25en0%20space]",
            "https://[fe80::1%25en0%25suffix]",
            "https://ntfy.example:443",
            "https://[::1]:443",
            "https://[fe80::1%25en0]:443",
        ] {
            let server = MockServer(
                authMode: .enforcing,
                pairingCodes: ["483911": .valid],
                pairingNtfyServerURL: serverURL
            )
            let model = PairingModel(
                client: APIClientFactory.mock(server: server),
                credentials: InMemoryCredentialStore())
            model.pairingCode = "483911"
            model.displayName = "Loopback grant"

            let credential = try #require(await model.pair())
            #expect(credential.ntfySubscription.serverURL == serverURL)
        }
    }

    @Test func malformedTokensNeverBecomeDurableAuthority() async throws {
        for token in [
            testDeviceToken(for: "device-9"),
            "fsd1.ZGV2aWNlLTE.eA",
        ] {
            let server = MockServer(
                authMode: .enforcing,
                pairingCodes: ["483911": .valid],
                pairingDeviceToken: token
            )
            let credentials = InMemoryCredentialStore()
            let model = PairingModel(
                client: APIClientFactory.mock(server: server), credentials: credentials)
            model.pairingCode = "483911"
            model.displayName = "Malformed grant"

            #expect(await model.pair() == nil)
            #expect(try credentials.load() == nil)
            guard case .failed(let message) = model.phase else {
                Issue.record("expected invalid grant failure, got \(model.phase)")
                continue
            }
            #expect(message.contains("private grant"))
            #expect(message.contains("revoke"))
        }
    }

    @Test func rejectionSurfacesOneUndifferentiatedMessage() async throws {
        let server = MockServer(
            authMode: .enforcing,
            pairingCodes: ["gone": .consumed, "old": .expired])
        let credentials = InMemoryCredentialStore()
        let model = PairingModel(
            client: APIClientFactory.mock(server: server), credentials: credentials)
        model.displayName = "probe"

        var failures: Set<String> = []
        for code in ["gone", "old", "never-minted"] {
            model.pairingCode = code
            #expect(await model.pair() == nil)
            guard case .failed(let message) = model.phase else {
                Issue.record("expected a rejection for \(code), got \(model.phase)")
                continue
            }
            failures.insert(message)
        }
        // Test 13's client face: the UI can say no more than the daemon
        // did, so every rejection reads identically.
        #expect(failures.count == 1)
        #expect(try credentials.load() == nil)
    }

    @Test func aGrantWhoseCredentialCannotBeStoredFailsLoud() async throws {
        // The token appears exactly once, in the grant; losing custody
        // is unrecoverable and must never present as paired.
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let model = PairingModel(
            client: APIClientFactory.mock(server: server),
            credentials: FailingCredentialStore())
        model.pairingCode = "483911"
        model.displayName = "Ben's iPhone"

        #expect(await model.pair() == nil)

        guard case .failed(let message) = model.phase else {
            Issue.record("expected a loud failure, got \(model.phase)")
            return
        }
        #expect(message.contains("revoke"))
    }
}

@Suite @MainActor struct AppSessionTests {
    @Test func aSessionWithoutACredentialNeedsPairingAndCompletes() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let credentials = InMemoryCredentialStore()
        let session = AppSession(
            client: APIClientFactory.mock(server: server) { (try? credentials.load())?.token },
            credentials: credentials,
            cache: InMemoryCacheStore()
        )
        guard case .needsPairing(let model) = session.phase else {
            Issue.record("expected the pairing gate, got \(session.phase)")
            return
        }

        model.pairingCode = "483911"
        model.displayName = "Ben's iPhone"
        let credential = try #require(await model.pair())
        session.completePairing(credential)

        guard case .ready(let coordinator) = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }
        // The synced surface runs under the minted identity and
        // credential: a full bootstrap round-trips the enforcing server.
        #expect(coordinator.store.device.deviceID == credential.deviceID)
        await coordinator.bootstrap()
        #expect(coordinator.store.freshness == .fresh)
        #expect(!coordinator.store.rows.isEmpty)
    }

    @Test func credentialsAndCacheAreScopedToTheDaemonDeployment() throws {
        // A device credential is minted by one daemon; the live
        // composition keys both the Keychain lookup and the cache
        // directory on the deployment, so a token paired with one daemon
        // can never be attached to a request for another.
        let a = URL(string: "https://Daemon.Example:8443/")!
        let sameAsA = URL(string: "https://daemon.example:8443")!
        let otherPort = URL(string: "https://daemon.example:9000")!
        let otherHost = URL(string: "https://other.example:8443")!

        #expect(AppSession.deploymentKey(for: a) == AppSession.deploymentKey(for: sameAsA))
        #expect(AppSession.deploymentKey(for: a) != AppSession.deploymentKey(for: otherPort))
        #expect(AppSession.deploymentKey(for: a) != AppSession.deploymentKey(for: otherHost))
        #expect(AppSession.cacheDirectory(for: a) == AppSession.cacheDirectory(for: sameAsA))
        #expect(AppSession.cacheDirectory(for: a) != AppSession.cacheDirectory(for: otherPort))

        // The directory derivation must be exactly as injective as the
        // key: URLs whose keys differ only in characters a naive
        // sanitization would collapse still get distinct directories.
        let colonPath = URL(string: "https://daemon.example/a:b")!
        let slashPath = URL(string: "https://daemon.example/a/b")!
        #expect(
            AppSession.deploymentKey(for: colonPath) != AppSession.deploymentKey(for: slashPath))
        #expect(
            AppSession.cacheDirectory(for: colonPath)
                != AppSession.cacheDirectory(for: slashPath))
    }

    @Test func aSessionWithACredentialIsReadyImmediately() async throws {
        let credentials = InMemoryCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-7", token: testDeviceToken(for: "device-7"),
                ntfySubscription: .mock)!)
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore()
        )
        guard case .ready(let coordinator) = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }
        #expect(coordinator.store.device.deviceID == "device-7")
    }
}
