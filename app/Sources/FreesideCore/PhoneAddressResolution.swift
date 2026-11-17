#if os(macOS)
    import Foundation
    import FreesideAPI
    import Network

    extension PhoneAddress {
        /// The address a phone enters for the daemon at `apiURL`. A Tailscale
        /// `apiURL` is used as it is. Otherwise, the supervised daemons'
        /// loopback case, it is this Mac's Tailscale IPv4 address at the same
        /// port, which assumes Tailscale forwards that port to the daemon, as
        /// prod's `tailscale serve` does. IPv4, because the iPhone app allows
        /// cleartext only to the numeric tailnet range (app/README.md, Daemon
        /// reachability). With no Tailscale address, only this Mac can reach
        /// the daemon.
        public static func resolve(apiURL: URL, localAddresses: Set<String>) -> PhoneAddress {
            if let host = apiURL.host, SameHostLoopbackMiddleware.isTailscaleIP(host) {
                return .tailnet(apiURL)
            }
            guard
                let tailnetIPv4 = localAddresses.filter({
                    IPv4Address($0) != nil && SameHostLoopbackMiddleware.isTailscaleIP($0)
                }).min(),
                var components = URLComponents(url: apiURL, resolvingAgainstBaseURL: false)
            else { return .thisMacOnly(apiURL) }
            components.host = tailnetIPv4
            return components.url.map(PhoneAddress.forwarded) ?? .thisMacOnly(apiURL)
        }

        /// `resolve` against this Mac's interfaces as they are now.
        public static func resolve(apiURL: URL) -> PhoneAddress {
            resolve(apiURL: apiURL, localAddresses: SameHostLoopbackMiddleware.localInterfaceAddresses())
        }
    }
#endif
