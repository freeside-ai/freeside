import Foundation
import FreesideAPI
import FreesideTestSupport
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// Get Code on the pairing screen (#3286): offered only while the screen
/// pairs with this Mac's supervised daemon, filling the field as operator
/// input.
@Suite @MainActor struct PairingGetCodeTests {
    private struct StubSource: PairingCodeSource {
        let answer: Result<MintedPairingCode, PairingCodeFailure>

        func mint() async throws(PairingCodeFailure) -> MintedPairingCode {
            try answer.get()
        }
    }

    private nonisolated static let minted = MintedPairingCode(
        code: "7KQ4M2XP", expiresAt: Date(timeIntervalSince1970: 1_794_872_116), apiURL: prodDaemonURL)
    private static let remoteDaemonURL = URL(string: "http://100.101.102.103:7331")!

    private func pairing(
        _ session: AppSession, sourceLocation: SourceLocation = #_sourceLocation
    ) throws -> PairingModel {
        guard case .needsPairing(let model) = session.phase else {
            Issue.record("expected pairing, got \(session.phase)", sourceLocation: sourceLocation)
            throw CancellationError()
        }
        return model
    }

    private func session(
        deploymentURL: URL?, localDaemonURL: URL? = prodDaemonURL,
        answer: Result<MintedPairingCode, PairingCodeFailure> = .success(minted),
        credentials: InMemoryCredentialStore = InMemoryCredentialStore()
    ) -> AppSession {
        AppSession(
            client: APIClientFactory.mock(), credentials: credentials, cache: InMemoryCacheStore(),
            deploymentURL: deploymentURL, localDaemonURL: localDaemonURL,
            pairingCodeSource: StubSource(answer: answer))
    }

    @Test func getCodeFillsTheFieldAndAReadinessChangeKeepsIt() async throws {
        let session = session(deploymentURL: prodDaemonURL)
        let model = try pairing(session)
        #expect(model.canGetCode)

        await model.getCode()

        #expect(model.pairingCode == "7KQ4M2XP")
        #expect(model.codeRequest == .idle)
        session.applyReadiness(nil)
        #expect(model.pairingCode == "7KQ4M2XP")
        session.applyReadiness(
            DaemonReadiness(apiURL: prodDaemonURL, pairingCode: "483911", environment: .prod, runID: "test-run"))
        #expect(model.pairingCode == "7KQ4M2XP")
    }

    @Test func aFailedGetCodeLeavesTheFieldAndSaysWhy() async throws {
        let session = session(deploymentURL: prodDaemonURL, answer: .failure(.timedOut))
        let model = try pairing(session)
        model.applyPairingCodeInput("483911")

        await model.getCode()

        #expect(model.pairingCode == "483911")
        #expect(model.codeRequest == .failed(PairingCodeFailure.timedOut.message))
    }

    /// Answers once the test opens the gate.
    private struct GatedSource: PairingCodeSource {
        let gate: AsyncStream<Void>

        func mint() async throws(PairingCodeFailure) -> MintedPairingCode {
            for await _ in gate { break }
            return minted
        }
    }

    @Test func aCodeTypedWhileGetCodeRunsIsKept() async throws {
        let (gate, open) = AsyncStream.makeStream(of: Void.self)
        let model = PairingModel(
            client: APIClientFactory.mock(), credentials: InMemoryCredentialStore(),
            codeSource: GatedSource(gate: gate))
        let request = Task { await model.getCode() }
        while model.codeRequest != .requesting { await Task.yield() }

        model.applyPairingCodeInput("483911")
        open.yield()
        await request.value

        #expect(model.pairingCode == "483911")
        #expect(model.codeRequest == .idle)
    }

    @Test func onlyThePairingScreenForThisMacsDaemonOffersGetCode() throws {
        #expect(try pairing(session(deploymentURL: Self.remoteDaemonURL)).canGetCode == false)
        #expect(try pairing(session(deploymentURL: prodDaemonURL, localDaemonURL: nil)).canGetCode == false)
        // The dev daemon is another daemon to a prod app.
        let devDaemonURL = try #require(FreesideEnvironment.dev.supervisedAPIURL)
        #expect(try pairing(session(deploymentURL: devDaemonURL)).canGetCode == false)

        let switching = session(deploymentURL: Self.remoteDaemonURL)
        switching.connect(serverURL: prodDaemonURL)
        #expect(try pairing(switching).canGetCode)
        switching.connect(serverURL: Self.remoteDaemonURL)
        #expect(try pairing(switching).canGetCode == false)
    }

    @Test func rePairingThisMacOffersGetCode() throws {
        let session = session(
            deploymentURL: prodDaemonURL,
            credentials: InMemoryCredentialStore(
                credential: DeviceCredential(
                    deviceID: "device-old", token: testDeviceToken(for: "device-old"), ntfySubscription: .mock)!))
        guard case .ready = session.phase else {
            Issue.record("expected a ready session, got \(session.phase)")
            return
        }

        try session.rePair()

        #expect(try pairing(session).canGetCode)
    }

    @Test func noSourceMeansNoGetCode() async throws {
        let model = PairingModel(client: APIClientFactory.mock(), credentials: InMemoryCredentialStore())
        #expect(!model.canGetCode)

        await model.getCode()

        #expect(model.codeRequest == .idle)
        #expect(model.pairingCode.isEmpty)
    }
}
