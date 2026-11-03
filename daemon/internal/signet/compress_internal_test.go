package signet

import "testing"

// TestAcceptsGzip pins the Accept-Encoding reading behind compressJSON: a
// gzip entry decides, then a * entry, and q=0 or an unparseable q refuses.
func TestAcceptsGzip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   bool
	}{
		{"no header", nil, false},
		{"empty header", []string{""}, false},
		{"identity", []string{"identity"}, false},
		{"gzip", []string{"gzip"}, true},
		{"case-insensitive coding", []string{"GZIP"}, true},
		{"gzip in a list", []string{"deflate, gzip"}, true},
		{"gzip on a later header line", []string{"deflate", "br, gzip;q=0.5"}, true},
		{"gzip refused", []string{"gzip;q=0"}, false},
		{"gzip refused with spaced q", []string{"gzip ; Q = 0.000"}, false},
		{"star", []string{"*"}, true},
		{"star refused", []string{"*;q=0"}, false},
		{"gzip refusal overrides star", []string{"gzip;q=0, *"}, false},
		{"gzip allowance overrides star refusal", []string{"*;q=0, gzip"}, true},
		{"malformed q refuses", []string{"gzip;q=high"}, false},
		{"other parameter is ignored", []string{"gzip;level=9"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptsGzip(tc.values); got != tc.want {
				t.Fatalf("acceptsGzip(%q) = %t, want %t", tc.values, got, tc.want)
			}
		})
	}
}
