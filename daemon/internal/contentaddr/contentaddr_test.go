package contentaddr_test

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// hex64 is a representative valid lowercase payload, exercised as the accepted
// canonical form and as the base the reject cases mutate away from.
const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestParse pins the accepted and rejected input sets across every axis the
// four former call sites cared about: algorithm prefix, exact length, case,
// non-hex bytes, whitespace, and empty input. Parse must return the exact
// prefix-stripped payload on accept and ("", false) on every rejection.
func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantHex string
		wantOK  bool
	}{
		// Accepted: the canonical form only.
		{"all zero", "sha256:" + strings.Repeat("0", 64), strings.Repeat("0", 64), true},
		{"all f", "sha256:" + strings.Repeat("f", 64), strings.Repeat("f", 64), true},
		{"mixed hex", "sha256:" + hex64, hex64, true},

		// Prefix.
		{"empty", "", "", false},
		{"no prefix", hex64, "", false},
		{"wrong algorithm", "sha512:" + hex64, "", false},
		{"uppercase prefix", "SHA256:" + hex64, "", false},
		{"missing colon", "sha256" + hex64, "", false},
		{"prefix only", "sha256:", "", false},

		// Length.
		{"one short", "sha256:" + hex64[:63], "", false},
		{"one long", "sha256:" + hex64 + "0", "", false},

		// Case: no folding.
		{"uppercase hex", "sha256:" + strings.ToUpper(hex64), "", false},
		{"mixed case hex", "sha256:" + "A" + hex64[1:], "", false},

		// Non-hex bytes at length 64.
		{"non-hex letter g", "sha256:" + strings.Repeat("0", 63) + "g", "", false},
		{"non-hex letter z", "sha256:" + strings.Repeat("z", 64), "", false},
		{"punctuation in payload", "sha256:" + strings.Repeat("0", 63) + "-", "", false},
		{"non-ascii in payload", "sha256:" + strings.Repeat("0", 62) + "é", "", false},

		// Whitespace: never tolerated.
		{"leading space", " sha256:" + hex64, "", false},
		{"trailing space", "sha256:" + hex64 + " ", "", false},
		{"embedded space", "sha256:" + strings.Repeat("0", 32) + " " + strings.Repeat("0", 31), "", false},
		{"embedded tab", "sha256:" + strings.Repeat("0", 32) + "\t" + strings.Repeat("0", 31), "", false},
		{"leading newline", "\nsha256:" + hex64, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHex, gotOK := contentaddr.Parse(tt.in)
			if gotOK != tt.wantOK {
				t.Fatalf("Parse(%q) ok = %v, want %v", tt.in, gotOK, tt.wantOK)
			}
			if gotHex != tt.wantHex {
				t.Fatalf("Parse(%q) hex = %q, want %q", tt.in, gotHex, tt.wantHex)
			}
			if got := contentaddr.Valid(tt.in); got != tt.wantOK {
				t.Fatalf("Valid(%q) = %v, want %v (must match Parse)", tt.in, got, tt.wantOK)
			}
		})
	}
}

// FuzzParse asserts the parser's invariants on arbitrary input: Valid always
// agrees with Parse; an accepted address is exactly "sha256:" + a 64-char
// lowercase-hex payload; and the returned payload round-trips back through
// Parse unchanged. This is the repo's first fuzz test; its seed corpus runs
// under an ordinary `go test`, with `-fuzz` available for extended runs.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"", "sha256:", hex64, "sha256:" + hex64,
		"sha256:" + strings.ToUpper(hex64), "sha512:" + hex64,
		"sha256:" + hex64 + " ", " sha256:" + hex64,
		"sha256:" + hex64[:63], "sha256:" + hex64 + "0",
		"sha256:" + strings.Repeat("g", 64),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		hexDigits, ok := contentaddr.Parse(raw)
		if ok != contentaddr.Valid(raw) {
			t.Fatalf("Parse ok = %v but Valid = %v for %q", ok, contentaddr.Valid(raw), raw)
		}
		if !ok {
			if hexDigits != "" {
				t.Fatalf("rejected input %q returned non-empty payload %q", raw, hexDigits)
			}
			return
		}
		if len(hexDigits) != 64 {
			t.Fatalf("accepted %q returned %d-char payload, want 64", raw, len(hexDigits))
		}
		if raw != "sha256:"+hexDigits {
			t.Fatalf("accepted %q but prefix+payload = %q", raw, "sha256:"+hexDigits)
		}
		for _, c := range hexDigits {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("accepted %q payload has non-lowercase-hex byte %q", raw, c)
			}
		}
		if got, ok2 := contentaddr.Parse("sha256:" + hexDigits); !ok2 || got != hexDigits {
			t.Fatalf("payload %q did not round-trip: Parse = (%q, %v)", hexDigits, got, ok2)
		}
	})
}
