#if os(macOS)
    import Darwin
    import Foundation
    import HTTPTypes
    import Network
    import OpenAPIRuntime

    /// Routes same-host requests to loopback (#1449). A daemon bound to a
    /// Tailscale-owned address also serves the identical API on 127.0.0.1 at the
    /// same port, and a host VPN (Mullvad) can drop this Mac's traffic to its own
    /// Tailscale address. So when the paired server address is a Tailscale IP
    /// assigned to this Mac, the app dials 127.0.0.1 at the same port instead.
    ///
    /// The paired URL stays the app's identity for the daemon (Keychain item,
    /// cache, deployment key), because only the request's destination changes,
    /// not `AppSession`'s keying. The rule is checked per request. Once it has
    /// chosen loopback it keeps loopback for the rest of the process, even if
    /// Tailscale later drops the address, so a mid-session VPN change cannot strand
    /// the app on the filtered path.
    ///
    /// macOS only: the phone is never on the daemon's host, and `FreesideAPI` also
    /// builds on Linux where the rule does not apply.
    public struct SameHostLoopbackMiddleware: ClientMiddleware {
        /// The set of IP-literal addresses currently assigned to this host.
        /// Injected so tests do not depend on the machine's interfaces.
        public typealias AddressSource = @Sendable () -> Set<String>

        private let addressSource: AddressSource
        private let decision = LoopbackDecision()

        public init(addressSource: @escaping AddressSource = SameHostLoopbackMiddleware.localInterfaceAddresses) {
            self.addressSource = addressSource
        }

        public func intercept(
            _ request: HTTPRequest,
            body: HTTPBody?,
            baseURL: URL,
            operationID: String,
            next: @Sendable (HTTPRequest, HTTPBody?, URL) async throws -> (HTTPResponse, HTTPBody?)
        ) async throws -> (HTTPResponse, HTTPBody?) {
            try await next(request, body, destination(for: baseURL))
        }

        private func destination(for baseURL: URL) -> URL {
            if let loopback = Self.loopbackURL(for: baseURL, localAddresses: addressSource()) {
                decision.markLoopback()
                return loopback
            }
            // Once loopback has been chosen, keep it even after the address is gone.
            if decision.isLoopback, let loopback = Self.loopbackURLIgnoringLocality(for: baseURL) {
                return loopback
            }
            return baseURL
        }

        /// The loopback URL a same-host client should dial for `baseURL`, or nil
        /// when the request must go to `baseURL` unchanged. It returns loopback
        /// only when every condition holds: the scheme is `http`; the host is an IP
        /// literal; the IP is in Tailscale's ranges (100.64.0.0/10 or
        /// fd7a:115c:a1e0::/48, the ranges the daemon's `isTailscaleAddr` uses); and
        /// the IP is assigned to this host. The four conditions are the guard that
        /// keeps a Mac paired to a remote daemon from ever sending its credential
        /// to its own loopback; do not loosen them (for example to cover
        /// hostnames).
        public static func loopbackURL(for baseURL: URL, localAddresses: Set<String>) -> URL? {
            guard let host = baseURL.host,
                let key = ipKey(host),
                isTailscaleIP(host),
                localAddresses.contains(where: { ipKey($0) == key })
            else { return nil }
            return loopbackURLIgnoringLocality(for: baseURL)
        }

        /// Rewrites `baseURL` to 127.0.0.1 at the same port and path when it is an
        /// `http` Tailscale IP literal, without the locality check. Used only after
        /// `loopbackURL` has already chosen loopback for this process.
        static func loopbackURLIgnoringLocality(for baseURL: URL) -> URL? {
            guard baseURL.scheme?.lowercased() == "http",
                let host = baseURL.host, isTailscaleIP(host),
                var components = URLComponents(url: baseURL, resolvingAgainstBaseURL: false)
            else { return nil }
            components.scheme = "http"
            components.host = "127.0.0.1"
            return components.url
        }

        /// A canonical comparison key for an IP literal (address family plus raw
        /// bytes), or nil when `value` is not an IP literal. Comparing keys rather
        /// than strings makes locality matching independent of textual form.
        static func ipKey(_ value: String) -> String? {
            if let v4 = IPv4Address(value) {
                return "4:" + [UInt8](v4.rawValue).map(String.init).joined(separator: ".")
            }
            if let v6 = IPv6Address(value) {
                return "6:" + [UInt8](v6.rawValue).map { String(format: "%02x", $0) }.joined()
            }
            return nil
        }

        /// Whether `value` is an IP literal inside Tailscale's ranges, mirroring the
        /// daemon's `isTailscaleAddr` bit test rather than a string prefix.
        static func isTailscaleIP(_ value: String) -> Bool {
            if let v4 = IPv4Address(value) {
                let b = [UInt8](v4.rawValue)
                return b.count == 4 && b[0] == 100 && (64...127).contains(b[1])
            }
            if let v6 = IPv6Address(value) {
                let b = [UInt8](v6.rawValue)
                return b.count == 16
                    && b[0] == 0xfd && b[1] == 0x7a && b[2] == 0x11
                    && b[3] == 0x5c && b[4] == 0xa1 && b[5] == 0xe0
            }
            return false
        }

        /// The IP-literal addresses assigned to this host's interfaces, read with
        /// `getifaddrs`. Loopback and link-local forms are irrelevant to the rule
        /// but harmless in the set, since the caller only matches Tailscale IPs.
        public static func localInterfaceAddresses() -> Set<String> {
            var addresses: Set<String> = []
            var head: UnsafeMutablePointer<ifaddrs>?
            guard getifaddrs(&head) == 0, let first = head else { return addresses }
            defer { freeifaddrs(head) }
            for pointer in sequence(first: first, next: { $0.pointee.ifa_next }) {
                guard let sockaddr = pointer.pointee.ifa_addr else { continue }
                let family = sockaddr.pointee.sa_family
                guard family == UInt8(AF_INET) || family == UInt8(AF_INET6) else { continue }
                var buffer = [CChar](repeating: 0, count: Int(NI_MAXHOST))
                let result = getnameinfo(
                    sockaddr, socklen_t(sockaddr.pointee.sa_len),
                    &buffer, socklen_t(buffer.count),
                    nil, 0, NI_NUMERICHOST
                )
                guard result == 0 else { continue }
                var host = buffer.withUnsafeBufferPointer { pointer in
                    pointer.baseAddress.map { String(cString: $0) } ?? ""
                }
                // Drop an IPv6 zone id (e.g. fe80::1%en0); Tailscale addresses carry
                // none, and the zone would break IP-key comparison.
                if let percent = host.firstIndex(of: "%") {
                    host = String(host[..<percent])
                }
                addresses.insert(host)
            }
            return addresses
        }
    }

    /// A process-lifetime latch: once the middleware routes to loopback it stays
    /// there. Guarded for the concurrent requests one client issues.
    private final class LoopbackDecision: @unchecked Sendable {
        private let lock = NSLock()
        private var loopback = false

        var isLoopback: Bool {
            lock.lock()
            defer { lock.unlock() }
            return loopback
        }

        func markLoopback() {
            lock.lock()
            loopback = true
            lock.unlock()
        }
    }
#endif
