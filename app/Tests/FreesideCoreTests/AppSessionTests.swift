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

/// Holds a credential but refuses to delete it, for the re-pair path where
/// the delete fails and the session must stay ready with the credential
/// intact (#1458).
private final class DeleteRefusingCredentialStore: DeviceCredentialStore, @unchecked Sendable {
    private let lock = NSLock()
    private var credential: DeviceCredential?

    init(credential: DeviceCredential?) { self.credential = credential }

    func load() throws -> DeviceCredential? { lock.withLock { credential } }
    func save(_ credential: DeviceCredential) throws {
        lock.withLock { self.credential = credential }
    }
    func delete() throws { throw StoreRefused() }
}

/// Reports the credential gone but still throws from delete, mirroring the
/// macOS store that removes the authoritative Data Protection item before a
/// later legacy-Keychain error surfaces (#1458 re-pair recovery).
private final class PartialDeleteCredentialStore: DeviceCredentialStore, @unchecked Sendable {
    private let lock = NSLock()
    private var credential: DeviceCredential?

    init(credential: DeviceCredential?) { self.credential = credential }

    func load() throws -> DeviceCredential? { lock.withLock { credential } }
    func save(_ credential: DeviceCredential) throws {
        lock.withLock { self.credential = credential }
    }
    func delete() throws {
        lock.withLock { credential = nil }
        throw StoreRefused()
    }
}

private struct LoadRefused: Error {}

/// Loads until a delete is attempted, then throws from load, mirroring a
/// locked or ACL-restricted Keychain where a failed delete cannot be
/// confirmed by a reload: the credential may still exist, so re-pair must
/// treat it as indeterminate and stay ready (#1458 re-pair recovery).
private final class ReloadFailingAfterDeleteCredentialStore: DeviceCredentialStore, @unchecked Sendable {
    private let lock = NSLock()
    private var credential: DeviceCredential?
    private var deleteAttempted = false

    init(credential: DeviceCredential?) { self.credential = credential }

