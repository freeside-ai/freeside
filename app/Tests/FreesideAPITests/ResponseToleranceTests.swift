import Foundation
import FreesideAPI
import HTTPTypes
import OpenAPIRuntime
import Testing

/// Forwards to the mock and adds a field to every JSON object in each
/// response body, standing in for a newer daemon whose responses carry
/// fields this build's contract doesn't name (api/README.md, Response
/// Tolerance). The spec's only map-typed field is request-only, so every
/// object in a response is a schema object and may gain a field.
private struct NewerDaemonTransport: ClientTransport {
    static let addedField = "added_by_a_newer_daemon"
    let server: MockServer

    func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        let (response, body) = try await MockServerTransport(server: server)
            .send(request, body: body, baseURL: baseURL, operationID: operationID)
        guard let body else { return (response, nil) }
        let data = try await Data(collecting: body, upTo: 16 << 20)
        let widened = Self.widen(try JSONSerialization.jsonObject(with: data, options: .fragmentsAllowed))
        return (response, HTTPBody(try JSONSerialization.data(withJSONObject: widened, options: .fragmentsAllowed)))
    }

    static func widen(_ value: Any) -> Any {
        switch value {
        case let object as [String: Any]:
            var widened = object.mapValues(widen)
            widened[addedField] = ["nested": [1, "two"]]
            return widened
        case let array as [Any]:
            return array.map(widen)
        default:
            return value
        }
    }
}

@Suite struct ResponseToleranceTests {
    // swift-format-ignore: NeverForceUnwrap
    private let serverURL = URL(string: "https://freeside.invalid")!

    @Test func bootstrapDecodesUnchangedWhenEveryObjectGainsAField() async throws {
        let server = MockServer()
        let plain = try await Client(serverURL: serverURL, transport: MockServerTransport(server: server))
            .getSyncBootstrap().ok.body.json
        let widened = try await Client(serverURL: serverURL, transport: NewerDaemonTransport(server: server))
            .getSyncBootstrap().ok.body.json
        #expect(widened == plain)
        #expect(!plain.attention_items.isEmpty && !plain.tasks.isEmpty)
    }

    /// CommandRejection has no discriminator, so the client takes the first
    /// arm that decodes. StaleTaskRejection's required fields are a subset of
    /// TaskLineChangeRejection's, so once both tolerate added fields only the
    /// arm order keeps a refused line change from reading as a stale Stop.
    @Test func eachRejectionDecodesAsItsOwnArm() throws {
        let task = try #require(TaskFixtures.defaultTasks().first)
        let lineChange = Components.Schemas.TaskLineChangeRejection(
            message: "an attempt of the role is running", reason: .role_running,
            replacement_task: task, sync_epoch: "epoch-1")
        let staleTask = Components.Schemas.StaleTaskRejection(
            message: "stale", replacement_task: task, sync_epoch: "epoch-1")

        let decodedLineChange = try JSONDecoder().decode(
            Components.Schemas.CommandRejection.self, from: JSONEncoder().encode(lineChange))
        #expect(decodedLineChange == .TaskLineChangeRejection(lineChange))
        let decodedStaleTask = try JSONDecoder().decode(
            Components.Schemas.CommandRejection.self, from: JSONEncoder().encode(staleTask))
        #expect(decodedStaleTask == .StaleTaskRejection(staleTask))
    }
}
