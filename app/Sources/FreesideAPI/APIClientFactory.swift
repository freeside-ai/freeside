import Foundation
import OpenAPIRuntime
import OpenAPIURLSession

#if canImport(FoundationNetworking)
    import FoundationNetworking
#endif

public enum APIClientFactory {
    /// Shared by every client this factory builds: dates decode
    /// leniently across the RFC 3339 shapes the daemon and the mock
    /// emit.
    public static let configuration = Configuration(dateTranscoder: .rfc3339)

    /// The uncached session behind the default live transport. The
    /// daemon is sole authority and the app's disposable cache is the
    /// only sanctioned client cache (plan §5.14), so no HTTP-level
    /// cache may persist responses: the shared URLSession's URLCache
    /// would otherwise write cacheable bodies — attachment bytes the
    /// contract keeps memory-only — to the platform disk cache.
    private static func uncachedSession() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.urlCache = nil
        configuration.requestCachePolicy = .reloadIgnoringLocalCacheData
        return URLSession(configuration: configuration)
    }

    /// The real-daemon client. Every operation except pairing requires
    /// the paired-device credential; the provider is consulted per
    /// request, so the same client works before pairing (no header) and
    /// after it. The transport is injectable so a test can wrap the
    /// URLSession one (e.g. to fail requests before they leave the
    /// process) without hand-building a Client that would drift from
    /// this composition; the default rides the uncached session above.
    public static func live(
        serverURL: URL,
        transport: (any ClientTransport)? = nil,
        token: @escaping BearerAuthMiddleware.TokenProvider = { nil }
    ) -> Client {
        Client(
            serverURL: serverURL,
            configuration: configuration,
            transport: transport
                ?? URLSessionTransport(configuration: .init(session: uncachedSession())),
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
        // swift-format-ignore: NeverForceUnwrap
        Client(
            serverURL: URL(string: "https://freeside.invalid")!,
            configuration: configuration,
            transport: MockServerTransport(server: server),
            middlewares: [BearerAuthMiddleware(token: token)]
        )
    }
}
