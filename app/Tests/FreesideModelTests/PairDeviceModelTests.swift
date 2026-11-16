import Foundation
import Testing

@testable import FreesideModel

/// A source that answers from a queue, one answer per `mint()`, and can
/// hold an answer until the test releases it.
private actor ScriptedPairingCodeSource: PairingCodeSource {
    private var answers: [Result<MintedPairingCode, PairingCodeFailure>]
    private var gate: CheckedContinuation<Void, Never>?
    private var holds: Bool
    private(set) var mintCount = 0

    init(_ answers: [Result<MintedPairingCode, PairingCodeFailure>], holds: Bool = false) {
        self.answers = answers
        self.holds = holds
    }

    func mint() async throws(PairingCodeFailure) -> MintedPairingCode {
        mintCount += 1
        let answer = answers.removeFirst()
        if holds {
            await withCheckedContinuation { gate = $0 }
        }
        return try answer.get()
    }

    func release() {
        holds = false
        gate?.resume()
        gate = nil
    }

    var isHolding: Bool { gate != nil }
}

private func mintedCode(_ code: String, at apiURL: String = "http://127.0.0.1:7331") -> MintedPairingCode {
    MintedPairingCode(
        code: code, expiresAt: Date(timeIntervalSince1970: 1_794_872_116), apiURL: URL(string: apiURL)!)
}

@Suite @MainActor struct PairDeviceModelTests {
    private static let phone = URL(string: "http://100.101.102.103:7331")!

    private func model(_ source: ScriptedPairingCodeSource) -> PairDeviceModel {
        PairDeviceModel(source: source, phoneAddress: { _ in .forwarded(Self.phone) })
    }

    @Test func aMintShowsTheCodeAndThePhonesAddress() async {
        let model = model(ScriptedPairingCodeSource([.success(mintedCode("AAAA1111"))]))
        #expect(model.state == .idle)

        await model.mint()

        #expect(model.state == .minted(mintedCode("AAAA1111"), .forwarded(Self.phone)))
    }

    @Test func aNewCodeReplacesTheOld() async {
        let model = model(
            ScriptedPairingCodeSource([.success(mintedCode("AAAA1111")), .success(mintedCode("BBBB2222"))]))

        await model.mint()
        await model.mint()

        #expect(model.state == .minted(mintedCode("BBBB2222"), .forwarded(Self.phone)))
    }

    @Test func aFailureShowsNoCodeAndANewCodeRetries() async {
        let model = model(
            ScriptedPairingCodeSource([.failure(.refused(exitStatus: 1)), .success(mintedCode("AAAA1111"))]))

        await model.mint()
        #expect(model.state == .failed(.refused(exitStatus: 1)))

        await model.mint()
        #expect(model.state == .minted(mintedCode("AAAA1111"), .forwarded(Self.phone)))
    }

    @Test func clearDropsTheCode() async {
        let model = model(ScriptedPairingCodeSource([.success(mintedCode("AAAA1111"))]))
        await model.mint()

        model.clear()

        #expect(model.state == .idle)
    }

    @Test func anAnswerAfterTheWindowClosedIsDropped() async {
        let source = ScriptedPairingCodeSource([.success(mintedCode("AAAA1111"))], holds: true)
        let model = model(source)

        let minting = Task { await model.mint() }
        while await !source.isHolding { await Task.yield() }
        #expect(model.state == .minting)
        model.clear()
        await source.release()
        await minting.value

        #expect(model.state == .idle)
    }
}
