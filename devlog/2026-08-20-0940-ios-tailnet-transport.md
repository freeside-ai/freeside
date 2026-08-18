# iOS Tailnet Transport

Work unit: #847. Mandatory note: this changes the iOS client's transport-security posture.

## Decision

Chose a narrow App Transport Security exception for cleartext HTTP to
`100.64.0.0/10` over a global cleartext allowance or a tailnet-wide HTTPS
configuration change. The production daemon already limits non-loopback
binding to an exact address reported by the local Tailscale client, and the
device API remains credential-authenticated; the client exception makes the
documented direct tailnet deployment work while ATS continues to protect every
destination outside Tailscale's IPv4 range.

On-device verification changed the earlier assumption that an IP-literal HTTP
URL bypassed ATS on iOS 17: the system rejected `http://100.95.223.55:7331`
with `NSURLErrorDomain` code `-1022` until the CIDR exception was present.

Rejected alternatives:

- `NSAllowsArbitraryLoads`, because the on-device path needs one bounded
  network range, not unrestricted cleartext transport.
- Enabling HTTPS for the whole tailnet during this work unit, because that is
  an account-level operational change and the daemon does not yet own a
  first-class HTTPS endpoint.
- A per-operator IP exception, because the committed app must remain usable
  when Tailscale assigns the daemon host another address inside its reserved
  range.

Revisit when the daemon exposes a first-class HTTPS endpoint suitable for the
iOS client: remove the cleartext exception and require `https://` deployment
URLs.
