import Foundation
import HTTPTypes
import OpenAPIRuntime
import OpenAPIURLSession

public enum APIClientFactory {
    public static func live(serverURL: URL) -> Client {
        Client(serverURL: serverURL, transport: URLSessionTransport())
    }

    public static func mock() -> Client {
        Client(
            serverURL: URL(string: "https://freeside.invalid")!,
            transport: MockClientTransport()
        )
    }
}

public struct MockClientTransport: ClientTransport {
    public init() {}

    public func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        switch operationID {
        case "getSyncRevision":
            let response = HTTPResponse(
                status: .ok,
                headerFields: [.contentType: "application/json"]
            )
            let json = #"{"sync_epoch":"mock-epoch","revision":1}"#
            return (response, HTTPBody(json))
        default:
            return (HTTPResponse(status: .notImplemented), nil)
        }
    }
}