    func load() throws -> DeviceCredential? {
        try lock.withLock {
            if deleteAttempted { throw LoadRefused() }
            return credential
        }
    }
    func save(_ credential: DeviceCredential) throws {
        lock.withLock { self.credential = credential }
    }
    func delete() throws {
        lock.withLock { deleteAttempted = true }
        throw StoreRefused()
    }
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
    @Test func freshDeviceRequiresConnectionUnlessDemoIsExplicit() {
        for (mock, pairingDemo, expected) in [
            (false, false, AppSession.LaunchMode.needsConnection),
            (true, false, .mock),
            (false, true, .pairingDemo),
        ] {
            #expect(
                AppSession.launchMode(
                    argumentServerURL: nil, pairingDemo: pairingDemo, mockMode: mock,
                    readiness: .absent, persistedServerURL: nil, localDaemonURL: nil,
                    hasCredential: { _ in
                        Issue.record("An unconfigured launch must not look up credentials")
                        return false
                    }) == expected)
        }
    }

    @Test func invalidExplicitServerRequiresConnectionInsteadOfDemoOrFallback() {
        #expect(
            AppSession.launchMode(
                argumentServerURL: "not-a-server", pairingDemo: true, mockMode: true,
                readiness: .absent, persistedServerURL: "https://daemon.example",
                localDaemonURL: prodDaemonURL,
                hasCredential: { _ in false }) == .needsConnection)
    }

    @Test func connectionAddressAcceptsOnlyUsableDaemonURLs() {
        #expect(AppSession.serverURL(from: " http://100.64.0.1:7331 \n")?.absoluteString == "http://100.64.0.1:7331")
        #expect(AppSession.serverURL(from: "https://daemon.example/freeside") != nil)
        for value in [
            "", "not-a-server", "file:///tmp/server", "http://host:65536", "http://host:0",
            "https://user:password@host", "https://host?token=value", "https://host#fragment",
        ] {
            #expect(AppSession.serverURL(from: value) == nil)
        }
    }

    @Test func launchResolutionUsesExplicitModesThenReadinessThenPersisted() {
        let local = DaemonReadiness(
            apiURL: URL(string: "http://127.0.0.1:7331")!, pairingCode: "483911", environment: .prod, runID: "test-run")

        #expect(
            AppSession.launchMode(
                argumentServerURL: "http://127.0.0.1:9000",
                pairingDemo: false,
                mockMode: true,
                readiness: .ready(local),
                persistedServerURL: "https://daemon.example",
                localDaemonURL: prodDaemonURL,
                hasCredential: { _ in false })
                == .live(URL(string: "http://127.0.0.1:9000")!, pairingCode: ""))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: true,
                mockMode: true,
                readiness: .ready(local),
                persistedServerURL: "https://daemon.example",
                localDaemonURL: prodDaemonURL,
                hasCredential: { _ in false }) == .pairingDemo)
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: true,
                readiness: .ready(local),
                persistedServerURL: "https://daemon.example",
                localDaemonURL: prodDaemonURL,
                hasCredential: { _ in false }) == .mock)
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: .ready(local),
                persistedServerURL: "https://daemon.example",
                localDaemonURL: prodDaemonURL,
                hasCredential: { _ in false })
                == .live(local.apiURL, pairingCode: "483911"))
        let staleLocal = DaemonReadiness(
            apiURL: URL(string: "http://127.0.0.1:49152")!, pairingCode: "stale-code", environment: .prod,
            runID: "test-run")
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: .ready(staleLocal),
                persistedServerURL: nil,
                localDaemonURL: prodDaemonURL,
                hasCredential: { _ in false })
                == .live(prodDaemonURL, pairingCode: ""))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: .absent,
                persistedServerURL: "https://daemon.example",
                localDaemonURL: prodDaemonURL,
                hasCredential: { _ in false })
                == .live(URL(string: "https://daemon.example")!, pairingCode: ""))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: .absent,
                persistedServerURL: nil,
                localDaemonURL: prodDaemonURL,
                hasCredential: { _ in false })
                == .live(prodDaemonURL, pairingCode: ""))
    }

    @Test func launchResolutionPrefersTheDeploymentWithACredential() {
        let readinessURL = URL(string: "http://127.0.0.1:7331")!
        let persistedURL = URL(string: "http://127.0.0.1:8677")!
        let readiness = DaemonReadiness(
            apiURL: readinessURL, pairingCode: "483911", environment: .prod, runID: "test-run")

        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: .ready(readiness),
                persistedServerURL: persistedURL.absoluteString,
                localDaemonURL: readinessURL,
                hasCredential: { $0 == persistedURL })
                == .live(persistedURL, pairingCode: ""))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: .ready(readiness),
                persistedServerURL: persistedURL.absoluteString,
                localDaemonURL: readinessURL,
                hasCredential: { _ in false })
                == .live(readinessURL, pairingCode: "483911"))
        #expect(
            AppSession.launchMode(
                argumentServerURL: nil,
                pairingDemo: false,
                mockMode: false,
                readiness: .ready(readiness),
                persistedServerURL: persistedURL.absoluteString,
                localDaemonURL: readinessURL,
                hasCredential: { $0 == readinessURL || $0 == persistedURL })
                == .live(readinessURL, pairingCode: "483911"))
        #expect(
            AppSession.launchMode(
                argumentServerURL: "http://127.0.0.1:9000",
                pairingDemo: false,
                mockMode: false,
                readiness: .ready(readiness),
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
                    readiness: .ready(readiness),
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

    /// Runs the tier-aware resolution with no explicit launch mode, recording
    /// which readiness files it reads.
    private func tierLaunchMode(
        _ environment: FreesideEnvironment,
        readinessDirectory: String? = nil,
        persistedServerURL: String? = nil,
        readiness: DaemonReadinessOutcome = .absent,
        fileManager: FileManager = .default,
        hasCredential: (URL) -> Bool = { _ in false },
        readPaths: inout [String]
    ) throws(AppSession.LaunchError) -> AppSession.LaunchMode {
        var paths: [String] = []
        defer { readPaths = paths }
        return try AppSession.launchMode(
            environment: environment,
            argumentServerURL: nil,
            pairingDemo: false,
            mockMode: false,
            readinessDirectory: readinessDirectory,
            persistedServerURL: persistedServerURL,
            readReadiness: {
                paths.append($0.path)
                return readiness
            },
            hasCredential: hasCredential,
            fileManager: fileManager)
    }

    @Test func supervisedTiersReadTheirOwnReadinessAndFallBackToTheirOwnPort() throws {
        for (environment, root, port) in [
            (FreesideEnvironment.prod, "/Freeside/daemon/readiness.json", 7331),
            (.dev, "/Freeside Dev/daemon/readiness.json", 7332),
        ] {
            var readPaths: [String] = []
            let url = try #require(URL(string: "http://127.0.0.1:\(port)"))
            #expect(try tierLaunchMode(environment, readPaths: &readPaths) == .live(url, pairingCode: ""))
            #expect(readPaths.count == 1)
            #expect(readPaths.first?.hasSuffix("/Application Support\(root)") == true)

            let readiness = DaemonReadiness(
                apiURL: url, pairingCode: "483911", environment: environment, runID: "test-run")
            #expect(
                try tierLaunchMode(environment, readiness: .ready(readiness), readPaths: &readPaths)
                    == .live(url, pairingCode: "483911"))
            // Another tier's daemon URL in this tier's file is not this tier's daemon.
            let foreign = DaemonReadiness(
                apiURL: try #require(URL(string: "http://127.0.0.1:\(port == 7331 ? 7332 : 7331)")),
                pairingCode: "foreign", environment: environment, runID: "test-run")
            #expect(
                try tierLaunchMode(environment, readiness: .ready(foreign), readPaths: &readPaths)
                    == .live(url, pairingCode: ""))
            #expect(
                try tierLaunchMode(
                    environment, persistedServerURL: "https://daemon.example", readPaths: &readPaths)
                    == .live(try #require(URL(string: "https://daemon.example")), pairingCode: ""))
        }
    }

    @Test func ephemeralWithoutArgumentsAsksForAConnection() throws {
        var readPaths: [String] = []
        #expect(
            try tierLaunchMode(.ephemeral, persistedServerURL: prodDaemonURL.absoluteString, readPaths: &readPaths)
                == .needsConnection)
        #expect(readPaths.isEmpty)
        #expect(
            try tierLaunchMode(
                .ephemeral, persistedServerURL: "https://daemon.example", readPaths: &readPaths)
                == .needsConnection)
    }

    @Test func ephemeralFollowsTheNamedReadinessDirectory() throws {
        let runURL = try #require(URL(string: "http://127.0.0.1:52811"))
        var readPaths: [String] = []
        #expect(
            try tierLaunchMode(
                .ephemeral, readinessDirectory: "/tmp/run-1",
                persistedServerURL: prodDaemonURL.absoluteString,
                readiness: .ready(
                    DaemonReadiness(apiURL: runURL, pairingCode: "483911", environment: .ephemeral, runID: "test-run")),
                readPaths: &readPaths)
                == .live(runURL, pairingCode: "483911"))
        #expect(readPaths == ["/tmp/run-1/readiness.json"])
        // A run that has not published (or whose code aged out) asks rather
        // than falling back to any other daemon.
        #expect(
            try tierLaunchMode(
                .ephemeral, readinessDirectory: "/tmp/run-1",
                persistedServerURL: prodDaemonURL.absoluteString, readPaths: &readPaths)
                == .needsConnection)
    }

    @Test func ephemeralNeverProbesStoredCredentials() throws {
        // Ephemeral credentials live in memory, and a Debug build shares
        // prod's Keychain identity, so resolution must not load a Keychain
        // item even when it follows a readiness file.
        let runURL = try #require(URL(string: "http://127.0.0.1:52811"))
        var probed: [URL] = []
        var readPaths: [String] = []
        #expect(
            try tierLaunchMode(
                .ephemeral, readinessDirectory: "/tmp/run-1",
                persistedServerURL: prodDaemonURL.absoluteString,
                readiness: .ready(
                    DaemonReadiness(apiURL: runURL, pairingCode: "483911", environment: .ephemeral, runID: "test-run")),
                hasCredential: {
                    probed.append($0)
                    return true
                },
                readPaths: &readPaths)
                == .live(runURL, pairingCode: "483911"))
        #expect(probed.isEmpty)
    }

    @Test func readinessDirectoryIsRefusedOutsideEphemeralOrWhenRelative() {
        var readPaths: [String] = []
        for environment in [FreesideEnvironment.prod, .dev] {
            #expect(throws: AppSession.LaunchError.readinessDirectoryInSupervisedTier(environment)) {
                try tierLaunchMode(environment, readinessDirectory: "/tmp/run-1", readPaths: &readPaths)
            }
        }
        #expect(throws: AppSession.LaunchError.readinessDirectoryNotAbsolute("run-1")) {
            try tierLaunchMode(.ephemeral, readinessDirectory: "run-1", readPaths: &readPaths)
        }
        #expect(readPaths.isEmpty)
    }

    @Test func ephemeralRefusesAReadinessDirectoryInsideASupervisedRoot() throws {
        let sandbox = FileManager.default.temporaryDirectory
            .appendingPathComponent("tier-roots-\(UUID().uuidString)", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: sandbox) }
        let applicationSupport = sandbox.appendingPathComponent("Application Support", isDirectory: true)
        let prodRoot = applicationSupport.appendingPathComponent("Freeside", isDirectory: true)
        try FileManager.default.createDirectory(
            at: prodRoot.appendingPathComponent("daemon"), withIntermediateDirectories: true)
        let alias = sandbox.appendingPathComponent("alias")
        try FileManager.default.createSymbolicLink(at: alias, withDestinationURL: prodRoot)
        let fileManager = ApplicationSupportOverride(applicationSupport)

        var readPaths: [String] = []
        for (path, owner) in [
            (prodRoot.path, FreesideEnvironment.prod),
            (prodRoot.appendingPathComponent("daemon").path, .prod),
            (applicationSupport.appendingPathComponent("Freeside Dev/daemon").path, .dev),
            // An alias resolves to the root it names.
            (alias.appendingPathComponent("daemon").path, .prod),
        ] {
            #expect(throws: AppSession.LaunchError.readinessDirectoryUnderSupervisedRoot(path, owner)) {
                try tierLaunchMode(
                    .ephemeral, readinessDirectory: path, fileManager: fileManager, readPaths: &readPaths)
            }
        }
        #expect(readPaths.isEmpty)

        // A sibling that only shares the root's name as a prefix is not inside it.
        let sibling = applicationSupport.appendingPathComponent("Freeside Devious/daemon").path
        #expect(
            try tierLaunchMode(
                .ephemeral, readinessDirectory: sibling, fileManager: fileManager, readPaths: &readPaths)
                == .needsConnection)
        #expect(readPaths == [sibling + "/readiness.json"])
    }

    @Test func explicitLaunchModesStillWinInEveryTier() throws {
        let explicitURL = try #require(URL(string: "http://127.0.0.1:9000"))
        for environment in FreesideEnvironment.allCases {
            // A refused file is present; every explicit mode wins over it.
            let refused = DaemonReadinessOutcome.refused(.daemonTooOld(app: environment))
            let readinessDirectory = environment.isSupervised ? nil : "/tmp/run-1"
            let resolve = { (argumentServerURL: String?, pairingDemo: Bool, mockMode: Bool) throws in
                try AppSession.launchMode(
                    environment: environment, argumentServerURL: argumentServerURL,
                    pairingDemo: pairingDemo, mockMode: mockMode,
                    readinessDirectory: readinessDirectory, persistedServerURL: nil,
                    readReadiness: { _ in refused }, hasCredential: { _ in false })
            }
            #expect(try resolve(nil, false, true) == .mock)
            #expect(try resolve(nil, true, false) == .pairingDemo)
            #expect(try resolve(explicitURL.absoluteString, false, false) == .live(explicitURL, pairingCode: ""))
            #expect(try resolve(nil, false, false) == .refused(.daemonTooOld(app: environment)))
        }
    }

    /// Each tier refuses a readiness file stamped for another environment
    /// and never falls through to its persisted URL or fixed port (#1504).
    @Test func aRefusedReadinessFileStopsAtTheConnectScreenInEveryTier() throws {
        for (environment, other) in [
            (FreesideEnvironment.prod, FreesideEnvironment.dev), (.dev, .prod), (.ephemeral, .dev),
        ] {
            let mismatch = DaemonReadinessRefusal.environmentMismatch(app: environment, daemon: other)
            var readPaths: [String] = []
            #expect(
                try tierLaunchMode(
                    environment,
                    readinessDirectory: environment.isSupervised ? nil : "/tmp/run-1",
                    persistedServerURL: "https://daemon.example",
                    readiness: .refused(mismatch),
                    hasCredential: { _ in true },
                    readPaths: &readPaths)
                    == .refused(mismatch))
            #expect(readPaths.count == 1)
            #expect(
                try tierLaunchMode(
                    environment,
                    readinessDirectory: environment.isSupervised ? nil : "/tmp/run-1",
                    readiness: .refused(.daemonTooOld(app: environment)),
                    readPaths: &readPaths)
                    == .refused(.daemonTooOld(app: environment)))
        }
    }

    @Test func aRefusedLaunchShowsTheRefusalUntilAnAddressIsTyped() throws {
        let refusal = DaemonReadinessRefusal.environmentMismatch(app: .prod, daemon: .dev)
        let session = AppSession.session(
            for: .refused(refusal), localDaemonURL: prodDaemonURL, cacheRoot: nil,
            credentialStore: { _ in InMemoryCredentialStore() }, persistServerURL: { _ in })
        guard case .needsConnection = session.phase else {
            Issue.record("expected the connect screen")
            return
        }
        #expect(session.connectionRefusal == refusal)

        session.connect(serverURL: try #require(URL(string: "http://127.0.0.1:9000")))
        #expect(session.connectionRefusal == nil)
    }

    @Test func aSessionWithoutALocalDaemonKeepsItsPrefillWhenReadinessDisappears() {
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore(),
            pairingCode: "run-code",
            deploymentURL: prodDaemonURL)
        guard case .needsPairing(let model) = session.phase else {
            Issue.record("expected pairing")
            return
        }
        session.applyReadiness(nil)
        #expect(model.pairingCode == "RUNC0DE")
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
            deploymentURL: prodDaemonURL,
            localDaemonURL: prodDaemonURL)
        guard case .needsPairing(let staleModel) = stale.phase else {
            Issue.record("expected stale readiness-backed pairing")
            return
        }
        stale.applyReadiness(
            DaemonReadiness(
                apiURL: prodDaemonURL, pairingCode: "fresh-code", environment: .prod, runID: "test-run"))
        #expect(staleModel.pairingCode == "FRESHC0DE")
        stale.applyReadiness(nil)
        #expect(staleModel.pairingCode.isEmpty)
        stale.applyReadiness(
            DaemonReadiness(
                apiURL: prodDaemonURL, pairingCode: "replacement-code", environment: .prod, runID: "test-run"))
        #expect(staleModel.pairingCode == "REP1ACEMENTC0DE")
        staleModel.pairingCode = ""
        stale.applyReadiness(
            DaemonReadiness(
                apiURL: prodDaemonURL, pairingCode: "newer-code", environment: .prod, runID: "test-run"))
        #expect(staleModel.pairingCode.isEmpty)
        staleModel.pairingCode = "operator-input"
        stale.applyReadiness(
            DaemonReadiness(
                apiURL: prodDaemonURL, pairingCode: "newest-code", environment: .prod, runID: "test-run"))
        #expect(staleModel.pairingCode == "operator-input")

        empty.applyReadiness(
            DaemonReadiness(
                apiURL: prodDaemonURL, pairingCode: "later-code", environment: .prod, runID: "test-run"))
        #expect(emptyModel.pairingCode.isEmpty)

        let local = AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore(),
            deploymentURL: prodDaemonURL,
            localDaemonURL: prodDaemonURL)
        guard case .needsPairing(let localModel) = local.phase else {
            Issue.record("expected local manual pairing")
            return
        }
        local.applyReadiness(
            DaemonReadiness(
                apiURL: prodDaemonURL, pairingCode: "later-code", environment: .prod, runID: "test-run"))
        #expect(localModel.pairingCode == "1ATERC0DE")
        localModel.pairingCode = "operator-input"
        local.applyReadiness(nil)
        #expect(localModel.pairingCode == "operator-input")
        local.applyReadiness(
            DaemonReadiness(
                apiURL: prodDaemonURL, pairingCode: "newer-code", environment: .prod, runID: "test-run"))
        #expect(localModel.pairingCode == "operator-input")

        let editedBeforeReadiness = AppSession(
            client: APIClientFactory.mock(),
            credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore(),
            deploymentURL: prodDaemonURL,
            localDaemonURL: prodDaemonURL)
        guard case .needsPairing(let editedModel) = editedBeforeReadiness.phase else {
            Issue.record("expected local manual pairing before readiness")
            return
        }
        editedModel.pairingCode = "manual-code"
        editedModel.pairingCode = ""
        editedBeforeReadiness.applyReadiness(
            DaemonReadiness(
                apiURL: prodDaemonURL, pairingCode: "late-code", environment: .prod, runID: "test-run"))
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
                readiness: .absent,
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

    @Test func changingServerPreservesSavedCredentialsAndDeployment() throws {
        let deploymentURL = URL(string: "http://100.64.0.1:7331")!
        let credential = DeviceCredential(
            deviceID: "device-change", token: testDeviceToken(for: "device-change"),
            ntfySubscription: .mock)!
        let credentials = InMemoryCredentialStore(credential: credential)
        var persisted: [URL] = []
        let session = AppSession(
            client: APIClientFactory.mock(), credentials: credentials,
            cache: InMemoryCacheStore(), deploymentURL: deploymentURL,
            persistServerURL: { persisted.append($0) })

        session.changeServer()

        guard case .needsConnection = session.phase else {
            Issue.record("expected address entry, got \(session.phase)")
            return
        }
        #expect(try credentials.load() == credential)
        #expect(persisted == [deploymentURL])
    }

    @Test func ephemeralKeepsDeviceCredentialsInMemory() {
        // An ephemeral run may reuse a fixed port with a fresh credential
        // database; a Keychain credential from an earlier run would skip
        // that run's pairing, so only the supervised tiers use the Keychain.
        let url = URL(string: "http://127.0.0.1:7400")!
        #expect(AppSession.credentialStore(for: .ephemeral)(url) is InMemoryCredentialStore)
        #expect(AppSession.credentialStore(for: .prod)(url) is KeychainCredentialStore)
        #expect(AppSession.credentialStore(for: .dev)(url) is KeychainCredentialStore)
    }

    @Test func aTypedServerUsesTheSessionsCredentialStore() {
        // `connect(serverURL:)` keeps the launch's credential rule, so a
        // server typed into an ephemeral app never reaches the Keychain.
        let typedURL = URL(string: "http://127.0.0.1:7400")!
        var requested: [URL] = []
        let session = AppSession(
            client: APIClientFactory.mock(), credentials: InMemoryCredentialStore(),
            cache: InMemoryCacheStore(), cacheRoot: nil,
            credentialStore: { url in
                requested.append(url)
                return InMemoryCredentialStore()
            },
            persistServerURL: { _ in })

        session.changeServer()
        session.connect(serverURL: typedURL)

        #expect(requested == [typedURL])
        guard case .needsPairing = session.phase else {
            Issue.record("expected pairing, got \(session.phase)")
            return
        }
    }

    @Test func aCredentialReadyLiveSessionPersistsItsDeploymentURL() async throws {
        // A live launch whose Keychain already holds a credential enters
        // `.ready` in init without pairing, so init is the only persistence
        // write; `completePairing` never runs. Skipping it strands the next
        // unadorned relaunch on address entry or a previously persisted server
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
                readiness: .absent,
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

    @Test func rePairDeletesTheCredentialAndReturnsToPairing() throws {
        // #1458 acceptance 1: the operator's in-app recovery from a rejected
        // credential clears this deployment's credential and reopens pairing.
        let credentials = InMemoryCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-old", token: testDeviceToken(for: "device-old"),
                ntfySubscription: .mock)!)
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore())
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }

        try session.rePair()

        guard case .needsPairing = session.phase else {
            Issue.record("expected pairing after re-pair, got \(session.phase)")
            return
        }
        #expect(try credentials.load() == nil)
    }

    @Test func rePairThatCannotDeleteStaysReadyAndKeepsTheCredential() throws {
        // #1458 acceptance 2: deleting a credential is unrecoverable, so a
        // failed delete must never present as paired-again; the session stays
        // ready over the still-stored credential and the error reaches the
        // caller.
        let credential = DeviceCredential(
            deviceID: "device-stuck", token: testDeviceToken(for: "device-stuck"),
            ntfySubscription: .mock)!
        let credentials = DeleteRefusingCredentialStore(credential: credential)
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore())
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }

        #expect(throws: StoreRefused.self) { try session.rePair() }

        guard case .ready = session.phase else {
            Issue.record("expected the session to stay ready, got \(session.phase)")
            return
        }
        #expect(try credentials.load() == credential)
    }

    @Test func rePairReturnsToPairingWhenDeleteRemovedTheCredentialThenThrew() throws {
        // #1458: on macOS `delete()` removes the authoritative credential
        // before a legacy-Keychain error can surface, so a thrown delete does
        // not prove the credential survived. When it is already gone, re-pair
        // must return to pairing rather than strand a "still paired" session
        // that has no usable bearer token.
        let credentials = PartialDeleteCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-old", token: testDeviceToken(for: "device-old"),
                ntfySubscription: .mock)!)
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore())
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }

        try session.rePair()

        guard case .needsPairing = session.phase else {
            Issue.record("expected pairing after a delete that removed the credential, got \(session.phase)")
            return
        }
        #expect(try credentials.load() == nil)
    }

    @Test func rePairStaysReadyWhenAFailedDeleteCannotBeConfirmedByReload() throws {
        // #1458 review: when `delete()` throws and the reload also throws (a
        // locked or ACL-restricted Keychain), the credential may still exist.
        // Re-pair must treat that as indeterminate: surface the failure and
        // stay ready rather than show pairing over a credential that might
        // still answer requests.
        let credentials = ReloadFailingAfterDeleteCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-stuck", token: testDeviceToken(for: "device-stuck"),
                ntfySubscription: .mock)!)
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore())
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }

        #expect(throws: StoreRefused.self) { try session.rePair() }

        guard case .ready = session.phase else {
            Issue.record("expected the session to stay ready on an indeterminate delete, got \(session.phase)")
            return
        }
    }

    @Test func rePairThenPairingReturnsToReadyWithoutChangingTheDeployment() async throws {
        // #1458 acceptance 3: after re-pairing, a fresh pairing mints a new
        // device and returns to ready against the same deployment.
        let deploymentURL = URL(string: "http://100.64.0.1:7331")!
        let server = MockServer(authMode: .enforcing, pairingCodes: ["483911": .valid])
        let credentials = InMemoryCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-old", token: testDeviceToken(for: "device-old"),
                ntfySubscription: .mock)!)
        var persisted: [URL] = []
        let session = AppSession(
            client: APIClientFactory.mock(server: server) { (try? credentials.load())?.token },
            credentials: credentials,
            cache: InMemoryCacheStore(),
            deploymentURL: deploymentURL,
            persistServerURL: { persisted.append($0) })
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }
        #expect(persisted == [deploymentURL])

        try session.rePair()
        guard case .needsPairing(let model) = session.phase else {
            Issue.record("expected pairing after re-pair, got \(session.phase)")
            return
        }
        model.pairingCode = "483911"
        model.displayName = "Ben's iPhone"
        await model.refreshFacts()
        let credential = try #require(await model.pair())
        session.completePairing(credential)

        guard case .ready(let coordinator) = session.phase else {
            Issue.record("expected a ready session after pairing, got \(session.phase)")
            return
        }
        #expect(coordinator.store.device.deviceID == credential.deviceID)
        #expect(credential.deviceID != "device-old")
        // completePairing records the same deployment again; the deployment
        // the session pairs against never changed.
        #expect(persisted == [deploymentURL, deploymentURL])
    }

    @Test func aRelaunchAfterRePairLandsOnPairing() throws {
        // #1458 acceptance 4: the delete persists, so a new session built on
        // the same store starts at pairing, not the revoked banner.
        let credentials = InMemoryCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-old", token: testDeviceToken(for: "device-old"),
                ntfySubscription: .mock)!)
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore())
        try session.rePair()

        let relaunched = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore())

        guard case .needsPairing = relaunched.phase else {
            Issue.record("expected a relaunch to land on pairing, got \(relaunched.phase)")
            return
        }
    }

    @Test func rePairPrefillsFromTheLastReadinessAndKeepsOperatorInput() throws {
        // #1458 acceptance 5: the fresh pairing screen prefills from the last
        // readiness the session received, the same as a launch does, and an
        // operator-typed code is never overwritten.
        let deploymentURL = prodDaemonURL
        let credentials = InMemoryCredentialStore(
            credential: DeviceCredential(
                deviceID: "device-old", token: testDeviceToken(for: "device-old"),
                ntfySubscription: .mock)!)
        let session = AppSession(
            client: APIClientFactory.mock(),
            credentials: credentials,
            cache: InMemoryCacheStore(),
            deploymentURL: deploymentURL)
        // The Mac delivers readiness on change, so a ready session receives
        // it well before it re-pairs; it is remembered, not applied yet.
        session.applyReadiness(
            DaemonReadiness(apiURL: deploymentURL, pairingCode: "fresh-code", environment: .prod, runID: "test-run"))
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }

        try session.rePair()
        guard case .needsPairing(let model) = session.phase else {
            Issue.record("expected pairing after re-pair, got \(session.phase)")
            return
        }
        #expect(model.pairingCode == "FRESHC0DE")

        model.pairingCode = "operator-input"
        session.applyReadiness(
            DaemonReadiness(apiURL: deploymentURL, pairingCode: "newer-code", environment: .prod, runID: "test-run"))
        #expect(model.pairingCode == "operator-input")
    }
}

/// Points the user-domain Application Support directory at a test sandbox,
/// so tier state roots resolve there.
private final class ApplicationSupportOverride: FileManager, @unchecked Sendable {
    private let applicationSupport: URL

    init(_ applicationSupport: URL) {
        self.applicationSupport = applicationSupport
        super.init()
    }

    override func urls(
        for directory: FileManager.SearchPathDirectory, in domainMask: FileManager.SearchPathDomainMask
    ) -> [URL] {
        directory == .applicationSupportDirectory && domainMask == .userDomainMask
            ? [applicationSupport] : super.urls(for: directory, in: domainMask)
    }
}
