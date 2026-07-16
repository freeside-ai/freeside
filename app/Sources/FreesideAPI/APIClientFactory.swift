import Foundation
import OpenAPIRuntime
import OpenAPIURLSession

public enum APIClientFactory {
    /// Shared by every client this factory builds (and the convergence
    /// harness's hand-built one): dates decode leniently across the
    /// RFC 3339 shapes the daemon and the mock emit.
    public static let configuration = Configuration(dateTranscoder: .rfc3339)

    /// The real-daemon client. Every operation except pairing requires
    /// the paired-device credential; the provider is consulted per
    /// request, so the same client works before pairing (no header) and
    /// after it.
    public static func live(
        serverURL: URL,
        token: @escaping BearerAuthMiddleware.TokenProvider = { nil }
    ) -> Client {
        Client(
            serverURL: serverURL,
            configuration: configuration,
            transport: URLSessionTransport(),
            middlewares: [BearerAuthMiddleware(token: token)]
        )
    }

    /// A generated client over a default-seeded in-process mock server.
    public static func mock() -> Client {
        mock(server: MockServer())
    }

    /// A generated client over the given mock server; callers hold the
    /// server to script staleness and to gate or fail responses. The
    /// token provider matters only against an enforcing-mode server.
    public static func mock(
        server: MockServer,
        token: @escaping BearerAuthMiddleware.TokenProvider = { nil }
    ) -> Client {
        Client(
            serverURL: URL(string: "https://freeside.invalid")!,
            configuration: configuration,
            transport: MockServerTransport(server: server),
            middlewares: [BearerAuthMiddleware(token: token)]
        )
    }
}
