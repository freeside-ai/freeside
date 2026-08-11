# Bundle the Supervised Daemon for `SMAppService`

Issue #509 originally proposed registering a bundle-shipped LaunchAgent whose
`Program` was templated to an arbitrary external `freesided` path. Chose instead
to copy that executable into `Contents/Resources`, seal it with the app, and use
the bundle-relative `BundleProgram` key. Apple's `SMAppService` contract controls
helpers inside the main app bundle, and its SDK guidance says an updated helper
or plist must be re-registered. Keeping the external path would make successful
registration and update behavior platform-dependent while leaving the daemon
outside the app's verified signature.

The installer therefore invalidates a per-app registration-generation marker
before fallible work and again after stopping the old app, immediately before
replacement. The next app launch unregisters and re-registers an enabled old
generation once; ordinary launches do not cycle the service. The daemon state
directory remains fixed at the app reader's Application Support path rather
than offering an installer-only override that would strand the readiness
pairing code.

The refute-first pass also confirmed that the readiness file needs exact-field,
loopback-only parsing but no additional client-side owner or mode gate: daemon
publication already uses mode `0600` beneath the accepted same-user state
directory. It rejected persisted-server precedence as a change here because the
approved #509 startup contract deliberately places readable local readiness
ahead of the older persisted fallback.

The readiness shape also has no explicit expiry even though its one-shot code
expires after ten minutes. The app therefore treats the atomic file
publication time as the code's freshness bound and clears an untouched
suggestion when that bound passes. This keeps the fix inside #509's exact
readiness contract; adding an expiry field or daemon-side rotation would be a
separate cross-component contract change.

The Phase 1 menu reports the supervision contract's two available facts:

- whether the LaunchAgent is registered and enabled; and
- whether a daemon answers the exact unauthenticated health contract on the
  fixed supervised listener.

It does not claim process-generation identity. Neither the exact health shape
nor readiness carries an instance identifier, and `SMAppService` exposes no
supported process identity to compare. Inferring that identity from a healthy
port would create provenance the contract does not supply.

Revisit when Apple supports registering an external executable with the same
relocation, signature, approval, and update guarantees as a bundled
`SMAppService` helper, or when the health/readiness contract gains an instance
identity that the app can verify against its registered service. Also revisit
the publication-time freshness proxy if readiness gains an explicit expiry or
the daemon rotates published pairing codes.
