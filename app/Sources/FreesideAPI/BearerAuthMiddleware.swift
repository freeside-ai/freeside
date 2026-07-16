import Foundation
import HTTPTypes
import OpenAPIRuntime

/// Injects the paired-device bearer credential (plan §5.14). The token
/// provider is consulted per request, so one client spans pairing: it
/// sends no Authorization header while the provider has nothing — which
/// is what the unauthenticated pairing exchange needs — and carries the
/// credential on every request after pairing mints it. The middleware
/// holds no token itself; custody stays with the provider's backing
/// store (Keychain in the apps).
public struct BearerAuthMiddleware: ClientMiddleware {
    public typealias TokenProvider = @Sendable () async -> String?

    private let token: TokenProvider

    public init(token: @escaping TokenProvider) {
        self.token = token
    }

    public func intercept(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String,
        next: @Sendable (HTTPRequest, HTTPBody?, URL) async throws -> (HTTPResponse, HTTPBody?)
    ) async throws -> (HTTPResponse, HTTPBody?) {
        var request = request
        if let token = await token() {
            request.headerFields[.authorization] = "Bearer \(token)"
        }
        return try await next(request, body, baseURL)
    }
}
