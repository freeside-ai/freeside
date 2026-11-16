# The Mac App Gets a Pairing Code by Running Its Bundled freesided

#3286 lets the operator get a pairing code from the Mac app, through
**Pair a Device…** and the pairing screen's **Get Code**. This note records
how the app asks the daemon for one.

## Decisions

- **Run the bundled CLI, not a native socket client.** The app runs
  `freesided pairing-code -state-dir <tier daemon state dir>` from its own
  bundle and parses the one JSON line it prints. A native client would need
  a hand-written HTTP/1.1 exchange over `AF_UNIX`, the server peer-UID check,
  the advertisement read, and the canonical state-directory match the socket
  requires: copies of Go code that already exists and is tested. The bundled
  `freesided` is the same file the tier's LaunchAgent runs, so the client
  can't drift from the daemon. No daemon code changed, which kept the unit
  Tier B: the gate that grants client authority stays in the daemon, and the
  app only shows a code the daemon minted.
- **The CLI's stdout is now an interface the app reads.** It is one object
  with exactly `api_url`, `pairing_code`, and `expires_at` (RFC 3339), and a
  non-zero exit on failure. The app parses it strictly
  (`MintedPairingCode.parse`), so a changed shape shows as unreadable output
  rather than a code with a wrong address or expiry. Changing that output
  now means changing the app's parser in the same unit.
- **The code never leaves memory.** Stderr is discarded unread, each failure
  carries fixed text, and `MintedPairingCode`'s description leaves the code
  out, so nothing logs, stores, or reports a code.
- **The phone's address assumes Tailscale forwards the same port.** A
  supervised daemon reports a loopback address. The window swaps in this
  Mac's Tailscale IPv4 and keeps the port, as prod's tailnet forwards today,
  and says so under the address.

The plan for #3286 made these choices and the owner handed it to
implementation unchanged.

Revisit when the daemon serves codes over a transport the app already
speaks, or when an Xcode-built app (which bundles no `freesided`) needs to
pair a phone.
