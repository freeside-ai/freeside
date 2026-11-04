import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

/// The mock's `getSyncRevision` long poll follows the daemon's rule: an
/// `after` equal to the revision is held until the revision or epoch moves or
/// the bound elapses; anything else answers at once. A wrongly held request
/// would wait out the default 25-second bound, so the time limit fails it.
@Suite(.timeLimit(.minutes(1))) struct MockServerRevisionWaitTests {
    @Test func anEqualCursorIsHeldUntilTheRevisionMoves() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let start = try await client.getSyncRevision().ok.body.json

        let wait = Task { try await client.getSyncRevision(query: .init(after: start.revision)).ok.body.json }
        await server.waitForHeldRevisionRequests(1)
        await server.advance(itemID: "item-spec_approval")

        let answer = try await wait.value
        #expect(answer.revision == start.revision + 1)
        #expect(answer.sync_epoch == start.sync_epoch)
        #expect(await server.receivedRevisionAfters == [nil, start.revision])
    }

    @Test func anEqualCursorIsHeldUntilTheEpochRotates() async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let start = try await client.getSyncRevision().ok.body.json

        let wait = Task { try await client.getSyncRevision(query: .init(after: start.revision)).ok.body.json }
        await server.waitForHeldRevisionRequests(1)
        await server.rotateEpoch()

        let answer = try await wait.value
        #expect(answer.sync_epoch != start.sync_epoch)
        #expect(answer.revision == start.revision)
    }

    @Test func anEqualCursorAnswersUnchangedAtTheBound() async throws {
        let server = MockServer()
        await server.setRevisionWaitBound(.milliseconds(10))
        let client = APIClientFactory.mock(server: server)
        let start = try await client.getSyncRevision().ok.body.json

        let answer = try await client.getSyncRevision(query: .init(after: start.revision)).ok.body.json
        #expect(answer == start)
    }

    @Test(arguments: [-1, 1] as [Int64])
    func aDifferentCursorAnswersAtOnce(offset: Int64) async throws {
        let server = MockServer()
        let client = APIClientFactory.mock(server: server)
        let start = try await client.getSyncRevision().ok.body.json

        let answer = try await client.getSyncRevision(query: .init(after: start.revision + offset)).ok.body.json
        #expect(answer == start)
    }

    @Test(arguments: ["after=abc", "after=-1", "after=", "after=1&after=2"])
    func aMalformedCursorIsABadRequest(query: String) async throws {
        let transport = MockServerTransport(server: MockServer())
        let (response, _) = try await transport.send(
            HTTPRequest(method: .get, scheme: nil, authority: nil, path: "/sync/revision?\(query)"),
            body: nil,
            // swift-format-ignore: NeverForceUnwrap
            baseURL: URL(string: "https://freeside.invalid")!,
            operationID: "getSyncRevision")
        #expect(response.status == .badRequest)
    }
}
