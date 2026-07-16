import Foundation
import OpenAPIRuntime
import OpenAPIURLSession

public enum APIClientFactory {
    public static func live(serverURL: URL) -> Client {
        Client(serverURL: serverURL, transport: URLSessionTransport())
    }

    /// A generated client over a default-seeded in-process mock server.
    public static func mock() -> Client {
        mock(server: MockServer())
    }

    /// A generated client over the given mock server; callers hold the
    /// server to script staleness and to gate or fail responses.
    public static func mock(server: MockServer) -> Client {
        Client(
            serverURL: URL(string: "https://freeside.invalid")!,
            transport: MockServerTransport(server: server)
        )
    }
}
