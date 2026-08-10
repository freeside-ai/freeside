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
	"encoding/hex"
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

// Format returns the canonical content address for a sha256 sum. It panics
// when sum is not exactly sha256.Size bytes because a non-sha256 sum is a
// programmer error and Format must never produce an address Parse rejects.
func Format(sum []byte) string {
	if len(sum) != sha256.Size {
		panic("contentaddr: sha256 sum must be exactly 32 bytes")
	}
	return prefix + hex.EncodeToString(sum)
}

// Sum returns the canonical content address for the sha256 sum of data.
func Sum(data []byte) string {
	sum := sha256.Sum256(data)
	return Format(sum[:])
}

// Hex returns the 64-character lowercase hex payload of a canonical content
// address. It returns the empty string when addr is not canonical.
func Hex(addr string) string {
	hexDigits, _ := Parse(addr)
	return hexDigits
}

// FromHex returns the canonical content address for a 64-character lowercase
// hex payload. It returns ("", false) when hexDigits is not canonical.
func FromHex(hexDigits string) (addr string, ok bool) {
	addr = prefix + hexDigits
	if !Valid(addr) {
		return "", false
	}
	return addr, true
}
