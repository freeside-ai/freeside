import Foundation

/// The address a phone enters to reach the daemon a code was minted on.
/// The supervised daemons listen on loopback, which a phone can't use, so
/// the Mac app derives the phone's address from this Mac's Tailscale one
/// (`PhoneAddress.resolve` in FreesideCore).
public enum PhoneAddress: Equatable, Sendable {
    /// The daemon's own address is already a Tailscale one.
    case tailnet(URL)
    /// This Mac's Tailscale IPv4 address at the daemon's port. It is right
    /// only when Tailscale forwards that port to the daemon, as prod's does.
    case forwarded(URL)
    /// This Mac has no Tailscale address: the daemon's own address, which
    /// only this Mac can reach.
    case thisMacOnly(URL)

    public var url: URL {
        switch self {
        case .tailnet(let url), .forwarded(let url), .thisMacOnly(let url): url
        }
    }
}
