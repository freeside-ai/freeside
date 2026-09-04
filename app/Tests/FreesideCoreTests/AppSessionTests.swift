import Foundation
import FreesideAPI
import Testing

@testable import FreesideCore

private struct StoreRefused: Error {}

/// A credential store whose save always fails, for the grant-custody
/// failure path.
private struct FailingCredentialStore: DeviceCredentialStore {
    func load() throws -> DeviceCredential? { nil }
    func save(_ credential: DeviceCredential) throws { throw StoreRefused() }
    func delete() throws {}
}

@Suite @MainActor struct PairingModelTests {
    @Test func previewFactsFollowTheCodeAndClearOnRejection() async throws {
        // Plan §5.14 pairing facts: a live code previews to the daemon's
        // facts without being consumed; editing the code drops them until
        // the daemon answers again; a dead code shows nothing, never why.
        let server = MockServer(
            authMode: .enforcing, pairingCodes: ["483911": .valid, "USEDUP": .consumed])
        let model = PairingModel(
            client: APIClientFactory.mock(server: server), credentials: InMemoryCredentialStore())
        await model.refreshFacts()
        #expect(model.facts == nil)

        model.pairingCode = "4839-11"
        await model.refreshFacts()
        let facts = try #require(model.facts)
        #expect(facts == MockServer.pairingFacts)
        #expect(PairingModel.connectionLabel(facts.connection_mode) == "Local")
        #expect(
            PairingModel.scopeLabel(facts.granted_scope)
                == "Full operator control, revocable from the host")

        model.pairingCode = "USEDUP"
        #expect(model.facts == nil)
        await model.refreshFacts()
        #expect(model.facts == nil)

