package signet

import (
	"fmt"
	"io"
)

// redactedSecret is the only rendering a Secret ever produces outside Reveal.
const redactedSecret = "[REDACTED]"

// Secret is a credential value (the ntfy access token) that must never
// appear in logs, errors, or artifacts. It mirrors the publish package's
// Secret rather than importing it: the redaction discipline is shared, the
// lane territory is not, and a cross-lane import for a 40-line type would
// couple signet releases to publish. Every implicit rendering — fmt verbs
// via Formatter, String, GoString, and JSON/text marshalling via
// MarshalText — yields "[REDACTED]"; Reveal is the single deliberate way
// out, so a leak requires a visible Reveal call at the use site.
type Secret string

// Reveal returns the underlying credential value. It is the only
// accessor; call it exactly where the value crosses to its intended
// consumer (an Authorization header), never to log.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer; it never returns the value.
func (Secret) String() string { return redactedSecret }

// GoString implements fmt.GoStringer, so %#v cannot leak the value.
func (Secret) GoString() string { return redactedSecret }

// Format implements fmt.Formatter for every verb (%s, %q, %v, %+v, %x,
// %d, ...), since a non-string verb like %x would otherwise bypass
// String and hex-dump the underlying bytes.
func (Secret) Format(f fmt.State, _ rune) {
	io.WriteString(f, redactedSecret) //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

// MarshalText implements encoding.TextMarshaler, which encoding/json
// also uses, so a Secret field marshals as "[REDACTED]" rather than its
// value.
func (Secret) MarshalText() ([]byte, error) { return []byte(redactedSecret), nil }

// UnmarshalText implements encoding.TextUnmarshaler so configuration
// sources decode credential fields straight into Secret, never through a
// plain string.
func (s *Secret) UnmarshalText(b []byte) error {
	*s = Secret(b)
	return nil
}
