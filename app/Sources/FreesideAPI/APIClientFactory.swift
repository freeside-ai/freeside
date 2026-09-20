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
    ///
    /// On macOS the client also carries the same-host loopback rule (#1449): a
    /// request to a Tailscale address assigned to this Mac is redirected to
    /// 127.0.0.1 at the same port, so a host VPN cannot drop the app's path to a
    /// daemon on its own machine. `localAddresses` overrides the interface read
    /// for tests; production reads the host's own interfaces.
    public static func live(
        serverURL: URL,
        transport: (any ClientTransport)? = nil,
        token: @escaping BearerAuthMiddleware.TokenProvider = { nil },
        localAddresses: (@Sendable () -> Set<String>)? = nil
    ) -> Client {
        var middlewares: [any ClientMiddleware] = [BearerAuthMiddleware(token: token)]
        #if os(macOS)
            // Outermost, so it redirects the base URL before the bearer middleware
            // and the transport see it; the bearer credential still rides every
            // request to the loopback twin, which serves the identical handler.
            middlewares.insert(
                SameHostLoopbackMiddleware(
                    addressSource: localAddresses ?? SameHostLoopbackMiddleware.localInterfaceAddresses
                ),
                at: 0
            )
        #endif
        return Client(
            serverURL: serverURL,
            configuration: configuration,
            transport: transport
                ?? URLSessionTransport(configuration: .init(session: uncachedSession())),
            middlewares: middlewares
        )
    }

    /// A generated client over a default-seeded in-process mock server.
    public static func mock() -> Client {
        mock(server: MockServer(automaticallyCompletesAgentWork: true))
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
