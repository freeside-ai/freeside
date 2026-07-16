package publish

import (
	"fmt"
	"io"
)

// redacted is the only rendering a Secret ever produces outside Reveal.
const redacted = "[REDACTED]"

// Secret is a credential value (private key PEM, installation token,
// webhook or client secret) that must never appear in logs, errors, or
// artifacts (issue #80 acceptance 4; docs/plan.md §5.10). Every implicit
// rendering — fmt verbs via Formatter, String, GoString, and JSON/text
// marshalling via MarshalText — yields "[REDACTED]"; Reveal is the single
// deliberate way out, so a leak requires a visible Reveal call at the use
// site. UnmarshalText accepts the real value, so API responses decode
// directly into Secret fields without an intermediate plain string.
type Secret string

// Reveal returns the underlying credential value. It is the only
// accessor; call it exactly where the value crosses to its intended
// consumer (an Authorization header, a key file write), never to log.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer; it never returns the value.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, so %#v cannot leak the value.
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter for every verb (%s, %q, %v, %+v, %x,
// %d, ...), since a non-string verb like %x would otherwise bypass
// String and hex-dump the underlying bytes.
func (Secret) Format(f fmt.State, _ rune) {
	io.WriteString(f, redacted) //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

// MarshalText implements encoding.TextMarshaler, which encoding/json
// also uses, so a Secret field marshals as "[REDACTED]" rather than its
// value. Persisting a real credential is therefore always an explicit
// Reveal at the persistence boundary (see Keystore.SaveApp).
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// UnmarshalText implements encoding.TextUnmarshaler so JSON API
// responses decode credential fields straight into Secret, never
// through a plain string.
func (s *Secret) UnmarshalText(b []byte) error {
	*s = Secret(b)
	return nil
}
