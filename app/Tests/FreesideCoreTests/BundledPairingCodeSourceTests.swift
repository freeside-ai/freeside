import Foundation
import Testing

@testable import FreesideCore

/// The CLI runner against fixture scripts standing in for the bundled
/// `freesided`. No fixture's failure text may carry its code.
@Suite struct BundledPairingCodeSourceTests {
    private static let code = "7KQ4M2XP"
    private static let line =
        #"{"api_url":"http://127.0.0.1:7331","pairing_code":"\#(code)","expires_at":"2026-11-16T23:35:16.5Z"}"#

    /// Writes `body` as an executable shell script in a fresh directory.
    private func fixture(_ body: String) throws -> URL {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("pairing-code-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let script = directory.appendingPathComponent("freesided")
        try Data("#!/bin/sh\n\(body)\n".utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: script.path)
        return script
    }

    private func mint(
        _ executable: URL?, arguments: [String] = [], timeout: Duration = .seconds(10)
    ) async -> Result<MintedPairingCode, PairingCodeFailure> {
        let source = BundledPairingCodeSource(executableURL: executable, arguments: arguments, timeout: timeout)
        do {
            return .success(try await source.mint())
        } catch {
            return .failure(error)
        }
    }

    @Test func aCodeOnStdoutIsReturned() async throws {
        // The fixture checks it got the arguments the app passes.
        let script = try fixture(
            #"""
            [ "$1" = pairing-code ] && [ "$2" = -state-dir ] && [ "$3" = /state/daemon ] || exit 9
            echo 'diagnostic noise' >&2
            echo '\#(Self.line)'
            """#)

        let minted = try await mint(script, arguments: ["pairing-code", "-state-dir", "/state/daemon"]).get()

        #expect(minted.code == Self.code)
        #expect(minted.apiURL == URL(string: "http://127.0.0.1:7331"))
    }

    @Test func aNonZeroExitIsARefusal() async throws {
        let script = try fixture(
            #"""
            echo '\#(Self.line)'
            echo 'freesided pairing-code: read pairing control address (is this daemon running?)' >&2
            exit 1
            """#)

        let result = await mint(script)

        #expect(result == .failure(.refused(exitStatus: 1)))
        Self.expectNoCode(in: result)
    }

    @Test func aMissingExecutableIsUnavailable() async throws {
        let missing = try fixture("exit 0").deletingLastPathComponent().appendingPathComponent("absent")

        #expect(await mint(missing) == .failure(.unavailable))
        #expect(await mint(nil) == .failure(.unavailable))
    }

    @Test func aHangingCLITimesOut() async throws {
        // `exec`, so terminating the process ends the sleep and its pipe.
        let script = try fixture("exec sleep 120")

        let result = await mint(script, timeout: .milliseconds(300))

        // Only the deadline yields `.timedOut`, and it wins only by firing
        // before the CLI exits and its output arrives, so the result alone
        // proves the deadline cut short a CLI that would run 120 s. It does
        // not pin the 300 ms value. No wall-clock bound: under the parallel
        // `swift test` run one measured scheduling delay, not the timeout,
        // and reached 14 s in CI (#3314).
        #expect(result == .failure(.timedOut))
    }

    @Test func cancellingAMintStopsTheCLI() async throws {
        // A CLI left running would still mint a code nobody sees.
        let script = try fixture(#"echo $$ > "$(dirname "$0")/pid"; exec sleep 30"#)
        let pidFile = script.deletingLastPathComponent().appendingPathComponent("pid")
        let minting = Task { await mint(script, timeout: .seconds(30)) }

        var pid: pid_t?
        for _ in 0..<200 where pid == nil {
            try await Task.sleep(for: .milliseconds(10))
            pid = (try? String(contentsOf: pidFile, encoding: .utf8))
                .flatMap { pid_t($0.trimmingCharacters(in: .whitespacesAndNewlines)) }
        }
        let started = try #require(pid)
        minting.cancel()
        _ = await minting.value

        var running = true
        for _ in 0..<200 where running {
            try await Task.sleep(for: .milliseconds(10))
            running = kill(started, 0) == 0
        }
        #expect(!running)
    }

    @Test(arguments: [
        "echo 'not json \(code)'",
        "echo '{\"pairing_code\":\"\(code)\"}'",
        ":",
    ])
    func garbageOnStdoutIsUnreadable(_ body: String) async throws {
        let result = await mint(try fixture(body))

        #expect(result == .failure(.unreadableOutput))
        Self.expectNoCode(in: result)
    }

    private static func expectNoCode(in result: Result<MintedPairingCode, PairingCodeFailure>) {
        guard case .failure(let failure) = result else { return }
        #expect(!failure.message.contains(code))
        #expect(!"\(failure)".contains(code))
        #expect(!String(reflecting: failure).contains(code))
    }

    @Test func onlyASupervisedTierHasASource() throws {
        #expect(BundledPairingCodeSource(environment: .ephemeral) == nil)
        let dev = try #require(BundledPairingCodeSource(environment: .dev))
        let stateDirectory = try #require(FreesideEnvironment.dev.daemonStateDirectory())
        #expect(dev.arguments == ["pairing-code", "-state-dir", stateDirectory.path])
    }
}
