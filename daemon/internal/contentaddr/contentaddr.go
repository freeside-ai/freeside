// Package contentaddr is the daemon's strict parser for the repo's canonical
// content-address form: exactly "sha256:" followed by 64 lowercase hex digits.
// It is a neutral leaf, depending only on the standard library, so every lane
// that gates a filesystem path, a credential field, or an untrusted returned
// object on that form shares one decision instead of maintaining its own copy.
//
// Usage:
//
//	if hex, ok := contentaddr.Parse(raw); ok {
//		// hex is the 64-char lowercase payload, prefix stripped
//	}
//
//	if !contentaddr.Valid(raw) {
//		// raw is not a canonical sha256 content address
//	}
//
// The accepted set is deliberately narrow: no case folding, no whitespace
// tolerance, no alternate algorithms, and the empty string is rejected. Each
// caller keeps its own named type, sentinel error, error context, and any
// package-specific policy around the call; this package decides only the
// string shape. It imports nothing from domain, export, signet, or publish.
package contentaddr

import (
	"crypto/sha256"
	"strings"
)

// prefix is the only algorithm this package recognizes; hexLen is the exact
// number of lowercase hex digits a sha256 digest carries (derived from the
// hash size so it can never drift from the algorithm).
const (
	prefix = "sha256:"
	hexLen = sha256.Size * 2 // 64
)

// Parse reports whether raw is a canonical sha256 content address and, when it
// is, returns the 64-character lowercase hex payload with the "sha256:" prefix
// stripped. On any deviation it returns ("", false).
func Parse(raw string) (hexDigits string, ok bool) {
	s, ok := strings.CutPrefix(raw, prefix)
	if !ok || len(s) != hexLen {
		return "", false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return s, true
}

// Valid reports whether raw is a canonical sha256 content address. It is Parse
// with the payload discarded, for callers that only need the yes/no decision.
func Valid(raw string) bool {
	_, ok := Parse(raw)
	return ok
}
