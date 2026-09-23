# Publish the Loopback Twin in Same-Host Readiness

Work unit: #1512. This revises decision 1 of
`devlog/2026-09-20-1257-same-host-loopback-listener.md` for `readiness.json`
only. Plan revision 69 says same-host clients always use loopback, and the
file's product reader is the same-host Mac app, whose parser requires a
loopback URL. The convergence script also reads the file for a loopback
foreground daemon. Publishing the primary Tailscale address there would make
a supervised daemon's file unusable to the app.

## Decision

Publish the loopback twin URL in `readiness.json` when the primary listener is
on Tailscale. A loopback-only daemon keeps its existing file value. The file
shape and pairing code remain unchanged.

Changing the stdout readiness line or the `pairing-code` response was rejected:
the phone pairs from those, so they continue to use the primary Tailscale
address. `connection_mode` also continues to describe the primary listener.

## Revisit When

The file gains a remote reader, or the app gains a hostname-paired flow.
