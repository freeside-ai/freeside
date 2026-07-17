# Device-scoped ntfy subscription in the pairing grant (issue #131)

The pairing grant now requires an `ntfy_subscription` containing the
configured server base URL and the new device's private topic. Chose an
ntfy-specific object over a generic notification union because Phase 1 has
one channel and APNs is explicitly out of scope; a premature union would
claim common semantics the channels do not yet share.

The subscription appears only in the one-time pairing grant, never in
`Device` or a sync resource. This keeps capability topics non-enumerable
across authenticated devices while letting the code holder receive the one
topic it just authorized. Pairing fails before code consumption when ntfy is
absent or invalid; issuing a bearer credential without a usable notification
subscription would make the successful contract response dishonest.

The grant carries the normalized ntfy base URL and topic, but no ntfy access
token. The existing token is daemon publisher authority, not a device-scoped
subscriber credential, so disclosing it would widen the trust boundary.
Authenticated ntfy deployments need a separate least-authority subscriber
credential design rather than reusing that secret.

The daemon computes the grant through the same `ntfyChannel.topic` path used
by delivery publication, preserving #69's derivation exactly. The app's live
convergence harness supplies a non-routable loopback ntfy sink because pairing
now correctly requires channel composition; its convergence suite does not
publish notifications.

## Refute-first verification

The fresh-context credential-leak and returned-object lens confirmed four
configuration classes that could make the grant unsafe or dishonest: URL
userinfo could disclose a publisher credential, remote cleartext HTTP could
expose the capability topic, query/fragment bases could publish somewhere
other than the granted address, and low-entropy HMAC keys could let one paired
device brute-force topics for other device IDs. The boundary now rejects all
four before pairing consumes its code; the topic key minimum is 32 bytes.

The lens also confirmed that the development harness generates a new topic key
on each process start. That composition is explicitly ephemeral, but reusing
its SQLite store across restarts would leave an earlier grant stale. Accepted
as outside this contract unit's stated rotation/re-keying non-goal and filed as
follow-up work rather than inventing key persistence here.

Follow-up: #133

## Automated review

Codex found that the first implementation decoded the required one-time
subscription in the generated client but let `PairingModel` discard it after
saving only the bearer token. Confirmed and fixed by making the validated ntfy
subscription part of `DeviceCredential` and storing the complete private grant
as one versioned Keychain record. A token-only legacy Keychain value now fails
loud and requires re-pairing: the missing topic cannot be reconstructed safely,
and treating that device as fully paired would preserve the original defect.
The disposable cache remains intentionally excluded from this credential
surface.

A second refute pass over that custody change confirmed three adjacent trust
boundary gaps: the returned token was not bound to the returned device ID,
Swift's permissive integer parser accepted non-canonical loopback spellings
the daemon rejects, and the disk-leak regression did not retain the new topic
while exercising persistence. `DeviceCredential` now validates the `fsd1`
envelope and requires its canonical base64url device segment to match the
snapshot, loopback IPv4 accepts ASCII decimal octets only, and the regression
asserts that neither plaintext nor base64 topic material enters the cache.
The re-check then found that the secret segment gate accepted truncated
base64url values. The gate now requires the contract's canonical encoding of
exactly 32 bytes, and every mock/preview credential uses that production shape.
The post-push review also found a remaining URL-parser mismatch: Swift treated
leading-zero IPv4 octets as decimal while the daemon rejects them and system
resolvers may reinterpret them. Cleartext loopback grants now require canonical
decimal octets, including the no-leading-zero rule.
A refute re-check caught the inverse mismatch before that fix was pushed:
expanded and IPv4-mapped IPv6 loopback literals accepted by Go would have
failed the app's textual `::1` check after pairing. The app now parses IPv6
semantically, recognizes mapped 127/8 addresses, and still applies the
canonical-octet rule when the mapped form uses dotted decimal.
That semantic parser is more permissive than Go for scoped IPv6 literals: it
can discard an unknown zone identifier and retain `::1`. The app explicitly
rejects `%` zones so unusable daemon-impossible grants cannot become durable.
The final automated pass found that URL parsing alone accepts explicit ports
outside the dialable TCP range. Both daemon configuration validation and app
grant validation now require explicit ports to be integers in 1-65535, so
pairing cannot consume a code for an undialable subscription.
The port refute pass found that Foundation decodes an encoded colon into an
unbracketed host without populating `URLComponents.port`. Unbracketed decoded
hosts containing `:` are now rejected, while bracketed IPv6 remains valid.
The final authority sweep generalized that finding: Foundation also decodes
other ASCII delimiters in an ordinary host, while Go rejects any percent-
encoded ASCII there. Unbracketed authorities now reject that whole class but
continue to accept percent-encoded UTF-8 hostnames; bracketed IPv6 keeps its
separate zone-aware grammar.
The bracketed-authority sweep then found that Foundation materializes bracket
contents Go rejects. Bracketed hosts now require a semantic IPv6 address and
permit a non-empty zone only behind Go's `%25` marker; malformed literals and
encoded address delimiters fail closed.
The zone sweep completed that grammar: zone escapes must be well formed and
may decode only to `%`, space, or a host-safe byte, matching Go's `encodeZone`
rules. Newline, slash, malformed escapes, and other unsafe bytes are rejected.
The closing refute pass found that a valid numeric port returned before those
authority checks. Port range and host grammar now run as one gate, and the
explicit-port variants of malformed hosts, literals, and zones are covered.

Revisit when: topic rotation or authenticated subscriber credentials are
designed, or when APNs joins in Phase 2 and supplies evidence for a shared
notification-subscription abstraction.
