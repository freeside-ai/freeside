import Foundation
import FreesideAPI
import FreesideTestSupport
import HTTPTypes
import OpenAPIRuntime
import Testing

@testable import FreesideCore
@testable import FreesideModel

/// Rewrites the transcript read's JSON answer: a daemon answer the client
/// must refuse rather than show under the invocation it asked for.
private struct RewrittenTranscriptTransport: ClientTransport {
    let base: MockServerTransport
    let rewrite: @Sendable (inout [String: Any]) -> Void

    func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        let (response, responseBody) = try await base.send(
            request, body: body, baseURL: baseURL, operationID: operationID)
        guard operationID == "getInvocationTranscript", let responseBody else { return (response, responseBody) }
        let data = try await Data(collecting: responseBody, upTo: 1 << 20)
        guard var object = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            return (response, HTTPBody(data))
        }
        rewrite(&object)
        return (response, HTTPBody(try JSONSerialization.data(withJSONObject: object)))
    }
}

@Suite @MainActor struct InvocationTranscriptReadTests {
    private let server = MockServer(
        timelines: RunFixtures.defaultTimelines() + [RunFixtures.refreshedHistoryTimeline()])

    private func coordinator(
        rewrite: (@Sendable (inout [String: Any]) -> Void)? = nil, status: Int? = nil
    ) -> SyncCoordinator {
        let base = MockServerTransport(server: server)
        var transport: any ClientTransport = base
        if let rewrite { transport = RewrittenTranscriptTransport(base: base, rewrite: rewrite) }
        if let status {
            transport = StatusOverrideTransport(
                base: base, operationID: "getInvocationTranscript", status: status, once: OneShot())
        }
        // swift-format-ignore: NeverForceUnwrap
        let client = Client(
            serverURL: URL(string: "https://freeside.invalid")!, configuration: APIClientFactory.configuration,
            transport: transport)
        return SyncCoordinator(client: client, cache: InMemoryCacheStore())
    }

    private static func producer(_ transcript: Components.Schemas.InvocationTranscript) -> String? {
        switch transcript.transcript?.value1.provenance {
        case .head_bound(let provenance): provenance.producer_invocation_id
        case .head_independent(let provenance): provenance.producer_invocation_id
        case nil: nil
        }
    }

    /// A completed writer's stream and a failed writer's retained stdout
    /// both come back with the prompt, bound to the invocation asked for.
    @Test func readsAnEndedWritersTranscriptAndPrompt() async throws {
        let coordinator = coordinator()
        let completed = "inv-\(RunFixtures.completedRunID)-1"
        let finished = try await coordinator.invocationTranscript(
            runID: RunFixtures.completedRunID, invocationID: completed)
        #expect(finished.availability == .available)
        #expect(finished.transcript?.value1.digest == RunFixtures.finishedTranscriptDigest)
        #expect(finished.transcript?.value1.metadata.media_type == .application_sol_jsonl)
        #expect(Self.producer(finished) == completed)
        #expect(finished.prompt == RunFixtures.writerPrompt)

        let failedID = "inv-\(RunFixtures.refreshedRunID)-1"
        let failed = try await coordinator.invocationTranscript(
            runID: RunFixtures.refreshedRunID, invocationID: failedID)
        #expect(failed.availability == .available)
        #expect(failed.transcript?.value1.metadata.media_type == .text_sol_plain)
        #expect(Self.producer(failed) == failedID)
    }

    /// A live writer has nothing kept yet: the read answers unavailable with
    /// no claim and no prompt.
    @Test func aLiveWriterReadsAsUnavailable() async throws {
        let live = try await coordinator().invocationTranscript(
            runID: RunFixtures.activeRunID, invocationID: "inv-\(RunFixtures.activeRunID)-2")
        #expect(live.availability == .unavailable)
        #expect(live.transcript == nil)
        #expect(live.prompt == nil)
    }

    /// The daemon's 404, for an unknown invocation, another run's, or a
    /// reviewer's, reads as unavailable.
    @Test func notFoundReadsAsUnavailable() async throws {
        let coordinator = coordinator()
        let reviewer = try #require(
            RunFixtures.refreshedHistoryTimeline().review?.value1.rounds.first?.invocation_id)
        for (runID, invocationID) in [
            (RunFixtures.completedRunID, "inv-missing"),
            (RunFixtures.completedRunID, "inv-\(RunFixtures.refreshedRunID)-1"),
            (RunFixtures.refreshedRunID, reviewer),
        ] {
            await #expect(throws: SyncCoordinator.InvocationTranscriptReadError.unavailable) {
                try await coordinator.invocationTranscript(runID: runID, invocationID: invocationID)
            }
        }
    }

    /// Any other status, such as the daemon's 500 for a claim set that
    /// disagrees with its invocation, is a failed read, not a missing
    /// transcript.
    @Test func anUndocumentedStatusIsAFailedRead() async {
        await #expect(throws: SyncCoordinator.InvocationTranscriptReadError.failed) {
            try await coordinator(status: 500).invocationTranscript(
                runID: RunFixtures.completedRunID, invocationID: "inv-\(RunFixtures.completedRunID)-1")
        }
    }

    /// An answer naming another run or invocation, or carrying another
    /// invocation's claim or a claim that isn't a transcript, is refused.
    @Test func refusesAnAnswerBoundElsewhere() async throws {
        let rewrites: [@Sendable (inout [String: Any]) -> Void] = [
            { $0["run_id"] = "run-elsewhere" },
            { $0["invocation_id"] = "inv-elsewhere" },
            { object in
                var claim = object["transcript"] as? [String: Any] ?? [:]
                var provenance = claim["provenance"] as? [String: Any] ?? [:]
                provenance["producer_invocation_id"] = "inv-elsewhere"
                claim["provenance"] = provenance
                object["transcript"] = claim
            },
            { object in
                var claim = object["transcript"] as? [String: Any] ?? [:]
                claim["label"] = "freeside.summary"
                object["transcript"] = claim
            },
        ]
        for rewrite in rewrites {
            await #expect(throws: SyncCoordinator.InvocationTranscriptReadError.bindingMismatch) {
                try await coordinator(rewrite: rewrite).invocationTranscript(
                    runID: RunFixtures.completedRunID, invocationID: "inv-\(RunFixtures.completedRunID)-1")
            }
        }
    }
}