        // The previewed code is still redeemable.
        model.pairingCode = "483911"
        model.displayName = "Ben's iPhone"
        await model.refreshFacts()
        #expect(model.facts != nil)
        #expect(await model.pair() != nil)
    }

    @Test func pairingStoresTheCredentialAndReturnsIt() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let credentials = InMemoryCredentialStore()
        let model = PairingModel(
            client: APIClientFactory.mock(server: server), credentials: credentials)
        #expect(!model.canSubmit)
        model.pairingCode = "483911"
        model.displayName = "Ben's iPhone"
        // Pair stays closed until the preview answers for this code.
        #expect(!model.canSubmit)
        await model.refreshFacts()
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

    @Test func separatorFormattedCodeSubmitsCanonically() async throws {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["AB011XYZ": .valid])
        let model = PairingModel(
            client: APIClientFactory.mock(server: server),
            credentials: InMemoryCredentialStore(),
            displayName: "Studio Mac")

        model.applyPairingCodeInput("  ab-oil xyz\n")

        #expect(model.pairingCode == "AB011XYZ")
        #expect(model.formattedPairingCode == "AB01-1XYZ")
        await model.refreshFacts()
        #expect(await model.pair() != nil)
    }

    @Test func rejectedPreviewLeavesPairDisabled() async {
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let model = PairingModel(
            client: APIClientFactory.mock(server: server),
            credentials: InMemoryCredentialStore(),
            displayName: "Studio Mac")

        model.pairingCode = "000000"
        await model.refreshFacts()

        #expect(model.facts == nil)
        #expect(!model.canSubmit)
        #expect(await model.pair() == nil)

        // A code the daemon does describe opens the control again.
        model.pairingCode = "483911"
        await model.refreshFacts()
        #expect(model.facts != nil)
        #expect(model.canSubmit)
    }

    @Test func deviceNamePrefillRemainsEditable() {
        let model = PairingModel(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            displayName: "Studio Mac")

        #expect(model.displayName == "Studio Mac")
        model.displayName = "Review iPhone"
        #expect(model.displayName == "Review iPhone")
    }

    @Test func failedPrefilledAttemptRemainsReplaceable() async {
        let model = PairingModel(
            client: APIClientFactory.mock(server: MockServer(authMode: .enforcing)),
            credentials: InMemoryCredentialStore(),
            displayName: "Studio Mac")
        model.prefillPairingCode("stale-code")

        #expect(await model.pair() == nil)
        model.prefillPairingCode("fresh-code")

        #expect(model.pairingCode == "FRESHC0DE")
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
            await model.refreshFacts()

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
            await model.refreshFacts()

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
            await model.refreshFacts()

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
        let credentials = InMemoryCredentialStore()

        // Pair is closed until the preview describes the code, so the
        // rejection the operator can still reach is a code that dies
        // between the preview and the exchange. Each way of dying must
        // read the same.
        var failures: Set<String> = []
        for deadState in [MockServer.PairingCodeState.consumed, .expired] {
            let server = MockServer(
                authMode: .enforcing, pairingCodes: ["483911": .valid])
            let model = PairingModel(
                client: APIClientFactory.mock(server: server), credentials: credentials)
            model.displayName = "probe"
            model.pairingCode = "483911"
            await model.refreshFacts()
            #expect(model.canSubmit)

            await server.seedPairingCode("483911", state: deadState)
            #expect(await model.pair() == nil)
            guard case .failed(let message) = model.phase else {
                Issue.record("expected a rejection for \(deadState), got \(model.phase)")
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
        await model.refreshFacts()

        #expect(await model.pair() == nil)

        guard case .failed(let message) = model.phase else {
            Issue.record("expected a loud failure, got \(model.phase)")
            return
        }
        #expect(message.contains("revoke"))
    }
}

@Suite @MainActor struct AppSessionTests {
    @Test func launchResolutionUsesExplicitModesThenReadinessThenPersisted() {
        let local = DaemonReadiness(
            apiURL: URL(string: "http://127.0.0.1:7331")!, pairingCode: "483911")

        #expect(
            AppSession.launchMode(
                argumentServerURL: "http://127.0.0.1:9000",
                pairingDemo: false,
                mockMode: true,
                readiness: local,
                persistedServerURL: "https://daemon.example",
                localDaemonURL: DaemonReadinessReader.supervisedAPIURL,
                hasCredential: { _ in false })
                == .live(URL(string: "http://127.0.0.1:9000")!, pairingCode: ""))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: true,
                mockMode: true,
                readiness: local,
                persistedServerURL: "https://daemon.example",
                localDaemonURL: DaemonReadinessReader.supervisedAPIURL,
                hasCredential: { _ in false }) == .pairingDemo)
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: true,
                readiness: local,
                persistedServerURL: "https://daemon.example",
                localDaemonURL: DaemonReadinessReader.supervisedAPIURL,
                hasCredential: { _ in false }) == .mock)
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: local,
                persistedServerURL: "https://daemon.example",
                localDaemonURL: DaemonReadinessReader.supervisedAPIURL,
                hasCredential: { _ in false })
                == .live(local.apiURL, pairingCode: "483911"))
        let staleLocal = DaemonReadiness(
            apiURL: URL(string: "http://127.0.0.1:49152")!, pairingCode: "stale-code")
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: staleLocal,
                persistedServerURL: nil,
                localDaemonURL: DaemonReadinessReader.supervisedAPIURL,
                hasCredential: { _ in false })
                == .live(DaemonReadinessReader.supervisedAPIURL, pairingCode: ""))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: nil,
                persistedServerURL: "https://daemon.example",
                localDaemonURL: DaemonReadinessReader.supervisedAPIURL,
                hasCredential: { _ in false })
                == .live(URL(string: "https://daemon.example")!, pairingCode: ""))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: nil,
                persistedServerURL: nil,
                localDaemonURL: DaemonReadinessReader.supervisedAPIURL,
                hasCredential: { _ in false })
                == .live(DaemonReadinessReader.supervisedAPIURL, pairingCode: ""))
    }

    @Test func launchResolutionPrefersTheDeploymentWithACredential() {
        let readinessURL = URL(string: "http://127.0.0.1:7331")!
        let persistedURL = URL(string: "http://127.0.0.1:8677")!
        let readiness = DaemonReadiness(apiURL: readinessURL, pairingCode: "483911")

        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: readiness,
                persistedServerURL: persistedURL.absoluteString,
                localDaemonURL: readinessURL,
                hasCredential: { $0 == persistedURL })
                == .live(persistedURL, pairingCode: ""))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: readiness,
                persistedServerURL: persistedURL.absoluteString,
                localDaemonURL: readinessURL,
                hasCredential: { _ in false })
                == .live(readinessURL, pairingCode: "483911"))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: readiness,
                persistedServerURL: persistedURL.absoluteString,
                localDaemonURL: readinessURL,
                hasCredential: { $0 == readinessURL || $0 == persistedURL })
                == .live(readinessURL, pairingCode: "483911"))
        #expect(
            AppSession.launchMode(
                argumentServerURL: "http://127.0.0.1:9000",
                pairingDemo: false,
                mockMode: false,
                readiness: readiness,
                persistedServerURL: persistedURL.absoluteString,
                localDaemonURL: readinessURL,
                hasCredential: { $0 == persistedURL })
                == .live(URL(string: "http://127.0.0.1:9000")!, pairingCode: ""))
        for malformedURL in ["%", "http://daemon.example:65536"] {
            var probedURLs: [URL] = []
            #expect(
                AppSession.launchMode(
                    argumentServerURL: nil,
                    pairingDemo: false,
                    mockMode: false,
                    readiness: readiness,
                    persistedServerURL: malformedURL,
                    localDaemonURL: readinessURL,
                    hasCredential: {
                        probedURLs.append($0)
                        return $0 != readinessURL
                    })
                    == .live(readinessURL, pairingCode: "483911"))
            #expect(probedURLs == [readinessURL])
        }
    }

    @Test func readinessPrefillsPairingWithoutChangingManualFallback() {
        let empty = AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore())
        guard case .needsPairing(let emptyModel) = empty.phase else {
            Issue.record("expected the manual pairing fallback")
            return
        }
        #expect(emptyModel.pairingCode.isEmpty)

        let prefilled = AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore(),
            pairingCode: "483911")
        guard case .needsPairing(let prefilledModel) = prefilled.phase else {
            Issue.record("expected readiness-backed pairing")
            return
        }
        #expect(prefilledModel.pairingCode == "483911")

        let stale = AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore(),
            pairingCode: "stale-code",
            deploymentURL: DaemonReadinessReader.supervisedAPIURL)
        guard case .needsPairing(let staleModel) = stale.phase else {
            Issue.record("expected stale readiness-backed pairing")
            return
        }
        stale.applyReadiness(
            DaemonReadiness(
                apiURL: DaemonReadinessReader.supervisedAPIURL, pairingCode: "fresh-code"))
        #expect(staleModel.pairingCode == "FRESHC0DE")
        stale.applyReadiness(nil)
        #expect(staleModel.pairingCode.isEmpty)
        stale.applyReadiness(
            DaemonReadiness(
                apiURL: DaemonReadinessReader.supervisedAPIURL, pairingCode: "replacement-code"))
        #expect(staleModel.pairingCode == "REP1ACEMENTC0DE")
        staleModel.pairingCode = ""
        stale.applyReadiness(
            DaemonReadiness(
                apiURL: DaemonReadinessReader.supervisedAPIURL, pairingCode: "newer-code"))
        #expect(staleModel.pairingCode.isEmpty)
        staleModel.pairingCode = "operator-input"
        stale.applyReadiness(
            DaemonReadiness(
                apiURL: DaemonReadinessReader.supervisedAPIURL, pairingCode: "newest-code"))
        #expect(staleModel.pairingCode == "operator-input")

        empty.applyReadiness(
            DaemonReadiness(
                apiURL: DaemonReadinessReader.supervisedAPIURL, pairingCode: "later-code"))
        #expect(emptyModel.pairingCode.isEmpty)

        let local = AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore(),
            deploymentURL: DaemonReadinessReader.supervisedAPIURL)
        guard case .needsPairing(let localModel) = local.phase else {
            Issue.record("expected local manual pairing")
            return
        }
        local.applyReadiness(
            DaemonReadiness(
                apiURL: DaemonReadinessReader.supervisedAPIURL, pairingCode: "later-code"))
        #expect(localModel.pairingCode == "1ATERC0DE")
        localModel.pairingCode = "operator-input"
        local.applyReadiness(nil)
        #expect(localModel.pairingCode == "operator-input")
        local.applyReadiness(
            DaemonReadiness(
                apiURL: DaemonReadinessReader.supervisedAPIURL, pairingCode: "newer-code"))
        #expect(localModel.pairingCode == "operator-input")

        let editedBeforeReadiness = AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore(),
            deploymentURL: DaemonReadinessReader.supervisedAPIURL)
        guard case .needsPairing(let editedModel) = editedBeforeReadiness.phase else {
            Issue.record("expected local manual pairing before readiness")
            return
        }
        editedModel.pairingCode = "manual-code"
        editedModel.pairingCode = ""
        editedBeforeReadiness.applyReadiness(
            DaemonReadiness(
                apiURL: DaemonReadinessReader.supervisedAPIURL, pairingCode: "late-code"))
        #expect(editedModel.pairingCode.isEmpty)
    }

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
        await model.refreshFacts()
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
        let encodedSlashPath = URL(string: "https://daemon.example/a%2Fb")!
        #expect(
            AppSession.deploymentKey(for: colonPath) != AppSession.deploymentKey(for: slashPath))
        #expect(
            AppSession.cacheDirectory(for: colonPath)
                != AppSession.cacheDirectory(for: slashPath))
        #expect(
            AppSession.deploymentKey(for: encodedSlashPath)
                != AppSession.deploymentKey(for: slashPath))
        #expect(
            AppSession.cacheDirectory(for: encodedSlashPath)
                != AppSession.cacheDirectory(for: slashPath))
    }

    @Test func completePairingPersistsTheLiveDeploymentURLForRelaunch() async throws {
        // On iOS an unadorned home-screen relaunch passes no launch
        // arguments and reads no daemon-host readiness file, so the paired
        // deployment URL must persist for `fromEnvironment()` to re-enter
        // live mode instead of the mock. A live session (deploymentURL set)
        // records it at pairing; the recorded value round-trips through
        // launchMode's persisted-URL branch back to the same deployment.
        let deploymentURL = URL(string: "http://100.64.0.1:7331")!
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let credentials = InMemoryCredentialStore()
        var persisted: [URL] = []
        let session = AppSession(
            client: APIClientFactory.mock(server: server) { (try? credentials.load())?.token },
            credentials: credentials,
            cache: InMemoryCacheStore(),
            deploymentURL: deploymentURL,
            persistServerURL: { persisted.append($0) }
        )
        guard case .needsPairing(let model) = session.phase else {
            Issue.record("expected the pairing gate, got \(session.phase)")
            return
        }
        model.pairingCode = "483911"
        model.displayName = "Ben's iPhone"
        await model.refreshFacts()
        let credential = try #require(await model.pair())
        session.completePairing(credential)

        #expect(persisted == [deploymentURL])
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: nil,
                persistedServerURL: persisted.first?.absoluteString,
                localDaemonURL: nil,
                hasCredential: { _ in true })
                == .live(deploymentURL, pairingCode: ""))
    }

    @Test func completePairingPersistsNothingWithoutALiveDeployment() async throws {
        // Mock and pairing-demo sessions carry no deploymentURL; pairing
        // them must not write a persisted URL that would strand a later
        // launch on a bogus deployment.
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let credentials = InMemoryCredentialStore()
        var persisted: [URL] = []
        let session = AppSession(
            client: APIClientFactory.mock(server: server) { (try? credentials.load())?.token },
            credentials: credentials,
            cache: InMemoryCacheStore(),
            persistServerURL: { persisted.append($0) }
        )
        guard case .needsPairing(let model) = session.phase else {
            Issue.record("expected the pairing gate, got \(session.phase)")
            return
        }
        model.pairingCode = "483911"
        model.displayName = "Ben's iPhone"
        await model.refreshFacts()
        let credential = try #require(await model.pair())
        session.completePairing(credential)

        #expect(persisted.isEmpty)
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

    @Test func aCredentialReadyLiveSessionPersistsItsDeploymentURL() async throws {
        // A live launch whose Keychain already holds a credential enters
        // `.ready` in init without pairing, so init is the only persistence
        // write; `completePairing` never runs. Skipping it strands the next
        // unadorned relaunch on the mock or a previously persisted server
        // (reinstall with preserved Keychain and cleared preferences, or
        // switching back to a previously paired daemon).
        let deploymentURL = URL(string: "http://100.64.0.1:7331")!
        let credentials = InMemoryCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-8", token: testDeviceToken(for: "device-8"),
                ntfySubscription: .mock)!)
        var persisted: [URL] = []
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore(),
            deploymentURL: deploymentURL,
            persistServerURL: { persisted.append($0) }
        )
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }
        #expect(persisted == [deploymentURL])
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: nil,
                persistedServerURL: persisted.first?.absoluteString,
                localDaemonURL: nil,
                hasCredential: { _ in true })
                == .live(deploymentURL, pairingCode: ""))
    }

    @Test func aCredentialReadySessionWithoutALiveDeploymentPersistsNothing() async throws {
        // Mock and pairing-demo sessions carry no deploymentURL even when a
        // credential is already present, so the immediate-ready init path
        // must not write a persisted URL that would strand a later launch.
        let credentials = InMemoryCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-9", token: testDeviceToken(for: "device-9"),
                ntfySubscription: .mock)!)
        var persisted: [URL] = []
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore(),
            persistServerURL: { persisted.append($0) }
        )
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }
        #expect(persisted.isEmpty)
    }
}
